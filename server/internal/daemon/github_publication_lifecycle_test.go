package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitHubPublicationRetryExhaustionAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		wantCalls int
	}{
		{name: "bounded_exhaustion", wantCalls: githubPublicationAttempts},
		{name: "cancellation", cancel: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := filepath.Join(t.TempDir(), "remote.git")
			work := filepath.Join(t.TempDir(), "work")
			gitTestRun(t, "", "init", "--bare", remote)
			gitTestRun(t, "", "init", "-b", "main", work)
			if err := os.WriteFile(filepath.Join(work, "file"), []byte("content"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"-C", work, "add", "file"}, {"-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "test"}, {"-C", work, "remote", "add", "origin", "https://github.com/owner/repo.git"}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s: %v", args, out, err)
				}
			}
			shaBytes, _ := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
			sha := strings.TrimSpace(string(shaBytes))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if tc.cancel {
					cancel()
				}
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			oldAPI, oldRemote := githubAPIBaseURL, githubRepositoryRemote
			githubAPIBaseURL = server.URL
			githubRepositoryRemote = func(string) string { return remote }
			defer func() { githubAPIBaseURL, githubRepositoryRemote = oldAPI, oldRemote }()

			session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
			_, _, err := session.finalizePublication(ctx, work, "canonical", sha)
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context cancellation", err)
			}
			if err == nil || calls != tc.wantCalls {
				t.Fatalf("error = %v, API calls = %d, want %d", err, calls, tc.wantCalls)
			}
		})
	}
}

func TestGitHubPublicationRejectsEmptyPullRequest(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	work := filepath.Join(t.TempDir(), "work")
	gitTestRun(t, "", "init", "--bare", remote)
	gitTestRun(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", work, "add", "file"}, {"-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "test"}, {"-C", work, "push", remote, "HEAD:refs/heads/main"}, {"-C", work, "remote", "add", "origin", "https://github.com/owner/repo.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	shaBytes, _ := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output()
	oldRemote := githubRepositoryRemote
	githubRepositoryRemote = func(string) string { return remote }
	defer func() { githubRepositoryRemote = oldRemote }()
	session, _ := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	if _, err := session.publish(context.Background(), work, "canonical", strings.TrimSpace(string(shaBytes))); err == nil || !strings.Contains(err.Error(), "empty pull request") {
		t.Fatalf("empty delivery error = %v", err)
	}
}

func TestGitHubPublicationRecoversAfterLostPullRequestResponse(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	work := filepath.Join(t.TempDir(), "work")
	gitTestRun(t, "", "init", "--bare", remote)
	gitTestRun(t, "", "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "base"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, work, "add", "base")
	gitTestRun(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	gitTestRun(t, work, "push", remote, "HEAD:refs/heads/main")
	gitTestRun(t, work, "checkout", "-b", "delivery")
	if err := os.WriteFile(filepath.Join(work, "delivery"), []byte("delivery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, work, "add", "delivery")
	gitTestRun(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "delivery")
	gitTestRun(t, work, "remote", "add", "origin", "https://github.com/owner/repo.git")
	sha := gitTestRun(t, work, "rev-parse", "HEAD")

	var created atomic.Bool
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls":
			w.Header().Set("Content-Type", "application/json")
			if created.Load() {
				_, _ = w.Write([]byte(`[{"number":42}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls":
			creates.Add(1)
			created.Store(true)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server cannot drop response")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/42":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"number":42,"html_url":"https://github.test/pull/42","head":{"ref":"delivery","sha":%q},"base":{"ref":"main"}}`, sha)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldAPI, oldRemote := githubAPIBaseURL, githubRepositoryRemote
	githubAPIBaseURL = server.URL
	githubRepositoryRemote = func(string) string { return remote }
	defer func() { githubAPIBaseURL, githubRepositoryRemote = oldAPI, oldRemote }()

	session, err := startGitHubCredentialSession([]GitHubCredential{{Repository: "owner/repo", Token: "installation-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	remoteSHA, pr, err := session.finalizePublication(context.Background(), work, "delivery", sha)
	if err != nil {
		t.Fatal(err)
	}
	if remoteSHA != sha || pr.Number != 42 || pr.HeadSHA != sha || creates.Load() != 1 {
		t.Fatalf("publication = remote %s PR %+v creates %d", remoteSHA, pr, creates.Load())
	}
}
