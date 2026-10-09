package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitHubCredentialSessionKeepsTokenOutOfEnvironmentAndDisk(t *testing.T) {
	versionOut, err := exec.Command("git", "version").Output()
	if err != nil || !gitVersionAtLeast(string(versionOut), 2, 31) {
		t.Skip("credential isolation requires Git >= 2.31")
	}

	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revoked = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	const token = "ghs_test_secret_token"
	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: token}})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{Agent: &AgentData{}, GitHubCredentials: []GitHubCredential{{Repository: "owner/repo", Token: token}}}
	session.apply(&task)
	if task.GitHubCredentials != nil {
		t.Fatal("raw GitHub token remained on the task after credential-helper setup")
	}
	for key, value := range task.Agent.CustomEnv {
		if strings.Contains(key, token) || strings.Contains(value, token) || key == "GITHUB_TOKEN" {
			t.Fatalf("token leaked through environment %q", key)
		}
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n	helper = !f() { echo username=personal; echo password=personal-token; }; f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Env = append(cmd.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1")
	for key, value := range task.Agent.CustomEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=owner/repo.git\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password="+token) {
		t.Fatalf("Git credential helper did not override the inherited personal helper: %s", out)
	}

	session.close(context.Background())
	if !revoked {
		t.Fatal("installation token was not revoked")
	}
}

func TestHandleTaskAcknowledgesGitHubCredentialAfterSetupAndRevokesOnLocalFailure(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(call string) {
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim-ack"):
			record("ack")
		case r.Method == http.MethodDelete:
			record("revoke")
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"running"}`))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = srv.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"runtime-1": {ID: "runtime-1", Provider: "claude"}},
		activeEnvRoots:     make(map[string]int),
		cancelPollInterval: time.Hour,
		cfg:                Config{WorkspacesRoot: t.TempDir()},
	}
	d.runner = taskRunnerFunc(func(_ context.Context, task Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		record("run")
		if task.GitHubCredentials != nil || task.GitCredentialHelper == "" {
			t.Fatal("runner received credentials before helper setup")
		}
		return TaskResult{}, errors.New("local launch failed")
	})

	d.handleTask(context.Background(), Task{
		ID:                  "task-1",
		RuntimeID:           "runtime-1",
		Agent:               &AgentData{Name: "test-agent"},
		GitHubCredentials:   []GitHubCredential{{Repository: "owner/repo", Token: "ghs_test_secret"}},
		GitHubCredentialAck: "ack-1",
	}, 0)

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if strings.Join(got, ",") != "ack,run,revoke" {
		t.Fatalf("credential lifecycle calls = %v, want [ack run revoke]", got)
	}
}

func TestGitHubCredentialSessionRevokesTokenWhenHelperSetupFails(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })
	t.Setenv("PATH", t.TempDir())

	if _, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "ghs_must_revoke"}}); err == nil {
		t.Fatal("credential session succeeded without git")
	}
	if len(methods) != 1 || methods[0] != http.MethodDelete {
		t.Fatalf("revocation methods = %v, want [DELETE]", methods)
	}
}
