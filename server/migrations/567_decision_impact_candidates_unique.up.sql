CREATE UNIQUE INDEX CONCURRENTLY decision_impact_candidate_scan_issue_uidx
    ON decision_impact_candidate (scan_id, candidate_issue_id);