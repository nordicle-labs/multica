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
    squad_id UUID NOT NULL,
    required_capability TEXT NOT NULL CHECK (required_capability <> ''),
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

-- A distinct human decision is immutable; runtime evidence is appended after
-- the approved intake is routed to a concrete task and SHA.
CREATE TABLE product_intake_approval (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    binding_id UUID NOT NULL,
    message_id TEXT NOT NULL CHECK (message_id <> ''),
    approver_id UUID NOT NULL,
    artifact_sha TEXT NOT NULL CHECK (artifact_sha ~ '^[0-9a-f]{40}$'),
    decision TEXT NOT NULL CHECK (decision IN ('approved', 'denied')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE product_intake_delivery (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    source_type TEXT NOT NULL,
    source_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    actor_id UUID NOT NULL,
    target_agent_id UUID NOT NULL,
    message_id TEXT NOT NULL CHECK (message_id <> ''),
    binding_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    task_id UUID NOT NULL,
    approval_id UUID,
    artifact_sha TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((approval_id IS NULL) = (artifact_sha IS NULL)),
    CHECK (artifact_sha IS NULL OR artifact_sha ~ '^[0-9a-f]{40}$')
);

CREATE OR REPLACE FUNCTION reject_product_intake_immutable_update()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER trg_product_intake_approval_immutable
BEFORE UPDATE ON product_intake_approval
FOR EACH ROW EXECUTE FUNCTION reject_product_intake_immutable_update();

CREATE TRIGGER trg_product_intake_delivery_immutable
BEFORE UPDATE ON product_intake_delivery
FOR EACH ROW EXECUTE FUNCTION reject_product_intake_immutable_update();
