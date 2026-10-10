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
)

const (
	recoveryManifestName = "manifest.json"
	maxRecoveryManifests = 100
)

// RecoveryManifest is the non-secret durable journal written before provider
// launch. A restart can preserve a finalized commit even if publication never
// ran.
type RecoveryManifest struct {
	TaskID          string `json:"task_id"`
	RuntimeID       string `json:"runtime_id,omitempty"`
	GitRoot         string `json:"git_root"`
	CanonicalBranch string `json:"canonical_branch"`
	BaseSHA         string `json:"base_sha"`
	ExpectedRefSHA  string `json:"expected_ref_sha"`
	HeadSHA         string `json:"head_sha,omitempty"`
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

func writeRecoveryManifest(root string, manifest RecoveryManifest, syncFile func(*os.File) error) error {
	dir, err := recoveryTaskDir(root, manifest.TaskID)
	if err != nil {
		return err
	}
	if manifest.GitRoot == "" || manifest.CanonicalBranch == "" || manifest.BaseSHA == "" || manifest.ExpectedRefSHA == "" {
		return errors.New("recovery manifest requires repository, branch, base, and expected ref")
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
