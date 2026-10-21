package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestAgentJSONCommandsRedactSecretBearingFields(t *testing.T) {
	const fakeSecret = "HERM1086_FAKE_CLI_SECRET"
	agent := map[string]any{
		"id":         "agent-1",
		"name":       "safe-name",
		"mcp_config": map[string]any{"mcpServers": map[string]any{"fake": map[string]any{"HeAdErS": map[string]any{"Authorization": fakeSecret}}}},
		"custom_env": map[string]any{"TOKEN": fakeSecret},
		"runtime_config": map[string]any{
			"safe":   "kept",
			"nested": map[string]any{"EnV": map[string]any{"TOKEN": fakeSecret}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agents" {
			_ = json.NewEncoder(w).Encode([]map[string]any{agent})
			return
		}
		_ = json.NewEncoder(w).Encode(agent)
	}))
	defer srv.Close()
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "workspace-1")
	t.Setenv("MULTICA_TOKEN", "mat_fake-token")
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")

	tests := []struct {
		name string
		run  func(*cobra.Command) error
	}{
		{"list", func(cmd *cobra.Command) error { return runAgentList(cmd, nil) }},
		{"get", func(cmd *cobra.Command) error { return runAgentGet(cmd, []string{"agent-1"}) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: tc.name}
			cmd.Flags().String("output", "json", "")
			cmd.Flags().String("profile", "", "")
			if tc.name == "list" {
				cmd.Flags().Bool("include-archived", false, "")
			}

			old := os.Stdout
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = w
			runErr := tc.run(cmd)
			_ = w.Close()
			os.Stdout = old
			out, readErr := io.ReadAll(r)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if runErr != nil {
				t.Fatalf("run agent %s: %v", tc.name, runErr)
			}

			text := string(out)
			for _, forbidden := range []string{fakeSecret, "mcp_config", "custom_env", "HeAdErS", "EnV"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("agent %s output retained %q: %s", tc.name, forbidden, text)
				}
			}
			if !strings.Contains(text, `"safe": "kept"`) {
				t.Fatalf("agent %s output removed safe data: %s", tc.name, text)
			}
		})
	}
}
