package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestDecisionImpactQueueFenceBlocksPendingAndFailedReviews(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
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
