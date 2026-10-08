package sqlite_test

import (
	"errors"
	"fmt"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Regression tests for the CountByState state-filter cap.
//
// CountByState's IN-clause is built from `?` placeholder markers only;
// state values are bound as SQL parameters (no interpolation). To stay
// safely under SQLite's bound-variable limit (SQLITE_MAX_VARIABLE_NUMBER,
// default 999 on builds that haven't been recompiled), the plugin caps
// the list length at sqliteMaxVariableNumber − countByStateBaseParams
// and rejects oversized lists via ErrStateFilterTooLarge — see the
// derivation tests in count_by_state_in_clause_test.go.

// TestCountByState_RejectsTooManyStates asserts the plugin rejects state
// lists above the cap with ErrStateFilterTooLarge. Without the cap, the
// plugin would forward the request to SQLite and fail with a cryptic
// "too many SQL variables" error — or, on builds with a higher limit,
// silently execute an unreasonably wide query.
func TestCountByState_RejectsTooManyStates(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	// Well above sqlite.MaxStateFilterSize.
	states := make([]string, sqlite.MaxStateFilterSize+1)
	for i := range states {
		states[i] = fmt.Sprintf("STATE_%d", i)
	}

	_, err = store.CountByState(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, states, nil)
	if err == nil {
		t.Fatalf("CountByState with %d states: expected error, got nil", len(states))
	}
	if !errors.Is(err, sqlite.ErrStateFilterTooLarge) {
		t.Fatalf("CountByState err = %v; want wraps ErrStateFilterTooLarge", err)
	}
}

// TestCountByState_AcceptsAtCap confirms the cap itself is inclusive and
// pins that state lists at the limit still execute successfully.
func TestCountByState_AcceptsAtCap(t *testing.T) {
	factory, ctx := setupSearcherTest(t)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	states := make([]string, sqlite.MaxStateFilterSize)
	for i := range states {
		states[i] = fmt.Sprintf("STATE_%d", i)
	}

	_, err = store.CountByState(ctx, spi.ModelRef{EntityName: "person", ModelVersion: "1"}, states, nil)
	if err != nil {
		t.Fatalf("CountByState with %d states (at cap): got err %v; expected success", len(states), err)
	}
}
