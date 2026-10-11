CREATE INDEX CONCURRENTLY decision_impact_candidate_decision_idx
    ON decision_impact_candidate (workspace_id, decision_issue_id);