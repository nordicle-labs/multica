package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type timeoutError struct{}

func (timeoutError) Error() string   { return "transient timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

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

func TestHandleTaskDoesNotPublishWithoutExplicitDeliveryIntent(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(call string) { mu.Lock(); calls = append(calls, call); mu.Unlock() }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/github-credentials"):
			record("acquire")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["repository"] != "owner/repo" {
				t.Fatalf("credential request = %#v, %v", body, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"credentials":[]}`))
			return
		case r.Method == http.MethodDelete:
			record("revoke")
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"running"}`))
		case strings.HasSuffix(r.URL.Path, "/fail"):
			record("fail")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = srv.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	d := &Daemon{client: NewClient(srv.URL), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workspaces: make(map[string]*workspaceState), runtimeIndex: map[string]Runtime{"runtime-1": {ID: "runtime-1", Provider: "claude"}}, activeEnvRoots: make(map[string]int), cancelPollInterval: time.Hour, cfg: Config{WorkspacesRoot: t.TempDir()}}
	work := t.TempDir()
	gitTestRun(t, "", "init", "-b", "canonical", work)
	gitTestRun(t, work, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if err := os.WriteFile(filepath.Join(work, "delivery.txt"), []byte("delivery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, work, "add", "delivery.txt")
	gitTestRun(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "delivery")
	descriptor := deriveRepositoryDescriptor(context.Background(), work)
	d.runner = taskRunnerFunc(func(_ context.Context, task Task, _ string, _ int, _ *slog.Logger) (TaskResult, error) {
		record("run")
		if task.GitHubCredentials != nil || task.GitCredentialHelper != "" {
			t.Fatal("runner received GitHub credentials")
		}
		return TaskResult{Status: "completed", BranchName: descriptor.Branch, CommitSHA: descriptor.CommitSHA, TreeSHA: descriptor.TreeSHA, DurableWorkDir: work}, nil
	})
	d.handleTask(context.Background(), Task{ID: "task-1", RuntimeID: "runtime-1", Agent: &AgentData{Name: "test-agent"}, Repos: []RepoData{{URL: "https://github.com/owner/repo.git"}}}, 0)

	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if strings.Join(got, ",") != "run,fail" {
		t.Fatalf("delivery calls = %v, want [run fail]", got)
	}
}

func TestHandleTaskPreservesGitHubRepoArtifactWithoutPublishing(t *testing.T) {
	t.Setenv("IS_SANDBOX", "1")
	var acquired bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/github-credentials"):
			acquired = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"credentials":[]}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"running"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	fakeBin := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakeBin, []byte(`#!/bin/sh
git init . >/dev/null
git symbolic-ref HEAD refs/heads/canonical
git config user.name test
git config user.email test@example.com
git remote add origin https://github.com/owner/repo.git
printf 'delivery\n' > delivery.txt
git add delivery.txt
git commit -m delivery >/dev/null
IFS= read -r _
printf '%s\n' '{"type":"system","session_id":"session-delivery"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"session-delivery","result":"done"}'
`))
	workspacesRoot := t.TempDir()
	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"runtime-1": {ID: "runtime-1", Provider: "claude"}},
		activeEnvRoots:     make(map[string]int),
		cancelPollInterval: time.Hour,
		cfg: Config{
			WorkspacesRoot: workspacesRoot,
			AgentTimeout:   5 * time.Second,
			ServerBaseURL:  srv.URL,
			Agents:         map[string]AgentEntry{"claude": {Path: fakeBin}},
		},
	}
	d.runner = taskRunnerFunc(d.runTask)
	d.handleTask(context.Background(), Task{
		ID: "task-delivery", WorkspaceID: "workspace-1", RuntimeID: "runtime-1",
		IssueID: "issue-1", AgentID: "agent-1", AuthToken: "mat_task", RemoteMCPDaemonToken: "claim-token",
		Agent: &AgentData{ID: "agent-1", Name: "test-agent"},
		Repos: []RepoData{{URL: "https://github.com/owner/repo.git"}}, DeliveryIntent: "artifact",
	}, 0)

	if acquired {
		t.Fatal("handleTask acquired GitHub credentials for artifact-only delivery")
	}
	if _, err := os.Stat(filepath.Join(workspacesRoot, "recovery", "task-delivery", "recovery.bundle")); err != nil {
		t.Fatalf("artifact-only delivery bundle missing: %v", err)
	}
}

func TestHandleTaskPreservesInPlaceArtifactWithoutPublishing(t *testing.T) {
	t.Setenv("IS_SANDBOX", "1")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitBin := t.TempDir()
	writeTestExecutable(t, filepath.Join(gitBin, "git"), []byte(`#!/bin/sh
case " $* " in
  *" init -b "*|*" branch --show-current "*) exit 129 ;;
esac
exec "$REAL_GIT" "$@"
`))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", gitBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var acquired bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/github-credentials"):
			acquired = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"credentials":[]}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"running"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	repo := t.TempDir()
	gitTestRun(t, "", "init", repo)
	gitTestRun(t, repo, "checkout", "-b", "canonical")
	gitTestRun(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if err := os.WriteFile(filepath.Join(repo, "delivery.txt"), []byte("delivery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repo, "add", "delivery.txt")
	gitTestRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "delivery")
	localRef, err := json.Marshal(localDirectoryRef{LocalPath: repo, DaemonID: "daemon-1", ExecutionMode: localDirectoryModeInPlace})
	if err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(t.TempDir(), "claude")
	writeTestExecutable(t, fakeBin, []byte(`#!/bin/sh
IFS= read -r _
printf '%s\n' '{"type":"system","session_id":"session-delivery"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"session-delivery","result":"done"}'
`))
	d := &Daemon{
		client:             NewClient(srv.URL),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		workspaces:         make(map[string]*workspaceState),
		runtimeIndex:       map[string]Runtime{"runtime-1": {ID: "runtime-1", Provider: "claude"}},
		activeEnvRoots:     make(map[string]int),
		localPathLocks:     NewLocalPathLocker(),
		cancelPollInterval: time.Hour,
		cfg: Config{
			DaemonID:       "daemon-1",
			WorkspacesRoot: t.TempDir(),
			AgentTimeout:   5 * time.Second,
			ServerBaseURL:  srv.URL,
			Agents:         map[string]AgentEntry{"claude": {Path: fakeBin}},
		},
	}
	d.runner = taskRunnerFunc(d.runTask)
	d.handleTask(context.Background(), Task{
		ID: "task-in-place", WorkspaceID: "workspace-1", RuntimeID: "runtime-1",
		IssueID: "issue-1", AgentID: "agent-1", AuthToken: "mat_task", RemoteMCPDaemonToken: "claim-token",
		Agent: &AgentData{ID: "agent-1", Name: "test-agent"},
		Repos: []RepoData{{URL: "https://github.com/owner/repo.git"}}, DeliveryIntent: "artifact",
		ProjectResources: []ProjectResourceData{{
			ID: "local-1", ResourceType: localDirectoryResourceType,
			ResourceRef: localRef,
		}},
	}, 0)

	if acquired {
		t.Fatal("handleTask acquired GitHub credentials for artifact-only delivery")
	}
	if _, err := os.Stat(filepath.Join(d.cfg.WorkspacesRoot, "recovery", "task-in-place", "recovery.bundle")); err != nil {
		t.Fatalf("in-place artifact bundle missing: %v", err)
	}
}

func TestGitHubCredentialSessionRetriesServerFailure(t *testing.T) {
	attempts := 0
	waits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "token"}})
	session.revokeWait = func(context.Context, time.Duration) error { waits++; return nil }
	if err := session.close(context.Background()); err != nil || attempts != 2 {
		t.Fatalf("close error = %v, attempts = %d; want success after 2 attempts", err, attempts)
	}
	if waits != 1 {
		t.Fatalf("backoff calls = %d, want 1 injectable wait", waits)
	}
}

func TestGitHubCredentialSessionRetriesTransientTimeout(t *testing.T) {
	attempts := 0
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, timeoutError{}
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "token"}})
	session.revokeWait = func(context.Context, time.Duration) error { return nil }
	if err := session.close(context.Background()); err != nil || attempts != 2 {
		t.Fatalf("close error = %v, attempts = %d; want success after transient timeout", err, attempts)
	}
}

func TestGitHubCredentialSessionStopsAfterBoundedAttempts(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "token"}})
	session.revokeWait = func(context.Context, time.Duration) error { return nil }
	err := session.close(context.Background())
	if err == nil || attempts != 3 {
		t.Fatalf("close error = %v, attempts = %d; want explicit error after 3 attempts", err, attempts)
	}
}

func TestGitHubCredentialSessionRetriesAgainAfterExhaustion(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts <= githubCredentialRevocationAttempts {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "token"}})
	session.revokeWait = func(context.Context, time.Duration) error { return nil }
	if err := session.close(context.Background()); err == nil {
		t.Fatal("first close succeeded after every bounded attempt failed")
	}
	if err := session.close(context.Background()); err != nil || attempts != githubCredentialRevocationAttempts+1 {
		t.Fatalf("fallback close error = %v, attempts = %d; want success on first fallback attempt", err, attempts)
	}
}

func TestGitHubCredentialSessionStopsOnCancellation(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "token"}})
	err := session.close(ctx)
	if !errors.Is(err, context.Canceled) || attempts != 0 {
		t.Fatalf("close error = %v, attempts = %d; want context cancellation without request", err, attempts)
	}
}

func TestGitHubCredentialSessionDoesNotRevokeTwiceAfterSuccess(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	oldURL := githubInstallationTokenURL
	githubInstallationTokenURL = server.URL
	t.Cleanup(func() { githubInstallationTokenURL = oldURL })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "token"}})
	if err := session.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("revocation attempts = %d, want exactly 1 after repeated close", attempts)
	}
}

func TestAcquireGitHubCredentialsUsesClaimDaemonToken(t *testing.T) {
	const claimToken = "mdt_claim_secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+claimToken {
			t.Fatalf("Authorization = %q, want claim daemon token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentials":[]}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL)
	client.SetToken("mul_global_must_not_be_used")
	_, err := client.AcquireGitHubCredentials(context.Background(), Task{
		ID: "task-1", RuntimeID: "runtime-1", RemoteMCPDaemonToken: claimToken,
	}, "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireGitHubCredentialsNeverFallsBackToGlobalToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want no global-token fallback", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL)
	client.SetToken("mul_global_must_not_be_used")
	if _, err := client.AcquireGitHubCredentials(context.Background(), Task{ID: "recovery-task", RuntimeID: "runtime-1"}, "owner/repo"); err == nil {
		t.Fatal("AcquireGitHubCredentials succeeded without a claim daemon token")
	}
}

func TestClaimDaemonTokenStaysOutOfAgentPrompt(t *testing.T) {
	const claimToken = "mdt_claim_secret"
	prompt := buildPromptBody(Task{IssueID: "issue-1", RemoteMCPDaemonToken: claimToken}, "codex")
	if strings.Contains(prompt, claimToken) {
		t.Fatal("claim daemon token leaked into agent prompt")
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
	if out, err := exec.Command("git", "-C", work, "remote", "add", "origin", "https://github.com/owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %s: %v", out, err)
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
	tracePath := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", tracePath)

	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "secret-never-logged"}})
	if err != nil {
		t.Fatal(err)
	}
	remoteSHA, err := session.publish(context.Background(), work, "canonical", sha)
	if err != nil || remoteSHA != sha {
		t.Fatalf("publish = %q, %v; want %s", remoteSHA, err, sha)
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(trace), "--force-with-lease=refs/heads/canonical:") || strings.Contains(string(trace), "secret-never-logged") {
		t.Fatalf("git trace lacks lease or leaks token: %s", trace)
	}
}

func TestGitHubCredentialSessionRejectsCredentialForDifferentOrigin(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "init", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	if out, err := exec.Command("git", "-C", work, "remote", "add", "origin", "git@github.com:owner/source.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %s: %v", out, err)
	}
	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/other", Token: "secret-never-logged"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.publish(context.Background(), work, "canonical", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "repository mismatch") {
		t.Fatalf("publish error = %v, want repository mismatch", err)
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
	if out, err := exec.Command("git", "-C", work, "remote", "add", "origin", "https://github.com/owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %s: %v", out, err)
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

func TestGitHubCredentialSessionRecoversConcurrentPullRequestCreation(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			listCalls++
			if listCalls == 1 {
				_, _ = w.Write([]byte(`[]`))
			} else {
				_, _ = w.Write([]byte(`[{"number":9}]`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for owner:canonical."}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/9":
			_, _ = w.Write([]byte(`{"number":9,"head":{"ref":"canonical","sha":"` + sha + `"},"base":{"ref":"main"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	pr, err := session.ensurePullRequest(context.Background(), "canonical", sha)
	if err != nil || pr.Number != 9 || listCalls != 2 {
		t.Fatalf("pull request = %+v, list calls = %d, error = %v", pr, listCalls, err)
	}
}

func TestGitHubCredentialSessionRejectsUnrelatedValidationError(t *testing.T) {
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			listCalls++
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"base branch is invalid"}]}`))
	}))
	t.Cleanup(server.Close)
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	if _, err := session.ensurePullRequest(context.Background(), "canonical", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "status 422") {
		t.Fatalf("error = %v, want fail-closed 422", err)
	}
	if listCalls != 1 {
		t.Fatalf("list calls = %d, want no conflict retry", listCalls)
	}
}
