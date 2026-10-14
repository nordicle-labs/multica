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

const githubPublicationAttempts = 3
const githubCredentialRevocationAttempts = 3

type githubCredentialSession struct {
	credentials  []GitHubCredential
	revoked      []bool
	revokeClient *http.Client
	revokeWait   func(context.Context, time.Duration) error
}

type githubPullRequest struct {
	Number  int
	URL     string
	HeadSHA string
}

// GitHubPublication is the host-side CLI boundary. Token is supplied
// separately so it cannot enter argv or serialized request files.
type GitHubPublication struct {
	Repository string `json:"repository"`
	WorkDir    string `json:"workdir"`
	Branch     string `json:"branch"`
	SHA        string `json:"sha"`
	RemoteSHA  string `json:"remote_sha"`
	PRNumber   int    `json:"pr_number"`
	PRURL      string `json:"pr_url"`
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
	session := &githubCredentialSession{
		credentials:  append([]GitHubCredential(nil), credentials...),
		revoked:      make([]bool, len(credentials)),
		revokeClient: &http.Client{Timeout: 10 * time.Second},
		revokeWait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
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
	baseProbe := exec.CommandContext(ctx, "git", "-C", workDir, "ls-remote", remote, "refs/heads/main")
	baseProbe.Env = env
	if out, probeErr := baseProbe.CombinedOutput(); probeErr != nil {
		return "", fmt.Errorf("host publication base probe failed: %s: %w", strings.TrimSpace(string(out)), probeErr)
	} else if fields := strings.Fields(string(out)); len(fields) > 0 && fields[0] == sha {
		return "", errors.New("host publication refuses an empty pull request: delivery SHA equals main")
	}
	current := ""
	leaseProbe := exec.CommandContext(ctx, "git", "-C", workDir, "ls-remote", remote, "refs/heads/"+branch)
	leaseProbe.Env = env
	if out, probeErr := leaseProbe.CombinedOutput(); probeErr != nil {
		return "", fmt.Errorf("host publication lease probe failed: %s: %w", strings.TrimSpace(string(out)), probeErr)
	} else if fields := strings.Fields(string(out)); len(fields) > 0 {
		current = fields[0]
	}
	if current == sha {
		return sha, nil
	}
	push := exec.CommandContext(ctx, "git", "-C", workDir, "push", "--force-with-lease=refs/heads/"+branch+":"+current, remote, sha+":refs/heads/"+branch)
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
	return s.finalizePublicationWithProgress(ctx, workDir, branch, sha, nil)
}

func (s *githubCredentialSession) finalizePublicationWithProgress(ctx context.Context, workDir, branch, sha string, progress func(string, string, githubPullRequest) error) (string, githubPullRequest, error) {
	var remoteSHA string
	var pullRequest githubPullRequest
	var err error
	for attempt := 1; attempt <= githubPublicationAttempts; attempt++ {
		remoteSHA, err = s.publish(ctx, workDir, branch, sha)
		if err == nil && progress != nil {
			err = progress("branch_published", remoteSHA, githubPullRequest{})
		}
		if err == nil {
			pullRequest, err = s.ensurePullRequest(ctx, branch, sha)
		}
		if err == nil && progress != nil {
			err = progress("PR_read_back", remoteSHA, pullRequest)
		}
		if err == nil || !retryablePublicationError(err) || attempt == githubPublicationAttempts {
			return remoteSHA, pullRequest, err
		}
		select {
		case <-ctx.Done():
			return remoteSHA, pullRequest, ctx.Err()
		case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
		}
	}
	return remoteSHA, pullRequest, err
}

func retryablePublicationError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *githubAPIError
	if errors.As(err, &apiErr) {
		return apiErr.status == http.StatusTooManyRequests || apiErr.status >= http.StatusInternalServerError
	}
	message := strings.ToLower(err.Error())
	return !strings.Contains(message, "mismatch") && !strings.Contains(message, "requires") && !strings.Contains(message, "permission")
}

func PublishGitHubAppSHA(ctx context.Context, publication GitHubPublication, token string) (result GitHubPublication, err error) {
	if strings.TrimSpace(token) == "" {
		return result, errors.New("GitHub App installation token is required")
	}
	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: publication.Repository, Token: token}})
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, session.close(context.Background())) }()
	remoteSHA, pr, err := session.finalizePublication(ctx, publication.WorkDir, publication.Branch, publication.SHA)
	publication.RemoteSHA, publication.PRNumber, publication.PRURL = remoteSHA, pr.Number, pr.URL
	return publication, err
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
	var revokeErr error
	for index, credential := range s.credentials {
		if s.revoked[index] {
			continue
		}
		var err error
		for attempt := 1; attempt <= githubCredentialRevocationAttempts; attempt++ {
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
				break
			}
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, http.MethodDelete, githubInstallationTokenURL, bytes.NewReader(nil))
			if err == nil {
				req.Header.Set("Authorization", "token "+credential.Token)
				req.Header.Set("Accept", "application/vnd.github+json")
				var resp *http.Response
				resp, err = s.revokeClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
						s.revoked[index] = true
						break
					}
					err = fmt.Errorf("github credential revocation returned status %d", resp.StatusCode)
					if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < http.StatusInternalServerError {
						break
					}
				}
			}
			if attempt < githubCredentialRevocationAttempts {
				if waitErr := s.revokeWait(ctx, time.Duration(attempt)*25*time.Millisecond); waitErr != nil {
					err = errors.Join(err, waitErr)
					break
				}
			}
		}
		if !s.revoked[index] {
			if err == nil {
				err = errors.New("github credential revocation was not confirmed")
			}
			if !strings.Contains(err.Error(), "github credential revocation") {
				err = fmt.Errorf("github credential revocation failed: %w", err)
			}
			revokeErr = errors.Join(revokeErr, err)
		}
	}
	return revokeErr
}
