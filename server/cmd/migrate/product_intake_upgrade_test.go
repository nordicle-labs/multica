package main

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestProductIntakeSchemaFreshAndRejectedArtifactUpgrade(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name    string
		fixture string
		files   []string
	}{
		{
			name: "fresh",
			files: []string{
				"565_product_intake_binding",
				"572_product_intake_approval_id",
				"576_product_intake_schema_repair",
				"577_product_intake_approval_id_repair",
			},
		},
		{
			name: "upgrade_from_rejected_artifact",
			fixture: `
				CREATE TABLE schema_migrations (
					version TEXT PRIMARY KEY,
					applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
				);
				INSERT INTO schema_migrations (version) VALUES ('565_product_intake_binding');
				CREATE TABLE product_intake_binding (
					id UUID NOT NULL DEFAULT gen_random_uuid(),
					workspace_id UUID NOT NULL,
					source_type TEXT NOT NULL,
					source_id TEXT NOT NULL,
					conversation_id TEXT NOT NULL,
					actor_id UUID NOT NULL,
					target_agent_id UUID NOT NULL,
					product_key TEXT NOT NULL,
					project_id UUID NOT NULL,
					risk_policy TEXT NOT NULL,
					revision BIGINT NOT NULL,
					status TEXT NOT NULL,
					valid_from TIMESTAMPTZ NOT NULL DEFAULT now(),
					valid_until TIMESTAMPTZ NOT NULL,
					created_by UUID NOT NULL,
					change_reason TEXT NOT NULL,
					created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
					revoked_by UUID,
					revoked_at TIMESTAMPTZ
				);
				INSERT INTO product_intake_binding (
					workspace_id, source_type, source_id, conversation_id, actor_id,
					target_agent_id, product_key, project_id, risk_policy, revision,
					status, valid_until, created_by, change_reason
				) VALUES (
					gen_random_uuid(), 'multica_chat', 'legacy', 'legacy', gen_random_uuid(),
					gen_random_uuid(), 'legacy', gen_random_uuid(), 'tests_review', 1,
					'active', now() + interval '1 day', gen_random_uuid(), 'legacy fixture'
				);`,
			files: []string{
				"572_product_intake_approval_id",
				"576_product_intake_schema_repair",
				"577_product_intake_approval_id_repair",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := createScratchSchema(t, ctx, base, "product_intake_")
			pool := openTestPoolWithSearchPath(t, schema)
			if tc.fixture != "" {
				if _, err := pool.Exec(ctx, tc.fixture); err != nil {
					t.Fatal(err)
				}
			}
			if err := runMigrations(ctx, pool, runOptions{
				Direction:             "up",
				Files:                 realMigrationFiles(t, tc.files, "up"),
				SchemaMigrationsTable: schema + ".schema_migrations",
				AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
				Hooks:                 hooksForDirection("up"),
				Conditions:            conditionsForDirection("up"),
			}); err != nil {
				t.Fatal(err)
			}

			var columns, tables, indexes, versions int
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM information_schema.columns
				WHERE table_schema=$1 AND table_name='product_intake_binding'
				  AND column_name IN ('squad_id', 'required_capability')
			`, schema).Scan(&columns); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM information_schema.tables
				WHERE table_schema=$1 AND table_name IN ('product_intake_approval', 'product_intake_delivery')
			`, schema).Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM pg_index i
				JOIN pg_class c ON c.oid=i.indexrelid
				JOIN pg_namespace n ON n.oid=c.relnamespace
				WHERE n.nspname=$1 AND c.relname='idx_product_intake_approval_id' AND i.indisvalid
			`, schema).Scan(&indexes); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ANY($1::text[])`, tc.files).Scan(&versions); err != nil {
				t.Fatal(err)
			}
			if columns != 2 || tables != 2 || indexes != 1 || versions != len(tc.files) {
				t.Fatalf("schema repair: columns=%d tables=%d indexes=%d versions=%d/%d", columns, tables, indexes, versions, len(tc.files))
			}
		})
	}
}

func TestProductIntakeVerdictHistoryRollbackRefusesDuplicateHistory(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "product_intake_rollback_")
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE product_intake_approval (
			id UUID NOT NULL DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL,
			binding_id UUID NOT NULL,
			message_id TEXT NOT NULL,
			approver_id UUID NOT NULL,
			artifact_sha TEXT NOT NULL,
			decision TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE UNIQUE INDEX idx_product_intake_approval_message
			ON product_intake_approval(workspace_id, binding_id, message_id);
	`); err != nil {
		t.Fatal(err)
	}
	versions := []string{
		"578_product_intake_approval_verdict_history",
		"579_product_intake_approval_current_verdict",
	}
	opts := runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, versions, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
	}
	if err := runMigrations(ctx, pool, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO product_intake_approval
			(workspace_id, binding_id, message_id, approver_id, artifact_sha, decision)
		VALUES
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'promotion', gen_random_uuid(), '0123456789abcdef0123456789abcdef01234567', 'approved'),
			('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'promotion', gen_random_uuid(), '0123456789abcdef0123456789abcdef01234567', 'denied')
	`); err != nil {
		t.Fatal(err)
	}

	opts.Direction = "down"
	opts.Files = realMigrationFiles(t, []string{versions[1], versions[0]}, "down")
	opts.Hooks = hooksForDirection("down")
	err := runMigrations(ctx, pool, opts)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back product intake verdict history") {
		t.Fatalf("rollback error = %v, want explicit verdict-history refusal", err)
	}
	var currentApplied, historyApplied bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, versions[1]).Scan(&currentApplied); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, versions[0]).Scan(&historyApplied); err != nil {
		t.Fatal(err)
	}
	if currentApplied || !historyApplied {
		t.Fatalf("migration state after refusal = current:%v history:%v, want false/true", currentApplied, historyApplied)
	}
	var currentIndexExists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('idx_product_intake_approval_current_verdict') IS NOT NULL`).Scan(&currentIndexExists); err != nil {
		t.Fatal(err)
	}
	if currentIndexExists {
		t.Fatal("current-verdict index remains after its successful rollback")
	}
}
