package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const TonySystemKey = "tony"

const (
	IntakeSourceMulticaChat = "multica_chat"
	IntakeSourceTelegram    = "telegram"
)

var (
	ErrIntakeBindingMissing = errors.New("product intake binding missing")
	ErrIntakeBindingStale   = errors.New("product intake binding stale")
	ErrIntakeIdentity       = errors.New("product intake identity is invalid")
	ErrIntakeUnclassified   = errors.New("product intake classification is required")
	ErrIntakeRiskPolicy     = errors.New("product intake risk policy is insufficient")
	ErrIntakeApproval       = errors.New("product intake requires explicit human approval")
)

type ProductIntakeRequest struct {
	WorkspaceID    pgtype.UUID
	ActorID        pgtype.UUID
	SourceType     string
	SourceID       string
	ConversationID string
	TargetAgentID  pgtype.UUID
	MessageID      string
	Text           string
	ApprovalID     pgtype.UUID
}

type ProductIntakeResult struct {
	Binding db.ProductIntakeBinding
	Issue   db.Issue
	Task    db.AgentTaskQueue
	Kind    string
	Resumed bool
}

type ProductIntakeService struct {
	Queries   *db.Queries
	TxStarter TxStarter
	Issues    *IssueService
	Tasks     *TaskService
}

func NewProductIntakeService(q *db.Queries, tx TxStarter, issues *IssueService, tasks *TaskService) *ProductIntakeService {
	return &ProductIntakeService{Queries: q, TxStarter: tx, Issues: issues, Tasks: tasks}
}

type ProductIntakeBindingRevisionRequest struct {
	WorkspaceID, ActorID, TargetAgentID, ProjectID, SquadID, CreatedBy pgtype.UUID
	SourceType, SourceID, ConversationID                               string
	ProductKey, RequiredCapability, RiskPolicy, ChangeReason           string
	ValidUntil                                                         time.Time
}

func (s *ProductIntakeService) CreateBindingRevision(ctx context.Context, in ProductIntakeBindingRevisionRequest) (db.ProductIntakeBinding, error) {
	if !in.WorkspaceID.Valid || !in.ActorID.Valid || !in.TargetAgentID.Valid || !in.ProjectID.Valid || !in.SquadID.Valid || !in.CreatedBy.Valid ||
		strings.TrimSpace(in.SourceID) == "" || strings.TrimSpace(in.ConversationID) == "" ||
		strings.TrimSpace(in.ProductKey) == "" || strings.TrimSpace(in.RequiredCapability) == "" || strings.TrimSpace(in.ChangeReason) == "" ||
		(in.SourceType != IntakeSourceMulticaChat && in.SourceType != IntakeSourceTelegram) {
		return db.ProductIntakeBinding{}, ErrIntakeIdentity
	}
	if _, ok := map[string]bool{"targeted_check": true, "tests_review": true, "security_integration": true, "explicit_approval_rollback": true}[in.RiskPolicy]; !ok {
		return db.ProductIntakeBinding{}, ErrIntakeRiskPolicy
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("begin product intake binding revision: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	databaseNow, err := qtx.GetProductIntakeDatabaseNow(ctx)
	if err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("read product intake database clock: %w", err)
	}
	if !databaseNow.Valid || !in.ValidUntil.After(databaseNow.Time) {
		return db.ProductIntakeBinding{}, ErrIntakeIdentity
	}
	if err := qtx.LockProductIntakeBindingSource(ctx, db.LockProductIntakeBindingSourceParams{
		WorkspaceID: util.UUIDToString(in.WorkspaceID), SourceType: in.SourceType,
		SourceID: in.SourceID, ConversationID: in.ConversationID,
		ActorID: util.UUIDToString(in.ActorID), TargetAgentID: util.UUIDToString(in.TargetAgentID),
	}); err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("lock product intake binding source: %w", err)
	}
	if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: in.ActorID, WorkspaceID: in.WorkspaceID}); err != nil {
		return db.ProductIntakeBinding{}, ErrIntakeIdentity
	}
	if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: in.CreatedBy, WorkspaceID: in.WorkspaceID}); err != nil {
		return db.ProductIntakeBinding{}, ErrIntakeIdentity
	}
	if _, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: in.TargetAgentID, WorkspaceID: in.WorkspaceID}); err != nil {
		return db.ProductIntakeBinding{}, ErrIntakeIdentity
	}
	if _, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: in.ProjectID, WorkspaceID: in.WorkspaceID}); err != nil {
		return db.ProductIntakeBinding{}, ErrIntakeIdentity
	}
	if err := validateIntakeRoute(ctx, qtx, in.WorkspaceID, in.SquadID, in.TargetAgentID, in.RequiredCapability); err != nil {
		return db.ProductIntakeBinding{}, err
	}
	key := db.NextProductIntakeBindingRevisionParams{
		WorkspaceID: in.WorkspaceID, SourceType: in.SourceType, SourceID: in.SourceID,
		ConversationID: in.ConversationID, ActorID: in.ActorID, TargetAgentID: in.TargetAgentID,
	}
	if err := qtx.RevokeActiveProductIntakeBinding(ctx, db.RevokeActiveProductIntakeBindingParams{
		RevokedBy: in.CreatedBy, WorkspaceID: in.WorkspaceID, SourceType: in.SourceType,
		SourceID: in.SourceID, ConversationID: in.ConversationID, ActorID: in.ActorID, TargetAgentID: in.TargetAgentID,
	}); err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("revoke product intake binding: %w", err)
	}
	revision, err := qtx.NextProductIntakeBindingRevision(ctx, key)
	if err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("next product intake binding revision: %w", err)
	}
	binding, err := qtx.InsertProductIntakeBindingRevision(ctx, db.InsertProductIntakeBindingRevisionParams{
		WorkspaceID: in.WorkspaceID, SourceType: in.SourceType, SourceID: in.SourceID,
		ConversationID: in.ConversationID, ActorID: in.ActorID, TargetAgentID: in.TargetAgentID,
		ProductKey: in.ProductKey, ProjectID: in.ProjectID, SquadID: in.SquadID,
		RequiredCapability: in.RequiredCapability, RiskPolicy: in.RiskPolicy,
		Revision: int64(revision), ValidUntil: pgtype.Timestamptz{Time: in.ValidUntil, Valid: true},
		CreatedBy: in.CreatedBy, ChangeReason: in.ChangeReason,
	})
	if err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("insert product intake binding revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.ProductIntakeBinding{}, fmt.Errorf("commit product intake binding revision: %w", err)
	}
	return binding, nil
}

type ProductIntakeApprovalRequest struct {
	WorkspaceID, BindingID, ApproverID pgtype.UUID
	MessageID, ArtifactSHA, Decision   string
}

var gitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// CreateApproval records a distinct immutable human decision. Process pins it
// to the concrete task and SHA when the approved intake is dispatched.
func (s *ProductIntakeService) CreateApproval(ctx context.Context, in ProductIntakeApprovalRequest) (db.ProductIntakeApproval, error) {
	if !in.WorkspaceID.Valid || !in.BindingID.Valid || !in.ApproverID.Valid || strings.TrimSpace(in.MessageID) == "" ||
		!gitSHA.MatchString(in.ArtifactSHA) || (in.Decision != "approved" && in.Decision != "denied") {
		return db.ProductIntakeApproval{}, ErrIntakeApproval
	}
	binding, err := s.Queries.GetProductIntakeBinding(ctx, in.BindingID)
	if err != nil || binding.WorkspaceID != in.WorkspaceID || binding.Status != "active" || binding.ActorID == in.ApproverID {
		return db.ProductIntakeApproval{}, ErrIntakeApproval
	}
	member, err := s.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: in.ApproverID, WorkspaceID: in.WorkspaceID})
	if err != nil || (member.Role != "owner" && member.Role != "admin") {
		return db.ProductIntakeApproval{}, ErrIntakeApproval
	}
	approval, err := s.Queries.InsertProductIntakeApproval(ctx, db.InsertProductIntakeApprovalParams{
		WorkspaceID: in.WorkspaceID, BindingID: in.BindingID, MessageID: in.MessageID,
		ApproverID: in.ApproverID, ArtifactSha: in.ArtifactSHA, Decision: in.Decision,
	})
	if err != nil {
		return db.ProductIntakeApproval{}, fmt.Errorf("persist product intake approval: %w", err)
	}
	return approval, nil
}

func IsTonyAgent(agent db.Agent) bool {
	return agent.SystemKey.Valid && agent.SystemKey.String == TonySystemKey
}

func IsProductIntakeRefusal(err error) bool {
	return errors.Is(err, ErrIntakeBindingStale) || errors.Is(err, ErrIntakeIdentity) ||
		errors.Is(err, ErrIntakeUnclassified) || errors.Is(err, ErrIntakeRiskPolicy) ||
		errors.Is(err, ErrIntakeApproval)
}

func ClassifyProductIntake(text string) (kind, title string, ok bool) {
	prefix, body, found := strings.Cut(strings.TrimSpace(text), ":")
	if !found || strings.TrimSpace(body) == "" {
		return "", "", false
	}
	kind = strings.ToLower(strings.TrimSpace(prefix))
	switch kind {
	case "idea", "bug", "feature", "incident", "research", "project", "release":
		return kind, strings.TrimSpace(body), true
	default:
		return "", "", false
	}
}

func (s *ProductIntakeService) Process(ctx context.Context, in ProductIntakeRequest) (ProductIntakeResult, error) {
	if !in.WorkspaceID.Valid || !in.ActorID.Valid || !in.TargetAgentID.Valid ||
		strings.TrimSpace(in.SourceID) == "" || strings.TrimSpace(in.ConversationID) == "" ||
		strings.TrimSpace(in.MessageID) == "" || (in.SourceType != IntakeSourceMulticaChat && in.SourceType != IntakeSourceTelegram) {
		return ProductIntakeResult{}, ErrIntakeIdentity
	}

	// Serialize the canonical issue/task decision with binding revisions for this
	// exact source identity. This closes the post-issue/pre-task race without
	// introducing a second deduplication mechanism.
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return ProductIntakeResult{}, fmt.Errorf("begin product intake: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	if err := qtx.LockProductIntakeBindingSource(ctx, db.LockProductIntakeBindingSourceParams{
		WorkspaceID: util.UUIDToString(in.WorkspaceID), SourceType: in.SourceType,
		SourceID: in.SourceID, ConversationID: in.ConversationID,
		ActorID: util.UUIDToString(in.ActorID), TargetAgentID: util.UUIDToString(in.TargetAgentID),
	}); err != nil {
		return ProductIntakeResult{}, fmt.Errorf("lock product intake: %w", err)
	}

	binding, err := qtx.GetLatestProductIntakeBinding(ctx, db.GetLatestProductIntakeBindingParams{
		WorkspaceID: in.WorkspaceID, SourceType: in.SourceType, SourceID: in.SourceID,
		ConversationID: in.ConversationID, ActorID: in.ActorID, TargetAgentID: in.TargetAgentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductIntakeResult{}, ErrIntakeBindingMissing
	}
	if err != nil {
		return ProductIntakeResult{}, fmt.Errorf("resolve product intake binding: %w", err)
	}
	now, err := qtx.GetProductIntakeDatabaseNow(ctx)
	if err != nil {
		return ProductIntakeResult{}, fmt.Errorf("read product intake database clock: %w", err)
	}
	if !now.Valid || binding.Status != "active" || now.Time.Before(binding.ValidFrom.Time) || !now.Time.Before(binding.ValidUntil.Time) {
		return ProductIntakeResult{}, ErrIntakeBindingStale
	}

	if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: in.ActorID, WorkspaceID: in.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProductIntakeResult{}, ErrIntakeIdentity
		}
		return ProductIntakeResult{}, fmt.Errorf("verify product intake actor: %w", err)
	}
	agent, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: in.TargetAgentID, WorkspaceID: in.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid || (in.SourceType == IntakeSourceMulticaChat && !IsTonyAgent(agent)) {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ProductIntakeResult{}, fmt.Errorf("verify product intake target: %w", err)
		}
		return ProductIntakeResult{}, ErrIntakeIdentity
	}
	if _, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: binding.ProjectID, WorkspaceID: in.WorkspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProductIntakeResult{}, ErrIntakeBindingStale
		}
		return ProductIntakeResult{}, fmt.Errorf("verify product intake project: %w", err)
	}
	if err := validateIntakeRoute(ctx, qtx, in.WorkspaceID, binding.SquadID, in.TargetAgentID, binding.RequiredCapability); err != nil {
		return ProductIntakeResult{}, err
	}

	deliveryID := productIntakeDeliveryID(in)
	delivery, deliveryErr := qtx.GetProductIntakeDelivery(ctx, db.GetProductIntakeDeliveryParams{
		WorkspaceID: in.WorkspaceID, SourceType: in.SourceType, SourceID: in.SourceID,
		ConversationID: in.ConversationID, ActorID: in.ActorID, TargetAgentID: in.TargetAgentID, MessageID: in.MessageID,
	})
	if deliveryErr != nil && !errors.Is(deliveryErr, pgx.ErrNoRows) {
		return ProductIntakeResult{}, fmt.Errorf("resolve product intake delivery: %w", deliveryErr)
	}

	kind, title, ok := ClassifyProductIntake(in.Text)
	if !ok {
		return ProductIntakeResult{}, ErrIntakeUnclassified
	}
	if !policyAllows(binding.RiskPolicy, kind) {
		return ProductIntakeResult{}, ErrIntakeRiskPolicy
	}
	approvalID := in.ApprovalID
	if deliveryErr == nil {
		approvalID = delivery.ApprovalID
	}
	approval, err := validateProductIntakeApproval(ctx, qtx, in, binding, approvalID)
	if err != nil {
		return ProductIntakeResult{}, err
	}
	if deliveryErr == nil {
		if delivery.ArtifactSha.Valid && (!approval.ID.Valid || approval.ArtifactSha != delivery.ArtifactSha.String) {
			return ProductIntakeResult{}, ErrIntakeApproval
		}
		issue, issueErr := qtx.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: delivery.IssueID, WorkspaceID: in.WorkspaceID})
		task, taskErr := qtx.GetProductIntakeTask(ctx, delivery.TaskID)
		if issueErr != nil || taskErr != nil {
			return ProductIntakeResult{}, ErrIntakeBindingStale
		}
		return ProductIntakeResult{Binding: binding, Issue: issue, Task: task, Kind: kind, Resumed: true}, nil
	}

	// IssueService commits issue + assigned task before this service records the
	// immutable delivery. Recover that exact pair by deterministic origin after
	// a crash or persistence failure; MessageID can never create a second pair.
	if issue, issueErr := qtx.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
		WorkspaceID: in.WorkspaceID,
		OriginType:  pgtype.Text{String: intakeOriginType(in.SourceType), Valid: true},
		OriginID:    deliveryID,
	}); issueErr == nil {
		task, taskErr := qtx.GetLatestProductIntakeTaskForIssue(ctx, db.GetLatestProductIntakeTaskForIssueParams{
			IssueID: issue.ID, AgentID: in.TargetAgentID,
		})
		if taskErr != nil {
			return ProductIntakeResult{}, fmt.Errorf("recover product intake task: %w", taskErr)
		}
		return s.persistIntakeDelivery(ctx, tx, qtx, in, binding, approval, deliveryID, issue, task, kind, true)
	} else if !errors.Is(issueErr, pgx.ErrNoRows) {
		return ProductIntakeResult{}, fmt.Errorf("recover product intake issue: %w", issueErr)
	}

	created, err := s.Issues.Create(ctx, IssueCreateParams{
		WorkspaceID:  in.WorkspaceID,
		Title:        title,
		Description:  pgtype.Text{String: strings.TrimSpace(in.Text), Valid: true},
		Status:       "todo",
		Priority:     intakePriority(binding.RiskPolicy),
		AssigneeType: pgtype.Text{String: "squad", Valid: true},
		AssigneeID:   binding.SquadID,
		CreatorType:  "member",
		CreatorID:    in.ActorID,
		ProjectID:    binding.ProjectID,
		OriginType:   pgtype.Text{String: intakeOriginType(in.SourceType), Valid: true},
		OriginID:     deliveryID,
	}, IssueCreateOpts{Platform: in.SourceType})
	if err == nil {
		task, taskErr := qtx.GetLatestActiveProductIntakeTask(ctx, db.GetLatestActiveProductIntakeTaskParams{
			IssueID: created.Issue.ID, AgentID: in.TargetAgentID,
		})
		if taskErr != nil {
			return ProductIntakeResult{}, taskErr
		}
		return s.persistIntakeDelivery(ctx, tx, qtx, in, binding, approval, deliveryID, created.Issue, task, kind, false)
	}
	if !errors.Is(err, ErrActiveDuplicate) || created.DuplicateIssue == nil {
		return ProductIntakeResult{}, fmt.Errorf("create product intake issue: %w", err)
	}

	issue := *created.DuplicateIssue
	task, taskErr := qtx.GetLatestActiveProductIntakeTask(ctx, db.GetLatestActiveProductIntakeTaskParams{
		IssueID: issue.ID, AgentID: in.TargetAgentID,
	})
	if errors.Is(taskErr, pgx.ErrNoRows) {
		task, taskErr = s.Tasks.EnqueueTaskForIssueByActor(ctx, issue, in.ActorID)
	}
	if taskErr != nil {
		return ProductIntakeResult{}, fmt.Errorf("resume product intake issue: %w", taskErr)
	}
	return s.persistIntakeDelivery(ctx, tx, qtx, in, binding, approval, deliveryID, issue, task, kind, true)
}

func validateProductIntakeApproval(ctx context.Context, q *db.Queries, in ProductIntakeRequest, binding db.ProductIntakeBinding, approvalID pgtype.UUID) (db.ProductIntakeApproval, error) {
	if binding.RiskPolicy == "explicit_approval_rollback" && !approvalID.Valid {
		return db.ProductIntakeApproval{}, ErrIntakeApproval
	}
	if !approvalID.Valid {
		return db.ProductIntakeApproval{}, nil
	}
	approval, err := q.GetProductIntakeApproval(ctx, db.GetProductIntakeApprovalParams{
		ID: approvalID, WorkspaceID: in.WorkspaceID, BindingID: binding.ID, MessageID: in.MessageID,
	})
	if err != nil || approval.Decision != "approved" || approval.ApproverID == in.ActorID {
		return db.ProductIntakeApproval{}, ErrIntakeApproval
	}
	member, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: approval.ApproverID, WorkspaceID: in.WorkspaceID,
	})
	if err != nil || (member.Role != "owner" && member.Role != "admin") {
		return db.ProductIntakeApproval{}, ErrIntakeApproval
	}
	return approval, nil
}

func (s *ProductIntakeService) persistIntakeDelivery(ctx context.Context, tx pgx.Tx, q *db.Queries, in ProductIntakeRequest, binding db.ProductIntakeBinding, approval db.ProductIntakeApproval, deliveryID pgtype.UUID, issue db.Issue, task db.AgentTaskQueue, kind string, resumed bool) (ProductIntakeResult, error) {
	params := db.InsertProductIntakeDeliveryParams{
		ID: deliveryID, WorkspaceID: in.WorkspaceID, SourceType: in.SourceType, SourceID: in.SourceID,
		ConversationID: in.ConversationID, ActorID: in.ActorID, TargetAgentID: in.TargetAgentID,
		MessageID: in.MessageID, BindingID: binding.ID, IssueID: issue.ID, TaskID: task.ID,
	}
	if approval.ID.Valid {
		params.ApprovalID = approval.ID
		params.ArtifactSha = pgtype.Text{String: approval.ArtifactSha, Valid: true}
	}
	if _, err := q.InsertProductIntakeDelivery(ctx, params); err != nil {
		return ProductIntakeResult{}, fmt.Errorf("persist product intake delivery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ProductIntakeResult{}, fmt.Errorf("commit product intake delivery: %w", err)
	}
	return ProductIntakeResult{Binding: binding, Issue: issue, Task: task, Kind: kind, Resumed: resumed}, nil
}

func productIntakeDeliveryID(in ProductIntakeRequest) pgtype.UUID {
	key := strings.Join([]string{util.UUIDToString(in.WorkspaceID), in.SourceType, in.SourceID, in.ConversationID, util.UUIDToString(in.ActorID), util.UUIDToString(in.TargetAgentID), in.MessageID}, "\x1f")
	return util.MustParseUUID(uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String())
}

func validateIntakeRoute(ctx context.Context, q *db.Queries, workspaceID, squadID, targetAgentID pgtype.UUID, capability string) error {
	squad, err := q.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: squadID, WorkspaceID: workspaceID})
	if err != nil || squad.ArchivedAt.Valid || squad.LeaderID != targetAgentID {
		return ErrIntakeIdentity
	}
	members, err := q.ListSquadMembers(ctx, squadID)
	if err != nil {
		return fmt.Errorf("verify product intake capability: %w", err)
	}
	for _, member := range members {
		if member.MemberType == "agent" && member.MemberID == targetAgentID && member.Role == capability {
			return nil
		}
	}
	return ErrIntakeRiskPolicy
}

func taskByID(ctx context.Context, q *db.Queries, id pgtype.UUID) (db.AgentTaskQueue, error) {
	if !id.Valid {
		return db.AgentTaskQueue{}, errors.New("product intake issue was created without an assigned task")
	}
	task, err := q.GetProductIntakeTask(ctx, id)
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("load product intake task: %w", err)
	}
	return task, nil
}

func policyAllows(policy, kind string) bool {
	required := map[string]int{
		"idea": 0, "research": 0,
		"bug": 1, "feature": 1, "project": 1,
		"incident": 2,
		"release":  3,
	}[kind]
	actual, ok := map[string]int{
		"targeted_check": 0, "tests_review": 1,
		"security_integration": 2, "explicit_approval_rollback": 3,
	}[policy]
	return ok && actual >= required
}

func intakePriority(policy string) string {
	switch policy {
	case "security_integration", "explicit_approval_rollback":
		return "high"
	case "tests_review":
		return "medium"
	default:
		return "low"
	}
}

func intakeOriginType(sourceType string) string {
	if sourceType == IntakeSourceTelegram {
		return "telegram_chat"
	}
	return sourceType
}
