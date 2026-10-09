package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestDecisionImpactReviewMetadataRequiresAuthenticatedOwner(t *testing.T) {
	issue := dbfx.Issue(t, "decision impact authority")
	agent := dbfx.Agent(t, "decision impact agent", testRuntimeID)
	task := dbfx.Task(t, agent, testutil.Cols{
		"runtime_id":          testRuntimeID,
		"issue_id":            issue,
		"status":              "running",
		"started_at":          testutil.Raw("now()"),
		"originator_user_id":  testUserID,
		"accountable_user_id": testUserID,
	})
	value := map[string]any{
		"state":                  "completed",
		"decision_issue_id":      issue,
		"authorized_member_id":   testUserID,
		"decision_changed":       "new vault",
		"impacted_and_corrected": []any{},
		"pending_impact":         []any{},
		"not_impacted":           []any{},
		"active_after_review":    []any{},
	}
	body, _ := json.Marshal(map[string]any{"value": value})

	call := func(asAgent bool) int {
		req := newRequest("PUT", "/", json.RawMessage(body))
		req = withURLParams(req, "id", issue, "key", decisionImpactMetadataKey)
		if asAgent {
			req.Header.Set("X-Agent-ID", agent)
			req.Header.Set("X-Task-ID", task)
			req.Header.Set("X-Actor-Source", "task_token")
		}
		rec := httptest.NewRecorder()
		testHandler.SetIssueMetadataKey(rec, req)
		return rec.Code
	}

	if got := call(true); got != http.StatusForbidden {
		t.Fatalf("task-token agent status = %d, want 403", got)
	}
	if got := call(false); got != http.StatusOK {
		t.Fatalf("workspace owner status = %d, want 200", got)
	}
}
