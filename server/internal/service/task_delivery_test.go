package service

import (
	"encoding/json"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAgentDeliveryContextSnapshotsAllowlistedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{name: "default", config: `{}`, want: "none"},
		{name: "artifact", config: `{"delivery_intent":"artifact"}`, want: "artifact"},
		{name: "publish", config: `{"delivery_intent":"publish"}`, want: "publish"},
		{name: "unknown", config: `{"delivery_intent":"admin"}`, want: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				DeliveryIntent string `json:"delivery_intent"`
			}
			if err := json.Unmarshal(agentDeliveryContext(db.Agent{RuntimeConfig: []byte(tc.config)}), &got); err != nil {
				t.Fatal(err)
			}
			if got.DeliveryIntent != tc.want {
				t.Fatalf("delivery intent = %q, want %q", got.DeliveryIntent, tc.want)
			}
		})
	}
}
