package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestStartsDecisionImpactReviewOnlyWhenMarkerIsIntroduced(t *testing.T) {
	marker := "DECISION CLEMENT: replace the old vault"
	plain := "ordinary edit"
	cases := []struct {
		name string
		prev db.Issue
		req  UpdateIssueRequest
		want bool
	}{
		{name: "description introduces marker", prev: db.Issue{Title: "Vault"}, req: UpdateIssueRequest{Description: &marker}, want: true},
		{name: "accented marker", prev: db.Issue{Title: "Vault"}, req: UpdateIssueRequest{Title: strptr("DÉCISION CLÉMENT — Vault")}, want: true},
		{name: "ordinary edit", prev: db.Issue{Title: "Vault"}, req: UpdateIssueRequest{Description: &plain}},
		{name: "existing decision changed", prev: db.Issue{Title: marker, Description: pgtype.Text{String: "old", Valid: true}}, req: UpdateIssueRequest{Description: &plain}, want: true},
		{name: "unchanged decision marker", prev: db.Issue{Title: marker}, req: UpdateIssueRequest{Title: &marker}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := startsDecisionImpactReview(tc.prev, tc.req); got != tc.want {
				t.Fatalf("startsDecisionImpactReview() = %v, want %v", got, tc.want)
			}
		})
	}
}

func strptr(value string) *string { return &value }
