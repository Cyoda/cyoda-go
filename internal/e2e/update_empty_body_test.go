package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestUpdateSingle_EmptyBody_400: PUT replaces the entity's data, so it needs
// a JSON payload. An empty body — on the loopback route and on a named
// transition — is refused with 400 BAD_REQUEST and nothing is committed: the
// entity keeps its transaction id and its state. A client that wants to fire a
// transition, or a loopback, without changing the data sends PATCH with `{}`.
func TestUpdateSingle_EmptyBody_400(t *testing.T) {
	model := "e2e-put-empty-body-" + randSuffix(t)
	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "put-empty-wf", "initialState": "CREATED", "active": true,
			"states": {
				"CREATED": {"transitions": [{"name": "go", "next": "DONE", "manual": true}]},
				"DONE": {}
			}
		}]
	}`)
	id := createEntityE2E(t, model, 1, `{"name":"A","amount":1,"status":"new"}`)
	before := getEntityTxID(t, id)

	for _, path := range []string{
		"/api/entity/JSON/" + id,         // loopback
		"/api/entity/JSON/" + id + "/go", // named transition
	} {
		resp := doAuth(t, http.MethodPut, path, "")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "BAD_REQUEST") {
			t.Errorf("PUT %s with an empty body: got %d, want 400 BAD_REQUEST: %s", path, resp.StatusCode, body)
		}
	}

	if after := getEntityTxID(t, id); after != before {
		t.Errorf("the entity was written (transactionId %s -> %s); want nothing committed", before, after)
	}
	if state := getEntityState(t, id); state != "CREATED" {
		t.Errorf("state = %q, want CREATED: the transition must not fire", state)
	}
}
