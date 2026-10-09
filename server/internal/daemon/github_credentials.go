package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var githubInstallationTokenURL = "https://api.github.com/installation/token"

type githubCredentialSession struct {
	helper string
	dir    string
	tokens []string
}

func startGitHubCredentialSession(credentials []GitHubCredential) (*githubCredentialSession, error) {
	if len(credentials) == 0 {
		return nil, nil
	}
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("GitHub App credentials require git credential-cache")
	}
	dir, err := os.MkdirTemp("", "multica-git-credentials-")
	if err != nil {
		return nil, err
	}
	session := &githubCredentialSession{
		helper: "cache --timeout=3600 --socket=" + filepath.Join(dir, "socket"),
		dir:    dir,
	}
	for _, credential := range credentials {
		if credential.Token == "" || credential.Repository == "" {
			if credential.Token != "" {
				session.tokens = append(session.tokens, credential.Token)
			}
			session.close(context.Background())
			return nil, fmt.Errorf("invalid GitHub App credential")
		}
		session.tokens = append(session.tokens, credential.Token)
		input := credentialInput(credential.Repository, credential.Token)
		cmd := exec.Command("git", "credential-cache", "--socket", filepath.Join(dir, "socket"), "store")
		cmd.Stdin = strings.NewReader(input)
		if out, err := cmd.CombinedOutput(); err != nil {
			session.close(context.Background())
			return nil, fmt.Errorf("store GitHub App credential: %s", strings.TrimSpace(string(out)))
		}
	}
	return session, nil
}

func credentialInput(repository, token string) string {
	return "protocol=https\nhost=github.com\npath=" + repository + ".git\nusername=x-access-token\npassword=" + token + "\n\n"
}

func (s *githubCredentialSession) apply(task *Task) {
	if s == nil || task.Agent == nil {
		return
	}
	if task.Agent.CustomEnv == nil {
		task.Agent.CustomEnv = map[string]string{}
	}
	task.Agent.CustomEnv["GIT_CONFIG_COUNT"] = "3"
	task.Agent.CustomEnv["GIT_CONFIG_KEY_0"] = "credential.helper"
	task.Agent.CustomEnv["GIT_CONFIG_VALUE_0"] = ""
	task.Agent.CustomEnv["GIT_CONFIG_KEY_1"] = "credential.helper"
	task.Agent.CustomEnv["GIT_CONFIG_VALUE_1"] = s.helper
	task.Agent.CustomEnv["GIT_CONFIG_KEY_2"] = "credential.useHttpPath"
	task.Agent.CustomEnv["GIT_CONFIG_VALUE_2"] = "true"
	task.GitCredentialHelper = s.helper
	task.GitHubCredentials = nil
}

func (s *githubCredentialSession) close(ctx context.Context) {
	if s == nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, token := range s.tokens {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, githubInstallationTokenURL, bytes.NewReader(nil))
		if err == nil {
			req.Header.Set("Authorization", "token "+token)
			req.Header.Set("Accept", "application/vnd.github+json")
			if resp, callErr := client.Do(req); callErr == nil {
				_ = resp.Body.Close()
			}
		}
	}
	cmd := exec.Command("git", "credential-cache", "--socket", filepath.Join(s.dir, "socket"), "exit")
	_ = cmd.Run()
	_ = os.RemoveAll(s.dir)
}
