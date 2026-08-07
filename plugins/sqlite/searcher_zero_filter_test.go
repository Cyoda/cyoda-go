package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	sqlite "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// A zero-value Filter means "match all". Postgres skips planQuery for it, with
// a comment saying why: planQuery treats the empty Op as non-pushable and
// installs the zero filter as a residual. sqlite's Search had no such guard, so
// the same filter arrived with a residual installed — which loses LIMIT
// pushdown and arms the scan budget on a query that has nothing to post-filter.
//
// The observable consequence is a filter-free search failing with
// ErrScanBudgetExhausted on a model larger than the budget, where postgres and
// memory return the rows.
func TestSearch_ZeroValueFilterDoesNotArmScanBudget(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "zero_filter_test.db")
	factory, err := sqlite.NewStoreFactoryForTestWithScanLimit(context.Background(), dbPath, 3)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	const rows = 5 // comfortably above the scan budget of 3
	for i := 0; i < rows; i++ {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: fmt.Sprintf("z%d", i), ModelRef: ref, State: "NEW"},
			Data: []byte(`{"val":"x"}`),
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := store.(spi.Searcher).Search(ctx, spi.Filter{}, spi.SearchOptions{
		ModelName: "item", ModelVersion: "1", Limit: 10,
	})
	if errors.Is(err, spi.ErrScanBudgetExhausted) {
		t.Fatal("a filter-free search armed the scan budget: the zero filter was installed as a residual")
	}
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != rows {
		t.Errorf("got %d rows, want %d", len(got), rows)
	}
}

// The explicit empty-AND spelling — what ConditionToFilter emits for a nil
// condition — already took the other branch. Both spellings of "match
// everything" must behave the same.
func TestSearch_EmptyAndFilterDoesNotArmScanBudget(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty_and_test.db")
	factory, err := sqlite.NewStoreFactoryForTestWithScanLimit(context.Background(), dbPath, 3)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	ctx := testCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	store, _ := factory.EntityStore(ctx)

	const rows = 5
	for i := 0; i < rows; i++ {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: fmt.Sprintf("a%d", i), ModelRef: ref, State: "NEW"},
			Data: []byte(`{"val":"x"}`),
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := store.(spi.Searcher).Search(ctx, spi.Filter{Op: spi.FilterAnd}, spi.SearchOptions{
		ModelName: "item", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != rows {
		t.Errorf("got %d rows, want %d", len(got), rows)
	}
}
