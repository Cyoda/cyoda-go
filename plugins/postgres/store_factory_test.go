package postgres_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// jsonEqual reports whether two json.RawMessage values are semantically equal
// (key ordering is irrelevant). Both inputs are unmarshalled into interface{}
// and compared via reflect.DeepEqual.
func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func ctxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: tid, Name: string(tid)},
		Roles:    []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func TestPostgresStoreFactory_SkeletonReturnsErrors(t *testing.T) {
	pool := newTestPool(t)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	factory := postgres.NewStoreFactory(pool)
	ctx := ctxWithTenant("test-tenant")

	// EntityStore is implemented — expect success
	entityStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Errorf("unexpected error from EntityStore: %v", err)
	}
	if entityStore == nil {
		t.Error("expected non-nil EntityStore")
	}

	// ModelStore is implemented — expect success
	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Errorf("unexpected error from ModelStore: %v", err)
	}
	if modelStore == nil {
		t.Error("expected non-nil ModelStore")
	}

	// KeyValueStore is implemented — expect success
	kvStore, err := factory.KeyValueStore(ctx)
	if err != nil {
		t.Errorf("unexpected error from KeyValueStore: %v", err)
	}
	if kvStore == nil {
		t.Error("expected non-nil KeyValueStore")
	}

	// MessageStore is implemented — expect success
	msgStore, err := factory.MessageStore(ctx)
	if err != nil {
		t.Errorf("unexpected error from MessageStore: %v", err)
	}
	if msgStore == nil {
		t.Error("expected non-nil MessageStore")
	}

	// WorkflowStore is implemented — expect success
	wfStore, err := factory.WorkflowStore(ctx)
	if err != nil {
		t.Errorf("unexpected error from WorkflowStore: %v", err)
	}
	if wfStore == nil {
		t.Error("expected non-nil WorkflowStore")
	}

	// StateMachineAuditStore is implemented — expect success
	smAuditStore, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Errorf("unexpected error from StateMachineAuditStore: %v", err)
	}
	if smAuditStore == nil {
		t.Error("expected non-nil StateMachineAuditStore")
	}
}

func TestPostgresStoreFactory_Close(t *testing.T) {
	dbURL := skipIfNoPostgres(t)
	cfg := postgres.DBConfig{URL: dbURL, MaxConns: 2, MinConns: 0, MaxConnIdleTime: "1m"}
	pool, err := postgres.NewPool(context.Background(), cfg)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}

	factory := postgres.NewStoreFactory(pool)
	if err := factory.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// TestPostgresStoreFactory_AsyncSearchStore_NoTenantContext verifies that
// AsyncSearchStore can be obtained without a UserContext in the context.
// This is required because app.go calls AsyncSearchStore(context.Background())
// at startup — before any request arrives.
// This test does NOT need a real postgres — it only tests the factory method.
func TestPostgresStoreFactory_AsyncSearchStore_NoTenantContext(t *testing.T) {
	// Create factory with nil pool — we only test that the factory method
	// does not fail on context.Background(), not that the store works.
	factory := postgres.NewStoreFactory(nil)

	// Must NOT error with context.Background() (no tenant)
	store, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore with no tenant context should not error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil AsyncSearchStore")
	}
}

func TestPostgresStoreFactory_NoTenantReturnsError(t *testing.T) {
	pool := newTestPool(t)
	factory := postgres.NewStoreFactory(pool)

	ctx := context.Background()
	_, err := factory.EntityStore(ctx)
	if err == nil {
		t.Error("expected error when no tenant in context")
	}
}
