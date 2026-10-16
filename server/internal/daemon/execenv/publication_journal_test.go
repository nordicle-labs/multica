package execenv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveryJournalUsesBundleAfterOriginalRepositoryDisappears(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{
		TaskID: "bundle-only", DeliveryIntent: "publish", Repository: "owner/repo",
		GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha, HeadSHA: sha,
	}
	if err := WriteRecoveryManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRecoveryArtifact(repo, root, manifest.TaskID, sha, worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}

	recovered, err := MaterializeRecoveryRepository(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, recovered, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("recovered HEAD = %s, want %s", got, sha)
	}
}

func TestRecoveryJournalBoundsTransientFailures(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{TaskID: "failing", DeliveryIntent: "publish", Repository: "owner/repo", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha, HeadSHA: sha}
	if err := WriteRecoveryManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	for range maxRecoveryAttempts {
		if err := RecordRecoveryFailure(root, manifest.TaskID, errors.New("temporary outage")); err != nil {
			t.Fatal(err)
		}
	}
	got, exists, err := ReadRecoveryManifest(root, manifest.TaskID)
	if err != nil || !exists {
		t.Fatalf("read journal exists=%v err=%v", exists, err)
	}
	if got.PublicationPhase != PublicationBlocked || got.RecoveryAttempts != maxRecoveryAttempts || got.LastError == "" {
		t.Fatalf("bounded recovery journal = %+v", got)
	}
}

func TestRecoveryJournalDeferralDoesNotConsumeAttempts(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{TaskID: "leased", DeliveryIntent: "publish", Repository: "owner/repo", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha}
	if err := WriteRecoveryManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	retryAt := time.Date(2026, time.October, 15, 13, 5, 0, 0, time.UTC)
	for range maxRecoveryAttempts + 2 {
		if err := RecordRecoveryDeferral(root, manifest.TaskID, retryAt); err != nil {
			t.Fatal(err)
		}
	}
	got, exists, err := ReadRecoveryManifest(root, manifest.TaskID)
	if err != nil || !exists || got.RecoveryAttempts != 0 || !got.NextAttemptAt.Equal(retryAt) || got.PublicationPhase == PublicationBlocked {
		t.Fatalf("deferred recovery exists=%v err=%v manifest=%+v", exists, err, got)
	}
}

func TestRecoveryJournalBlockDoesNotConsumeAttempts(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{TaskID: "blocked", DeliveryIntent: "publish", Repository: "owner/repo", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha}
	if err := WriteRecoveryManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if err := RecordRecoveryBlock(root, manifest.TaskID, errors.New("publication credential expiry is unknown")); err != nil {
		t.Fatal(err)
	}
	got, exists, err := ReadRecoveryManifest(root, manifest.TaskID)
	if err != nil || !exists || got.RecoveryAttempts != 0 || got.PublicationPhase != PublicationBlocked || got.LastError == "" || !got.NextAttemptAt.IsZero() {
		t.Fatalf("blocked recovery exists=%v err=%v manifest=%+v", exists, err, got)
	}
}

func TestPublicationJournalPersistsDurableLifecycleWithoutCredential(t *testing.T) {
	repo := newTestRepo(t)
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	manifest := RecoveryManifest{TaskID: "publish-task", DeliveryIntent: "publish", GitRoot: repo, CanonicalBranch: "main", BaseSHA: sha, ExpectedRefSHA: sha, HeadSHA: sha}
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

func TestRecoveryPublicationClaimIsExclusive(t *testing.T) {
	root := t.TempDir()
	const taskID = "concurrent-publication"
	if err := os.MkdirAll(filepath.Join(root, taskID), 0o700); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	releaseFirst := make(chan struct{})
	var claimed atomic.Int32
	var attempted atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, ok, err := ClaimRecoveryPublication(root, taskID)
			attempted.Add(1)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if !ok {
				return
			}
			claimed.Add(1)
			<-releaseFirst
			release()
		}()
	}
	close(start)
	for attempted.Load() < 2 {
	}
	close(releaseFirst)
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("publication claims = %d, want exactly one", claimed.Load())
	}
}
