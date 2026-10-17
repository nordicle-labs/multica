package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	recoveryManifestName         = "manifest.json"
	recoveryPublicationLockName  = ".publication.lock"
	maxRecoveryManifests         = 100
	PublicationPrepared          = "prepared"
	PublicationCredentialMinted  = "credential_minted"
	PublicationBranchPublished   = "branch_published"
	PublicationPRReadBack        = "PR_read_back"
	PublicationCredentialRevoked = "credential_revoked"
	PublicationBlocked           = "blocked"
	maxRecoveryAttempts          = 5
)

// ClaimRecoveryPublication serializes publication attempts for one durable
// artifact. The advisory lock is released automatically if the daemon exits.
func ClaimRecoveryPublication(root, taskID string) (release func(), claimed bool, err error) {
	if _, err := recoveryTaskDir(root, taskID); err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, false, err
	}
	lock, err := openLockFile(filepath.Join(root, "."+taskID+recoveryPublicationLockName))
	if err != nil {
		return nil, false, err
	}
	claimed, err = lockFileExclusiveNonBlocking(lock)
	if err != nil || !claimed {
		_ = lock.Close()
		return nil, claimed, err
	}
	return func() { releaseLockFile(lock) }, true, nil
}

// RecoveryManifest is the non-secret durable journal written before provider
// launch. A restart can preserve a finalized commit even if publication never
// ran.
type RecoveryManifest struct {
	TaskID            string    `json:"task_id"`
	RuntimeID         string    `json:"runtime_id,omitempty"`
	DeliveryIntent    string    `json:"delivery_intent"`
	Repository        string    `json:"repository,omitempty"`
	GitRoot           string    `json:"git_root"`
	CanonicalBranch   string    `json:"canonical_branch"`
	BaseSHA           string    `json:"base_sha"`
	ExpectedRefSHA    string    `json:"expected_ref_sha"`
	HeadSHA           string    `json:"head_sha,omitempty"`
	PublicationPhase  string    `json:"publication_phase"`
	RemoteSHA         string    `json:"remote_sha,omitempty"`
	PullRequestNumber int       `json:"pull_request_number,omitempty"`
	PullRequestURL    string    `json:"pull_request_url,omitempty"`
	RecoveryAttempts  int       `json:"recovery_attempts,omitempty"`
	NextAttemptAt     time.Time `json:"next_attempt_at,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
}

// RecordRecoveryFailure persists bounded retry state without storing a token.
func RecordRecoveryFailure(root, taskID string, failure error) error {
	manifest, exists, err := ReadRecoveryManifest(root, taskID)
	if err != nil || !exists {
		return err
	}
	manifest.RecoveryAttempts++
	manifest.LastError = failure.Error()
	if len(manifest.LastError) > 1024 {
		manifest.LastError = manifest.LastError[:1024]
	}
	if manifest.RecoveryAttempts >= maxRecoveryAttempts {
		manifest.PublicationPhase = PublicationBlocked
		manifest.NextAttemptAt = time.Time{}
	} else {
		manifest.NextAttemptAt = time.Now().UTC().Add(time.Duration(manifest.RecoveryAttempts) * time.Minute)
	}
	return WriteRecoveryManifest(root, manifest)
}

// RecordRecoveryDeferral schedules a retry without consuming a publication
// attempt. The server uses this while an earlier credential lease is live.
func RecordRecoveryDeferral(root, taskID string, retryAt time.Time) error {
	manifest, exists, err := ReadRecoveryManifest(root, taskID)
	if err != nil || !exists {
		return err
	}
	manifest.NextAttemptAt = retryAt.UTC()
	return WriteRecoveryManifest(root, manifest)
}

// RecordRecoveryBlock preserves a definitive server refusal without turning it
// into a retry attempt. An operator must resolve the durable reason explicitly.
func RecordRecoveryBlock(root, taskID string, failure error) error {
	manifest, exists, err := ReadRecoveryManifest(root, taskID)
	if err != nil || !exists {
		return err
	}
	manifest.PublicationPhase = PublicationBlocked
	manifest.NextAttemptAt = time.Time{}
	manifest.LastError = failure.Error()
	if len(manifest.LastError) > 1024 {
		manifest.LastError = manifest.LastError[:1024]
	}
	return WriteRecoveryManifest(root, manifest)
}

func RecoveryRetryReady(manifest RecoveryManifest, now time.Time) bool {
	return manifest.PublicationPhase != PublicationBlocked && (manifest.NextAttemptAt.IsZero() || !now.Before(manifest.NextAttemptAt))
}

// MaterializeRecoveryRepository reconstructs an immutable publication checkout
// from the verified bundle, so retry does not depend on the mutable source tree.
func MaterializeRecoveryRepository(root string, manifest RecoveryManifest) (string, error) {
	dir, err := recoveryTaskDir(root, manifest.TaskID)
	if err != nil {
		return "", err
	}
	bundle := filepath.Join(dir, "recovery.bundle")
	if _, err := os.Stat(bundle); err != nil {
		return "", fmt.Errorf("recovery bundle unavailable: %w", err)
	}
	checkout := filepath.Join(dir, "repository")
	if err := os.RemoveAll(checkout); err != nil {
		return "", err
	}
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return "", err
	}
	if out, err := runGit(checkout, "init"); err != nil {
		return "", fmt.Errorf("initialize recovery repository: %s: %w", strings.TrimSpace(out), err)
	}
	if out, err := runGit(checkout, "fetch", bundle, "refs/multica/recovery/"+manifest.TaskID); err != nil {
		return "", fmt.Errorf("fetch recovery bundle: %s: %w", strings.TrimSpace(out), err)
	}
	if out, err := runGit(checkout, "checkout", "-B", manifest.CanonicalBranch, manifest.HeadSHA); err != nil {
		return "", fmt.Errorf("checkout recovery bundle: %s: %w", strings.TrimSpace(out), err)
	}
	if manifest.Repository != "" {
		if out, err := runGit(checkout, "remote", "add", "origin", "https://github.com/"+manifest.Repository+".git"); err != nil {
			return "", fmt.Errorf("configure recovery repository: %s: %w", strings.TrimSpace(out), err)
		}
	}
	return checkout, nil
}

func recoveryTaskDir(root, taskID string) (string, error) {
	if root == "" || taskID == "" || filepath.Base(taskID) != taskID || strings.ContainsAny(taskID, `/\\`) {
		return "", errors.New("invalid recovery root or task id")
	}
	return filepath.Join(root, taskID), nil
}

// WriteRecoveryManifest atomically creates or updates a task journal.
func WriteRecoveryManifest(root string, manifest RecoveryManifest) error {
	return writeRecoveryManifest(root, manifest, func(file *os.File) error { return file.Sync() })
}

// AdvanceRecoveryManifest records non-secret publication evidence after each
// completed side effect. The installation token is never part of the journal.
func AdvanceRecoveryManifest(root, taskID, phase, remoteSHA string, prNumber int, prURL string) error {
	dir, err := recoveryTaskDir(root, taskID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, recoveryManifestName))
	if err != nil {
		return err
	}
	var manifest RecoveryManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	order := map[string]int{
		PublicationPrepared: 0, PublicationCredentialMinted: 1,
		PublicationBranchPublished: 2, PublicationPRReadBack: 3,
		PublicationCredentialRevoked: 4,
	}
	current := manifest.PublicationPhase
	if current == "" {
		current = PublicationPrepared
	}
	if _, ok := order[phase]; !ok || order[phase] > order[current]+1 {
		return fmt.Errorf("invalid publication phase transition %s -> %s", current, phase)
	}
	if order[phase] < order[current] {
		if remoteSHA != "" && manifest.RemoteSHA != "" && remoteSHA != manifest.RemoteSHA {
			return fmt.Errorf("publication remote SHA changed from %s to %s", manifest.RemoteSHA, remoteSHA)
		}
		return nil
	}
	manifest.PublicationPhase = phase
	if remoteSHA != "" {
		manifest.RemoteSHA = remoteSHA
	}
	if prNumber != 0 {
		manifest.PullRequestNumber, manifest.PullRequestURL = prNumber, prURL
	}
	return WriteRecoveryManifest(root, manifest)
}

// WriteFinalizedRecoveryManifest keeps the pre-launch base/ref evidence when a
// local worktree is finalized, while creating the journal for other sources.
func WriteFinalizedRecoveryManifest(root string, finalized RecoveryManifest) error {
	dir, err := recoveryTaskDir(root, finalized.TaskID)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, recoveryManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return WriteRecoveryManifest(root, finalized)
	}
	if err != nil {
		return err
	}
	var existing RecoveryManifest
	if err := json.Unmarshal(data, &existing); err != nil {
		return err
	}
	if existing.TaskID != finalized.TaskID || existing.DeliveryIntent != finalized.DeliveryIntent || existing.GitRoot != finalized.GitRoot || existing.CanonicalBranch != finalized.CanonicalBranch {
		return errors.New("finalized recovery manifest does not match prepared artifact")
	}
	if existing.HeadSHA != "" && existing.HeadSHA != finalized.HeadSHA {
		return errors.New("finalized recovery manifest head changed")
	}
	existing.HeadSHA = finalized.HeadSHA
	if existing.RuntimeID == "" {
		existing.RuntimeID = finalized.RuntimeID
	}
	if existing.Repository == "" {
		existing.Repository = finalized.Repository
	}
	return WriteRecoveryManifest(root, existing)
}

func writeRecoveryManifest(root string, manifest RecoveryManifest, syncFile func(*os.File) error) error {
	dir, err := recoveryTaskDir(root, manifest.TaskID)
	if err != nil {
		return err
	}
	if manifest.GitRoot == "" || manifest.CanonicalBranch == "" || manifest.BaseSHA == "" || manifest.ExpectedRefSHA == "" {
		return errors.New("recovery manifest requires repository, branch, base, and expected ref")
	}
	if manifest.PublicationPhase == "" {
		manifest.PublicationPhase = PublicationPrepared
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create recovery manifest directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict recovery manifest directory: %w", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = syncFile(tmp)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write recovery manifest: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, recoveryManifestName)); err != nil {
		return fmt.Errorf("publish recovery manifest: %w", err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open recovery manifest directory: %w", err)
	}
	if err = syncFile(dirHandle); err != nil {
		_ = dirHandle.Close()
		return fmt.Errorf("sync recovery manifest directory: %w", err)
	}
	if err := dirHandle.Close(); err != nil {
		return fmt.Errorf("close recovery manifest directory: %w", err)
	}
	return nil
}

func RemoveRecoveryManifest(root, taskID string) error {
	dir, err := recoveryTaskDir(root, taskID)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, recoveryManifestName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// LoadRecoveryManifests returns at most one bounded startup batch.
func LoadRecoveryManifests(root string) ([]RecoveryManifest, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	manifests := make([]RecoveryManifest, 0, min(len(entries), maxRecoveryManifests))
	var loadErr error
	scanned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if scanned >= maxRecoveryManifests {
			break
		}
		scanned++
		path := filepath.Join(root, entry.Name(), recoveryManifestName)
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		var manifest RecoveryManifest
		if readErr != nil || json.Unmarshal(data, &manifest) != nil || manifest.TaskID != entry.Name() {
			loadErr = errors.Join(loadErr, fmt.Errorf("invalid recovery manifest %s", path))
			continue
		}
		manifests = append(manifests, manifest)
	}
	return manifests, loadErr
}

func ReadRecoveryManifest(root, taskID string) (RecoveryManifest, bool, error) {
	dir, err := recoveryTaskDir(root, taskID)
	if err != nil {
		return RecoveryManifest{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, recoveryManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return RecoveryManifest{}, false, nil
	}
	if err != nil {
		return RecoveryManifest{}, false, err
	}
	var manifest RecoveryManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.TaskID != taskID {
		return RecoveryManifest{}, false, fmt.Errorf("invalid recovery manifest for %s", taskID)
	}
	return manifest, true, nil
}

// VerifyRecoveryManifest proves the finalized branch still names the recorded
// descendant before restart reconciliation publishes it.
func VerifyRecoveryManifest(manifest RecoveryManifest) error {
	if manifest.HeadSHA == "" {
		return errors.New("recovery manifest has no finalized head")
	}
	head, err := runGitTrimmed(manifest.GitRoot, "rev-parse", "--verify", "refs/heads/"+manifest.CanonicalBranch)
	if err != nil || head != manifest.HeadSHA {
		return fmt.Errorf("recovery branch for %s no longer matches finalized head", manifest.TaskID)
	}
	if _, err := runGit(manifest.GitRoot, "merge-base", "--is-ancestor", manifest.BaseSHA, head); err != nil {
		return fmt.Errorf("recovery head for %s does not descend from its base", manifest.TaskID)
	}
	return nil
}

// ResumeRecoveryManifests processes a bounded batch before startup GC. Each
// artifact operation takes the repository lock inside CreateRecoveryArtifact.
func ResumeRecoveryManifests(root string, logger *slog.Logger) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	resumed := 0
	var resumeErr error
	for _, entry := range entries {
		if resumed >= maxRecoveryManifests {
			break
		}
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), recoveryManifestName)
		data, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			resumeErr = errors.Join(resumeErr, readErr)
			continue
		}
		var manifest RecoveryManifest
		if json.Unmarshal(data, &manifest) != nil || manifest.TaskID != entry.Name() {
			resumeErr = errors.Join(resumeErr, fmt.Errorf("invalid recovery manifest %s", path))
			continue
		}
		sha := manifest.HeadSHA
		if sha == "" {
			sha, readErr = runGitTrimmed(manifest.GitRoot, "rev-parse", "--verify", "refs/heads/"+manifest.CanonicalBranch)
			if readErr != nil {
				resumeErr = errors.Join(resumeErr, fmt.Errorf("resolve recovery branch for %s: %w", manifest.TaskID, readErr))
				continue
			}
		}
		if _, ancestryErr := runGit(manifest.GitRoot, "merge-base", "--is-ancestor", manifest.BaseSHA, sha); ancestryErr != nil {
			resumeErr = errors.Join(resumeErr, fmt.Errorf("recovery head for %s does not descend from its base", manifest.TaskID))
			continue
		}
		if _, createErr := CreateRecoveryArtifact(manifest.GitRoot, root, manifest.TaskID, sha, logger); createErr != nil {
			resumeErr = errors.Join(resumeErr, createErr)
			continue
		}
		if removeErr := RemoveRecoveryManifest(root, manifest.TaskID); removeErr != nil {
			resumeErr = errors.Join(resumeErr, removeErr)
			continue
		}
		resumed++
	}
	return resumed, resumeErr
}
