package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Point-in-time boundaries in these tests must come from the DATABASE clock,
// never from time.Now() in the test process.
//
// This plugin stamps valid_time / transaction_time / LastModifiedDate from
// postgres itself (`SELECT CURRENT_TIMESTAMP`, entity_store.go), so a boundary
// taken from the test process's clock is a two-clock comparison. Against a
// testcontainer the database runs in the Docker VM, whose clock was measured
// lagging the macOS host by 10–13 ms under CPU load — the condition every
// `make race` / `go test ./...` run creates — versus 0.2–1.8 ms idle. That is
// more than the few-millisecond sleeps these tests used as a guard, and the
// skew direction (DB behind host) makes a later DB write compare as earlier
// than a host instant, silently resolving an as-at read to the wrong version.
// See issue #460.
//
// Call dbNow between writes to get "an instant after everything written so
// far". Note EntityStore.Save takes a defensive copy of its argument
// (Ownership Rule 4), so a write's stamp cannot be read back off the entity
// the test passed in — read the clock, or read the version back from the store.

// dbNow returns the database's current transaction timestamp — the same clock
// the plugin stamps versions from.
func dbNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&now); err != nil {
		t.Fatalf("read DB clock: %v", err)
	}
	return now
}
