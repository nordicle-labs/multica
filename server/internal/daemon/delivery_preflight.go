package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/deliverycontract"
)

func prepareRuntimeBrief(env *execenv.Environment, provider string, task execenv.TaskContextForEnv) (string, error) {
	if env.LocalDirectory || env.LocalWorktree != nil {
		return execenv.BuildRuntimeBrief(provider, task), nil
	}
	return execenv.InjectRuntimeConfig(env.WorkDir, provider, task)
}

func buildDeliveryPreflight(ctx context.Context, workDir string, repositoryRequired, authenticated bool) deliverycontract.Preflight {
	profile := deliverycontract.CurrentProfile()
	hash, _ := profile.Hash()
	report := deliverycontract.Preflight{
		ProfileVersion:     profile.Version,
		ProfileHash:        hash,
		RepositoryRequired: repositoryRequired,
		Authenticated:      authenticated,
		Toolsets:           append([]string(nil), profile.RequiredToolsets...),
	}
	if info, err := os.Stat(workDir); err == nil && info.IsDir() {
		report.ResourceReady = true
	}
	if f, err := os.CreateTemp("", "multica-delivery-preflight-*"); err == nil {
		report.DiskWritable = true
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}
	if _, err := resolveSelfExecutable(); err == nil {
		report.CLIAvailable = true
	}
	if !repositoryRequired {
		return report
	}
	report.Repository.Git = gitPreflight(ctx, workDir, "rev-parse", "--is-inside-work-tree") == "true"
	report.Repository.Worktree = report.Repository.Git
	report.Repository.Branch = gitPreflight(ctx, workDir, "branch", "--show-current")
	report.Repository.CommitSHA = gitPreflight(ctx, workDir, "rev-parse", "HEAD")
	return report
}

func gitPreflight(ctx context.Context, workDir string, args ...string) string {
	cmdArgs := append([]string{"-C", workDir}, args...)
	out, err := exec.CommandContext(ctx, "git", cmdArgs...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
