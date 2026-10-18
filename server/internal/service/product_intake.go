package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	ApprovedBy     pgtype.UUID
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
	Now       func() time.Time
}

func NewProductIntakeService(q *db.Queries, tx TxStarter, issues *IssueService, tasks *TaskService) *ProductIntakeService {
	return &ProductIntakeService{Queries: q, TxStarter: tx, Issues: issues, Tasks: tasks, Now: time.Now}
}

type ProductIntakeBindingRevisionRequest struct {
	WorkspaceID, ActorID, TargetAgentID, ProjectID, CreatedBy pgtype.UUID
	SourceType, SourceID, ConversationID                      string
	ProductKey, RiskPolicy, ChangeReason                      string
	ValidUntil                                                time.Time
}

func (s *ProductIntakeService) CreateBindingRevision(ctx context.Context, in ProductIntakeBindingRevisionRequest) (db.ProductIntakeBinding, error) {
	if !in.WorkspaceID.Valid || !in.ActorID.Valid || !in.TargetAgentID.Valid || !in.ProjectID.Valid || !in.CreatedBy.Valid ||
		strings.TrimSpace(in.SourceID) == "" || strings.TrimSpace(in.ConversationID) == "" ||
		strings.TrimSpace(in.ProductKey) == "" || strings.TrimSpace(in.ChangeReason) == "" ||
		!in.ValidUntil.After(s.Now()) || (in.SourceType != IntakeSourceMulticaChat && in.SourceType != IntakeSourceTelegram) {
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
		ProductKey: in.ProductKey, ProjectID: in.ProjectID, RiskPolicy: in.RiskPolicy,
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
	now := s.Now()
	if binding.Status != "active" || now.Before(binding.ValidFrom.Time) || !now.Before(binding.ValidUntil.Time) {
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

	kind, title, ok := ClassifyProductIntake(in.Text)
	if !ok {
		return ProductIntakeResult{}, ErrIntakeUnclassified
	}
	if !policyAllows(binding.RiskPolicy, kind) {
		return ProductIntakeResult{}, ErrIntakeRiskPolicy
	}
	if binding.RiskPolicy == "explicit_approval_rollback" && !in.ApprovedBy.Valid {
		return ProductIntakeResult{}, ErrIntakeApproval
	}
	if in.ApprovedBy.Valid {
		if in.ApprovedBy != in.ActorID {
			return ProductIntakeResult{}, ErrIntakeApproval
		}
		if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
			UserID: in.ApprovedBy, WorkspaceID: in.WorkspaceID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ProductIntakeResult{}, ErrIntakeApproval
			}
			return ProductIntakeResult{}, fmt.Errorf("verify product intake approval: %w", err)
		}
	}

	created, err := s.Issues.Create(ctx, IssueCreateParams{
		WorkspaceID:  in.WorkspaceID,
		Title:        title,
		Description:  pgtype.Text{String: strings.TrimSpace(in.Text), Valid: true},
		Status:       "todo",
		Priority:     intakePriority(binding.RiskPolicy),
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
		AssigneeID:   in.TargetAgentID,
		CreatorType:  "member",
		CreatorID:    in.ActorID,
		ProjectID:    binding.ProjectID,
		OriginType:   pgtype.Text{String: intakeOriginType(in.SourceType), Valid: true},
		OriginID:     binding.ID,
	}, IssueCreateOpts{Platform: in.SourceType})
	if err == nil {
		task, taskErr := taskByID(ctx, qtx, created.AssignedTaskID)
		if taskErr != nil {
			return ProductIntakeResult{}, taskErr
		}
		return ProductIntakeResult{Binding: binding, Issue: created.Issue, Task: task, Kind: kind}, nil
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
	return ProductIntakeResult{Binding: binding, Issue: issue, Task: task, Kind: kind, Resumed: true}, nil
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
