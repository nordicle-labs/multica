package agent

import "testing"

func TestHermesPromptTextCarriesOutOfTreeRuntimeBrief(t *testing.T) {
	if got, want := hermesPromptText("runtime brief", "task prompt"), "runtime brief\n\n---\n\ntask prompt"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	if got := hermesPromptText("", "task prompt"); got != "task prompt" {
		t.Fatalf("empty system prompt changed task prompt: %q", got)
	}
}
