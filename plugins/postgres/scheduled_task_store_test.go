package postgres_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func TestPostgres_ScheduledTaskStoreConformance(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	spi.RunScheduledTaskStoreConformance(t, func() spi.StoreFactory {
		return postgres.NewStoreFactory(pool)
	})
}

// TestPostgres_ScheduledTaskArm_RollbackIsAtomic proves that a ScheduledTask
// Upsert issued through the context-resolving querier (f.querier()) inside
// an entity transaction is genuinely part of that pgx.Tx: rolling the
// transaction back must leave no trace of the arm. Unlike memory/sqlite,
// postgres needs no bespoke staging/flush machinery for this — the store
// writes straight through the same *pgx.Tx the entity write uses, so
// ROLLBACK covers it for free.
func TestPostgres_ScheduledTaskArm_RollbackIsAtomic(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	uuids := newTestUUIDGenerator()
	tm := postgres.NewTransactionManager(pool, uuids)
	factory := postgres.NewStoreFactoryWithTMForTest(pool, tm)

	ctx := ctxWithTenant("tenant-A")
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	sts, err := factory.ScheduledTaskStore(txCtx)
	if err != nil {
		t.Fatalf("ScheduledTaskStore: %v", err)
	}

	task := spi.ScheduledTask{
		ID:            "e1:S:T",
		TenantID:      "tenant-A",
		Type:          spi.ScheduledTaskFireTransition,
		ScheduledTime: 1000,
		EntityID:      "e1",
		SourceState:   "S",
		Transition:    "T",
	}
	if err := sts.Upsert(txCtx, task); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Read outside any transaction — the arm must never have committed.
	if _, found, err := sts.Get(context.Background(), "e1:S:T"); err != nil {
		t.Fatalf("Get: %v", err)
	} else if found {
		t.Fatal("expected scheduled task to be absent after rollback (Upsert must have joined the entity tx, not auto-committed)")
	}
}

// TestPostgres_ScheduledTaskStore_ScanDueIsCrossTenant is finding F7: ScanDue
// is a trusted system read used by the scheduler's due-task scan loop,
// called outside any per-tenant request (no app.current_tenant GUC set, no
// tenant in ctx). scheduled_tasks is deliberately NOT enrolled in row-level
// security (see migration 000004) so this read is never accidentally
// scoped to a single tenant. This test proves ScanDue returns rows armed
// under at least two distinct tenants in one call.
func TestPostgres_ScheduledTaskStore_ScanDueIsCrossTenant(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	factory := postgres.NewStoreFactory(pool)

	// Arm one task per tenant, each through its own tenant-scoped request
	// context (mirroring how the engine arms tasks during a transition).
	for _, tenant := range []spi.TenantID{"tenant-A", "tenant-B", "tenant-C"} {
		ctx := ctxWithTenant(tenant)
		sts, err := factory.ScheduledTaskStore(ctx)
		if err != nil {
			t.Fatalf("ScheduledTaskStore(%s): %v", tenant, err)
		}
		task := spi.ScheduledTask{
			ID:            "e:" + string(tenant),
			TenantID:      tenant,
			Type:          spi.ScheduledTaskFireTransition,
			ScheduledTime: 1000,
			EntityID:      "e",
			SourceState:   "S",
			Transition:    "T",
		}
		if err := sts.Upsert(ctx, task); err != nil {
			t.Fatalf("Upsert(%s): %v", tenant, err)
		}
	}

	// ScanDue runs from a background/tenant-less context — the scheduler's
	// scan loop context, not a request context.
	sts, err := factory.ScheduledTaskStore(context.Background())
	if err != nil {
		t.Fatalf("ScheduledTaskStore(background): %v", err)
	}
	due, err := sts.ScanDue(context.Background(), 2000, 100)
	if err != nil {
		t.Fatalf("ScanDue: %v", err)
	}

	seen := map[spi.TenantID]bool{}
	for _, task := range due {
		seen[task.TenantID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("ScanDue must be cross-tenant: want rows from >=2 tenants, got %+v (tasks=%+v)", seen, due)
	}
	for _, tenant := range []spi.TenantID{"tenant-A", "tenant-B", "tenant-C"} {
		if !seen[tenant] {
			t.Errorf("ScanDue missing tenant %s: got %+v", tenant, due)
		}
	}
}

// TestPostgres_ScheduledTasksTable_NotRLSEnrolled documents and locks in the
// F7 decision: scheduled_tasks is deliberately excluded from row-level
// security (unlike every other tenant table in this schema), because
// ScanDue's cross-tenant read must never be scoped to a single tenant even
// if RLS enforcement is later strengthened (FORCE + non-owner role) for the
// rest of the schema. If this test starts failing, RLS was added to
// scheduled_tasks — revisit ScanDue's cross-tenant contract before doing so.
func TestPostgres_ScheduledTasksTable_NotRLSEnrolled(t *testing.T) {
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })

	var rlsEnabled bool
	err := pool.QueryRow(context.Background(),
		"SELECT relrowsecurity FROM pg_class WHERE relname = 'scheduled_tasks'").Scan(&rlsEnabled)
	if err != nil {
		t.Fatalf("failed to check RLS for scheduled_tasks: %v", err)
	}
	if rlsEnabled {
		t.Error("scheduled_tasks must NOT have RLS enabled — ScanDue is a trusted cross-tenant system read (F7)")
	}
}
