package memory_test

import (
	"context"
	"strings"
	"testing"
)

// TestGetResultIDs_PaginationCheckedBeforeTenantResolution: sqlite and postgres
// both reject invalid pagination before they resolve the tenant; memory
// resolved the tenant first. With both inputs bad — the realistic shape of a
// request that arrives without a resolvable user context — the same call
// reported two different errors depending on the backend.
//
// Pagination first is the right order regardless: offset/limit are pure
// argument validation, decidable without touching any state.
func TestGetResultIDs_PaginationCheckedBeforeTenantResolution(t *testing.T) {
	store := newSearchStore(t)

	// No user context at all AND invalid pagination.
	_, _, err := store.GetResultIDs(context.Background(), "job-x", -1, 0)
	if err == nil {
		t.Fatal("GetResultIDs with no user context and invalid pagination returned no error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "offset") && !strings.Contains(msg, "limit") {
		t.Errorf("GetResultIDs reported %q — want the pagination error sqlite and postgres report, "+
			"not the tenant-resolution one", msg)
	}
}
