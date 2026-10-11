package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecisionImpactReviewMetadataRequiresAuthenticatedOwner(t *testing.T) {
	issue := dbfx.Issue(t, "decision impact authority")
	agent := dbfx.Agent(t, "decision impact agent", testRuntimeID)
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      parseUUID(testUserID),
		WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load workspace owner: %v", err)
	}
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
		"not_impacted": []any{map[string]any{
			"issue": "TEST-1", "relation": "project", "impact": "no impact", "action": "no impact proved", "proof": "reviewed",
		}},
		"active_after_review": []any{},
	}
	body, _ := json.Marshal(map[string]any{"value": value})

	call := func(asAgent bool) int {
		req := newRequest("PUT", "/", json.RawMessage(body))
		req = withURLParams(req, "id", issue, "key", decisionImpactMetadataKey)
		req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, member))
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
