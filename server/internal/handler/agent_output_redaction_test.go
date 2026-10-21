package handler

import (
	"bytes"
	"encoding/json"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAgentResponseOmitsSecretBearingConfiguration(t *testing.T) {
	const (
		mcpSecret    = "HERM1086_FAKE_MCP_SECRET"
		headerSecret = "HERM1086_FAKE_HEADER_SECRET"
		envSecret    = "HERM1086_FAKE_ENV_SECRET"
	)

	resp := (&Handler{}).agentToResponse(db.Agent{
		RuntimeConfig: []byte(`{"safe":"kept","nested":{"HeAdErS":{"Authorization":"` + headerSecret + `"},"EnV":{"TOKEN":"` + envSecret + `"}}}`),
		CustomEnv:     []byte(`{"API_KEY":"` + envSecret + `"}`),
		McpConfig:     []byte(`{"mcpServers":{"fake":{"headers":{"Authorization":"` + mcpSecret + `"}}}}`),
	})

	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	for _, secret := range [][]byte{[]byte(mcpSecret), []byte(headerSecret), []byte(envSecret)} {
		if bytes.Contains(out, secret) {
			t.Fatalf("agent response leaked synthetic secret %q: %s", secret, out)
		}
	}
	for _, field := range [][]byte{[]byte(`"mcp_config"`), []byte(`"HeAdErS"`), []byte(`"EnV"`)} {
		if bytes.Contains(out, field) {
			t.Fatalf("agent response retained sensitive field %s: %s", field, out)
		}
	}
	if !bytes.Contains(out, []byte(`"safe":"kept"`)) {
		t.Fatalf("agent response removed non-sensitive runtime config: %s", out)
	}
	if !resp.McpConfigRedacted {
		t.Fatal("mcp_config_redacted must report configured MCP data")
	}
	if !resp.HasCustomEnv || resp.CustomEnvKeyCount != 1 {
		t.Fatalf("custom env metadata = (%v, %d), want (true, 1)", resp.HasCustomEnv, resp.CustomEnvKeyCount)
	}
}
