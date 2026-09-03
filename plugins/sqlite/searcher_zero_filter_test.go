package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	sqlite "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// A zero-value Filter means "match all". planQuery treats the empty Op as
// non-pushable and would install the zero filter as its own residual, so Search
// guards the call behind planFor. Both spellings of "match everything" — the
// zero Filter{} and the explicit empty AND that ConditionToFilter emits for a
// nil condition — must return every row.
//
// The guard's own consequence (a needless residual costs LIMIT pushdown and
// disables native GROUP BY) is asserted directly on planFor in
// TestPlanFor_MatchAllLeavesNoResidual; these two cover the production entry
// point end to end.
func TestSearch_ZeroValueFilterReturnsAllRows(t *testing.T) {
	assertMatchAllReturnsEveryRow(t, "zero_filter_test.db", "z", spi.Filter{})
}

func TestSearch_EmptyAndFilterReturnsAllRows(t *testing.T) {
	assertMatchAllReturnsEveryRow(t, "empty_and_test.db", "a", spi.Filter{Op: spi.FilterAnd})
}

func assertMatchAllReturnsEveryRow(t *testing.T, dbName, idPrefix string, filter spi.Filter) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), dbName)
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
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
			Meta: spi.EntityMeta{ID: fmt.Sprintf("%s%d", idPrefix, i), ModelRef: ref, State: "NEW"},
			Data: []byte(`{"val":"x"}`),
		}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := store.Search(ctx, filter, spi.SearchOptions{
		ModelName: "item", ModelVersion: "1", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != rows {
		t.Errorf("got %d rows, want %d", len(got), rows)
	}
}
