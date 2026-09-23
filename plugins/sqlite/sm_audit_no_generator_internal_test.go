package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestSMAuditStore_Record_NoGenerator pins the fail-closed guard in
// smAuditStore.Record: a factory built without initTransactionManager has no
// spi.UUIDGenerator wired in, and Record must error rather than fall back to
// a caller-supplied or improvised id — and nothing is written (see
// spi.StateMachineAuditStore). newStoreFactory alone (not
// NewStoreFactoryForTest, which also calls initTransactionManager) is the
// cheap way to reach that state.
func TestSMAuditStore_Record_NoGenerator(t *testing.T) {
	cfg := config{
		Path:                   filepath.Join(t.TempDir(), "audit-no-gen.db"),
		AutoMigrate:            true,
		BusyTimeout:            5 * time.Second,
		CacheSizeKiB:           64000,
		ReaderPoolSize:         defaultReaderPoolSize(),
		SchemaExtendMaxRetries: 8,
	}
	f, err := newStoreFactory(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newStoreFactory: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: spi.TenantID("sm-tenant-no-gen")},
	})
	store, err := f.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}

	if err := store.Record(ctx, "entity-1", spi.StateMachineEvent{
		EventType: spi.SMEventStarted, EntityID: "entity-1", State: "NEW",
	}); err == nil {
		t.Fatal("Record with no generator configured: want an error, got nil")
	}

	events, err := store.GetEvents(ctx, "entity-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Record errored but %d events were recorded; want 0", len(events))
	}
}
