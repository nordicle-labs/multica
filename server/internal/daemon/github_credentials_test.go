package daemon

import (
	"context"
	"encoding/json"
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

func TestGitHubCredentialSessionKeepsTokenOutOfProviderAndRevokes(t *testing.T) {
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
	task := Task{Agent: &AgentData{CustomEnv: map[string]string{"GITHUB_TOKEN": "ambient"}}, GitHubCredentials: []GitHubCredential{{Repository: "owner/repo", Token: token}}}
	stripGitHubCredentials(&task)
	if task.GitHubCredentials != nil || task.GitCredentialHelper != "" {
		t.Fatal("GitHub credential reached provider task state")
	}
	for key, value := range task.Agent.CustomEnv {
		if strings.Contains(key, token) || strings.Contains(value, token) || key == "GITHUB_TOKEN" {
			t.Fatalf("token leaked through environment %q", key)
		}
	}
	if err := session.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("installation token was not revoked")
	}
}

func TestHandleTaskDoesNotAcquireGitHubCredentialBeforeRunnerReturns(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(call string) { mu.Lock(); calls = append(calls, call); mu.Unlock() }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/github-credentials"):
			record("acquire")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"credentials":[]}`))
			return
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

	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workspaces: make(map[string]*workspaceState), runtimeIndex: map[string]Runtime{"runtime-1": {ID: "runtime-1", Provider: "claude"}}, activeEnvRoots: make(map[string]int), cancelPollInterval: time.Hour, cfg: Config{WorkspacesRoot: t.TempDir()}}
	d.runner = taskRunnerFunc(func(_ context.Context, task Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		record("run")
		if task.GitHubCredentials != nil || task.GitCredentialHelper != "" {
			t.Fatal("runner received GitHub credentials")
		}
		return TaskResult{BranchName: "canonical", CommitSHA: strings.Repeat("a", 40), DurableWorkDir: t.TempDir()}, nil
	})
	d.handleTask(context.Background(), Task{ID: "task-1", RuntimeID: "runtime-1", Agent: &AgentData{Name: "test-agent"}, Repos: []RepoData{{URL: "https://github.com/owner/repo.git"}}}, 0)

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if strings.Join(got, ",") != "run,acquire" {
		t.Fatalf("credential lifecycle calls = %v, want [run acquire]", got)
	}
}

func TestGitHubCredentialSessionRevokesTokenWhenCredentialIsInvalid(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })
	if _, err := startGitHubCredentialSession([]GitHubCredential{{Token: "ghs_must_revoke"}}); err == nil {
		t.Fatal("credential session succeeded with invalid repository")
	}
	if len(methods) != 1 || methods[0] != http.MethodDelete {
		t.Fatalf("revocation methods = %v, want [DELETE]", methods)
	}
}

func TestGitHubCredentialSessionPublishesExactSHAAndVerifiesRemote(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	work := filepath.Join(t.TempDir(), "work")
	for _, cmd := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", work}} {
		if out, err := exec.Command("git", cmd...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", cmd, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", work, "add", "file.txt"}, {"-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	shaBytes, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	oldRemote := githubRepositoryRemote
	githubRepositoryRemote = func(string) string { return remote }
	t.Cleanup(func() { githubRepositoryRemote = oldRemote })

	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "secret-never-logged"}})
	if err != nil {
		t.Fatal(err)
	}
	remoteSHA, err := session.publish(context.Background(), work, "canonical", sha)
	if err != nil || remoteSHA != sha {
		t.Fatalf("publish = %q, %v; want %s", remoteSHA, err, sha)
	}
}

func TestGitHubCredentialSessionCreatesAndVerifiesPullRequest(t *testing.T) {
	const branch = "agent/engineering-developer/herm-959"
	remote := filepath.Join(t.TempDir(), "remote.git")
	work := filepath.Join(t.TempDir(), "work")
	for _, cmd := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", work}} {
		if out, err := exec.Command("git", cmd...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", cmd, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", work, "add", "file.txt"}, {"-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	shaBytes, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaBytes))
	oldRemote := githubRepositoryRemote
	githubRepositoryRemote = func(string) string { return remote }
	t.Cleanup(func() { githubRepositoryRemote = oldRemote })

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer installation-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["head"] != branch || body["base"] != "main" || body["title"] == "" || body["body"] == "" {
				t.Fatalf("create body = %#v", body)
			}
			_, _ = w.Write([]byte(`{"number":42}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/42":
			_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/owner/repo/pull/42","head":{"ref":"` + branch + `","sha":"` + sha + `"},"base":{"ref":"main"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	remoteSHA, pr, err := session.finalizePublication(context.Background(), work, branch, sha)
	if err != nil {
		t.Fatal(err)
	}
	if remoteSHA != sha || pr.Number != 42 || pr.HeadSHA != sha {
		t.Fatalf("remote SHA = %q, pull request = %+v", remoteSHA, pr)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %v", requests)
	}
}

func TestGitHubCredentialSessionReusesOpenPullRequest(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/owner/repo/pulls" {
			_, _ = w.Write([]byte(`[{"number":7}]`))
			return
		}
		_, _ = w.Write([]byte(`{"number":7,"head":{"ref":"canonical","sha":"` + sha + `"},"base":{"ref":"main"}}`))
	}))
	t.Cleanup(server.Close)
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	pr, err := session.ensurePullRequest(context.Background(), "canonical", sha)
	if err != nil || pr.Number != 7 {
		t.Fatalf("pull request = %+v, %v", pr, err)
	}
	if strings.Join(methods, ",") != "GET,GET" {
		t.Fatalf("methods = %v, want no POST", methods)
	}
}

func TestGitHubCredentialSessionRejectsWrongPullRequestSHA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/owner/repo/pulls" {
			_, _ = w.Write([]byte(`[{"number":7}]`))
			return
		}
		_, _ = w.Write([]byte(`{"number":7,"head":{"ref":"canonical","sha":"wrong"},"base":{"ref":"main"}}`))
	}))
	t.Cleanup(server.Close)
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	if _, err := session.ensurePullRequest(context.Background(), "canonical", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "pull request mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func TestGitHubCredentialSessionReportsMissingPullRequestPermission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	if _, err := session.ensurePullRequest(context.Background(), "canonical", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "lacks Pull requests permission") {
		t.Fatalf("error = %v", err)
	}
}
