-- name: GetLatestProductIntakeBinding :one
SELECT *
FROM product_intake_binding
WHERE workspace_id = @workspace_id
  AND source_type = @source_type
  AND source_id = @source_id
  AND conversation_id = @conversation_id
  AND actor_id = @actor_id
  AND target_agent_id = @target_agent_id
ORDER BY revision DESC
LIMIT 1;

-- name: LockProductIntakeBindingSource :exec
SELECT pg_advisory_xact_lock(hashtextextended(
    concat_ws(E'\x1f', @workspace_id::text, @source_type::text, @source_id::text,
        @conversation_id::text, @actor_id::text, @target_agent_id::text),
    0
));

-- name: RevokeActiveProductIntakeBinding :exec
UPDATE product_intake_binding
SET status = 'revoked', revoked_by = @revoked_by, revoked_at = now()
WHERE workspace_id = @workspace_id
  AND source_type = @source_type
  AND source_id = @source_id
  AND conversation_id = @conversation_id
  AND actor_id = @actor_id
  AND target_agent_id = @target_agent_id
  AND status = 'active';

-- name: NextProductIntakeBindingRevision :one
SELECT COALESCE(MAX(revision), 0::bigint) + 1
FROM product_intake_binding
WHERE workspace_id = @workspace_id
  AND source_type = @source_type
  AND source_id = @source_id
  AND conversation_id = @conversation_id
  AND actor_id = @actor_id
  AND target_agent_id = @target_agent_id;

-- name: InsertProductIntakeBindingRevision :one
INSERT INTO product_intake_binding (
    workspace_id, source_type, source_id, conversation_id, actor_id, target_agent_id,
    product_key, project_id, risk_policy, revision, status, valid_from,
    valid_until, created_by, change_reason
) VALUES (
    @workspace_id, @source_type, @source_id, @conversation_id, @actor_id, @target_agent_id,
    @product_key, @project_id, @risk_policy, @revision, 'active', now(),
    @valid_until, @created_by, @change_reason
)
RETURNING *;

-- name: GetLatestActiveProductIntakeTask :one
SELECT *
FROM agent_task_queue
WHERE issue_id = @issue_id
  AND agent_id = @agent_id
  AND status IN ('queued', 'deferred', 'dispatched', 'running', 'waiting_local_directory')
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetProductIntakeTask :one
SELECT * FROM agent_task_queue WHERE id = $1;
