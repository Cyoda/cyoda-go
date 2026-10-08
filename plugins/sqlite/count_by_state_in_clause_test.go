package sqlite_test

import (
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Tests SQLite IN-clause parameterization.
//
// The state-filter IN-clause is composed entirely of `?` markers; state
// values are bound as SQL parameters. To stay safely under SQLite's
// SQLITE_MAX_VARIABLE_NUMBER (default 999), the cap on the state list is
// derived from the SQLite limit minus the count of other parameters bound
// in the same query (tenant_id, model_name, model_version → 3). Oversized
// lists are rejected at the helper boundary with a wrapped sentinel error,
// not a SQL driver-error leak.

// TestCountByState_CapDerivedFromSQLiteVariableLimit asserts the cap is
// expressed as the SQLite default minus the number of other parameters
// bound in the CountByState query, rather than an unrelated arbitrary
// constant. This pins the documented derivation as part of the contract.
func TestCountByState_CapDerivedFromSQLiteVariableLimit(t *testing.T) {
	const sqliteDefaultMaxVars = 999
	const baseParams = 3 // tenant_id, model_name, model_version
	want := sqliteDefaultMaxVars - baseParams
	if got := sqlite.MaxStateFilterSize; got != want {
		t.Fatalf("MaxStateFilterSize = %d; want %d (SQLITE_MAX_VARIABLE_NUMBER %d - %d base params)",
			got, want, sqliteDefaultMaxVars, baseParams)
	}
}

// TestCountByState_ManyStateFilter_ReturnsCorrectCounts pins the
// many-state-filter execution path: with a state list near the cap, the
// query must execute without error AND return correct counts for the
// states actually present in the data set. This guards against any future
// refactor of the placeholder-list construction silently breaking the
// IN-clause semantics (e.g. wrong number of `?` markers, wrong arg
// ordering, off-by-one in the cap).
func TestCountByState_ManyStateFilter_ReturnsCorrectCounts(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// The setup helper saves 5 entities all in state "NEW". We construct
	// a state list at the cap and place "NEW" at a non-trivial position
	// to catch any off-by-one in the placeholder/argument ordering.
	states := make([]string, sqlite.MaxStateFilterSize)
	for i := range states {
		states[i] = fmt.Sprintf("STATE_%d", i)
	}
	// Replace the middle slot with the state our fixtures actually use.
	states[len(states)/2] = "NEW"

	got, err := store.CountByState(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, states, nil)
	if err != nil {
		t.Fatalf("CountByState with %d states: %v", len(states), err)
	}

	if got["NEW"] != 5 {
		t.Fatalf("CountByState[NEW] = %d; want 5 (got full map: %#v)", got["NEW"], got)
	}
	// No other configured state matches any fixture entity.
	for st, n := range got {
		if st != "NEW" && n != 0 {
			t.Errorf("unexpected non-zero count for state %q: %d", st, n)
		}
	}
}

// TestCountByState_SmallStateFilter_ReturnsCorrectCounts is the canonical
// small-N case: with a 10-element filter, counts for each requested state
// must be returned correctly. Matched-state buckets get the correct count;
// non-matched states are simply absent from the result map.
func TestCountByState_SmallStateFilter_ReturnsCorrectCounts(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	states := []string{"S0", "S1", "S2", "S3", "S4", "NEW", "S6", "S7", "S8", "S9"}

	got, err := store.CountByState(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, states, nil)
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	if got["NEW"] != 5 {
		t.Fatalf("CountByState[NEW] = %d; want 5", got["NEW"])
	}
}

// TestCountByState_OversizedRejection_WrapsSentinelError pins that
// oversized state lists yield a wrapped sentinel error usable with
// errors.Is — i.e. NOT a raw SQL driver "too many SQL variables" error
// leaked through the helper boundary.
func TestCountByState_OversizedRejection_WrapsSentinelError(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// One past the cap.
	states := make([]string, sqlite.MaxStateFilterSize+1)
	for i := range states {
		states[i] = fmt.Sprintf("STATE_%d", i)
	}

	_, err = store.CountByState(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, states, nil)
	if err == nil {
		t.Fatalf("CountByState: expected error for %d states, got nil", len(states))
	}
	if !errors.Is(err, sqlite.ErrStateFilterTooLarge) {
		t.Fatalf("CountByState err = %v; want errors.Is(err, ErrStateFilterTooLarge)", err)
	}
	// The error message should be informative without leaking SQL internals.
	msg := err.Error()
	if msg == "" {
		t.Fatalf("err.Error() returned empty string")
	}
}
