package sqlite

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestSortEntitiesByOrder_CtxExpiredAbortsBeforeSorting is a clean,
// deterministic unit test (no SQL, no timing race) for spec D5's pre-sort
// check: sortEntitiesByOrder must check ctx.Err() before running sort.Slice,
// so a request whose deadline has already passed does not pay for an O(n log
// n) sort of a result set computed past that deadline. Calling
// sortEntitiesByOrder directly isolates this from the driver/database-sql
// behavior exercised by the SQL-touching loops (see searcher_cancellation_test.go).
func TestSortEntitiesByOrder_CtxExpiredAbortsBeforeSorting(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{ID: "b"}},
		{Meta: spi.EntityMeta{ID: "a"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	<-ctx.Done()

	err := sortEntitiesByOrder(ctx, rows, nil)
	if err == nil {
		t.Fatal("sortEntitiesByOrder with expired ctx: expected error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("sortEntitiesByOrder with expired ctx: err = %v, want chain containing context.DeadlineExceeded", err)
	}
	// The abort must happen BEFORE sorting: rows must still be in their
	// original (unsorted) order.
	if rows[0].Meta.ID != "b" || rows[1].Meta.ID != "a" {
		t.Errorf("sortEntitiesByOrder with expired ctx: rows were reordered (got ids %q, %q) — sort ran despite the expired ctx", rows[0].Meta.ID, rows[1].Meta.ID)
	}
}

// TestSortEntitiesByOrder_LiveCtxSorts proves the happy path still works: a
// live ctx sorts normally by entity_id ascending (the default order).
func TestSortEntitiesByOrder_LiveCtxSorts(t *testing.T) {
	rows := []*spi.Entity{
		{Meta: spi.EntityMeta{ID: "b"}},
		{Meta: spi.EntityMeta{ID: "a"}},
	}

	if err := sortEntitiesByOrder(context.Background(), rows, nil); err != nil {
		t.Fatalf("sortEntitiesByOrder with live ctx: unexpected error: %v", err)
	}
	if rows[0].Meta.ID != "a" || rows[1].Meta.ID != "b" {
		t.Errorf("sortEntitiesByOrder with live ctx: got ids %q, %q, want a, b", rows[0].Meta.ID, rows[1].Meta.ID)
	}
}
