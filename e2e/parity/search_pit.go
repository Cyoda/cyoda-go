package parity

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunSearchPointInTime asserts that a predicate search at a snapshot returns
// the snapshot-correct result set on every backend — exercising the Searcher
// (or in-memory fallback) through a point-in-time read. It is the cross-engine
// guard for predicate-pushdown ≡ in-memory at a PIT, and the parity counterpart
// to the postgres B1 projection unit test.
func RunSearchPointInTime(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-pit"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// v1: status=active.
	id, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	t1 := pitbLatestChangeTime(t, c, id)

	// Space ≥1ms so v2 lands in a distinct millisecond (commercial backend
	// stores ms precision — see pit_boundary.go rationale).
	time.Sleep(2 * time.Millisecond)
	if err := c.UpdateEntityData(t, id, `{"name":"Alice","status":"inactive"}`); err != nil {
		t.Fatalf("UpdateEntityData: %v", err)
	}
	t2 := pitbLatestChangeTime(t, c, id)
	if !t1.Before(t2) {
		t.Fatalf("version timestamps not increasing: t1=%s t2=%s",
			t1.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano))
	}

	activeCond := `{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"}`

	// At t1 the snapshot shows status=active → 1 hit.
	at1, err := c.SyncSearchAt(t, modelName, modelVersion, activeCond, t1)
	if err != nil {
		t.Fatalf("SyncSearchAt(t1): %v", err)
	}
	if len(at1) != 1 {
		t.Errorf("status=active @t1: want 1, got %d", len(at1))
	}

	// At t2 the snapshot shows status=inactive → the active search misses.
	at2, err := c.SyncSearchAt(t, modelName, modelVersion, activeCond, t2)
	if err != nil {
		t.Fatalf("SyncSearchAt(t2): %v", err)
	}
	if len(at2) != 0 {
		t.Errorf("status=active @t2: want 0 (now inactive), got %d", len(at2))
	}
}
