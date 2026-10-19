package telegram

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/service"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type intakeCanaryReplier struct{}

func (intakeCanaryReplier) Reply(context.Context, engine.ResolvedInstallation, channel.InboundMessage, engine.Result) {
}
func (intakeCanaryReplier) ReplyDurable(context.Context, engine.ResolvedInstallation, channel.InboundMessage, engine.Result) error {
	return nil
}

func TestTelegramProductIntakeCanaryTraversesAdapterServicesAndRuntimeClaim(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	tag := uuid.NewString()
	fx := dbfx.New(testPool, "", "")
	fx.WorkspaceID = fx.Workspace(t, "telegram product "+tag, "telegram-product-"+tag)
	fx.UserID = fx.User(t, "Telegram product", "telegram-product-"+tag+"@example.test")
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "telegram-product-"+tag)
	agentID := fx.Agent(t, "telegram-product-"+tag, runtimeID)
	projectID := fx.Project(t, "telegram-product-"+tag)
	squadID := fx.Squad(t, "telegram-product-"+tag, agentID)
	fx.SquadMember(t, squadID, "agent", agentID, dbfx.Cols{"role": "product-engineering"})

	q := db.New(testPool)
	botID := time.Now().UnixNano()
	inst, err := q.UpsertChannelInstallation(ctx, db.UpsertChannelInstallationParams{
		WorkspaceID: util.MustParseUUID(fx.WorkspaceID), AgentID: util.MustParseUUID(agentID),
		ChannelType: string(TypeTelegram), Config: []byte(fmt.Sprintf(`{"app_id":%d}`, botID)), InstallerUserID: util.MustParseUUID(fx.UserID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateChannelUserBinding(ctx, db.CreateChannelUserBindingParams{
		WorkspaceID: util.MustParseUUID(fx.WorkspaceID), MulticaUserID: util.MustParseUUID(fx.UserID),
		InstallationID: inst.ID, ChannelType: string(TypeTelegram), ChannelUserID: "111", Config: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	bus := events.New()
	tasks := service.NewTaskService(q, testPool, nil, bus)
	issues := service.NewIssueService(q, testPool, bus, nil, tasks)
	intake := service.NewProductIntakeService(q, testPool, issues, tasks)
	if _, err := intake.CreateBindingRevision(ctx, service.ProductIntakeBindingRevisionRequest{
		WorkspaceID: util.MustParseUUID(fx.WorkspaceID), ActorID: util.MustParseUUID(fx.UserID),
		SourceType: service.IntakeSourceTelegram, SourceID: util.UUIDToString(inst.ID), ConversationID: "222",
		TargetAgentID: util.MustParseUUID(agentID), ProductKey: "operating-system", ProjectID: util.MustParseUUID(projectID),
		SquadID: util.MustParseUUID(squadID), RequiredCapability: "product-engineering", RiskPolicy: "tests_review",
		ValidUntil: time.Now().Add(time.Hour), CreatedBy: util.MustParseUUID(fx.UserID), ChangeReason: "canary",
	}); err != nil {
		t.Fatal(err)
	}

	router := engine.NewRouter(issues, tasks, q, engine.RouterConfig{ProductIntake: intake})
	router.Register(TypeTelegram, NewTelegramResolverSet(q, testPool, intakeCanaryReplier{}, nil, nil))
	connector := &telegramChannel{botID: botID, botUsername: "multica_bot", handler: router.Handle}
	if err := connector.dispatch(ctx, Update{UpdateID: 7, Message: &Message{
		MessageID: 8, From: &User{ID: 111, FirstName: "Ada"}, Chat: Chat{ID: 222, Type: "private"},
		Text: "feature: adapter canary " + tag,
	}}); err != nil {
		t.Fatal(err)
	}

	var taskID string
	if err := testPool.QueryRow(ctx, `SELECT id FROM agent_task_queue WHERE agent_id=$1 AND issue_id IS NOT NULL ORDER BY created_at DESC LIMIT 1`, agentID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	claimed, err := tasks.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil || claimed == nil || util.UUIDToString(claimed.ID) != taskID {
		t.Fatalf("claim task: task=%+v err=%v", claimed, err)
	}
	started, err := tasks.StartTask(ctx, claimed.ID)
	if err != nil || started == nil || !started.StartedAt.Valid {
		t.Fatalf("start task: task=%+v err=%v", started, err)
	}
	t.Logf("Telegram canary task_id=%s started_at=%s", taskID, started.StartedAt.Time.Format(time.RFC3339Nano))
}
