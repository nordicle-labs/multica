CREATE UNIQUE INDEX CONCURRENTLY idx_product_intake_binding_active
    ON product_intake_binding(
        workspace_id, source_type, source_id, conversation_id, actor_id, target_agent_id
    )
    WHERE status = 'active';
