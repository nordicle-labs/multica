package execenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateRecoveryArtifactPinsSHAAndVerifiesBundle(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	artifact, err := CreateRecoveryArtifact(repo, t.TempDir(), "task-123", sha, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Ref != "refs/multica/recovery/task-123" || !artifact.BundleVerified {
		t.Fatalf("artifact = %+v", artifact)
	}
	if got := gitRun(t, repo, "rev-parse", artifact.Ref); got != sha {
		t.Fatalf("recovery ref = %s, want %s", got, sha)
	}
	info, err := os.Stat(artifact.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || filepath.Base(artifact.BundlePath) != "recovery.bundle" {
		t.Fatalf("bundle mode/path = %o %s", info.Mode().Perm(), artifact.BundlePath)
	}
}
