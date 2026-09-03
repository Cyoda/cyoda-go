package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// TestIterate_DoesNotBlockConcurrentAsyncWrite: a non-tx Iterate that holds
// its rows open (mid-scan — Next() has returned true but the iterator has
// not been drained or Closed) must not starve a concurrent write against the
// same StoreFactory.
//
// Before the dedicated reader connection, Iterate and the async-search
// store's SaveResults shared one writer *sql.DB capped to a single
// connection (SetMaxOpenConns(1) — SQLite is single-writer). An open,
// undrained Rows checks out that sole connection for the life of the
// iterator, so a concurrent SaveResults write (itself a short BeginTx/
// Commit) blocks waiting for a connection that never frees up until the
// iterator is closed — here, never, since the test holds it open
// deliberately. A bounded deadline turns that hang into a deterministic
// failure instead of stalling the whole suite.
func TestIterate_DoesNotBlockConcurrentAsyncWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "reader_deadlock.db")
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
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

	// Seed more than one row so Next() returning true leaves the iterator
	// genuinely mid-scan (not already at EOF, where Rows may have been
	// released back to the pool already).
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e%d", i)
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, ModelRef: ref, State: "NEW"},
			Data: []byte(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	iterable := store
	it, err := iterable.Iterate(ctx, ref, spi.Filter{}, spi.IterateOptions{})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	t.Cleanup(func() { _ = it.Close() })
	if !it.Next() {
		t.Fatalf("Iterate: expected at least one row, got none (err=%v)", it.Err())
	}
	// it is now mid-scan: its underlying *sql.Rows is open, not drained, not
	// closed — deliberately, to hold whichever connection served it.

	asyncStore, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	job := &spi.SearchJob{
		ID:         "job-1",
		Status:     "RUNNING",
		ModelRef:   ref,
		CreateTime: time.Now(),
	}
	if err := asyncStore.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ids := func(yield func(string) bool) { yield("e0") }
		done <- asyncStore.SaveResults(ctx, job.ID, 1, ids)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SaveResults: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SaveResults blocked for 5s behind an open non-tx Iterate — " +
			"a single shared writer connection starves concurrent writes")
	}
}
