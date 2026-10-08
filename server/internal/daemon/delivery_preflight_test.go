package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestPrepareRuntimeBriefDoesNotModifyLocalRepository(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "AGENTS.md")
	original := []byte("# Repository instructions\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	brief, err := prepareRuntimeBrief(&execenv.Environment{WorkDir: workDir, LocalDirectory: true}, "hermes", execenv.TaskContextForEnv{IssueID: "issue"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "Multica Agent Runtime") {
		t.Fatalf("runtime brief missing from out-of-tree payload: %q", brief)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("tracked AGENTS.md changed: %q", got)
	}
}
