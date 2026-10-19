package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestTonyChatProductIntakeCanaryCreatesAndResumesRealTask(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony", testRuntimeID, testutil.Cols{
		"system_key": service.TonySystemKey,
	})
	projectID := dbfx.Project(t, "Tony intake")
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{
		"explicitly_created_at": testutil.Raw("now()"),
	})
	binding := createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")
	if binding.Revision != 1 {
		t.Fatalf("first binding revision = %d, want 1", binding.Revision)
	}

	send := func(intakeID string) SendChatMessageResponse {
		t.Helper()
		req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{
			"content": "feature: deterministic Tony intake", "intake_id": intakeID,
		})
		req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
		w := httptest.NewRecorder()
		testHandler.SendChatMessage(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("Tony intake send: expected 201, got %d: %s", w.Code, w.Body.String())
		}
		var out SendChatMessageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode Tony intake response: %v", err)
		}
		return out
	}

	first := send(uuid.NewString())
	if !first.Intake || first.IssueID == "" || first.TaskID == "" {
		t.Fatalf("first Tony intake response missing real issue/task: %+v", first)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, first.IssueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, first.IssueID)
	claimed, err := testHandler.TaskService.ClaimTask(context.Background(), util.MustParseUUID(agentID))
	if err != nil || claimed == nil || uuidToString(claimed.ID) != first.TaskID {
		t.Fatalf("claim canary task: task=%+v err=%v", claimed, err)
	}
	started, err := testHandler.TaskService.StartTask(context.Background(), claimed.ID)
	if err != nil || started == nil || !started.StartedAt.Valid {
		t.Fatalf("start canary task through runtime service: task=%+v err=%v", started, err)
	}
	t.Logf("Tony canary issue_id=%s task_id=%s started_at=%s", first.IssueID, first.TaskID, started.StartedAt.Time.Format(time.RFC3339Nano))

	resumed := send(uuid.NewString())
	if resumed.IssueID != first.IssueID || resumed.TaskID != first.TaskID || resumed.StartedAt == "" {
		t.Fatalf("Tony resume lost canonical running task: first=%+v resumed=%+v", first, resumed)
	}

	testutil.Call(t, testHandler.CancelTaskByUser, cancelTaskByUserRequest(t, testUserID, first.TaskID)).Want(http.StatusOK)
	restarted := service.NewProductIntakeService(testHandler.Queries, testPool, testHandler.ProductIntake.Issues, testHandler.ProductIntake.Tasks)
	recovered, err := restarted.Process(context.Background(), service.ProductIntakeRequest{
		WorkspaceID: util.MustParseUUID(testWorkspaceID), ActorID: util.MustParseUUID(testUserID),
		SourceType: service.IntakeSourceMulticaChat, SourceID: sessionID, ConversationID: sessionID,
		TargetAgentID: util.MustParseUUID(agentID), MessageID: first.MessageID, Text: "feature: deterministic Tony intake",
	})
	if err != nil || uuidToString(recovered.Task.ID) != first.TaskID || recovered.Task.Status != "cancelled" {
		t.Fatalf("restart resurrected cancelled intake: result=%+v err=%v", recovered, err)
	}
	at := time.Now().Add(time.Hour)
	wakeup, err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).Create(
		context.Background(), util.MustParseUUID(first.IssueID), util.MustParseUUID(testUserID), pgtype.UUID{},
		service.WakeupInput{AgentID: agentID, Instruction: "revalidate before continuing", Kind: "at", At: &at},
	)
	if err != nil || !wakeup.ID.Valid {
		t.Fatalf("persist bounded intake wakeup: wakeup=%+v err=%v", wakeup, err)
	}
	dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE id = $1`, wakeup.ID)
	var chatMessages int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&chatMessages); err != nil {
		t.Fatalf("count direct chat messages: %v", err)
	}
	if chatMessages != 0 {
		t.Fatalf("Tony intake fell through to direct chat: messages=%d", chatMessages)
	}
}

func TestTonyChatProductIntakeFailsClosedWithoutBinding(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony unbound", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"explicitly_created_at": testutil.Raw("now()")})
	req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{
		"content": "feature: must not fall through", "intake_id": uuid.NewString(),
	})
	req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
	w := httptest.NewRecorder()
	testHandler.SendChatMessage(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("unbound Tony intake: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var messages, tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, sessionID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || tasks != 0 {
		t.Fatalf("unbound Tony intake mutated direct chat: messages=%d tasks=%d", messages, tasks)
	}
}

func TestTonyChatProductIntakeConcurrentDeliveryKeepsOneIssueAndTask(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony concurrent", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
	projectID := dbfx.Project(t, "Tony concurrent")
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"explicitly_created_at": testutil.Raw("now()")})
	createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")

	start := make(chan struct{})
	intakeID := uuid.NewString()
	results := make(chan SendChatMessageResponse, 2)
	errs := make(chan string, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{
				"content": fmt.Sprintf("feature: title %d", index), "intake_id": intakeID,
			})
			req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
			w := httptest.NewRecorder()
			testHandler.SendChatMessage(w, req)
			if w.Code != http.StatusCreated {
				errs <- w.Body.String()
				return
			}
			var out SendChatMessageResponse
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				errs <- err.Error()
				return
			}
			results <- out
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(results)
	got := make([]SendChatMessageResponse, 0, 2)
	for result := range results {
		got = append(got, result)
	}
	if len(got) != 2 || got[0].IssueID != got[1].IssueID || got[0].TaskID != got[1].TaskID {
		t.Fatalf("concurrent intake did not converge: %+v", got)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, got[0].IssueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, got[0].IssueID)
}

func TestTonyReleaseRequiresDistinctImmutableSHAApproval(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony release", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
	projectID := dbfx.Project(t, "Tony release")
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"explicitly_created_at": testutil.Raw("now()")})
	binding := createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "explicit_approval_rollback")
	approverID := dbfx.User(t, "Tony approver", "tony-approver-"+uuid.NewString()+"@example.test")
	dbfx.Member(t, testWorkspaceID, approverID, "admin")
	intakeID := uuid.NewString()
	approval, err := testHandler.ProductIntake.CreateApproval(context.Background(), service.ProductIntakeApprovalRequest{
		WorkspaceID: util.MustParseUUID(testWorkspaceID), BindingID: binding.ID,
		ApproverID: util.MustParseUUID(approverID), MessageID: intakeID,
		ArtifactSHA: "0123456789abcdef0123456789abcdef01234567", Decision: "approved",
	})
	if err != nil {
		t.Fatalf("create distinct approval: %v", err)
	}

	send := func(approvalID string) *httptest.ResponseRecorder {
		t.Helper()
		req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{
			"content": "release: candidate", "intake_id": intakeID, "approval_id": approvalID,
		})
		req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
		w := httptest.NewRecorder()
		testHandler.SendChatMessage(w, req)
		return w
	}

	if w := send(testUserID); w.Code != http.StatusConflict {
		t.Fatalf("forged approval: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	w := send(uuidToString(approval.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("authenticated approval: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var out SendChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, out.IssueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, out.IssueID)
	if _, err := testPool.Exec(context.Background(), `UPDATE product_intake_approval SET decision='denied' WHERE id=$1`, approval.ID); err == nil {
		t.Fatal("approval decision was mutable")
	}
	if _, err := testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, approverID); err != nil {
		t.Fatal(err)
	}
	if revoked := send(uuidToString(approval.ID)); revoked.Code != http.StatusConflict {
		t.Fatalf("revoked approver resumed release: expected 409, got %d: %s", revoked.Code, revoked.Body.String())
	}
}

func TestTonyProductIntakeRecoversMessageIDAfterDeliveryPersistenceGap(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony recovery", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
	projectID := dbfx.Project(t, "Tony recovery")
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"explicitly_created_at": testutil.Raw("now()")})
	createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")
	intakeID := uuid.NewString()

	send := func(content string) SendChatMessageResponse {
		t.Helper()
		req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]any{"content": content, "intake_id": intakeID})
		req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
		w := httptest.NewRecorder()
		testHandler.SendChatMessage(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("Tony recovery send: expected 201, got %d: %s", w.Code, w.Body.String())
		}
		var out SendChatMessageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := send("feature: original title")
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, first.IssueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, first.IssueID)
	if _, err := testPool.Exec(context.Background(), `DELETE FROM product_intake_delivery WHERE message_id=$1`, intakeID); err != nil {
		t.Fatal(err)
	}
	retried := send("feature: changed retry title")
	if retried.IssueID != first.IssueID || retried.TaskID != first.TaskID {
		t.Fatalf("MessageID retry created new work: first=%+v retry=%+v", first, retried)
	}
	var issues, tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue WHERE id=$1`, first.IssueID).Scan(&issues); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, first.IssueID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if issues != 1 || tasks != 1 {
		t.Fatalf("MessageID recovery counts: issues=%d tasks=%d", issues, tasks)
	}
}

func TestTonyProductIntakeRepairsIssueWithoutTask(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony issue-only recovery", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
	projectID := dbfx.Project(t, "Tony issue-only recovery")
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"explicitly_created_at": testutil.Raw("now()")})
	binding := createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")
	intakeID := uuid.NewString()
	originKey := strings.Join([]string{testWorkspaceID, service.IntakeSourceMulticaChat, sessionID, sessionID, testUserID, agentID, intakeID}, "\x1f")
	originID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(originKey)).String()
	issueID := dbfx.Issue(t, "crashed before task enqueue", testutil.Cols{
		"project_id": projectID, "assignee_type": "squad", "assignee_id": uuidToString(binding.SquadID),
		"origin_type": service.IntakeSourceMulticaChat, "origin_id": originID,
	})
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE id = $1`, issueID)
	var tasksBeforeRetry int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID).Scan(&tasksBeforeRetry); err != nil {
		t.Fatal(err)
	}
	if tasksBeforeRetry != 0 {
		t.Fatalf("crash gap has %d tasks before retry, want 0", tasksBeforeRetry)
	}

	result, err := testHandler.ProductIntake.Process(context.Background(), service.ProductIntakeRequest{
		WorkspaceID: util.MustParseUUID(testWorkspaceID), ActorID: util.MustParseUUID(testUserID),
		SourceType: service.IntakeSourceMulticaChat, SourceID: sessionID, ConversationID: sessionID,
		TargetAgentID: util.MustParseUUID(agentID), MessageID: intakeID, Text: "feature: retry after issue commit",
	})
	if err != nil {
		t.Fatalf("retry issue-only intake: %v", err)
	}
	if uuidToString(result.Issue.ID) != issueID || !result.Resumed {
		t.Fatalf("retry did not resume canonical issue: %+v", result)
	}
	var issues, tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue WHERE id=$1`, issueID).Scan(&issues); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if issues != 1 || tasks != 1 {
		t.Fatalf("issue-only recovery counts: issues=%d tasks=%d", issues, tasks)
	}
	claimed, err := testHandler.TaskService.ClaimTask(context.Background(), util.MustParseUUID(agentID))
	if err != nil || claimed == nil || claimed.ID != result.Task.ID {
		t.Fatalf("claim repaired intake task: task=%+v err=%v", claimed, err)
	}
	started, err := testHandler.TaskService.StartTask(context.Background(), claimed.ID)
	if err != nil || started == nil || !started.StartedAt.Valid {
		t.Fatalf("start repaired intake task: task=%+v err=%v", started, err)
	}
}

func TestProductIntakeBindingRevisionIsMonotoneAndAudited(t *testing.T) {
	agentID := dbfx.Agent(t, "Tony revision", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
	projectID := dbfx.Project(t, "Tony revision")
	sourceID := uuid.NewString()
	first := createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sourceID, sourceID, agentID, projectID, "targeted_check")
	second := createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sourceID, sourceID, agentID, projectID, "tests_review")
	if first.Revision != 1 || second.Revision != 2 || second.CreatedBy != util.MustParseUUID(testUserID) || second.ChangeReason == "" {
		t.Fatalf("binding history is not monotone/audited: first=%+v second=%+v", first, second)
	}
	var firstStatus string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM product_intake_binding WHERE id = $1`, first.ID).Scan(&firstStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "revoked" {
		t.Fatalf("previous binding status = %q, want revoked", firstStatus)
	}
}

func TestTonyProductIntakeRevalidatesBindingScopeClassificationAndRole(t *testing.T) {
	request := func(agentID, sessionID, text string) service.ProductIntakeRequest {
		return service.ProductIntakeRequest{
			WorkspaceID: util.MustParseUUID(testWorkspaceID), ActorID: util.MustParseUUID(testUserID),
			SourceType: service.IntakeSourceMulticaChat, SourceID: sessionID, ConversationID: sessionID,
			TargetAgentID: util.MustParseUUID(agentID), MessageID: uuid.NewString(), Text: text,
		}
	}

	t.Run("unknown classification", func(t *testing.T) {
		agentID := dbfx.Agent(t, "Tony unclassified", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
		projectID := dbfx.Project(t, "Tony unclassified")
		sessionID := dbfx.ChatSession(t, agentID)
		createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")
		_, err := testHandler.ProductIntake.Process(context.Background(), request(agentID, sessionID, "do something"))
		if !errors.Is(err, service.ErrIntakeUnclassified) {
			t.Fatalf("unknown classification error = %v", err)
		}
	})

	t.Run("project moved out of workspace", func(t *testing.T) {
		agentID := dbfx.Agent(t, "Tony cross-project", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
		projectID := dbfx.Project(t, "Tony cross-project")
		sessionID := dbfx.ChatSession(t, agentID)
		binding := createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")
		foreignWorkspaceID := dbfx.Workspace(t, "Foreign intake workspace", "foreign-intake-"+uuid.NewString())
		foreignProjectID := dbfx.Insert(t, "project", testutil.Cols{
			"workspace_id": foreignWorkspaceID, "title": "Foreign intake project", "status": "planned", "priority": "none",
		})
		if _, err := testPool.Exec(context.Background(), `UPDATE product_intake_binding SET project_id = $1 WHERE id = $2`, foreignProjectID, binding.ID); err != nil {
			t.Fatal(err)
		}
		_, err := testHandler.ProductIntake.Process(context.Background(), request(agentID, sessionID, "feature: no cross-talk"))
		if !errors.Is(err, service.ErrIntakeBindingStale) {
			t.Fatalf("cross-workspace project error = %v", err)
		}
	})

	t.Run("target loses Tony role", func(t *testing.T) {
		agentID := dbfx.Agent(t, "Tony revoked role", testRuntimeID, testutil.Cols{"system_key": service.TonySystemKey})
		projectID := dbfx.Project(t, "Tony revoked role")
		sessionID := dbfx.ChatSession(t, agentID)
		createProductIntakeBinding(t, service.IntakeSourceMulticaChat, sessionID, sessionID, agentID, projectID, "tests_review")
		if _, err := testPool.Exec(context.Background(), `UPDATE agent SET system_key = NULL WHERE id = $1`, agentID); err != nil {
			t.Fatal(err)
		}
		_, err := testHandler.ProductIntake.Process(context.Background(), request(agentID, sessionID, "feature: revoked target"))
		if !errors.Is(err, service.ErrIntakeIdentity) {
			t.Fatalf("revoked Tony role error = %v", err)
		}
	})
}

func createProductIntakeBinding(t *testing.T, sourceType, sourceID, conversationID, agentID, projectID, policy string) db.ProductIntakeBinding {
	t.Helper()
	squadID := dbfx.Squad(t, "intake-"+uuid.NewString(), agentID)
	dbfx.SquadMember(t, squadID, "agent", agentID, testutil.Cols{"role": "product-engineering"})
	binding, err := testHandler.ProductIntake.CreateBindingRevision(context.Background(), service.ProductIntakeBindingRevisionRequest{
		WorkspaceID: util.MustParseUUID(testWorkspaceID), ActorID: util.MustParseUUID(testUserID), SourceType: sourceType,
		SourceID: sourceID, ConversationID: conversationID, TargetAgentID: util.MustParseUUID(agentID),
		ProductKey: "operating-system", ProjectID: util.MustParseUUID(projectID), SquadID: util.MustParseUUID(squadID),
		RequiredCapability: "product-engineering", RiskPolicy: policy,
		ValidUntil: time.Now().Add(time.Hour), CreatedBy: util.MustParseUUID(testUserID), ChangeReason: "test binding",
	})
	if err != nil {
		t.Fatalf("create product intake binding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM product_intake_binding WHERE id = $1`, binding.ID)
	})
	return binding
}
