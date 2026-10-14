package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskEnvironmentIsolationAcrossDistinctTasks(t *testing.T) {
	first := taskMulticaEnvironment(Task{ID: "task-one", AgentID: "agent-one", WorkspaceID: "ws-one"}, "first", "mat_first_private", "/private/config-one", "/private/root-one", "/private/work-one", "https://one.invalid", 19001, 1, "/private/tmp-one")
	first["FIRST_TASK_PRIVATE"] = "private-sentinel-one"
	second := taskMulticaEnvironment(Task{ID: "task-two", AgentID: "agent-two", WorkspaceID: "ws-two"}, "second", "mat_second_private", "/private/config-two", "/private/root-two", "/private/work-two", "https://two.invalid", 19002, 2, "/private/tmp-two")

	runTask := func(taskEnv map[string]string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", "pwd; env")
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for key, value := range taskEnv {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute task: %s: %v", out, err)
		}
		return string(out)
	}
	firstObserved := runTask(first)
	if !strings.Contains(firstObserved, "private-sentinel-one") {
		t.Fatalf("first task did not observe its own private environment:\n%s", firstObserved)
	}
	observed := runTask(second)
	for _, private := range []string{"task-one", "agent-one", "ws-one", "mat_first_private", "/private/config-one", "/private/root-one", "/private/work-one", "/private/tmp-one", "private-sentinel-one"} {
		if strings.Contains(observed, private) {
			t.Fatalf("second task observed first-task private value %q:\n%s", private, observed)
		}
	}
	if second["MULTICA_TASK_ID"] != "task-two" || first["MULTICA_TASK_ID"] != "task-one" {
		t.Fatalf("task maps alias: first=%q second=%q", first["MULTICA_TASK_ID"], second["MULTICA_TASK_ID"])
	}
}

func TestCanonicalRepositoryDescriptorMatrix(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "seed")
	gitTestRun(t, "", "init", "-b", "delivery", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("same tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, seed, "add", "tracked.txt")
	gitTestRun(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	wantHead := gitTestRun(t, seed, "rev-parse", "HEAD")
	wantTree := gitTestRun(t, seed, "rev-parse", "HEAD^{tree}")

	githubRepo := filepath.Join(t.TempDir(), "github-repo")
	inPlace := filepath.Join(t.TempDir(), "in-place")
	worktreeRoot := filepath.Join(t.TempDir(), "worktree-root")
	for _, target := range []string{githubRepo, inPlace, worktreeRoot} {
		gitTestRun(t, "", "clone", seed, target)
	}
	gitTestRun(t, worktreeRoot, "checkout", "--detach", "HEAD")
	worktree := filepath.Join(t.TempDir(), "task-worktree")
	gitTestRun(t, worktreeRoot, "worktree", "add", worktree, "delivery")

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "github_repo", path: githubRepo},
		{name: "local_directory_worktree", path: worktree},
		{name: "local_directory_in_place", path: inPlace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor, err := finalizeRepositoryDelivery(context.Background(), tc.path, true)
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.GitRoot == "" || descriptor.Branch != "delivery" || descriptor.CommitSHA != wantHead || descriptor.TreeSHA != wantTree {
				t.Fatalf("descriptor = %+v, want branch delivery head %s tree %s", descriptor, wantHead, wantTree)
			}
		})
	}

	gitTestRun(t, inPlace, "checkout", "--detach", "HEAD")
	if _, err := finalizeRepositoryDelivery(context.Background(), inPlace, true); err == nil || !strings.Contains(err.Error(), "missing branch or SHA") {
		t.Fatalf("detached required delivery error = %v", err)
	}
}

func TestAttachRepositoryDeliveryRejectsIncoherentIdentity(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	gitTestRun(t, "", "init", "-b", "delivery", repo)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repo, "add", "tracked.txt")
	gitTestRun(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "delivery")

	for _, tc := range []struct {
		name   string
		result TaskResult
		want   string
	}{
		{name: "branch", result: TaskResult{BranchName: "other"}, want: "branch mismatch"},
		{name: "head", result: TaskResult{CommitSHA: strings.Repeat("a", 40)}, want: "SHA mismatch"},
		{name: "tree", result: TaskResult{TreeSHA: strings.Repeat("b", 40)}, want: "tree mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.result
			if err := attachRepositoryDelivery(context.Background(), repo, &result); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("attachRepositoryDelivery error = %v, want %q", err, tc.want)
			}
		})
	}
}

func gitTestRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
