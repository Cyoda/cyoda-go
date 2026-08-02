package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// execDB runs a statement against the test database as the given tenant and
// returns the number of rows affected. Test-only sibling of queryDB.
func execDB(t *testing.T, tenantID, sql string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := dbPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	// NOTE: test-only — tenantID is a hardcoded constant, not user input. Do not use this pattern in production code.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.current_tenant = '%s'", tenantID)); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return tag.RowsAffected()
}

// TestGetTransitions_UnaffectedByDatabaseClockAhead pins that a transitions
// read with NO pointInTime returns the entity's CURRENT state, independent of
// how the database's clock compares to this process's.
//
// The postgres backend stamps entity_versions.valid_time from the database
// (`SELECT CURRENT_TIMESTAMP`), while the handler ran in the cyoda-go process.
// Defaulting the absent pointInTime to the process's time.Now() and issuing a
// historical GetAsAt therefore compared two clocks: whenever the database's
// clock ran ahead of the app's, a just-written version had
// valid_time > now and the read found nothing — answering 404 for an entity
// that demonstrably exists. That is a read-your-own-write violation and a
// wrong-but-available answer, which `.claude/rules/correctness-over-availability.md`
// forbids. A request that asks for "now" wants the current version, not a
// point-in-time query about it.
//
// The skew is simulated deterministically by pushing the stored valid_time
// into the future, which is exactly what an ahead-of-app database clock does.
func TestGetTransitions_UnaffectedByDatabaseClockAhead(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-transitions-clockskew"
	wf := `{ "importMode": "REPLACE", "workflows": [{
		"version": "1.1", "name": "skew-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			"CREATED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true}]},
			"APPROVED": {}
		}}]}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Bob","status":"draft"}`)

	// Sanity: with the clocks agreeing, the transition is listed.
	if got := getTransitionNames(t, entityID); len(got) != 1 || got[0] != "approve" {
		t.Fatalf("precondition: transitions = %v, want [approve]", got)
	}

	// Simulate a database clock running one hour ahead of this process.
	if n := execDB(t, "test-tenant",
		`UPDATE entity_versions SET valid_time = valid_time + interval '1 hour'
		 WHERE tenant_id = $1 AND entity_id = $2`, "test-tenant", entityID); n == 0 {
		t.Fatalf("expected to shift at least one entity_versions row for %s", entityID)
	}

	// The entity still exists and is still in CREATED, so the transitions read
	// must still answer "approve" — it must not become a 404.
	if got := getTransitionNames(t, entityID); len(got) != 1 || got[0] != "approve" {
		t.Errorf("transitions with a DB clock ahead of the app = %v, want [approve] — "+
			"an absent pointInTime must read the current version, not GetAsAt(process clock)", got)
	}
}

// getTransitionNames issues GET /api/entity/{id}/transitions with no
// pointInTime and returns the transition names, failing on any non-200.
func getTransitionNames(t *testing.T, entityID string) []string {
	t.Helper()
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/transitions", entityID), "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET transitions: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var names []string
	if err := json.Unmarshal([]byte(body), &names); err != nil {
		t.Fatalf("decode transitions: %v: %s", err, body)
	}
	return names
}
