package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestScheduledTask_LegacyRow_ArmedByReadsZeroValue verifies that a row
// written directly against the scheduled_tasks table without setting the
// armed_by_id/armed_by_kind columns (relying on migration 000006's
// DEFAULT ” backfill) reads back through Get as the zero Principal — never
// a synthesized one. The SPI's own ScheduledTaskStoreConformance fixture
// cannot exercise this exact path (it only has the generic StoreFactory
// surface, so its "legacy" case is the ScheduledTask struct's own zero
// value for ArmedBy, not an actual pre-migration row); this plugin-local
// test covers the storage layer directly.
func TestScheduledTask_LegacyRow_ArmedByReadsZeroValue(t *testing.T) {
	url := os.Getenv("CYODA_TEST_DB_URL")
	if url == "" {
		t.Skip("CYODA_TEST_DB_URL not set — skipping PostgreSQL test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer pool.Close()

	if err := dropSchema(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = dropSchema(pool) })

	// Insert a row omitting armed_by_id/armed_by_kind entirely — the exact
	// shape of a row written before migration 000006 existed.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO scheduled_tasks
		 (id,tenant_id,type,scheduled_time,entity_id,model_name,model_version,transition,source_state,armed_at,attempt_count)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		"legacy:S:T", "tenant-A", string(spi.ScheduledTaskFireTransition), int64(1000),
		"legacy", "Order", 1, "T", "S", int64(0), 0); err != nil {
		t.Fatalf("insert legacy scheduled_tasks row: %v", err)
	}

	factory := NewStoreFactory(pool)
	sts, err := factory.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}
	got, found, err := sts.Get(context.Background(), "legacy:S:T")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !found {
		t.Fatal("expected the legacy row to be found")
	}
	if got.ArmedBy != (spi.Principal{}) {
		t.Errorf("legacy row ArmedBy = %+v, want zero Principal", got.ArmedBy)
	}
}
