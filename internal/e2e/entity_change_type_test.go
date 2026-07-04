package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestGetEntityChanges_CanonicalChangeType asserts that GET /api/entity/{id}/changes
// returns the canonical present-tense changeType spelling (CREATE/UPDATE/DELETE),
// not the internal past-tense spelling (CREATED/UPDATED/DELETED).
// The create record is the last element because the server returns newest-first.
func TestGetEntityChanges_CanonicalChangeType(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-changetype"
	importModel(t, model, 1)
	id := createEntityE2E(t, model, 1, `{"x":1}`)
	resp := doAuth(t, http.MethodGet, "/api/entity/"+id+"/changes", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntityChangesMetadata: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var changes []map[string]any
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(changes) < 1 {
		t.Fatalf("expected >=1 change")
	}
	// The create record must use the canonical present-tense spelling.
	// Newest-first order: the last element is the oldest (create) entry.
	last := changes[len(changes)-1]
	if last["changeType"] != "CREATE" {
		t.Errorf("changeType = %v, want CREATE (canonical)", last["changeType"])
	}
}
