package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func attrInternalCtx(tenant, userID string, kind spi.PrincipalKind) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: userID,
		Kind:   kind,
		Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

// TestTombstone_HasMetaBlobWithAttribution verifies that a delete written
// through EntityStore.Delete (non-tx) produces an entity_versions row whose
// meta BLOB is non-NULL and, once unmarshaled, carries the attributed
// PrincipalKind and the full Executor Principal — the fix for the previous
// meta=NULL tombstone insert.
func TestTombstone_HasMetaBlobWithAttribution(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tombstone-meta.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()

	ctx := attrInternalCtx("tenant-A", "alice", spi.PrincipalUser)
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-meta-1", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"}},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if err := store.Delete(ctx, "e-meta-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	var metaJSON sql.NullString
	var userID string
	err = f.db.QueryRowContext(context.Background(),
		`SELECT json(meta), user_id FROM entity_versions
		 WHERE tenant_id = ? AND entity_id = ? AND change_type = 'DELETED'`,
		"tenant-A", "e-meta-1").Scan(&metaJSON, &userID)
	if err != nil {
		t.Fatalf("query tombstone row: %v", err)
	}
	if !metaJSON.Valid || metaJSON.String == "" {
		t.Fatal("expected the tombstone's meta BLOB to be non-NULL, carrying attribution")
	}
	if userID != "alice" {
		t.Errorf("user_id column = %q, want %q", userID, "alice")
	}

	parsed, err := unmarshalEntityMeta([]byte(metaJSON.String))
	if err != nil {
		t.Fatalf("unmarshalEntityMeta: %v", err)
	}
	if parsed.ChangeUserKind != spi.PrincipalUser {
		t.Errorf("ChangeUserKind = %v, want %v", parsed.ChangeUserKind, spi.PrincipalUser)
	}
	want := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	if parsed.ChangeExecutor != want {
		t.Errorf("ChangeExecutor = %+v, want %+v", parsed.ChangeExecutor, want)
	}
}

// TestLegacyNullMetaTombstone_ReadsBackZeroValues verifies backward
// compatibility: a tombstone row written before this change (meta = NULL,
// per the old hardcoded insert) must still read back via GetVersionHistory
// with a zero Executor and empty AttributedKind — never an error.
func TestLegacyNullMetaTombstone_ReadsBackZeroValues(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-tombstone.db")
	f, err := NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	defer f.Close()

	ctx := attrInternalCtx("tenant-A", "alice", spi.PrincipalUser)
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-legacy-1", TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"}},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Simulate a legacy tombstone: meta = NULL, user_id = '' — exactly what
	// the pre-fix hardcoded INSERT produced.
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO entity_versions
		 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
		 VALUES (?, ?, ?, ?, ?, NULL, NULL, 'DELETED', '', ?, '')`,
		"tenant-A", "e-legacy-1", "Order", "1", 2, 1000); err != nil {
		t.Fatalf("insert legacy tombstone row: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-legacy-1")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatalf("expected last version to be the legacy DELETE tombstone")
	}
	if tomb.AttributedKind != "" {
		t.Errorf("legacy tombstone AttributedKind = %q, want empty (zero value)", tomb.AttributedKind)
	}
	if tomb.Executor != (spi.Principal{}) {
		t.Errorf("legacy tombstone Executor = %+v, want zero value", tomb.Executor)
	}
}
