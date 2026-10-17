package execenv

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type RecoveryArtifact struct {
	Ref            string
	BundlePath     string
	BundleVerified bool
}

// CreateRecoveryArtifact pins sha in the repository and writes a verified
// daemon-owned bundle. It contains no credentials and is safe to resume after a
// daemon restart.
func CreateRecoveryArtifact(gitRoot, recoveryRoot, taskID, sha string, logger *slog.Logger) (RecoveryArtifact, error) {
	var out RecoveryArtifact
	if gitRoot == "" || recoveryRoot == "" || taskID == "" || sha == "" {
		return out, fmt.Errorf("recovery requires repository, root, task id, and SHA")
	}
	gitRoot, err := resolveGitRoot(gitRoot)
	if err != nil {
		return out, err
	}
	unlock, err := lockGitRoot(gitRoot, logger)
	if err != nil {
		return out, err
	}
	defer unlock()
	if _, err := runGitTrimmed(gitRoot, "rev-parse", "--verify", sha+"^{commit}"); err != nil {
		return out, fmt.Errorf("recovery SHA is not a commit: %w", err)
	}
	out.Ref = "refs/multica/recovery/" + taskID
	if result, err := runGit(gitRoot, "update-ref", out.Ref, sha); err != nil {
		return out, fmt.Errorf("create recovery ref: %s: %w", strings.TrimSpace(result), err)
	}
	dir := filepath.Join(recoveryRoot, taskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return out, fmt.Errorf("create recovery directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return out, fmt.Errorf("restrict recovery directory: %w", err)
	}
	out.BundlePath = filepath.Join(dir, "recovery.bundle")
	if result, err := runGit(gitRoot, "bundle", "create", out.BundlePath, out.Ref); err != nil {
		return out, fmt.Errorf("create recovery bundle: %s: %w", strings.TrimSpace(result), err)
	}
	if err := os.Chmod(out.BundlePath, 0o600); err != nil {
		return out, fmt.Errorf("restrict recovery bundle: %w", err)
	}
	if result, err := runGit(gitRoot, "bundle", "verify", out.BundlePath); err != nil {
		return out, fmt.Errorf("verify recovery bundle: %s: %w", strings.TrimSpace(result), err)
	}
	out.BundleVerified = true
	return out, nil
}
