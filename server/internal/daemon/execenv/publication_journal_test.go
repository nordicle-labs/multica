package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationJournalPersistsDurableLifecycleWithoutCredential(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{TaskID: "publish-task", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha, HeadSHA: sha}
	if err := WriteRecoveryManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		phase  string
		remote string
		pr     int
		url    string
	}{
		{phase: PublicationCredentialMinted},
		{phase: PublicationBranchPublished, remote: sha},
		{phase: PublicationPRReadBack, remote: sha, pr: 42, url: "https://github.test/pull/42"},
		{phase: PublicationCredentialRevoked, remote: sha, pr: 42, url: "https://github.test/pull/42"},
	}
	for _, step := range steps {
		if err := AdvanceRecoveryManifest(root, manifest.TaskID, step.phase, step.remote, step.pr, step.url); err != nil {
			t.Fatalf("advance %s: %v", step.phase, err)
		}
	}
	loaded, err := LoadRecoveryManifests(root)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load journal = %+v, %v", loaded, err)
	}
	got := loaded[0]
	if got.PublicationPhase != PublicationCredentialRevoked || got.RemoteSHA != sha || got.PullRequestNumber != 42 {
		t.Fatalf("journal = %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(root, manifest.TaskID, recoveryManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghs_") || strings.Contains(string(data), "installation-secret") {
		t.Fatalf("journal persisted credential: %s", data)
	}
}
