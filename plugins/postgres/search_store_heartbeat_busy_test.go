package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// A Heartbeat tick that lands while the job's own SaveResults chunk
// transaction holds the row FOR UPDATE must not wait forever and must not
// surface the raw 55P03: it is a distinct, transient "busy" outcome the
// search service can tell apart from a fencing refusal.
func TestPGSearchStore_Heartbeat_RowLockedBySaveResults_ReturnsMarkedBusy(t *testing.T) {
	factory := setupSearchTest(t)
	ctx := ctxWithTenant("busy-tenant")
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	if err := store.CreateJob(ctx, newRunningJob("job-busy", "busy-tenant", time.Now())); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Hold the job row FOR UPDATE the way SaveResults' per-chunk transaction
	// does, on a connection of its own (the main pool), so Heartbeat (on the
	// scheduler pool) has to wait for it.
	pool := postgres.PoolForTest(factory)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin locking tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var status string
	var epoch int64
	if err := tx.QueryRow(context.Background(),
		`SELECT status, epoch FROM search_jobs WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		"job-busy", "busy-tenant").Scan(&status, &epoch); err != nil {
		t.Fatalf("lock job row: %v", err)
	}

	err = store.Heartbeat(ctx, "job-busy", 1)
	if err == nil {
		t.Fatal("Heartbeat against a row locked by an open transaction: got nil, want an error")
	}
	if !errors.Is(err, spi.ErrTaskBusy) {
		t.Fatalf("Heartbeat against a locked row: got %v, want an error marked spi.ErrTaskBusy", err)
	}
}
