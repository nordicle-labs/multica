-- Normalized scan state keeps exhaustive decision-impact matrices out of the
-- issue.metadata 8 KiB envelope. Relationships are validated by the handler;
-- no foreign keys are used so workspace deletion remains application-owned.
CREATE TABLE decision_impact_candidate (
    scan_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    decision_issue_id UUID NOT NULL,
    candidate_issue_id UUID NOT NULL,
    issue_identifier TEXT NOT NULL,
    relation TEXT NOT NULL,
    expected_action TEXT NOT NULL,
    issue_revision BIGINT NOT NULL,
    issue_status TEXT NOT NULL,
    active_run_ids UUID[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);