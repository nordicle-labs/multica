-- name: DeleteDecisionImpactCandidatesForIssue :exec
DELETE FROM decision_impact_candidate
WHERE workspace_id = @workspace_id AND decision_issue_id = @decision_issue_id;

-- name: CreateDecisionImpactCandidate :exec
INSERT INTO decision_impact_candidate (
    scan_id, workspace_id, decision_issue_id, candidate_issue_id,
    issue_identifier, relation, expected_action, issue_revision, issue_status,
    active_run_ids
) VALUES (
    @scan_id, @workspace_id, @decision_issue_id, @candidate_issue_id,
    @issue_identifier, @relation, @expected_action, @issue_revision,
    @issue_status, @active_run_ids
);

-- name: LockDecisionImpactCandidatesForCompletion :many
SELECT c.scan_id, c.candidate_issue_id, c.issue_identifier, c.relation,
       c.expected_action, c.issue_revision, c.issue_status,
       i.revision AS current_revision, i.status AS current_status,
       EXISTS (
           SELECT 1 FROM agent_task_queue t
           WHERE t.issue_id = i.id
             AND t.status IN ('queued', 'deferred', 'dispatched', 'running', 'waiting_local_directory')
       ) AS has_active_runs,
       COALESCE(s.category, '')::text AS status_category
FROM decision_impact_candidate c
JOIN issue i ON i.id = c.candidate_issue_id AND i.workspace_id = c.workspace_id
LEFT JOIN issue_status s ON s.workspace_id = i.workspace_id AND s.key = i.status
WHERE c.scan_id = @scan_id
  AND c.workspace_id = @workspace_id
  AND c.decision_issue_id = @decision_issue_id
ORDER BY c.issue_identifier
FOR UPDATE OF i;