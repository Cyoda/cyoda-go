package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// With every main-pool connection held by an open entity transaction, the
// executor's liveness writes still land: they run on the scheduler pool.
func TestPGSearchStore_LivenessSurvivesAnExhaustedMainPool(t *testing.T) {
	pool := newTestPoolSized(t, 2)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	factory := postgres.NewStoreFactory(pool)
	factory.InitTransactionManager(newTestUUIDGenerator())
	t.Cleanup(func() { postgres.CloseSchedulerPoolsForTest(factory) })

	ctx := ctxWithTenant("starve-tenant")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	if err := store.CreateJob(ctx, newRunningJob("job-starve", "starve-tenant", time.Now())); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	for i := 0; i < 2; i++ {
		postgres.BeginGuardedForTest(t, tm, ctx) // holds one main-pool connection each
	}

	liveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := store.Heartbeat(liveCtx, "job-starve", 1); err != nil {
		t.Fatalf("Heartbeat with the main pool exhausted: %v", err)
	}
	if _, err := store.ClaimStale(liveCtx, time.Hour, 10); err != nil {
		t.Fatalf("ClaimStale with the main pool exhausted: %v", err)
	}
}
