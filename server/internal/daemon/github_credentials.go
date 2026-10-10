package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/githubapp"
)

var githubInstallationTokenURL = "https://api.github.com/installation/token"
var githubAPIBaseURL = "https://api.github.com"
var githubRepositoryRemote = func(repository string) string { return "https://github.com/" + repository + ".git" }

type githubCredentialSession struct {
	credentials []GitHubCredential
}

type githubPullRequest struct {
	Number  int
	URL     string
	HeadSHA string
}

type githubAPIError struct {
	status int
	body   string
}

func (e *githubAPIError) Error() string {
	return fmt.Sprintf("GitHub pull request API returned status %d", e.status)
}

func githubRepositoryForWorkDir(workDir string) (string, error) {
	out, err := exec.Command("git", "-C", workDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", fmt.Errorf("resolve repository origin: %w", err)
	}
	repository, err := githubapp.ParseRepository(strings.TrimSpace(string(out)))
	if err != nil {
		return "", fmt.Errorf("resolve repository origin: %w", err)
	}
	return repository, nil
}

func startGitHubCredentialSession(credentials []GitHubCredential) (*githubCredentialSession, error) {
	if len(credentials) == 0 {
		return nil, nil
	}
	session := &githubCredentialSession{credentials: append([]GitHubCredential(nil), credentials...)}
	for _, credential := range session.credentials {
		if credential.Token == "" || credential.Repository == "" {
			_ = session.close(context.Background())
			return nil, errors.New("invalid GitHub App credential")
		}
	}
	return session, nil
}

// stripGitHubCredentials removes ambient or legacy claim-time Git credentials
// before provider context construction. V3 claims contain no token at all.
func stripGitHubCredentials(task *Task) {
	if task == nil {
		return
	}
	task.GitHubCredentials = nil
	task.GitCredentialHelper = ""
	if task.Agent == nil {
		return
	}
	for key := range task.Agent.CustomEnv {
		if key == "GITHUB_TOKEN" || strings.HasPrefix(key, "GIT_CONFIG_") || key == "GIT_ASKPASS" {
			delete(task.Agent.CustomEnv, key)
		}
	}
}

// publish pushes exactly sha to the canonical branch and then proves the remote
// ref resolves to that same SHA. The token exists only in the git child process
// environment; it is never placed in argv, a config file, or a helper cache.
func (s *githubCredentialSession) publish(ctx context.Context, workDir, branch, sha string) (string, error) {
	if s == nil || len(s.credentials) != 1 {
		return "", errors.New("host publication requires exactly one repository credential")
	}
	if workDir == "" || branch == "" || sha == "" {
		return "", errors.New("host publication requires workdir, canonical branch, and SHA")
	}
	credential := s.credentials[0]
	repository, err := githubRepositoryForWorkDir(workDir)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(repository, credential.Repository) {
		return "", fmt.Errorf("GitHub App credential repository mismatch: origin is %s, credential is for %s", repository, credential.Repository)
	}
	taskDir, err := os.MkdirTemp("", "multica-git-publish-")
	if err != nil {
		return "", fmt.Errorf("prepare host publication: %w", err)
	}
	defer os.RemoveAll(taskDir)
	taskpass := filepath.Join(taskDir, "askpass.sh")
	if err := os.WriteFile(taskpass, []byte("#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' x-access-token ;; *) printf '%s\\n' \"$MULTICA_GIT_TOKEN\" ;; esac\n"), 0o700); err != nil {
		return "", fmt.Errorf("prepare host publication: %w", err)
	}
	remote := githubRepositoryRemote(credential.Repository)
	env := append(os.Environ(), "GIT_ASKPASS="+taskpass, "GIT_TERMINAL_PROMPT=0", "MULTICA_GIT_TOKEN="+credential.Token)
	push := exec.CommandContext(ctx, "git", "-C", workDir, "push", remote, sha+":refs/heads/"+branch)
	push.Env = env
	if out, err := push.CombinedOutput(); err != nil {
		return "", fmt.Errorf("host publication push failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	probe := exec.CommandContext(ctx, "git", "-C", workDir, "ls-remote", remote, "refs/heads/"+branch)
	probe.Env = env
	out, err := probe.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("host publication remote verification failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != sha {
		remoteSHA := ""
		if len(fields) > 0 {
			remoteSHA = fields[0]
		}
		return remoteSHA, fmt.Errorf("host publication remote SHA mismatch: got %s, want %s", remoteSHA, sha)
	}
	return fields[0], nil
}

func (s *githubCredentialSession) finalizePublication(ctx context.Context, workDir, branch, sha string) (string, githubPullRequest, error) {
	remoteSHA, err := s.publish(ctx, workDir, branch, sha)
	if err != nil {
		return remoteSHA, githubPullRequest{}, err
	}
	pullRequest, err := s.ensurePullRequest(ctx, branch, sha)
	return remoteSHA, pullRequest, err
}

func (s *githubCredentialSession) ensurePullRequest(ctx context.Context, branch, sha string) (githubPullRequest, error) {
	if s == nil || len(s.credentials) != 1 {
		return githubPullRequest{}, errors.New("host pull request requires exactly one repository credential")
	}
	credential := s.credentials[0]
	parts := strings.Split(credential.Repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || branch == "" || sha == "" {
		return githubPullRequest{}, errors.New("host pull request requires repository, canonical branch, and SHA")
	}
	repoPath := "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/pulls"
	query := url.Values{"state": {"open"}, "head": {parts[0] + ":" + branch}, "base": {"main"}, "per_page": {"1"}}
	findOpen := func() (int, error) {
		var open []struct {
			Number int `json:"number"`
		}
		if err := githubAPI(ctx, credential.Token, http.MethodGet, repoPath+"?"+query.Encode(), nil, &open); err != nil {
			return 0, err
		}
		if len(open) == 0 {
			return 0, nil
		}
		return open[0].Number, nil
	}
	number, err := findOpen()
	if err != nil {
		return githubPullRequest{}, err
	}
	if number == 0 {
		var created struct {
			Number int `json:"number"`
		}
		body := map[string]string{
			"head":  branch,
			"base":  "main",
			"title": boundedGitHubText("Deliver "+branch, 256),
			"body":  boundedGitHubText("Automated host-side delivery of `"+branch+"` at `"+sha+"`.", 1024),
		}
		if err := githubAPI(ctx, credential.Token, http.MethodPost, repoPath, body, &created); err != nil {
			var apiErr *githubAPIError
			if !errors.As(err, &apiErr) || apiErr.status != http.StatusUnprocessableEntity || !strings.Contains(strings.ToLower(apiErr.body), "pull request already exists") {
				return githubPullRequest{}, err
			}
			number, err = findOpen()
			if err != nil {
				return githubPullRequest{}, err
			}
			if number == 0 {
				return githubPullRequest{}, errors.New("concurrent GitHub pull request was not found after creation conflict")
			}
		} else {
			number = created.Number
		}
	}
	if number <= 0 {
		return githubPullRequest{}, errors.New("GitHub pull request response omitted number")
	}
	var verified struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := githubAPI(ctx, credential.Token, http.MethodGet, repoPath+"/"+strconv.Itoa(number), nil, &verified); err != nil {
		return githubPullRequest{}, err
	}
	if verified.Head.Ref != branch || verified.Base.Ref != "main" || verified.Head.SHA != sha {
		return githubPullRequest{}, fmt.Errorf("GitHub pull request mismatch: head=%s sha=%s base=%s, want head=%s sha=%s base=main", verified.Head.Ref, verified.Head.SHA, verified.Base.Ref, branch, sha)
	}
	return githubPullRequest{Number: verified.Number, URL: verified.HTMLURL, HeadSHA: verified.Head.SHA}, nil
}

func githubAPI(ctx context.Context, token, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(githubAPIBaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("GitHub pull request API failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return errors.New("GitHub App installation lacks Pull requests permission")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &githubAPIError{status: resp.StatusCode, body: string(data)}
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode GitHub pull request response: %w", err)
	}
	return nil
}

func boundedGitHubText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (s *githubCredentialSession) close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var revokeErr error
	for _, credential := range s.credentials {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, githubInstallationTokenURL, bytes.NewReader(nil))
		if err == nil {
			req.Header.Set("Authorization", "token "+credential.Token)
			req.Header.Set("Accept", "application/vnd.github+json")
			if resp, callErr := client.Do(req); callErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
					revokeErr = errors.Join(revokeErr, fmt.Errorf("github credential revocation returned status %d", resp.StatusCode))
				}
			} else {
				revokeErr = errors.Join(revokeErr, fmt.Errorf("github credential revocation failed: %w", callErr))
			}
		} else {
			revokeErr = errors.Join(revokeErr, fmt.Errorf("github credential revocation request failed: %w", err))
		}
	}
	return revokeErr
}
