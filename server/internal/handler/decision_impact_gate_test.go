package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/decisionimpact"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecisionImpactQueueFenceBlocksPendingAndFailedReviews(t *testing.T) {
	if testPool == nil {
		t.Fatal("database required for decision impact queue fence")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "decisionimpact")

	for _, state := range []string{"pending", "failed"} {
		if _, err := testPool.Exec(ctx, `UPDATE issue SET metadata=jsonb_build_object('decision_impact_review', jsonb_build_object('state', $2::text)) WHERE id=$1`, f.victimIssue, state); err != nil {
			t.Fatalf("set %s review: %v", state, err)
		}
		err := enqueueViaRealQuery(ctx, testHandler.Queries, f.neighbourAgent, f.neighbourRuntime, f.victimIssue)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s review enqueue error = %v, want pgx.ErrNoRows", state, err)
		}
	}

	if _, err := testPool.Exec(ctx, `UPDATE issue SET metadata=jsonb_build_object('decision_impact_review', jsonb_build_object('state', 'completed')) WHERE id=$1`, f.victimIssue); err != nil {
		t.Fatal(err)
	}
	if err := enqueueViaRealQuery(ctx, testHandler.Queries, f.neighbourAgent, f.neighbourRuntime, f.victimIssue); err != nil {
		t.Fatalf("completed review blocked enqueue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1 AND status='queued'`, f.victimIssue)
	})
}

func TestDecisionImpactQueueFenceSerializesConcurrentGateOpen(t *testing.T) {
	if testPool == nil {
		t.Fatal("database required for decision impact queue fence")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "decisionimpact-race")
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE issue SET metadata=jsonb_build_object('decision_impact_review', jsonb_build_object('state', 'pending')) WHERE id=$1`, f.victimIssue); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- enqueueViaRealQuery(ctx, testHandler.Queries, f.neighbourAgent, f.neighbourRuntime, f.victimIssue)
	}()
	select {
	case err := <-done:
		t.Fatalf("enqueue escaped uncommitted gate update: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("enqueue error = %v, want pgx.ErrNoRows", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue remained blocked after gate commit")
	}
}

func TestCreateDecisionWithAssigneeOpensGateBeforeEnqueue(t *testing.T) {
	if testPool == nil {
		t.Fatal("database required for decision impact create gate")
	}
	agentID := dbfx.Agent(t, "decision impact create assignee", testRuntimeID)
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(context.Background(), db.GetMemberByUserAndWorkspaceParams{
		UserID: parseUUID(testUserID), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "DECISION CLEMENT — replace direction", "assignee_type": "agent", "assignee_id": agentID,
	})
	req = req.WithContext(middleware.SetMemberContext(req.Context(), testWorkspaceID, member))
	rec := httptest.NewRecorder()
	testHandler.CreateIssue(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateIssue status = %d: %s", rec.Code, rec.Body.String())
	}
	var created IssueResponse
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	issueID := parseUUID(created.ID)
	tasks, err := testHandler.Queries.ListTasksByIssue(context.Background(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("decision creation enqueued %d tasks before review", len(tasks))
	}
	stored, err := testHandler.Queries.GetIssueInWorkspace(context.Background(), db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(stored.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata[decisionImpactMetadataKey]) == 0 {
		t.Fatal("created decision is missing pending impact review")
	}
}

func TestBeginDecisionImpactReviewRejectsStaleRevision(t *testing.T) {
	if testPool == nil {
		t.Fatal("database required for decision impact OCC")
	}
	ctx := context.Background()
	issueID := parseUUID(dbfx.Issue(t, "decision impact stale revision"))
	stale, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET priority='high', revision=revision+1 WHERE id=$1`, issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.beginDecisionImpactReview(ctx, stale, parseUUID(testUserID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale gate open error = %v, want pgx.ErrNoRows", err)
	}
	current, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := parseIssueMetadata(current.Metadata)[decisionImpactMetadataKey]; present {
		t.Fatal("stale gate open wrote decision impact metadata")
	}
}

func TestDecisionImpactScanReadsPastFirstPageAndComments(t *testing.T) {
	if testPool == nil {
		t.Fatal("database required for decision impact pagination")
	}
	ctx := context.Background()
	projectID := dbfx.Project(t, "decision impact large project")
	decisionID := parseUUID(dbfx.Issue(t, "DECISION CLEMENT pagination", testutil.Cols{"project_id": projectID}))
	decision, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: decisionID, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	prefix := testHandler.getIssuePrefix(ctx, decision.WorkspaceID)
	decisionIdentifier := fmt.Sprintf("%s-%d", prefix, decision.Number)
	for i := 0; i < 100; i++ {
		dbfx.Issue(t, fmt.Sprintf("pagination filler %03d", i), testutil.Cols{"project_id": projectID})
	}
	targetID := dbfx.Issue(t, "comment-only impact candidate", testutil.Cols{"project_id": projectID})
	for i := 0; i < 35; i++ {
		dbfx.Comment(t, targetID, fmt.Sprintf("old filler comment %02d", i))
	}
	dbfx.Comment(t, targetID, "still depends on "+decisionIdentifier)
	dependencyID := dbfx.Issue(t, "graph-only impact candidate")
	dbfx.Insert(t, "issue_dependency", testutil.Cols{"issue_id": dependencyID, "depends_on_issue_id": util.UUIDToString(decisionID), "type": "blocks"})
	sharedProjectID := dbfx.Project(t, "shared resource project")
	sharedIssueID := dbfx.Issue(t, "shared-resource impact candidate", testutil.Cols{"project_id": sharedProjectID})
	for _, candidateProjectID := range []string{projectID, sharedProjectID} {
		dbfx.Insert(t, "project_resource", testutil.Cols{
			"project_id": candidateProjectID, "workspace_id": testWorkspaceID, "resource_type": "github_repo",
			"resource_ref": testutil.Raw(`'{"url":"https://example.invalid/shared"}'::jsonb`), "position": 0, "created_by": testUserID,
		})
	}
	if err := testHandler.scanDecisionImpactReview(ctx, decision, parseUUID(testUserID)); err != nil {
		t.Fatal(err)
	}
	stored, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: decisionID, WorkspaceID: decision.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(stored.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	var review decisionimpact.Review
	if err := json.Unmarshal(metadata[decisionImpactMetadataKey], &review); err != nil {
		t.Fatal(err)
	}
	if len(review.PendingImpact) != 0 {
		t.Fatalf("exhaustive matrix leaked into bounded metadata: %#v", review.PendingImpact)
	}
	var metadataBytes int
	if err := testPool.QueryRow(ctx, `SELECT pg_column_size(metadata) FROM issue WHERE id=$1`, decisionID).Scan(&metadataBytes); err != nil {
		t.Fatal(err)
	}
	if metadataBytes > 8192 {
		t.Fatalf("normalized metadata size = %d, exceeds 8192", metadataBytes)
	}
	candidates, err := testHandler.Queries.LockDecisionImpactCandidatesForCompletion(ctx, db.LockDecisionImpactCandidatesForCompletionParams{
		ScanID: parseUUID(review.ScanID), WorkspaceID: decision.WorkspaceID, DecisionIssueID: decisionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) < 102 {
		t.Fatalf("normalized candidates = %d, want at least 102", len(candidates))
	}
	targetIssue, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(targetID), WorkspaceID: decision.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	targetIdentifier := fmt.Sprintf("%s-%d", prefix, targetIssue.Number)
	dependencyIssue, _ := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(dependencyID), WorkspaceID: decision.WorkspaceID})
	sharedIssue, _ := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(sharedIssueID), WorkspaceID: decision.WorkspaceID})
	wantRelations := map[string]string{
		targetIdentifier: "comment",
		fmt.Sprintf("%s-%d", prefix, dependencyIssue.Number): "dependency",
		fmt.Sprintf("%s-%d", prefix, sharedIssue.Number):     "shared_resource",
	}
	for _, row := range candidates {
		if want, ok := wantRelations[row.IssueIdentifier]; ok && row.Relation == want {
			delete(wantRelations, row.IssueIdentifier)
		}
	}
	if len(wantRelations) != 0 {
		t.Fatalf("missing normalized relations %#v in candidates %#v", wantRelations, candidates)
	}
}

func TestDecisionImpactCompletionRevalidatesCorrectedCandidate(t *testing.T) {
	review := decisionimpact.Review{ImpactedAndCorrected: []decisionimpact.MatrixRow{{
		IssueIdentifier: "HERM-885", Relation: "reference", Impact: "obsolete", Action: decisionimpact.Supersede, Proof: "owner closed obsolete issue",
	}}}
	candidate := db.LockDecisionImpactCandidatesForCompletionRow{
		IssueIdentifier: "HERM-885", Relation: "reference", IssueRevision: 2,
		CurrentRevision: 3, CurrentStatus: "cancelled", StatusCategory: "closed", HasActiveRuns: true,
	}
	if err := validateDecisionImpactCompletion(review, []db.LockDecisionImpactCandidatesForCompletionRow{candidate}); err == nil {
		t.Fatal("corrected candidate with an active run was accepted")
	}
	candidate.HasActiveRuns = false
	if err := validateDecisionImpactCompletion(review, []db.LockDecisionImpactCandidatesForCompletionRow{candidate}); err != nil {
		t.Fatalf("closed candidate without active runs was rejected: %v", err)
	}
}
