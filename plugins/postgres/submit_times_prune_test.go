package postgres_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
	"github.com/google/uuid"
)

// TestPruneSubmitTimes_KeepsAnotherTenantsLiveRow pins the scope of the
// submit_times housekeeping. The DELETE it issues is bounded only by
// submit_time — it carries no tenant predicate, so one tenant's commit
// sweeps every tenant's rows. That is intended (TTL-expired rows are
// garbage whoever owns them) and it is one of exactly two statements in
// this plugin with no tenant scope, so it is worth a test rather than a
// comment: the bound parameter is the only thing standing between
// opportunistic housekeeping and a cross-tenant delete.
//
// Both rows below belong to a tenant that never commits anything. The
// expired one must go and the live one must stay — and the expired one is
// the load-bearing half: without it, a prune that never ran at all would
// leave the live row in place and the test would pass having proved
// nothing.
func TestPruneSubmitTimes_KeepsAnotherTenantsLiveRow(t *testing.T) {
	factory, tm := setupEntityTestWithTM(t)
	pool := postgres.PoolForTest(factory)
	bg := context.Background()

	const otherTenant = "prune-tenant-b"
	expiredID := uuid.NewString()
	liveID := uuid.NewString()

	// submitTimeTTL is an hour; -2h is unambiguously expired and now is
	// unambiguously inside it, so neither assertion rides on the exact TTL.
	if _, err := pool.Exec(bg,
		`INSERT INTO submit_times (tenant_id, tx_id, submit_time) VALUES ($1, $2, $3), ($1, $4, $5)`,
		otherTenant, expiredID, time.Now().Add(-2*time.Hour), liveID, time.Now()); err != nil {
		t.Fatalf("plant tenant-B submit_times rows: %v", err)
	}

	// A commit by a different tenant. The prune is opportunistic and
	// rate-limited, but lastSubmitTimePruneNano starts at zero, so the first
	// commit of a fresh manager always takes the window.
	ctx := ctxWithTenant("prune-tenant-a")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       uuid.NewString(),
			ModelRef: spi.ModelRef{EntityName: "prune-probe", ModelVersion: "1"},
		},
		Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rowCount := func(txID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(bg,
			`SELECT count(*) FROM submit_times WHERE tenant_id = $1 AND tx_id = $2`,
			otherTenant, txID).Scan(&n); err != nil {
			t.Fatalf("count submit_times row: %v", err)
		}
		return n
	}

	if got := rowCount(expiredID); got != 0 {
		t.Fatalf("tenant B's expired row survived: got %d rows, want 0 — the prune did not run, "+
			"so the live-row assertion below would prove nothing", got)
	}
	if got := rowCount(liveID); got != 1 {
		t.Errorf("tenant A's commit deleted tenant B's live submit_times row: got %d rows, want 1", got)
	}
}
