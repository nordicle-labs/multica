package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRerunIssueWithDeliveryIntentPersistsTaskScopedOverride(t *testing.T) {
	svc, pool, creatorID, agentID, issueID, _ := rerunQueueFixture(t)
	intent := "publish"

	task, err := svc.RerunIssueWithDeliveryIntent(context.Background(), util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), &intent, nil, func(agent db.Agent) bool { return agent.ID.Valid })
	if err != nil {
		t.Fatalf("RerunIssueWithDeliveryIntent: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, task.ID) })

	var taskContext, runtimeConfig []byte
	if err := pool.QueryRow(context.Background(), `SELECT context FROM agent_task_queue WHERE id = $1`, task.ID).Scan(&taskContext); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT runtime_config FROM agent WHERE id = $1`, agentID).Scan(&runtimeConfig); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(taskContext, &got); err != nil {
		t.Fatal(err)
	}
	if got["delivery_intent"] != "publish" {
		t.Fatalf("delivery_intent = %v, want publish", got["delivery_intent"])
	}
	if string(runtimeConfig) != "{}" {
		t.Fatalf("runtime_config changed: %s", runtimeConfig)
	}
}

func TestRerunIssueWithDeliveryIntentRefusesPendingSlotWithoutMutation(t *testing.T) {
	svc, pool, creatorID, _, issueID, runtimeID := rerunQueueFixture(t)
	var agentID string
	if err := pool.QueryRow(context.Background(), `SELECT assignee_id::text FROM issue WHERE id = $1`, issueID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	pendingID := seedRerunTask(t, pool, agentID, runtimeID, issueID, "queued")
	intent := "publish"

	_, err := svc.RerunIssueWithDeliveryIntent(context.Background(), util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), &intent, nil, func(db.Agent) bool { return true })
	if !errors.Is(err, ErrRerunPendingSlot) {
		t.Fatalf("err = %v, want ErrRerunPendingSlot", err)
	}
	if got := taskStatus(t, pool, pendingID); got != "queued" {
		t.Fatalf("pending task status = %q, want queued", got)
	}
}

func TestRerunIssueWithDeliveryIntentConcurrentCollisionCreatesOneRow(t *testing.T) {
	svc, pool, creatorID, _, issueID, _ := rerunQueueFixture(t)
	intent := "publish"
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.RerunIssueWithDeliveryIntent(context.Background(), util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), &intent, nil, func(db.Agent) bool { return true })
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var success, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrRerunPendingSlot):
			conflicts++
		default:
			t.Fatalf("unexpected rerun error: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d, want 1/1", success, conflicts)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND context->>'delivery_intent' = 'publish'`, issueID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("publish rows = %d, want 1", rows)
	}
}

func TestRerunIssueWithDeliveryIntentDoesNotLeakToConcurrentOrdinaryEnqueue(t *testing.T) {
	svc, pool, creatorID, agentID, issueID, _ := rerunQueueFixture(t)
	var otherIssueID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, assignee_type, assignee_id, priority, number)
		SELECT workspace_id, 'ordinary concurrent issue', 'member', $1, 'agent', $2, 'medium', number + 1
		FROM issue WHERE id = $3 RETURNING id`, creatorID, agentID, issueID).Scan(&otherIssueID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, otherIssueID) })
	otherIssue, err := svc.Queries.GetIssue(context.Background(), util.MustParseUUID(otherIssueID))
	if err != nil {
		t.Fatal(err)
	}

	intent := "publish"
	start := make(chan struct{})
	var rerunTask, ordinaryTask db.AgentTaskQueue
	var rerunErr, ordinaryErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		rerunTaskPtr, err := svc.RerunIssueWithDeliveryIntent(context.Background(), util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), &intent, nil, func(db.Agent) bool { return true })
		rerunErr = err
		if rerunTaskPtr != nil {
			rerunTask = *rerunTaskPtr
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		ordinaryTask, ordinaryErr = svc.EnqueueTaskForIssueByActor(context.Background(), otherIssue, util.MustParseUUID(creatorID))
	}()
	close(start)
	wg.Wait()
	if rerunErr != nil || ordinaryErr != nil {
		t.Fatalf("rerun err=%v, ordinary err=%v", rerunErr, ordinaryErr)
	}
	var rerunIntent, ordinaryIntent string
	if err := pool.QueryRow(context.Background(), `SELECT context->>'delivery_intent' FROM agent_task_queue WHERE id = $1`, rerunTask.ID).Scan(&rerunIntent); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT context->>'delivery_intent' FROM agent_task_queue WHERE id = $1`, ordinaryTask.ID).Scan(&ordinaryIntent); err != nil {
		t.Fatal(err)
	}
	if rerunIntent != "publish" || ordinaryIntent != "none" {
		t.Fatalf("rerun intent=%q ordinary intent=%q, want publish/none", rerunIntent, ordinaryIntent)
	}
}

func TestRerunIssueWithDeliveryIntentRejectsInvalid(t *testing.T) {
	invalid := "ship"
	if _, err := (&TaskService{}).RerunIssueWithDeliveryIntent(context.Background(), pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, &invalid, nil, nil); !errors.Is(err, ErrInvalidDeliveryIntent) {
		t.Fatalf("invalid intent err = %v, want ErrInvalidDeliveryIntent", err)
	}
}

func TestRerunIssueWithDeliveryIntentRejectsUnauthorizedWithoutMutation(t *testing.T) {
	svc, pool, creatorID, _, issueID, _ := rerunQueueFixture(t)
	var before, after int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&before)
	publish := "publish"
	_, err := svc.RerunIssueWithDeliveryIntent(context.Background(), util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(creatorID), &publish, nil, func(db.Agent) bool { return false })
	if !errors.Is(err, ErrRerunManageNotAllowed) {
		t.Fatalf("unauthorized err = %v, want ErrRerunManageNotAllowed", err)
	}
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&after)
	if after != before {
		t.Fatalf("task count changed: %d -> %d", before, after)
	}
}
