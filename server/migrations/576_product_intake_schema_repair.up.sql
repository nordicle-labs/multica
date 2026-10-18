ALTER TABLE product_intake_binding
    ADD COLUMN IF NOT EXISTS squad_id UUID,
    ADD COLUMN IF NOT EXISTS required_capability TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'product_intake_binding'::regclass
          AND conname = 'product_intake_binding_squad_required'
    ) THEN
        ALTER TABLE product_intake_binding
            ADD CONSTRAINT product_intake_binding_squad_required
            CHECK (squad_id IS NOT NULL) NOT VALID;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'product_intake_binding'::regclass
          AND conname = 'product_intake_binding_capability_required'
    ) THEN
        ALTER TABLE product_intake_binding
            ADD CONSTRAINT product_intake_binding_capability_required
            CHECK (required_capability IS NOT NULL AND required_capability <> '') NOT VALID;
    END IF;
END;
$$;

CREATE TABLE IF NOT EXISTS product_intake_approval (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    binding_id UUID NOT NULL,
    message_id TEXT NOT NULL CHECK (message_id <> ''),
    approver_id UUID NOT NULL,
    artifact_sha TEXT NOT NULL CHECK (artifact_sha ~ '^[0-9a-f]{40}$'),
    decision TEXT NOT NULL CHECK (decision IN ('approved', 'denied')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS product_intake_delivery (
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

DROP TRIGGER IF EXISTS trg_product_intake_approval_immutable ON product_intake_approval;
CREATE TRIGGER trg_product_intake_approval_immutable
BEFORE UPDATE ON product_intake_approval
FOR EACH ROW EXECUTE FUNCTION reject_product_intake_immutable_update();

DROP TRIGGER IF EXISTS trg_product_intake_delivery_immutable ON product_intake_delivery;
CREATE TRIGGER trg_product_intake_delivery_immutable
BEFORE UPDATE ON product_intake_delivery
FOR EACH ROW EXECUTE FUNCTION reject_product_intake_immutable_update();
