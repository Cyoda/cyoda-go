package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// seedReaderPoolFixture creates a factory with rows seeded for the reader-pool
// tests and returns the factory, a tenant context, the model ref, and the
// entity store.
func seedReaderPoolFixture(t *testing.T, name string) (*sqlite.StoreFactory, context.Context, spi.ModelRef, spi.EntityStore) {
	t.Helper()
	dir := t.TempDir()
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	// More than one row so a single Next() leaves the iterator genuinely
	// mid-scan, holding whichever reader connection served it.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e%d", i)
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	return factory, ctx, ref, store
}

// TestReadDB_ConcurrentIteratorsRunInParallel: two non-tx Iterate cursors held
// open at the same time must both make progress.
//
// The reader *sql.DB was introduced to stop a long, undrained scan from
// starving other work, but was itself capped at SetMaxOpenConns(1) — which
// reintroduces the same starvation among readers: the second Iterate's
// QueryContext blocks in database/sql waiting for the sole connection, which
// only frees when the first iterator's *sql.Rows is closed. WAL journal mode
// permits concurrent readers, and no reader path ever opens a transaction, so
// nothing requires the cap. A bounded deadline turns the hang into a
// deterministic failure.
func TestReadDB_ConcurrentIteratorsRunInParallel(t *testing.T) {
	_, ctx, ref, store := seedReaderPoolFixture(t, "reader_pool_iter.db")

	iterable, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("entityStore does not implement spi.Iterable")
	}

	first, err := iterable.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate (first): %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if !first.Next() {
		t.Fatalf("first Iterate: expected a row, got none (err=%v)", first.Err())
	}
	// first is now mid-scan and deliberately not drained or closed.

	done := make(chan error, 1)
	go func() {
		second, err := iterable.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{})
		if err != nil {
			done <- fmt.Errorf("Iterate (second): %w", err)
			return
		}
		defer second.Close()
		if !second.Next() {
			done <- fmt.Errorf("second Iterate: expected a row, got none (err=%v)", second.Err())
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second concurrent Iterate blocked for 5s behind the first — " +
			"the reader pool serialises readers instead of using WAL's concurrent-reader capability")
	}
}

// TestReadDB_GetPageNotStarvedByOpenIterator: an interactive GetPage must not
// queue behind a long streaming scan that is holding a reader connection.
// Same root cause as the test above, exercised through the pathway an
// interactive API request actually takes.
func TestReadDB_GetPageNotStarvedByOpenIterator(t *testing.T) {
	_, ctx, ref, store := seedReaderPoolFixture(t, "reader_pool_page.db")

	iterable, ok := store.(spi.Iterable)
	if !ok {
		t.Fatal("entityStore does not implement spi.Iterable")
	}
	it, err := iterable.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	t.Cleanup(func() { _ = it.Close() })
	if !it.Next() {
		t.Fatalf("Iterate: expected a row, got none (err=%v)", it.Err())
	}

	done := make(chan error, 1)
	go func() {
		page, err := store.GetPage(ctx, ref, 2, 0, nil)
		if err != nil {
			done <- fmt.Errorf("GetPage: %w", err)
			return
		}
		if len(page) != 2 {
			done <- fmt.Errorf("GetPage: want 2 entities, got %d", len(page))
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetPage blocked for 5s behind an open streaming Iterate — " +
			"the reader pool serialises interactive reads behind long scans")
	}
}

// TestReadDB_EveryPooledConnectionIsConfigured: PRAGMA cache_size,
// foreign_keys and busy_timeout are per-connection settings. Now that readDB
// runs a real pool, configuring it by executing PRAGMAs over the *sql.DB
// would only reach whichever connection happened to serve them — the rest
// would silently fall back to SQLite's defaults (2 MiB page cache, foreign
// keys off, the driver's 1-minute busy timeout). The reader therefore carries
// its PRAGMAs in the DSN, which the driver replays on every connection it
// opens; this test checks every connection in a saturated pool, not just the
// first one handed out.
func TestReadDB_EveryPooledConnectionIsConfigured(t *testing.T) {
	factory, _, _, _ := seedReaderPoolFixture(t, "reader_pool_pragmas.db")

	readDB := sqlite.ReadDBForTest(factory)
	n := sqlite.ReaderPoolSizeForTest()
	ctx := context.Background()

	// Hold every connection at once so each check lands on a distinct one.
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := readDB.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire reader connection %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})

	for i, c := range conns {
		var cacheSize, foreignKeys, busyTimeout int
		if err := c.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cacheSize); err != nil {
			t.Fatalf("conn %d: PRAGMA cache_size: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d: PRAGMA foreign_keys: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d: PRAGMA busy_timeout: %v", i, err)
		}
		// NewStoreFactoryForTest uses CacheSizeKiB=64000, BusyTimeout=5s.
		if cacheSize != -64000 {
			t.Errorf("conn %d: cache_size = %d, want -64000 (configured page cache missing on this connection)", i, cacheSize)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1", i, foreignKeys)
		}
		if busyTimeout != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, busyTimeout)
		}
	}

	// The writer keeps its single connection and applyPragmas, but its DSN
	// dropped a "_busy_timeout" parameter this driver never understood (it
	// takes PRAGMAs via "_pragma"), so confirm the configured timeout is
	// still in force there.
	var writerBusyTimeout int
	if err := sqlite.DBForTest(factory).QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&writerBusyTimeout); err != nil {
		t.Fatalf("writer PRAGMA busy_timeout: %v", err)
	}
	if writerBusyTimeout != 5000 {
		t.Errorf("writer busy_timeout = %d, want 5000", writerBusyTimeout)
	}
}
