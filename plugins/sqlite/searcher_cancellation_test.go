package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// expiredSearchCtx returns a context derived from parent that is already past
// its deadline (context.WithTimeout(parent, 0), waited on Done()). Mirrors the
// memory plugin's expiredCtx helper (plugins/memory/searcher_test.go) so both
// backends prove the same contract: a pre-expired ctx aborts the scan instead
// of running an already-expired request to completion.
func expiredSearchCtx(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 0)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

// TestSearch_PreExpiredCtxAborts: a pre-expired ctx must abort the non-tx
// committed-pushdown scan (searchCommitted's row loop) rather than returning
// a full result set computed past the deadline. Spec D5: sqlite's row fetch
// is implicitly cancellable via database/sql — QueryContext itself already
// rejects an already-Done ctx before any row is fetched — but the explicit
// amortized checks added to the loops are what guarantee deterministic,
// driver-independent behavior for the stages database/sql does not cover
// (see TestSearch_TxOverlayBufferLoop_AbortsEarly for the pure-Go stage that
// this pre-expired-ctx shape cannot reach).
func TestSearch_PreExpiredCtxAborts(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	// setupSearcherTest already seeded 5 "person" entities; add 5 more of a
	// distinct model so the total scanned set is ~10 rows, matching the T12
	// shape without disturbing the existing "person" fixture semantics.
	extraRef := spi.ModelRef{EntityName: "widget", ModelVersion: "1"}
	for i := 0; i < 5; i++ {
		id := "w-" + string(rune('0'+i))
		_, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: extraRef, State: "NEW"},
			Data: []byte(`{"n":1}`),
		})
		if err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	deadCtx := expiredSearchCtx(t, ctx)

	got, err := searcher.Search(deadCtx, spi.Filter{}, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        100,
	})
	if err == nil {
		t.Fatalf("Search with pre-expired ctx: expected error, got %d results", len(got))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Search with pre-expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	if len(got) != 0 {
		t.Errorf("Search with pre-expired ctx: got %d results, want 0", len(got))
	}
}

// TestSearch_PreExpiredCtxAborts_InTx: the read-your-own-writes overlay path
// (searchTxOverlay) must abort the same way the non-tx path does when handed
// a pre-expired ctx, even though the underlying transaction is otherwise
// live. The deadline belongs to the request, not the transaction. Like the
// non-tx case, QueryContext rejects the already-Done ctx up front (before the
// buffer loop runs); see TestSearch_TxOverlayBufferLoop_AbortsEarly for the
// buffer-loop-specific coverage.
func TestSearch_PreExpiredCtxAborts_InTx(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Save an in-tx buffered row so the overlay's buffer-match loop has
	// something to walk, not just the committed stream.
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{ID: "e-buf", ModelRef: ref, State: "NEW"},
		Data: []byte(`{"name":"Buffered","age":1,"city":"Berlin"}`),
	}); err != nil {
		t.Fatalf("Save in-tx: %v", err)
	}

	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}

	tx := spi.GetTransaction(txCtx)
	deadTxCtx := spi.WithTransaction(expiredSearchCtx(t, ctx), tx)

	got, err := searcher.Search(deadTxCtx, spi.Filter{}, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        100,
	})
	if err == nil {
		t.Fatalf("in-tx Search with pre-expired ctx: expected error, got %d results", len(got))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("in-tx Search with pre-expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	if len(got) != 0 {
		t.Errorf("in-tx Search with pre-expired ctx: got %d results, want 0", len(got))
	}
}

// TestSearch_MidScanTimeoutChainsDeadlineExceeded: a context whose deadline
// expires WHILE the committed row loop is streaming (not before Query
// starts) must still produce an error chaining context.DeadlineExceeded. The
// sqlite driver's own interrupt mechanism can win this race and surface a raw
// driver error via rows.Err() — the loop's post-iteration ctx check is what
// guarantees the caller always gets the deterministic, chainable error
// regardless of which mechanism (database/sql's Done()-channel watcher vs.
// the driver's progress-handler interrupt) wins.
func TestSearch_MidScanTimeoutChainsDeadlineExceeded(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "midscan.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "bulk", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}

	const n = 8000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bulk-%05d", i)
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"n":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()

	got, err := searcher.Search(timeoutCtx, spi.Filter{}, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        n + 1,
	})
	if err == nil {
		t.Fatalf("Search with mid-scan-expiring ctx over %d rows: expected error, got %d results (deadline too generous for this scan size — increase n)", n, len(got))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Search with mid-scan-expiring ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
}

// TestSearch_TxOverlayBufferLoop_AbortsEarly proves the tx-buffer match loop
// (searchTxOverlay's pure-Go `for id, e := range tx.Buffer` loop, which never
// touches SQL) checks ctx itself rather than relying on the committed-side
// SQL query to catch an expired deadline downstream. A pre-expired ctx can't
// exercise this: Search's committed QueryContext call runs before the buffer
// loop and already rejects an already-Done ctx (see
// TestSearch_PreExpiredCtxAborts_InTx). So this test starts with a LIVE ctx
// (the committed query — zero matching rows for a private "ghost" model —
// succeeds) and a short real deadline that elapses while the buffer loop is
// still walking a large tx.Buffer. Without the loop's own amortized check,
// the full buffer (and the sort that follows) would be processed to
// completion before the merge ever revisits SQL, burning the entire scan
// cost after the deadline has already passed. With the check, the loop
// aborts within ~1024 entries of the deadline elapsing, so the whole call
// returns in a small fraction of the time a full unchecked scan would take.
func TestSearch_TxOverlayBufferLoop_AbortsEarly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "buffer_abort.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	// A model with no committed rows at all: the committed-snapshot SQL query
	// this test's search issues matches zero rows, so it completes almost
	// instantly regardless of the deadline, isolating the buffer loop as the
	// dominant cost.
	ref := spi.ModelRef{EntityName: "ghost", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	tm, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	searcher, ok := store.(spi.Searcher)
	if !ok {
		t.Fatal("entityStore does not implement spi.Searcher")
	}

	_, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Buffer a large number of entries under a data-path filter (JSON
	// extraction per entity, not a free comparison) so the unchecked loop's
	// full-scan cost is large and reliably measurable against a short
	// deadline.
	const n = 1_000_000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("g-%06d", i)
		city := "Munich"
		if i%2 == 0 {
			city = "Berlin"
		}
		if _, err := store.Save(txCtx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(fmt.Sprintf(`{"city":%q}`, city)),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	cityBerlinFilter := spi.Filter{
		Op: spi.FilterEq, Path: "city", Source: spi.SourceData,
		Value: "Berlin", Declared: []spi.DataType{spi.String},
	}

	deadline := 20 * time.Millisecond
	timeoutCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	tx := spi.GetTransaction(txCtx)
	probedTxCtx := spi.WithTransaction(timeoutCtx, tx)

	start := time.Now()
	got, err := searcher.Search(probedTxCtx, cityBerlinFilter, spi.SearchOptions{
		ModelName:    ref.EntityName,
		ModelVersion: ref.ModelVersion,
		Limit:        n + 1,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Search over %d buffered entries with %s deadline: expected error, got %d results", n, deadline, len(got))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Search over buffered entries with expiring ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	// A full unchecked scan of n=1,000,000 buffered entries (JSON-extracting
	// a field from each) takes on the order of several hundred milliseconds
	// on typical hardware (observed ~500ms before the buffer loop's own
	// amortized check was added). The check aborts within ~1024 entries of
	// the deadline firing, so the whole call should return in well under
	// that — 300ms gives generous headroom above the check overhead while
	// still being far short of a full unchecked scan.
	const bound = 300 * time.Millisecond
	if elapsed > bound {
		t.Errorf("Search over %d buffered entries took %s (> %s bound) — buffer loop does not appear to abort early on ctx expiry", n, elapsed, bound)
	}
}
