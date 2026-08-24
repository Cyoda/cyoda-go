package postgres_test

// pit_committed_only_test.go — every point-in-time read is committed-only,
// including inside a transaction.
//
// The SPI contract for a point-in-time read is that it ignores any ambient
// transaction and answers from committed state as of the requested instant.
// The postgres plugin routes ordinary reads through the context-resolving
// Querier, which joins the caller's pgx.Tx — so a point-in-time read issued
// there sees the transaction's OWN uncommitted writes.
//
// The `transaction_time <= CURRENT_TIMESTAMP` guard in the PIT queries cannot
// prevent it: Save stamps valid_time/transaction_time from CURRENT_TIMESTAMP,
// which PostgreSQL fixes at TRANSACTION START, so inside the writing
// transaction the comparison reduces to T_start <= T_start — trivially true.
// The only thing that actually reads committed state is running the query off
// the transaction, on the pool. GetPage's asAt path already does this
// (entity_store.go's getPageAsAt); these tests hold the rest of the family to
// the same behaviour.
//
// Every test below writes INSIDE a transaction and then issues the
// point-in-time read from that same transaction, at an `asAt` deliberately in
// the future so the PIT window itself can never be what hides the write — only
// committed-only routing can.

import (
	"context"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

var pitModel = spi.ModelRef{EntityName: "pitperson", ModelVersion: "1"}

const pitCommittedTenant = spi.TenantID("pit-committed-tenant")

// pitFuture is the asAt every test uses: far enough ahead that an
// in-transaction write's own valid_time falls inside the PIT window, so the
// window can never be the reason a write is invisible.
func pitFuture() time.Time { return time.Now().UTC().Add(1 * time.Hour) }

// setupPITCommitted seeds one committed entity ("pit-committed", data
// {"v":"committed"}) and returns the factory, the TM and a tenant context.
func setupPITCommitted(t *testing.T) (*postgres.StoreFactory, *postgres.TransactionManager, context.Context) {
	t.Helper()
	factory, tm := setupFCWTest(t)
	ctx := ctxWithTenant(pitCommittedTenant)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "pit-committed", ModelRef: pitModel, State: "NEW"},
		Data: []byte(`{"v":"committed"}`),
	}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	return factory, tm, ctx
}

// beginPITTx opens a transaction, writes a brand-new entity ("pit-dirty") and
// updates the seeded one to {"v":"dirty"} — both uncommitted — and returns the
// tx-scoped store and context. The caller's point-in-time read must see
// neither.
func beginPITTx(t *testing.T, factory *postgres.StoreFactory, tm *postgres.TransactionManager, ctx context.Context) (spi.EntityStore, context.Context) {
	t.Helper()
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })

	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}
	if _, err := txStore.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "pit-dirty", ModelRef: pitModel, State: "NEW"},
		Data: []byte(`{"v":"dirty"}`),
	}); err != nil {
		t.Fatalf("in-tx Save (new): %v", err)
	}
	if _, err := txStore.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "pit-committed", ModelRef: pitModel, State: "CHANGED"},
		Data: []byte(`{"v":"dirty"}`),
	}); err != nil {
		t.Fatalf("in-tx Save (update): %v", err)
	}
	return txStore, txCtx
}

// TestPITCommittedOnly_GetAsAt: GetAsAt inside a transaction must answer from
// committed state — the committed version of an entity the transaction has
// updated, and ErrNotFound for one the transaction created.
func TestPITCommittedOnly_GetAsAt(t *testing.T) {
	factory, tm, ctx := setupPITCommitted(t)
	txStore, txCtx := beginPITTx(t, factory, tm, ctx)
	asAt := pitFuture()

	got, err := txStore.GetAsAt(txCtx, "pit-committed", asAt)
	if err != nil {
		t.Fatalf("GetAsAt(pit-committed): %v", err)
	}
	if string(got.Data) != `{"v":"committed"}` {
		t.Errorf("GetAsAt in-tx = %s, want {\"v\":\"committed\"} — a point-in-time read must not see the "+
			"transaction's own uncommitted update", got.Data)
	}

	if _, err := txStore.GetAsAt(txCtx, "pit-dirty", asAt); err == nil {
		t.Error("GetAsAt(pit-dirty) in-tx returned an entity — a point-in-time read must not see an entity " +
			"the transaction created but has not committed")
	}
}

// TestPITCommittedOnly_GetAllAsAt: same contract, collection form.
func TestPITCommittedOnly_GetAllAsAt(t *testing.T) {
	factory, tm, ctx := setupPITCommitted(t)
	txStore, txCtx := beginPITTx(t, factory, tm, ctx)

	got, err := txStore.GetAllAsAt(txCtx, pitModel, pitFuture())
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	assertCommittedOnly(t, "GetAllAsAt", got)
}

// TestPITCommittedOnly_Search: Search with opts.PointInTime set.
func TestPITCommittedOnly_Search(t *testing.T) {
	factory, tm, ctx := setupPITCommitted(t)
	txStore, txCtx := beginPITTx(t, factory, tm, ctx)

	asAt := pitFuture()
	got, err := txStore.(spi.Searcher).Search(txCtx, spi.Filter{}, spi.SearchOptions{
		ModelName:    pitModel.EntityName,
		ModelVersion: pitModel.ModelVersion,
		Limit:        100,
		PointInTime:  &asAt,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	assertCommittedOnly(t, "Search", got)
}

// TestPITCommittedOnly_Iterate: Iterate with opts.PointInTime set. OrderBy is
// left empty — ordered iteration inside a transaction is unsupported per the
// spi.Iterable contract, and this test is about visibility, not order.
func TestPITCommittedOnly_Iterate(t *testing.T) {
	factory, tm, ctx := setupPITCommitted(t)
	txStore, txCtx := beginPITTx(t, factory, tm, ctx)

	asAt := pitFuture()
	it, err := txStore.(spi.Iterable).Iterate(txCtx, pitModel, spi.Filter{}, spi.IterateOptions{PointInTime: &asAt})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var got []*spi.Entity
	for it.Next() {
		got = append(got, it.Entity())
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Iterate Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate Err: %v", err)
	}
	assertCommittedOnly(t, "Iterate", got)
}

// TestPITCommittedOnly_GetPageAsAt is the control: GetPage's asAt path already
// routes off the transaction (entity_store.go's getPageAsAt), and this pins
// that behaviour so the rest of the family has a same-package reference for
// what "committed-only" looks like.
func TestPITCommittedOnly_GetPageAsAt(t *testing.T) {
	factory, tm, ctx := setupPITCommitted(t)
	txStore, txCtx := beginPITTx(t, factory, tm, ctx)

	asAt := pitFuture()
	got, err := txStore.GetPage(txCtx, pitModel, 100, 0, &asAt)
	if err != nil {
		t.Fatalf("GetPage(asAt): %v", err)
	}
	assertCommittedOnly(t, "GetPage(asAt)", got)
}

// assertCommittedOnly checks that a point-in-time result set contains exactly
// the committed entity, at its committed payload — no uncommitted create, and
// no uncommitted update showing through.
func assertCommittedOnly(t *testing.T, method string, got []*spi.Entity) {
	t.Helper()

	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.Meta.ID)
	}
	sort.Strings(ids)

	if len(ids) != 1 || ids[0] != "pit-committed" {
		t.Fatalf("%s in-tx returned %v, want [pit-committed] — a point-in-time read must not see the "+
			"transaction's own uncommitted writes", method, ids)
	}
	if string(got[0].Data) != `{"v":"committed"}` {
		t.Errorf("%s in-tx returned %s, want {\"v\":\"committed\"} — a point-in-time read must not see the "+
			"transaction's own uncommitted update", method, got[0].Data)
	}
}

// TestPITCommittedOnly_NonTxUnaffected guards the other direction: outside a
// transaction, every point-in-time read still answers normally. Routing PIT off
// the ambient transaction must not change what a plain non-transactional read
// returns.
func TestPITCommittedOnly_NonTxUnaffected(t *testing.T) {
	factory, _, ctx := setupPITCommitted(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	asAt := pitFuture()

	one, err := store.GetAsAt(ctx, "pit-committed", asAt)
	if err != nil {
		t.Fatalf("GetAsAt: %v", err)
	}
	if string(one.Data) != `{"v":"committed"}` {
		t.Errorf("non-tx GetAsAt = %s, want {\"v\":\"committed\"}", one.Data)
	}

	all, err := store.GetAllAsAt(ctx, pitModel, asAt)
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	assertCommittedOnly(t, "non-tx GetAllAsAt", all)

	found, err := store.(spi.Searcher).Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName:    pitModel.EntityName,
		ModelVersion: pitModel.ModelVersion,
		Limit:        100,
		PointInTime:  &asAt,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	assertCommittedOnly(t, "non-tx Search", found)

	it, err := store.(spi.Iterable).Iterate(ctx, pitModel, spi.Filter{}, spi.IterateOptions{PointInTime: &asAt})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	var iterated []*spi.Entity
	for it.Next() {
		iterated = append(iterated, it.Entity())
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Iterate Close: %v", err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Iterate Err: %v", err)
	}
	assertCommittedOnly(t, "non-tx Iterate", iterated)
}
