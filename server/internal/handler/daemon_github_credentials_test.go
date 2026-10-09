package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/githubapp"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var githubClaimTestInstallationID atomic.Int64

func setupGitHubClaimTestBroker(t *testing.T, repoURLs ...string) {
	t.Helper()
	repositories := make([]string, 0, len(repoURLs))
	owners := make(map[string]struct{})
	for _, repoURL := range repoURLs {
		repository, err := githubapp.ParseRepository(repoURL)
		if err != nil {
			t.Fatal(err)
		}
		repositories = append(repositories, repository)
		owner := strings.SplitN(repository, "/", 2)[0]
		if _, exists := owners[owner]; exists {
			continue
		}
		owners[owner] = struct{}{}
		dbfx.Insert(t, "github_installation", testutil.Cols{
			"installation_id": 9_440_000 + githubClaimTestInstallationID.Add(1),
			"account_login":   owner,
			"account_type":    "Organization",
			"workspace_id":    testWorkspaceID,
		})
	}
	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_APP_ID", "943")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		repos := make([]map[string]string, 0, len(repositories))
		for _, repository := range repositories {
			repos = append(repos, map[string]string{"full_name": repository})
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "github-test-token", "repositories": repos})
	}))
	t.Cleanup(srv.Close)
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldBase })
}

func completeGitHubClaimForTest(t *testing.T, ack string) {
	t.Helper()
	if ack == "" || testHandler.takePendingGitHubClaim(ack) == nil {
		t.Fatalf("GitHub claim acknowledgement %q is not pending", ack)
	}
}

func TestGitHubInstallationForRepositoryMatchesOwnerExactly(t *testing.T) {
	installations := []db.GithubInstallation{
		{InstallationID: 11, AccountLogin: "other-owner"},
		{InstallationID: 22, AccountLogin: "Target-Owner"},
	}
	installation, err := githubInstallationForRepository(installations, "target-owner/shared-name")
	if err != nil {
		t.Fatal(err)
	}
	if installation.InstallationID != 22 {
		t.Fatalf("installation = %d, want 22", installation.InstallationID)
	}
	for _, repository := range []string{"missing/shared-name", "invalid"} {
		if _, err := githubInstallationForRepository(installations, repository); err == nil {
			t.Fatalf("repository %q unexpectedly matched an installation", repository)
		}
	}
}
