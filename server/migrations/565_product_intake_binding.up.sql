-- Versioned source/channel -> product -> project -> risk-policy bindings.
-- Relationships are soft by repository policy and are revalidated by the service.
CREATE TABLE product_intake_binding (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    source_type TEXT NOT NULL CHECK (source_type IN ('multica_chat', 'telegram')),
    source_id TEXT NOT NULL CHECK (source_id <> ''),
    conversation_id TEXT NOT NULL CHECK (conversation_id <> ''),
    actor_id UUID NOT NULL,
    target_agent_id UUID NOT NULL,
    product_key TEXT NOT NULL CHECK (product_key <> ''),
    project_id UUID NOT NULL,
    risk_policy TEXT NOT NULL CHECK (risk_policy IN (
        'targeted_check',
        'tests_review',
        'security_integration',
        'explicit_approval_rollback'
    )),
    revision BIGINT NOT NULL CHECK (revision > 0),
    status TEXT NOT NULL CHECK (status IN ('active', 'revoked')),
    valid_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until TIMESTAMPTZ NOT NULL,
    created_by UUID NOT NULL,
    change_reason TEXT NOT NULL CHECK (change_reason <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_by UUID,
    revoked_at TIMESTAMPTZ,
    CHECK (valid_until > valid_from),
    CHECK ((status = 'active' AND revoked_at IS NULL AND revoked_by IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL AND revoked_by IS NOT NULL))
);
