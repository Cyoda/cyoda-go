// Package txsearchryw holds the cross-backend parity acceptance test for
// in-transaction read-your-own-writes (RYW) search (issue #420, Task 16).
//
// WHY A STORE-LEVEL PACKAGE (and not an entry in e2e/parity/registry.go).
//
// The e2e/parity registry harness is strictly black-box: it drives cyoda-go
// as a subprocess over HTTP/gRPC via BackendFixture, which exposes no store or
// TransactionManager handle. Two of the RYW invariants this task must pin are
// NOT expressible through that black box:
//
//   - Delete-then-Save the SAME id inside one tx (the invariant fixed in
//     Task 7): entity ids are server-assigned on create (see
//     internal/domain/entity/service.go — `entityID := uuid.UUID(
//     h.uuids.NewTimeUUID())`), so there is no HTTP/gRPC path to re-save a
//     just-deleted id. A "delete + create a fresh id" approximation would not
//     exercise the tombstone-resurrection code path at all.
//   - Deterministic entity_id-tiebreak ordering across a buffered add: random
//     server-assigned UUIDs make the tiebreak order unknowable to a hand oracle.
//
// The faithful acceptance oracle is therefore store-level, exactly mirroring
// plugins/{memory,sqlite,postgres}/searcher_tx_test.go, but run against ALL
// THREE in-tree backends from a single table so the identical RYW scenario is
// asserted to produce byte-identical results everywhere. The root module
// already requires all three plugins (go.mod) and go.work composes them
// locally, so they are importable in-process here.
//
// A divergence between backends surfaced by this test is a real bug to fix in
// the diverging backend (per .claude/rules — "backend divergence is a bug"),
// never a reason to weaken the assertion.
package txsearchryw

import (
	"context"
	"flag"
	"log"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/cyoda-platform/cyoda-go/internal/testpg"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// pgPool is the shared PostgreSQL pool for the postgres backend, or nil when a
// container could not be started (short mode / Docker unavailable). When nil,
// the postgres sub-test skips; memory and sqlite always run.
var pgPool *pgxpool.Pool

func TestMain(m *testing.M) {
	flag.Parse()

	// memory + sqlite need no container; postgres does. In short mode we skip
	// the container entirely and let the postgres sub-test skip.
	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	opts := append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("txryw_test"),
		tcpostgres.WithUsername("testuser"),
		tcpostgres.WithPassword("testpass"),
	}, testpg.HardenedOptions()...)

	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine", opts...)
	if err != nil {
		// Docker not available: run memory+sqlite only (postgres sub-test skips).
		log.Printf("txsearchryw: postgres container unavailable, running memory+sqlite only: %v", err)
		os.Exit(m.Run())
	}
	defer func() {
		testpg.DumpDiagnosticsIfDied(ctx, pgContainer)
		_ = pgContainer.Terminate(ctx)
	}()

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("txsearchryw: connection string: %v", err)
	}
	pool, err := postgres.NewPool(ctx, postgres.DBConfig{URL: connStr, MaxConns: 5, MinConns: 1})
	if err != nil {
		log.Fatalf("txsearchryw: pool: %v", err)
	}
	defer pool.Close()
	if err := postgres.Migrate(pool); err != nil {
		log.Fatalf("txsearchryw: migrate: %v", err)
	}
	pgPool = pool

	os.Exit(m.Run())
}
