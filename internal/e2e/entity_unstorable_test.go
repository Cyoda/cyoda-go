package e2e_test

// Coverage for the remaining members of the "valid JSON that no backend can
// store" family, and for the empty-document round-trip.
//
// An unpaired UTF-16 surrogate escape and a byte sequence that is not valid
// UTF-8 are both accepted by Go's JSON parser and both rejected by PostgreSQL
// text/jsonb, so they used to reach the store and come back as 500 with a
// support ticket. They are client input errors: 400.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func setupUnstorableModel(t *testing.T, model string) {
	t.Helper()
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "unstorable-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)
}

// TestEntity_UnstorableStrings_400 asserts every write path rejects an unpaired
// surrogate and invalid UTF-8 with 400 rather than failing in the store.
//
// As in the NUL test, the collection bodies use %q because those endpoints take
// the payload as a JSON-encoded string; %s would be rejected by the outer array
// parser and never reach the guard.
func TestEntity_UnstorableStrings_400(t *testing.T) {
	const model = "e2e-unstorable-str"
	setupUnstorableModel(t, model)

	entityID := createEntityE2E(t, model, 1, `{"name":"ok","amount":1,"status":"new"}`)

	payloads := map[string]string{
		"lone-high-surrogate": "{\"name\":\"a\\ud800b\",\"amount\":1,\"status\":\"new\"}",
		"lone-low-surrogate":  "{\"name\":\"a\\udc00b\",\"amount\":1,\"status\":\"new\"}",
		"invalid-utf8":        "{\"name\":\"a\xffb\",\"amount\":1,\"status\":\"new\"}",
	}

	for name, payload := range payloads {
		writes := []struct {
			path   string
			method string
			path2  string
			body   string
		}{
			{path: "create", method: http.MethodPost, path2: fmt.Sprintf("/api/entity/JSON/%s/1", model), body: payload},
			{path: "create-batch-array", method: http.MethodPost, path2: fmt.Sprintf("/api/entity/JSON/%s/1", model), body: "[" + payload + "]"},
			{path: "create-collection", method: http.MethodPost, path2: "/api/entity/JSON",
				body: fmt.Sprintf(`[{"model":{"name":%q,"version":1},"payload":%q}]`, model, payload)},
			{path: "update", method: http.MethodPut, path2: fmt.Sprintf("/api/entity/JSON/%s", entityID), body: payload},
			{path: "update-collection", method: http.MethodPut, path2: "/api/entity/JSON",
				body: fmt.Sprintf(`[{"id":%q,"payload":%q}]`, entityID, payload)},
		}
		for _, w := range writes {
			t.Run(name+"/"+w.path, func(t *testing.T) {
				resp := doAuth(t, w.method, w.path2, w.body)
				body := readBody(t, resp)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status=%d, want 400 (unstorable text is a client error, not a storage failure); body: %s",
						resp.StatusCode, body)
				}
				assertErrorCode(t, body, "BAD_REQUEST")
			})
		}
	}

	// Nothing may have been persisted, and the clean entity must be untouched.
	if count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model); count != 1 {
		t.Errorf("expected exactly the 1 clean entity after the rejected writes, got %d", count)
	}
	if data := getEntityData(t, entityID, ""); data["name"] != "ok" {
		t.Errorf("existing entity modified by a rejected write: name=%v", data["name"])
	}
}

// TestEntity_StorableTextStillAccepted guards the rejection from over-reaching.
// A correctly paired surrogate is ordinary text, and so is a client legitimately
// sending U+FFFD.
func TestEntity_StorableTextStillAccepted(t *testing.T) {
	const model = "e2e-unstorable-ok"
	setupUnstorableModel(t, model)

	bodies := map[string]string{
		"valid-surrogate-pair":   "{\"name\":\"a\\ud83d\\ude00b\",\"amount\":1,\"status\":\"new\"}",
		"literal-emoji":          "{\"name\":\"a😀b\",\"amount\":1,\"status\":\"new\"}",
		"client-sent-U+FFFD":     "{\"name\":\"a\\ufffdb\",\"amount\":1,\"status\":\"new\"}",
		"backslash-then-ud800":   "{\"name\":\"a\\\\ud800b\",\"amount\":1,\"status\":\"new\"}",
		"accented-latin":         "{\"name\":\"caf\\u00e9\",\"amount\":1,\"status\":\"new\"}",
		"tab-newline-and-quotes": "{\"name\":\"a\\tb\\n\\\"c\\\"\",\"amount\":1,\"status\":\"new\"}",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), body)
			respBody := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200 — this text is storable; body: %s", resp.StatusCode, respBody)
			}
		})
	}
}

// TestEntity_EmptyDocument_RoundTrips covers the defect where an empty payload
// wrote successfully and then made itself — and every list of its model —
// permanently unreadable.
//
// postgres merges _meta into the domain data, so `{}` was stored as
// `{"_meta":...}`; on read, _meta was removed and nothing remained, so the
// entity's Data came back empty. Decoding an empty slice is io.EOF, which
// surfaced as 500 SERVER_ERROR. memory and sqlite always round-tripped `{}`.
func TestEntity_EmptyDocument_RoundTrips(t *testing.T) {
	const model = "e2e-empty-doc"
	setupUnstorableModel(t, model)

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), `{}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create {}: status=%d, want 200; body: %s", resp.StatusCode, body)
	}
	var created []map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil || len(created) == 0 {
		t.Fatalf("create {}: unparseable response: %s", body)
	}
	ids, _ := created[0]["entityIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("create {}: no entityIds: %s", body)
	}
	emptyID, _ := ids[0].(string)

	// The entity itself must be readable.
	resp = doAuth(t, http.MethodGet, "/api/entity/"+emptyID, "")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the empty entity: status=%d, want 200; body: %s", resp.StatusCode, body)
	}

	// And so must the whole model's list — this is what a single empty document
	// used to take down.
	resp = doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LIST the model containing an empty document: status=%d, want 200; body: %s",
			resp.StatusCode, body)
	}
}

// TestEntity_EmptyDocumentUpdate_DoesNotBrickEntity covers the more damaging
// face of the same defect: updating a healthy, readable entity to `{}` made
// that entity permanently unreadable.
func TestEntity_EmptyDocumentUpdate_DoesNotBrickEntity(t *testing.T) {
	const model = "e2e-empty-doc-update"
	setupUnstorableModel(t, model)

	entityID := createEntityE2E(t, model, 1, `{"name":"ok","amount":1,"status":"new"}`)

	resp := doAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", entityID), `{}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT {}: status=%d, want 200; body: %s", resp.StatusCode, body)
	}

	resp = doAuth(t, http.MethodGet, "/api/entity/"+entityID, "")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after updating to {}: status=%d, want 200 — the entity must not become unreadable; body: %s",
			resp.StatusCode, body)
	}

	resp = doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "")
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LIST after updating an entity to {}: status=%d, want 200; body: %s", resp.StatusCode, body)
	}
}
