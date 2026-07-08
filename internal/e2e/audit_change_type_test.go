package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAuditEntityChangeType_CanonicalSpelling asserts that GET /api/audit/entity/{id}
// returns canonical present-tense changeType values (CREATE/UPDATE/DELETE), not the
// internal storage spelling (CREATED/UPDATED/DELETED). This is the audit-endpoint
// half of E8 — the entity/changes endpoint half is in entity_change_type_test.go.
func TestAuditEntityChangeType_CanonicalSpelling(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-audit-changetype"
	importModel(t, model, 1)
	id := createEntityE2E(t, model, 1, `{"x":1}`)

	resp := doAuth(t, http.MethodGet, "/api/audit/entity/"+id, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/audit/entity/%s: expected 200, got %d: %s", id, resp.StatusCode, body)
	}

	// The audit endpoint returns a paginated envelope: {"items":[...],"pagination":{...}}.
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode audit response: %v: %s", err, body)
	}
	events := envelope.Items

	// Find the EntityChange event for the create operation.
	var found bool
	for _, ev := range events {
		if ev["auditEventType"] != "EntityChange" {
			continue
		}
		ct, _ := ev["changeType"].(string)
		if ct != "CREATE" {
			t.Errorf("audit EntityChange changeType = %q, want CREATE (canonical)", ct)
		}
		found = true
		break
	}
	if !found {
		t.Errorf("no EntityChange event found in audit response for entity %s: %s", id, body)
	}
}
