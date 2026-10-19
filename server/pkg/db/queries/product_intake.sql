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

-- name: GetProductIntakeDatabaseNow :one
SELECT now()::timestamptz;

-- name: GetProductIntakeBinding :one
SELECT * FROM product_intake_binding WHERE id = $1;

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
    product_key, project_id, squad_id, required_capability, risk_policy, revision, status, valid_from,
    valid_until, created_by, change_reason
) VALUES (
    @workspace_id, @source_type, @source_id, @conversation_id, @actor_id, @target_agent_id,
    @product_key, @project_id, @squad_id, @required_capability, @risk_policy, @revision, 'active', now(),
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

-- name: GetLatestProductIntakeTaskForIssue :one
SELECT * FROM agent_task_queue
WHERE issue_id = @issue_id AND agent_id = @agent_id
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetProductIntakeApproval :one
SELECT * FROM product_intake_approval
WHERE id = @id AND workspace_id = @workspace_id AND binding_id = @binding_id
  AND message_id = @message_id;

-- name: InsertProductIntakeApproval :one
INSERT INTO product_intake_approval (
    workspace_id, binding_id, message_id, approver_id, artifact_sha, decision
) VALUES (@workspace_id, @binding_id, @message_id, @approver_id, @artifact_sha, @decision)
RETURNING *;

-- name: GetCurrentProductIntakeApproval :one
SELECT * FROM product_intake_approval
WHERE workspace_id = @workspace_id AND binding_id = @binding_id
  AND message_id = @message_id AND artifact_sha = @artifact_sha
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetProductIntakeDelivery :one
SELECT * FROM product_intake_delivery
WHERE workspace_id = @workspace_id AND source_type = @source_type
  AND source_id = @source_id AND conversation_id = @conversation_id
  AND actor_id = @actor_id AND target_agent_id = @target_agent_id
  AND message_id = @message_id;

-- name: InsertProductIntakeDelivery :one
INSERT INTO product_intake_delivery (
    id, workspace_id, source_type, source_id, conversation_id, actor_id,
    target_agent_id, message_id, binding_id, issue_id, task_id, approval_id, artifact_sha
) VALUES (
    @id, @workspace_id, @source_type, @source_id, @conversation_id, @actor_id,
    @target_agent_id, @message_id, @binding_id, @issue_id, @task_id,
    sqlc.narg('approval_id'), sqlc.narg('artifact_sha')
)
RETURNING *;
