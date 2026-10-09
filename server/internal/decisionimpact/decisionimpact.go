package decisionimpact

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type Action string

const (
	Continue  Action = "continue"
	Amend     Action = "amend"
	Pause     Action = "pause"
	Close     Action = "close"
	Supersede Action = "supersede/cancel"
	NoImpact  Action = "no impact proved"
)

type MutationKind string

const CancelRun MutationKind = "cancel_run"

type Decision struct {
	Identifier string `json:"identifier"`
	ProjectID  string `json:"project_id"`
	ActorType  string `json:"actor_type"`
	ActorRole  string `json:"actor_role"`
}

type Issue struct {
	Identifier       string   `json:"identifier"`
	ProjectID        string   `json:"project_id"`
	ParentIdentifier string   `json:"parent_identifier,omitempty"`
	References       []string `json:"references,omitempty"`
	Status           string   `json:"status"`
	RequestedAction  Action   `json:"requested_action,omitempty"`
}

type ObsolescenceProof struct {
	DecisionIdentifier string `json:"decision_identifier"`
	IssueIdentifier    string `json:"issue_identifier"`
	RunID              string `json:"run_id"`
	Obsolete           bool   `json:"obsolete"`
}

type Run struct {
	ID              string             `json:"id"`
	IssueIdentifier string             `json:"issue_identifier"`
	Status          string             `json:"status"`
	TriggerText     string             `json:"trigger_text,omitempty"`
	Proof           *ObsolescenceProof `json:"proof,omitempty"`
}

type Input struct {
	Decision Decision `json:"decision"`
	Issues   []Issue  `json:"issues"`
	Runs     []Run    `json:"runs"`
}

type MatrixRow struct {
	IssueIdentifier string `json:"issue"`
	Relation        string `json:"relation"`
	Impact          string `json:"impact"`
	Action          Action `json:"action"`
	Proof           string `json:"proof"`
}

type Mutation struct {
	Kind            MutationKind `json:"kind"`
	IssueIdentifier string       `json:"issue"`
	TargetID        string       `json:"target_id"`
}

type Result struct {
	Matrix    []MatrixRow `json:"matrix"`
	Mutations []Mutation  `json:"mutations"`
}

type Report struct {
	DecisionChanged      string      `json:"decision_changed"`
	ImpactedAndCorrected []MatrixRow `json:"impacted_and_corrected"`
	PendingImpact        []MatrixRow `json:"pending_impact"`
	NotImpacted          []MatrixRow `json:"not_impacted"`
	ActiveAfterReview    []MatrixRow `json:"active_after_review"`
}

type Review struct {
	State                string      `json:"state"`
	DecisionIssueID      string      `json:"decision_issue_id"`
	AuthorizedMemberID   string      `json:"authorized_member_id"`
	DecisionChanged      string      `json:"decision_changed"`
	ImpactedAndCorrected []MatrixRow `json:"impacted_and_corrected"`
	PendingImpact        []MatrixRow `json:"pending_impact"`
	NotImpacted          []MatrixRow `json:"not_impacted"`
	ActiveAfterReview    []MatrixRow `json:"active_after_review"`
}

func ValidateReview(raw []byte, authorizedMemberID string) error {
	var review Review
	if err := json.Unmarshal(raw, &review); err != nil {
		return fmt.Errorf("decode decision impact review: %w", err)
	}
	if review.AuthorizedMemberID != authorizedMemberID {
		return errors.New("decision impact review authority does not match the authenticated owner")
	}
	if _, err := uuid.Parse(review.DecisionIssueID); err != nil {
		return errors.New("decision impact review requires a valid decision_issue_id")
	}
	switch review.State {
	case "pending", "failed":
		return nil
	case "completed":
		if review.DecisionChanged == "" {
			return errors.New("completed decision impact review requires decision_changed")
		}
		if len(review.PendingImpact) != 0 {
			return errors.New("completed decision impact review cannot contain pending impact")
		}
		return nil
	default:
		return errors.New("decision impact review state must be pending, failed, or completed")
	}
}

func Analyze(in Input) Result {
	result := Result{Matrix: make([]MatrixRow, 0, len(in.Issues))}
	for _, issue := range in.Issues {
		relation := relationToDecision(in.Decision, issue)
		action := normalizedAction(issue.RequestedAction)
		row := MatrixRow{IssueIdentifier: issue.Identifier, Relation: relation, Action: action}
		if relation == "none" {
			row.Action = NoImpact
			row.Impact = "no deterministic relation"
			row.Proof = "project, graph and references did not match"
		} else {
			row.Impact = string(row.Action)
			row.Proof = relation
		}
		result.Matrix = append(result.Matrix, row)
	}

	if in.Decision.ActorType != "member" || in.Decision.ActorRole != "owner" {
		return result
	}
	for _, run := range in.Runs {
		if !activeRun(run.Status) || !validProof(in.Decision, run) {
			continue
		}
		result.Mutations = append(result.Mutations, Mutation{Kind: CancelRun, IssueIdentifier: run.IssueIdentifier, TargetID: run.ID})
	}
	return result
}

func ConfirmApplied(result Result, confirmedTargetIDs map[string]bool) Report {
	report := Report{}
	confirmedIssues := make(map[string]bool)
	for _, mutation := range result.Mutations {
		if confirmedTargetIDs[mutation.TargetID] {
			confirmedIssues[mutation.IssueIdentifier] = true
		}
	}
	for _, row := range result.Matrix {
		switch row.Action {
		case Continue:
			report.ActiveAfterReview = append(report.ActiveAfterReview, row)
		case NoImpact:
			report.NotImpacted = append(report.NotImpacted, row)
		default:
			if confirmedIssues[row.IssueIdentifier] {
				report.ImpactedAndCorrected = append(report.ImpactedAndCorrected, row)
			} else {
				report.PendingImpact = append(report.PendingImpact, row)
			}
		}
	}
	return report
}

func relationToDecision(decision Decision, issue Issue) string {
	if issue.Identifier == decision.Identifier {
		return "decision"
	}
	if issue.ParentIdentifier == decision.Identifier {
		return "child"
	}
	for _, ref := range issue.References {
		if ref == decision.Identifier {
			return "reference"
		}
	}
	if decision.ProjectID != "" && issue.ProjectID == decision.ProjectID {
		return "project"
	}
	return "none"
}

func normalizedAction(action Action) Action {
	switch action {
	case Continue, Amend, Pause, Close, Supersede:
		return action
	default:
		return NoImpact
	}
}

func activeRun(status string) bool {
	return status == "queued" || status == "dispatched" || status == "running" || status == "waiting_local_directory" || status == "deferred"
}

func validProof(decision Decision, run Run) bool {
	proof := run.Proof
	if proof == nil || !proof.Obsolete || proof.DecisionIdentifier != decision.Identifier || proof.IssueIdentifier != run.IssueIdentifier || proof.RunID != run.ID {
		return false
	}
	_, err := uuid.Parse(run.ID)
	return err == nil
}
