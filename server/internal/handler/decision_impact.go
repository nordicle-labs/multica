package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionimpact"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const decisionImpactMetadataKey = "decision_impact_review"

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
	return decisionimpact.HasMarker(value)
}

func (h *Handler) beginDecisionImpactReview(ctx context.Context, issue db.Issue, memberID pgtype.UUID) (db.Issue, error) {
	payload, err := decisionimpact.PendingReview(util.UUIDToString(issue.ID), util.UUIDToString(memberID), issue.Title)
	if err != nil {
		return db.Issue{}, err
	}
	_, err = h.Queries.SetIssueMetadataKeyIfRevision(ctx, db.SetIssueMetadataKeyIfRevisionParams{
		ID:               issue.ID,
		WorkspaceID:      issue.WorkspaceID,
		Key:              decisionImpactMetadataKey,
		Value:            payload,
		ExpectedRevision: issue.Revision,
	})
	if err != nil {
		return db.Issue{}, err
	}
	return h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
}

func (h *Handler) scanDecisionImpactReview(ctx context.Context, decision db.Issue, memberID pgtype.UUID) error {
	const pageSize = 100
	var rows []db.Issue
	for page := int32(0); page < 1000; page++ {
		batch, err := h.Queries.ListDecisionImpactIssuesPage(ctx, db.ListDecisionImpactIssuesPageParams{
			WorkspaceID: decision.WorkspaceID, PageOffset: page * pageSize, PageLimit: pageSize,
		})
		if err != nil {
			return fmt.Errorf("list decision impact issues page %d: %w", page, err)
		}
		rows = append(rows, batch...)
		if len(batch) < pageSize {
			break
		}
		if page == 999 {
			return errors.New("decision impact scan exceeded 100000 issues")
		}
	}

	prefix := h.getIssuePrefix(ctx, decision.WorkspaceID)
	identifiers := make(map[pgtype.UUID]string, len(rows))
	for _, issue := range rows {
		identifiers[issue.ID] = fmt.Sprintf("%s-%d", prefix, issue.Number)
	}
	input := decisionimpact.Input{Decision: decisionimpact.Decision{
		Identifier: identifiers[decision.ID], ProjectID: util.UUIDToString(decision.ProjectID),
		ActorType: "member", ActorRole: "owner", Text: decision.Title + "\n" + decision.Description.String,
	}}
	for _, issue := range rows {
		comments, err := h.Queries.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{
			IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, Limit: 30,
		})
		if err != nil {
			return fmt.Errorf("list comments for %s: %w", identifiers[issue.ID], err)
		}
		commentText := make([]string, len(comments))
		for i, comment := range comments {
			commentText[i] = comment.Content
		}
		candidate := decisionimpact.Issue{
			Identifier: identifiers[issue.ID], ProjectID: util.UUIDToString(issue.ProjectID), Status: issue.Status,
			Text: issue.Title + "\n" + issue.Description.String, Comments: commentText,
		}
		if issue.ParentIssueID.Valid {
			candidate.ParentIdentifier = identifiers[issue.ParentIssueID]
		}
		input.Issues = append(input.Issues, candidate)
		runs, err := h.Queries.ListDecisionImpactRunsByIssue(ctx, issue.ID)
		if err != nil {
			return fmt.Errorf("list active runs for %s: %w", candidate.Identifier, err)
		}
		for _, run := range runs {
			input.Runs = append(input.Runs, decisionimpact.Run{
				ID: util.UUIDToString(run.ID), IssueIdentifier: candidate.Identifier, Status: run.Status,
			})
		}
	}
	result := decisionimpact.Analyze(input)
	matrix := result.Matrix[:0]
	for _, row := range result.Matrix {
		if row.Relation != "none" {
			matrix = append(matrix, row)
		}
	}
	payload, err := decisionimpact.PendingReviewWithMatrix(
		util.UUIDToString(decision.ID), util.UUIDToString(memberID), decision.Title, matrix,
	)
	if err != nil {
		return err
	}
	_, err = h.Queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		ID: decision.ID, WorkspaceID: decision.WorkspaceID, Key: decisionImpactMetadataKey, Value: payload,
	})
	return err
}
