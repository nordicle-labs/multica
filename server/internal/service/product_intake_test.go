package service

import (
	"errors"
	"testing"
)

func TestClassifyProductIntakeAndRiskMatrix(t *testing.T) {
	tests := []struct {
		text, kind, title string
		ok                bool
	}{
		{"idea: tighten copy", "idea", "tighten copy", true},
		{"BUG: prevent duplicate dispatch", "bug", "prevent duplicate dispatch", true},
		{"feature: route Tony", "feature", "route Tony", true},
		{"incident: leaked permission", "incident", "leaked permission", true},
		{"research: compare adapters", "research", "compare adapters", true},
		{"project: migrate intake", "project", "migrate intake", true},
		{"release: activate candidate", "release", "activate candidate", true},
		{"feature:", "", "", false},
		{"please do this", "", "", false},
		{"unknown: no fallback", "", "", false},
	}
	for _, tc := range tests {
		kind, title, ok := ClassifyProductIntake(tc.text)
		if kind != tc.kind || title != tc.title || ok != tc.ok {
			t.Errorf("ClassifyProductIntake(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.text, kind, title, ok, tc.kind, tc.title, tc.ok)
		}
	}

	if !policyAllows("tests_review", "feature") || policyAllows("targeted_check", "feature") {
		t.Fatal("feature must require tests + independent review")
	}
	if !policyAllows("security_integration", "incident") || policyAllows("tests_review", "incident") {
		t.Fatal("incident must require security + integration")
	}
	if !policyAllows("explicit_approval_rollback", "release") || policyAllows("security_integration", "release") {
		t.Fatal("release must require explicit approval + rollback")
	}
	if !IsProductIntakeRefusal(ErrIntakeBindingStale) || IsProductIntakeRefusal(errors.New("database unavailable")) {
		t.Fatal("product refusals must be distinguishable from retryable infrastructure errors")
	}
}
