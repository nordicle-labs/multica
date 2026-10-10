package execenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestResumeRecoveryManifestsBeforeGC(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	base := gitRun(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "after-restart.txt"), "durable\n")
	gitRun(t, repo, "add", "after-restart.txt")
	gitRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "durable")
	head := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{
		TaskID: "restart-task", GitRoot: repo, CanonicalBranch: "main",
		BaseSHA: base, ExpectedRefSHA: base, HeadSHA: head,
	}
	if err := WriteRecoveryManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "restart-task", recoveryManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(data)), "token") || strings.Contains(strings.ToLower(string(data)), "credential") {
		t.Fatalf("recovery manifest contains credential material: %s", data)
	}
	resumed, err := ResumeRecoveryManifests(root, worktreeTestLogger())
	if err != nil || resumed != 1 {
		t.Fatalf("resume = %d, %v", resumed, err)
	}
	if got := gitRun(t, repo, "rev-parse", "refs/multica/recovery/restart-task"); got != head {
		t.Fatalf("recovery ref = %s, want %s", got, head)
	}
	bundle := filepath.Join(root, "restart-task", "recovery.bundle")
	if out, err := runGit(repo, "bundle", "verify", bundle); err != nil {
		t.Fatalf("verify resumed bundle: %s: %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "restart-task", recoveryManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest still present after resume: %v", err)
	}
}

func TestWriteRecoveryManifestSyncsFileAndDirectory(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	manifest := RecoveryManifest{TaskID: "durable-task", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha}
	syncs := 0
	if err := writeRecoveryManifest(t.TempDir(), manifest, func(*os.File) error {
		syncs++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if syncs != 2 {
		t.Fatalf("sync calls = %d, want temporary file and containing directory", syncs)
	}
}

func TestWriteRecoveryManifestFailsClosedOnSyncError(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("sync_%d", failAt), func(t *testing.T) {
			repo := newTestRepo(t)
			sha := gitRun(t, repo, "rev-parse", "HEAD")
			manifest := RecoveryManifest{TaskID: "sync-failure", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha}
			want := errors.New("sync failed")
			calls := 0
			err := writeRecoveryManifest(t.TempDir(), manifest, func(*os.File) error {
				calls++
				if calls == failAt {
					return want
				}
				return nil
			})
			if !errors.Is(err, want) {
				t.Fatalf("writeRecoveryManifest error = %v, want sync failure", err)
			}
		})
	}
}
