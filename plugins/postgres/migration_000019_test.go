package postgres_test

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// Migration 19 adds model_lifecycle: one row per model, filled from models
// for every model that exists when it runs, with row-level security and the
// tenant policy of models. Down drops it.
func TestMigration_ModelLifecycleFilledFromModels(t *testing.T) {
	ctx := context.Background()
	pool := newCTPool(t, freshCTDatabase(t), 4, nil)
	if err := postgres.MigrateToVersionForTest(pool, 18); err != nil {
		t.Fatalf("migrate to 18: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO models (tenant_id, model_name, model_version, doc) VALUES
			('t1', 'm', '1', '{"state":"LOCKED"}'),
			('t1', 'm', '2', '{"state":"UNLOCKED"}'),
			('t2', 'm', '1', '{"state":"LOCKED"}')`); err != nil {
		t.Fatalf("seed models: %v", err)
	}

	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT tenant_id, model_name, model_version, generation
		FROM model_lifecycle ORDER BY tenant_id, model_name, model_version`)
	if err != nil {
		t.Fatalf("read model_lifecycle: %v", err)
	}
	type row struct {
		tenant, name, version string
		generation            int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenant, &r.name, &r.version, &r.generation); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []row{{"t1", "m", "1", 0}, {"t1", "m", "2", 0}, {"t2", "m", "1", 0}}
	if len(got) != len(want) {
		t.Fatalf("model_lifecycle rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model_lifecycle row %d = %v, want %v", i, got[i], want[i])
		}
	}

	var rlsEnabled, rlsForced bool
	if err := pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'model_lifecycle'`).
		Scan(&rlsEnabled, &rlsForced); err != nil {
		t.Fatalf("read RLS flags: %v", err)
	}
	if !rlsEnabled || rlsForced {
		t.Errorf("model_lifecycle RLS enabled %v, forced %v; want enabled, not forced (as models)", rlsEnabled, rlsForced)
	}
	var modelsQual, lifecycleQual string
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT qual FROM pg_policies WHERE tablename = 'models'),
		        (SELECT qual FROM pg_policies WHERE tablename = 'model_lifecycle')`).
		Scan(&modelsQual, &lifecycleQual); err != nil {
		t.Fatalf("read policies: %v", err)
	}
	if lifecycleQual != modelsQual {
		t.Errorf("model_lifecycle policy %q, want the models policy %q", lifecycleQual, modelsQual)
	}

	if err := postgres.MigrateToVersionForTest(pool, 18); err != nil {
		t.Fatalf("migrate down to 18: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'model_lifecycle')`).
		Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Error("model_lifecycle still exists after down")
	}
}
