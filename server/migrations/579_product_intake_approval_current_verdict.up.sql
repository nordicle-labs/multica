CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_product_intake_approval_current_verdict ON product_intake_approval(workspace_id, binding_id, message_id, artifact_sha, created_at DESC, id DESC);
