package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			for _, args := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", work}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %s: %v", args, out, err)
				}
			}
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
	for _, args := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", work}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
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
