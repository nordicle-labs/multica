package handler

import (
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

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
