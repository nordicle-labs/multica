package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionimpact"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const decisionImpactMetadataKey = "decision_impact_review"

var errInvalidDecisionImpactCompletion = errors.New("invalid decision impact completion")

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
	issuesByIdentifier := make(map[string]db.Issue, len(rows))
	projectIDs := make([]pgtype.UUID, 0, len(rows))
	seenProjects := map[string]bool{}
	for _, issue := range rows {
		identifiers[issue.ID] = fmt.Sprintf("%s-%d", prefix, issue.Number)
		issuesByIdentifier[identifiers[issue.ID]] = issue
		projectID := util.UUIDToString(issue.ProjectID)
		if projectID != "" && !seenProjects[projectID] {
			seenProjects[projectID] = true
			projectIDs = append(projectIDs, issue.ProjectID)
		}
	}
	dependencies, err := h.Queries.ListDecisionImpactDependencies(ctx, decision.WorkspaceID)
	if err != nil {
		return fmt.Errorf("list decision impact dependencies: %w", err)
	}
	dependent := map[string]bool{}
	for _, dependency := range dependencies {
		if dependency.IssueID == decision.ID {
			dependent[util.UUIDToString(dependency.DependsOnIssueID)] = true
		}
		if dependency.DependsOnIssueID == decision.ID {
			dependent[util.UUIDToString(dependency.IssueID)] = true
		}
	}
	sharedResourceProjects := map[string]bool{}
	if decision.ProjectID.Valid && len(projectIDs) > 0 {
		resources, err := h.Queries.ListProjectResourcesForProjects(ctx, projectIDs)
		if err != nil {
			return fmt.Errorf("list decision impact project resources: %w", err)
		}
		decisionResources := map[string]bool{}
		for _, resource := range resources {
			key := resource.ResourceType + ":" + string(resource.ResourceRef)
			if resource.ProjectID == decision.ProjectID {
				decisionResources[key] = true
			}
		}
		for _, resource := range resources {
			key := resource.ResourceType + ":" + string(resource.ResourceRef)
			if resource.ProjectID != decision.ProjectID && decisionResources[key] {
				sharedResourceProjects[util.UUIDToString(resource.ProjectID)] = true
			}
		}
	}
	input := decisionimpact.Input{Decision: decisionimpact.Decision{
		Identifier: identifiers[decision.ID], ProjectID: util.UUIDToString(decision.ProjectID),
		ActorType: "member", ActorRole: "owner", Text: decision.Title + "\n" + decision.Description.String,
	}}
	runIDsByIssue := map[string][]pgtype.UUID{}
	for _, issue := range rows {
		var commentText []string
		for page := int32(0); page < 1000; page++ {
			comments, err := h.Queries.ListDecisionImpactCommentsPage(ctx, db.ListDecisionImpactCommentsPageParams{
				IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, PageOffset: page * pageSize, PageLimit: pageSize,
			})
			if err != nil {
				return fmt.Errorf("list comments page %d for %s: %w", page, identifiers[issue.ID], err)
			}
			for _, comment := range comments {
				commentText = append(commentText, comment.Content)
			}
			if len(comments) < pageSize {
				break
			}
			if page == 999 {
				return fmt.Errorf("comment scan for %s exceeded 100000 rows", identifiers[issue.ID])
			}
		}
		candidate := decisionimpact.Issue{
			Identifier: identifiers[issue.ID], ProjectID: util.UUIDToString(issue.ProjectID), Status: issue.Status,
			Text: issue.Title + "\n" + issue.Description.String, Comments: commentText,
			Dependency: dependent[util.UUIDToString(issue.ID)], SharedResource: sharedResourceProjects[util.UUIDToString(issue.ProjectID)],
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
			runIDsByIssue[candidate.Identifier] = append(runIDsByIssue[candidate.Identifier], run.ID)
			input.Runs = append(input.Runs, decisionimpact.Run{
				ID: util.UUIDToString(run.ID), IssueIdentifier: candidate.Identifier, Status: run.Status,
			})
		}
	}
	result := decisionimpact.Analyze(input)
	matrix := make([]decisionimpact.MatrixRow, 0, len(result.Matrix))
	for _, row := range result.Matrix {
		if row.Relation != "none" {
			matrix = append(matrix, row)
		}
	}
	scanID := util.MustParseUUID(uuid.NewString())
	payload, err := decisionimpact.PendingReviewForScan(
		util.UUIDToString(scanID), util.UUIDToString(decision.ID), util.UUIDToString(memberID), decision.Title, len(matrix),
	)
	if err != nil {
		return err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	if err := qtx.DeleteDecisionImpactCandidatesForIssue(ctx, db.DeleteDecisionImpactCandidatesForIssueParams{
		WorkspaceID: decision.WorkspaceID, DecisionIssueID: decision.ID,
	}); err != nil {
		return err
	}
	for _, row := range matrix {
		issue := issuesByIdentifier[row.IssueIdentifier]
		activeRunIDs := runIDsByIssue[row.IssueIdentifier]
		if activeRunIDs == nil {
			activeRunIDs = []pgtype.UUID{}
		}
		if err := qtx.CreateDecisionImpactCandidate(ctx, db.CreateDecisionImpactCandidateParams{
			ScanID: scanID, WorkspaceID: decision.WorkspaceID, DecisionIssueID: decision.ID,
			CandidateIssueID: issue.ID, IssueIdentifier: row.IssueIdentifier, Relation: row.Relation,
			ExpectedAction: string(row.Action), IssueRevision: issue.Revision, IssueStatus: issue.Status,
			ActiveRunIds: activeRunIDs,
		}); err != nil {
			return err
		}
	}
	if _, err := qtx.SetIssueMetadataKeyIfRevision(ctx, db.SetIssueMetadataKeyIfRevisionParams{
		ID: decision.ID, WorkspaceID: decision.WorkspaceID, Key: decisionImpactMetadataKey,
		Value: payload, ExpectedRevision: decision.Revision,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateDecisionImpactCompletion(review decisionimpact.Review, candidates []db.LockDecisionImpactCandidatesForCompletionRow) error {
	classified := map[string]struct {
		row       decisionimpact.MatrixRow
		corrected bool
	}{}
	for _, group := range []struct {
		rows      []decisionimpact.MatrixRow
		corrected bool
	}{{review.ImpactedAndCorrected, true}, {review.NotImpacted, false}, {review.ActiveAfterReview, false}} {
		for _, row := range group.rows {
			if _, duplicate := classified[row.IssueIdentifier]; duplicate {
				return fmt.Errorf("candidate %s is classified more than once", row.IssueIdentifier)
			}
			classified[row.IssueIdentifier] = struct {
				row       decisionimpact.MatrixRow
				corrected bool
			}{row, group.corrected}
		}
	}
	if len(classified) != len(candidates) {
		return fmt.Errorf("completed review classifies %d candidates; current scan requires %d", len(classified), len(candidates))
	}
	for _, candidate := range candidates {
		classification, ok := classified[candidate.IssueIdentifier]
		if !ok {
			return fmt.Errorf("completed review omits scanned candidate %s", candidate.IssueIdentifier)
		}
		row := classification.row
		if row.Relation != candidate.Relation {
			return fmt.Errorf("candidate %s relation changed from %s to %s", candidate.IssueIdentifier, candidate.Relation, row.Relation)
		}
		if !classification.corrected {
			continue
		}
		if row.Action == decisionimpact.Continue || row.Action == decisionimpact.NoImpact {
			return fmt.Errorf("candidate %s cannot be corrected with action %s", candidate.IssueIdentifier, row.Action)
		}
		if candidate.HasActiveRuns {
			return fmt.Errorf("candidate %s still has active or deferred runs", candidate.IssueIdentifier)
		}
		switch row.Action {
		case decisionimpact.Close, decisionimpact.Supersede:
			if candidate.CurrentStatus != "done" && candidate.CurrentStatus != "cancelled" && candidate.StatusCategory != "done" && candidate.StatusCategory != "closed" {
				return fmt.Errorf("candidate %s is declared corrected but is not closed", candidate.IssueIdentifier)
			}
		case decisionimpact.Amend, decisionimpact.Pause:
			if candidate.CurrentRevision <= candidate.IssueRevision {
				return fmt.Errorf("candidate %s is declared corrected but was not amended", candidate.IssueIdentifier)
			}
		}
	}
	return nil
}

func (h *Handler) completeDecisionImpactReview(ctx context.Context, issue db.Issue, raw []byte) (db.SetIssueMetadataKeyRow, error) {
	review, err := decisionimpact.DecodeReview(raw)
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	scanID, err := util.ParseUUID(review.ScanID)
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	candidates, err := qtx.LockDecisionImpactCandidatesForCompletion(ctx, db.LockDecisionImpactCandidatesForCompletionParams{
		ScanID: scanID, WorkspaceID: issue.WorkspaceID, DecisionIssueID: issue.ID,
	})
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	if err := validateDecisionImpactCompletion(review, candidates); err != nil {
		return db.SetIssueMetadataKeyRow{}, fmt.Errorf("%w: %v", errInvalidDecisionImpactCompletion, err)
	}
	compact, err := decisionimpact.CompactCompletedReview(raw)
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	row, err := qtx.SetIssueMetadataKeyIfRevision(ctx, db.SetIssueMetadataKeyIfRevisionParams{
		ID: issue.ID, WorkspaceID: issue.WorkspaceID, Key: decisionImpactMetadataKey,
		Value: compact, ExpectedRevision: issue.Revision,
	})
	if err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.SetIssueMetadataKeyRow{}, err
	}
	return db.SetIssueMetadataKeyRow{ID: row.ID, WorkspaceID: row.WorkspaceID, Metadata: row.Metadata, Revision: row.Revision}, nil
}
