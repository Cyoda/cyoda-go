package postgres_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// Migration 18 drops model_schema_extensions.tx_id: an extension never
// joins the caller's transaction, so there is no transaction to record.
// Down restores the column NOT NULL, with the empty string for existing rows.
func TestMigration18_DropsSchemaExtensionTxID(t *testing.T) {
	pool := newCTPool(t, freshCTDatabase(t), 4, nil)
	if err := postgres.MigrateToVersionForTest(pool, 17); err != nil {
		t.Fatalf("migrate to 17: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO model_schema_extensions (tenant_id, model_name, model_version, kind, payload, tx_id)
		VALUES ('seed', 'm', '1', 'delta', '"d0"', 'tx-1')`); err != nil {
		t.Fatalf("seed an extension row: %v", err)
	}

	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if exists, _ := columnDefault(t, pool, "model_schema_extensions", "tx_id"); exists {
		t.Error("model_schema_extensions.tx_id still exists")
	}

	if err := postgres.MigrateToVersionForTest(pool, 17); err != nil {
		t.Fatalf("migrate down to 17: %v", err)
	}
	if exists, def := columnDefault(t, pool, "model_schema_extensions", "tx_id"); !exists || def != "''::text" {
		t.Errorf("model_schema_extensions.tx_id after down: exists %v, default %q; want ''::text", exists, def)
	}
	if columnNullable(t, pool, "model_schema_extensions", "tx_id") {
		t.Error("model_schema_extensions.tx_id after down admits NULL")
	}
}
