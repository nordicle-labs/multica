package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/deliverycontract"
)

func prepareRuntimeBrief(env *execenv.Environment, provider string, task execenv.TaskContextForEnv) (string, error) {
	if env.LocalDirectory || env.LocalWorktree != nil {
		return execenv.BuildRuntimeBrief(provider, task), nil
	}
	return execenv.InjectRuntimeConfig(env.WorkDir, provider, task)
}

func buildDeliveryPreflight(ctx context.Context, workDir string, repositoryRequired, authenticated bool, worktree *execenv.LocalWorktree) deliverycontract.Preflight {
	profile := deliverycontract.CurrentProfile()
	hash, _ := profile.Hash()
	report := deliverycontract.Preflight{
		ProfileVersion:     profile.Version,
		ProfileHash:        hash,
		RepositoryRequired: repositoryRequired,
		Authenticated:      authenticated,
		Toolsets:           append([]string(nil), profile.RequiredToolsets...),
		Services:           probeDeliveryServices(ctx, os.Getenv("MULTICA_DELIVERY_TEST_DATABASE_URL")),
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
	report.Repository = deriveRepositoryDescriptor(ctx, workDir)
	if worktree != nil {
		report.Repository.GitRoot = worktree.GitRoot
		report.Repository.Branch = worktree.Branch
		report.CanonicalBranch = worktree.Branch
		report.RunBaseSHA = worktree.BaseCommit
		report.CanonicalRefSHA = worktree.CanonicalRefExpected
	}
	return report
}

// deriveRepositoryDescriptor is the single Git boundary for every repository
// source, so no delivery evidence is accepted from provider output.
func deriveRepositoryDescriptor(ctx context.Context, workDir string) deliverycontract.Repository {
	git := gitPreflight(ctx, workDir, "rev-parse", "--is-inside-work-tree") == "true"
	if !git {
		return deliverycontract.Repository{}
	}
	return deliverycontract.Repository{
		Git:       true,
		GitRoot:   gitPreflight(ctx, workDir, "rev-parse", "--show-toplevel"),
		Branch:    gitPreflight(ctx, workDir, "branch", "--show-current"),
		CommitSHA: gitPreflight(ctx, workDir, "rev-parse", "HEAD"),
		TreeSHA:   gitPreflight(ctx, workDir, "rev-parse", "HEAD^{tree}"),
		Worktree:  true,
	}
}

func finalizeRepositoryDelivery(ctx context.Context, workDir string, required bool) (deliverycontract.Repository, error) {
	descriptor := deriveRepositoryDescriptor(ctx, workDir)
	if required && (descriptor.Branch == "" || descriptor.CommitSHA == "") {
		return descriptor, fmt.Errorf("required repository delivery is missing branch or SHA")
	}
	return descriptor, nil
}

func probeDeliveryServices(parent context.Context, databaseURL string) map[string]bool {
	services := map[string]bool{"postgres": false, "pgvector": false}
	if strings.TrimSpace(databaseURL) == "" {
		return services
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return services
	}
	defer conn.Close(context.Background())
	var vectorAvailable bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')").Scan(&vectorAvailable); err != nil {
		return services
	}
	services["postgres"] = true
	services["pgvector"] = vectorAvailable
	return services
}

func gitPreflight(ctx context.Context, workDir string, args ...string) string {
	cmdArgs := append([]string{"-C", workDir}, args...)
	out, err := exec.CommandContext(ctx, "git", cmdArgs...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
