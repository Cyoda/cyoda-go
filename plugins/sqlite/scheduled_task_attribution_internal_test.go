package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestScheduledTask_LegacyRow_ArmedByReadsZeroValue verifies that a row
// written directly against the scheduled_tasks table without setting the
// armed_by_id/armed_by_kind columns (relying on the migration's DEFAULT ”)
// reads back through Get as the zero Principal — never a synthesized one.
// The SPI's own ScheduledTaskStoreConformance fixture cannot exercise this
// exact path (it only has the generic StoreFactory surface, so its "legacy"
// case is the ScheduledTask struct's own zero value for ArmedBy, not an
// actual pre-migration row); this plugin-local test covers the storage
// layer directly.
func TestScheduledTask_LegacyRow_ArmedByReadsZeroValue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-armed-by.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()

	// Insert a row omitting armed_by_id/armed_by_kind entirely — the exact
	// shape of a row written before this migration existed.
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO scheduled_tasks
		 (id,tenant_id,type,scheduled_time,entity_id,model_name,model_version,transition,source_state,armed_at,attempt_count)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"legacy:S:T", "tenant-A", string(spi.ScheduledTaskFireTransition), int64(1000),
		"legacy", "Order", "1", "T", "S", int64(0), 0); err != nil {
		t.Fatalf("insert legacy scheduled_tasks row: %v", err)
	}

	sts, err := f.ScheduledTaskStore(context.Background())
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
