package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompleteTaskCarriesRecoveryDescriptor(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := NewClient(srv.URL).completeTaskWithRetrySchedule(
		context.Background(), "task-1", "done", "branch", "", "/work", false, "", "/durable",
		"refs/multica/recovery/task-1", "/recovery/task-1/recovery.bundle", true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if body["recovery_ref"] != "refs/multica/recovery/task-1" || body["recovery_bundle_verified"] != true {
		t.Fatalf("recovery descriptor = %#v", body)
	}
}
