package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/service"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRouter_TelegramProductIntakeCanaryUsesRealIssueAndTaskServices(t *testing.T) {
	pool := invokeGateTestDB(t)
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	fx := dbfx.New(pool, "", "")
	fx.WorkspaceID = fx.Workspace(t, "telegram-intake "+tag, "telegram-intake-"+tag)
	fx.UserID = fx.User(t, "Telegram intake "+tag, "telegram-intake-"+tag+"@example.test")
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "telegram-intake-"+tag)
	agentID := fx.Agent(t, "telegram-intake-"+tag, runtimeID)
	projectID := fx.Project(t, "telegram-intake-"+tag)
	installationID := uuid.NewString()

	queries := db.New(pool)
	bus := events.New()
	tasks := service.NewTaskService(queries, pool, nil, bus)
	issues := service.NewIssueService(queries, pool, bus, nil, tasks)
	intake := service.NewProductIntakeService(queries, pool, issues, tasks)
	if _, err := intake.CreateBindingRevision(context.Background(), service.ProductIntakeBindingRevisionRequest{
		WorkspaceID: util.MustParseUUID(fx.WorkspaceID), ActorID: util.MustParseUUID(fx.UserID),
		SourceType: service.IntakeSourceTelegram, SourceID: installationID, ConversationID: "chat-42:topic-7",
		TargetAgentID: util.MustParseUUID(agentID), ProductKey: "operating-system",
		ProjectID: util.MustParseUUID(projectID), RiskPolicy: "tests_review",
		ValidUntil: time.Now().Add(time.Hour), CreatedBy: util.MustParseUUID(fx.UserID), ChangeReason: "integration canary",
	}); err != nil {
		t.Fatalf("create Telegram intake binding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM product_intake_binding WHERE source_id = $1`, installationID)
	})

	h := newHarness(t)
	h.inst.inst = ResolvedInstallation{
		ID: util.MustParseUUID(installationID), WorkspaceID: util.MustParseUUID(fx.WorkspaceID),
		AgentID: util.MustParseUUID(agentID), InstallerUserID: util.MustParseUUID(fx.UserID), Active: true,
	}
	h.ident.id = ResolvedIdentity{UserID: util.MustParseUUID(fx.UserID)}
	h.router = NewRouter(h.issues, tasks, h.reader, RouterConfig{Logger: discardLogger(), ProductIntake: intake})
	h.router.Register(channel.Type("telegram"), ResolverSet{
		Installation: h.inst, Identity: h.ident, Dedup: h.dedup,
		Session: h.binder, Audit: h.audit, Replier: h.replier, OriginType: "telegram_chat",
	})
	msg := p2pMessage(t)
	msg.Source.ChannelType = channel.Type("telegram")
	msg.Source.ChatID = "chat-42"
	msg.Source.ThreadID = "topic-7"
	msg.MessageID = "chat-42:9"
	msg.CommandText = "feature: Telegram DB canary " + tag
	if err := h.router.Handle(context.Background(), msg); err != nil {
		t.Fatalf("route Telegram intake: %v", err)
	}

	var issue db.Issue
	if err := pool.QueryRow(context.Background(), `
		SELECT id, number, title, project_id, origin_type, origin_id
		FROM issue WHERE workspace_id = $1 AND title = $2
	`, fx.WorkspaceID, "Telegram DB canary "+tag).Scan(
		&issue.ID, &issue.Number, &issue.Title, &issue.ProjectID, &issue.OriginType, &issue.OriginID,
	); err != nil {
		t.Fatalf("load Telegram intake issue: %v", err)
	}
	var task db.AgentTaskQueue
	if err := pool.QueryRow(context.Background(), `
		SELECT id, issue_id, agent_id, status, started_at
		FROM agent_task_queue WHERE issue_id = $1
	`, issue.ID).Scan(&task.ID, &task.IssueID, &task.AgentID, &task.Status, &task.StartedAt); err != nil {
		t.Fatalf("load Telegram intake task: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issue.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue.ID)
	})
	if !task.ID.Valid || task.IssueID != issue.ID || task.AgentID != util.MustParseUUID(agentID) {
		t.Fatalf("Telegram intake task is not correlated: %+v", task)
	}
	if h.binder.ensureCalls != 0 || h.dedup.marks() != 1 {
		t.Fatalf("Telegram intake crossed into Chat or lost dedup: ensure=%d marks=%d", h.binder.ensureCalls, h.dedup.marks())
	}
}
