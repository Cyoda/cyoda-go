package e2e_test

// A payload carrying U+0000 is valid JSON and passes schema validation, but
// PostgreSQL text/jsonb cannot store it. It used to reach the store and fail
// there, returning 500 SERVER_ERROR with a support ticket for what is a client
// input error. Every write path must now reject it at the boundary with 400.

import (
	"fmt"
	"net/http"
	"testing"
)

// nulPayload is a JSON body whose "name" string carries a NUL (U+0000) escape.
const nulPayload = "{\"name\":\"a\\u0000b\",\"amount\":1,\"status\":\"new\"}"

func setupNulModel(t *testing.T, model string) {
	t.Helper()
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "nul-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)
}

// TestEntity_NulInPayload_400 covers every write path that accepts an entity
// payload: single create, batch-array create, collection create, update, and
// collection update.
//
// The two collection bodies use %q, not %s, and that is load-bearing: the
// collection endpoints declare Payload as a string (a JSON-ENCODED document,
// see internal/domain/entity/handler.go). Embedding a raw object instead makes
// the outer array parse fail with "invalid JSON array" — still a 400, so the
// assertion passes, but from the array parser rather than from the guard under
// test. These two subtests shipped that way and were vacuous.
func TestEntity_NulInPayload_400(t *testing.T) {
	const model = "e2e-nul-payload"
	setupNulModel(t, model)

	// A clean entity to aim the update paths at.
	entityID := createEntityE2E(t, model, 1, `{"name":"ok","amount":1,"status":"new"}`)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name: "create", method: http.MethodPost,
			path: fmt.Sprintf("/api/entity/JSON/%s/1", model),
			body: nulPayload,
		},
		{
			name: "create-batch-array", method: http.MethodPost,
			path: fmt.Sprintf("/api/entity/JSON/%s/1", model),
			body: "[" + nulPayload + "]",
		},
		{
			name: "create-collection", method: http.MethodPost,
			path: "/api/entity/JSON",
			body: fmt.Sprintf(`[{"model":{"name":%q,"version":1},"payload":%q}]`, model, nulPayload),
		},
		{
			name: "update", method: http.MethodPut,
			path: fmt.Sprintf("/api/entity/JSON/%s", entityID),
			body: nulPayload,
		},
		{
			name: "update-collection", method: http.MethodPut,
			path: "/api/entity/JSON",
			body: fmt.Sprintf(`[{"id":%q,"payload":%q}]`, entityID, nulPayload),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuth(t, tc.method, tc.path, tc.body)
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (a NUL in the payload is a client error, not a storage failure); body: %s",
					resp.StatusCode, body)
			}
			assertErrorCode(t, body, "BAD_REQUEST")
		})
	}

	// The rejected writes must have left nothing behind, and the pre-existing
	// entity must be untouched.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model)
	if count != 1 {
		t.Errorf("expected exactly the 1 clean entity after the rejected writes, got %d", count)
	}
	if data := getEntityData(t, entityID, ""); data["name"] != "ok" {
		t.Errorf("existing entity was modified by a rejected write: name=%v, want \"ok\"", data["name"])
	}
}

// TestEntity_NulAdjacentPayloadsStillAccepted guards the rejection from being
// over-broad: other control characters and text that merely looks like an
// escape are legitimate payload content.
func TestEntity_NulAdjacentPayloadsStillAccepted(t *testing.T) {
	const model = "e2e-nul-adjacent"
	setupNulModel(t, model)

	bodies := map[string]string{
		"tab-and-newline":   "{\"name\":\"a\\tb\\nc\",\"amount\":1,\"status\":\"new\"}",
		"literal-backslash": "{\"name\":\"a\\\\u0000b\",\"amount\":1,\"status\":\"new\"}",
		"unicode-escape":    "{\"name\":\"a\\u00e9b\",\"amount\":1,\"status\":\"new\"}",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), body)
			respBody := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200 — this payload is storable; body: %s", resp.StatusCode, respBody)
			}
		})
	}
}
