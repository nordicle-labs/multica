package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var githubInstallationTokenURL = "https://api.github.com/installation/token"
var githubRepositoryRemote = func(repository string) string { return "https://github.com/" + repository + ".git" }

type githubCredentialSession struct {
	credentials []GitHubCredential
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

// apply strips claim-time secrets before the provider is created. Publication
// is performed by the host daemon after worktree finalization.
func (s *githubCredentialSession) apply(task *Task) {
	if s == nil || task == nil {
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
