package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const decisionImpactMetadataKey = "decision_impact_review"

type decisionImpactPending struct {
	State                string `json:"state"`
	DecisionIssueID      string `json:"decision_issue_id"`
	AuthorizedMemberID   string `json:"authorized_member_id"`
	DecisionChanged      string `json:"decision_changed"`
	ImpactedAndCorrected []any  `json:"impacted_and_corrected"`
	PendingImpact        []any  `json:"pending_impact"`
	NotImpacted          []any  `json:"not_impacted"`
	ActiveAfterReview    []any  `json:"active_after_review"`
}

func startsDecisionImpactReview(prev db.Issue, req UpdateIssueRequest) bool {
	before := prev.Title + "\n" + prev.Description.String
	afterTitle := prev.Title
	if req.Title != nil {
		afterTitle = *req.Title
	}
	afterDescription := prev.Description.String
	if req.Description != nil {
		afterDescription = *req.Description
	}
	after := afterTitle + "\n" + afterDescription
	return before != after && hasDecisionMarker(after)
}

func hasDecisionMarker(value string) bool {
	upper := strings.ToUpper(value)
	return strings.Contains(upper, "DECISION CLEMENT") ||
		strings.Contains(upper, "DECISION CLÉMENT")
}

func (h *Handler) beginDecisionImpactReview(ctx context.Context, issue db.Issue, memberID pgtype.UUID) (db.Issue, error) {
	payload, err := json.Marshal(decisionImpactPending{
		State:                "pending",
		DecisionIssueID:      util.UUIDToString(issue.ID),
		AuthorizedMemberID:   util.UUIDToString(memberID),
		DecisionChanged:      issue.Title,
		ImpactedAndCorrected: []any{},
		PendingImpact:        []any{},
		NotImpacted:          []any{},
		ActiveAfterReview:    []any{},
	})
	if err != nil {
		return db.Issue{}, err
	}
	_, err = h.Queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		ID:          issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Key:         decisionImpactMetadataKey,
		Value:       payload,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, err
	}
	return h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
}
