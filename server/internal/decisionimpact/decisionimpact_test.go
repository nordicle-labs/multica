package decisionimpact

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAnalyzeHistoricalDecisionScenario(t *testing.T) {
	raw, err := os.ReadFile("testdata/herm_935.json")
	if err != nil {
		t.Fatal(err)
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}

	result := Analyze(in)
	assertAction(t, result, "HERM-888", Continue)
	assertAction(t, result, "HERM-885", Close)
	assertAction(t, result, "HERM-934", Continue)
	if len(result.Mutations) != 1 || result.Mutations[0].TargetID != "0199ee0c-7518-7000-8000-000000000927" {
		t.Fatalf("mutations = %#v, want exact obsolete HERM-927 run", result.Mutations)
	}
	if result.Mutations[0].Kind != CancelRun {
		t.Fatalf("mutation kind = %q, want %q", result.Mutations[0].Kind, CancelRun)
	}
}

func TestAnalyzeFailsClosedWithoutStructuredProof(t *testing.T) {
	base := Input{
		Decision: Decision{Identifier: "HERM-931", ProjectID: "brain", ActorType: "member", ActorRole: "owner"},
		Issues:   []Issue{{Identifier: "HERM-885", ProjectID: "brain", RequestedAction: Supersede}},
		Runs:     []Run{{ID: "0199ee0c-7518-7000-8000-000000000927", IssueIdentifier: "HERM-885", Status: "running", TriggerText: "not obsolete: HERM-931 replaces HERM-885? no; keep this run active"}},
	}
	if got := Analyze(base); len(got.Mutations) != 0 {
		t.Fatalf("free text authorized mutation: %#v", got.Mutations)
	}
	base.Decision.ActorType = "agent"
	base.Runs[0].Proof = &ObsolescenceProof{DecisionIdentifier: "HERM-931", IssueIdentifier: "HERM-885", RunID: base.Runs[0].ID, Obsolete: true}
	if got := Analyze(base); len(got.Mutations) != 0 {
		t.Fatalf("agent provenance authorized mutation: %#v", got.Mutations)
	}
}

func TestConfirmAppliedReportsOnlyRevalidatedMutations(t *testing.T) {
	analysis := Result{
		Matrix: []MatrixRow{
			{IssueIdentifier: "HERM-885", Action: Close},
			{IssueIdentifier: "HERM-934", Action: Continue},
		},
		Mutations: []Mutation{{Kind: CancelRun, IssueIdentifier: "HERM-885", TargetID: "0199ee0c-7518-7000-8000-000000000927"}},
	}
	report := ConfirmApplied(analysis, map[string]bool{})
	if len(report.ImpactedAndCorrected) != 0 || len(report.PendingImpact) != 1 {
		t.Fatalf("unapplied mutation reported as corrected: %#v", report)
	}
	report = ConfirmApplied(analysis, map[string]bool{"0199ee0c-7518-7000-8000-000000000927": true})
	if len(report.ImpactedAndCorrected) != 1 || len(report.PendingImpact) != 0 {
		t.Fatalf("confirmed mutation report = %#v", report)
	}
}

func TestValidateReviewRequiresOwnerIdentityAndNoPendingCompletion(t *testing.T) {
	const owner = "0199ee0c-7518-7000-8000-000000000001"
	valid := []byte(`{"state":"completed","decision_issue_id":"0199ee0c-7518-7000-8000-000000000931","authorized_member_id":"` + owner + `","decision_changed":"new vault","pending_impact":[]}`)
	if err := ValidateReview(valid, owner); err != nil {
		t.Fatalf("valid completed review: %v", err)
	}
	if err := ValidateReview(valid, "0199ee0c-7518-7000-8000-000000000002"); err == nil {
		t.Fatal("mismatched owner identity was accepted")
	}
	pending := []byte(`{"state":"completed","decision_issue_id":"0199ee0c-7518-7000-8000-000000000931","authorized_member_id":"` + owner + `","decision_changed":"new vault","pending_impact":[{"issue":"HERM-885"}]}`)
	if err := ValidateReview(pending, owner); err == nil {
		t.Fatal("completed review with pending impact was accepted")
	}
}

func assertAction(t *testing.T, result Result, identifier string, want Action) {
	t.Helper()
	for _, row := range result.Matrix {
		if row.IssueIdentifier == identifier {
			if row.Action != want {
				t.Fatalf("%s action = %q, want %q", identifier, row.Action, want)
			}
			return
		}
	}
	t.Fatalf("missing matrix row for %s", identifier)
}
