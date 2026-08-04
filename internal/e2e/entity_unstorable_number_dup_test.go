package e2e_test

// Coverage for the last two members of the "valid JSON that cannot be stored
// consistently" family: numbers outside PostgreSQL's numeric range, and
// duplicate object names.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// setupUnboundModel imports a model whose "big" field infers UNBOUND_INTEGER
// and "dec" infers UNBOUND_DECIMAL, then locks it and attaches a trivial
// workflow. The sample matters: on a plain INTEGER field an over-range literal
// is rejected earlier with 400 INCOMPATIBLE_TYPE and never reaches the store,
// so a test built on the standard sample would pass for the wrong reason.
func setupUnboundModel(t *testing.T, model string) {
	t.Helper()
	resp := doAuth(t, http.MethodPost,
		fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model),
		`{"name":"x","big":1e400,"dec":1e-400,"status":"d"}`)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import unbound model: %d %s", resp.StatusCode, body)
	}
	resp = doAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), "")
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock unbound model: %d %s", resp.StatusCode, body)
	}
	resp = doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), `{
		"importMode":"REPLACE",
		"workflows":[{"version":"1.1","name":"unbound-wf","initialState":"NONE","active":true,
			"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},"CREATED":{}}}]}`)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow: %d %s", resp.StatusCode, body)
	}
}

// TestEntity_NumberOutOfRange_400 asserts a literal PostgreSQL numeric cannot
// hold is rejected at the boundary rather than failing inside the store.
func TestEntity_NumberOutOfRange_400(t *testing.T) {
	const model = "e2e-num-range"
	setupUnboundModel(t, model)
	path := fmt.Sprintf("/api/entity/JSON/%s/1", model)

	rejected := map[string]string{
		"weight-just-over":   `{"name":"x","big":1e131072,"dec":1,"status":"d"}`,
		"weight-far-over":    `{"name":"x","big":1e1000000,"dec":1,"status":"d"}`,
		"weight-via-digits":  `{"name":"x","big":12e131071,"dec":1,"status":"d"}`,
		"scale-just-over":    `{"name":"x","big":1,"dec":1e-16384,"status":"d"}`,
		"scale-via-fraction": `{"name":"x","big":1,"dec":1.5e-16383,"status":"d"}`,
	}
	for name, body := range rejected {
		t.Run("rejected/"+name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, path, body)
			respBody := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (a number no backend can store is a client error); body: %s",
					resp.StatusCode, respBody)
			}
			assertErrorCode(t, respBody, "BAD_REQUEST")
		})
	}

	// The guard must stop exactly at the limit, not before it.
	accepted := map[string]string{
		"weight-at-limit":     `{"name":"x","big":1e131071,"dec":1,"status":"d"}`,
		"scale-at-limit":      `{"name":"x","big":1,"dec":1e-16383,"status":"d"}`,
		"leading-zeros":       `{"name":"x","big":0.0001e131075,"dec":1,"status":"d"}`,
		"zero-huge-exponent":  `{"name":"x","big":0e999999999,"dec":1,"status":"d"}`,
		"high-precision":      `{"name":"x","big":123456789012345678901234567890,"dec":1,"status":"d"}`,
		"ordinary-scientific": `{"name":"x","big":1e400,"dec":1.5e-400,"status":"d"}`,
	}
	for name, body := range accepted {
		t.Run("accepted/"+name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, path, body)
			respBody := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200 — this number is storable; body: %s", resp.StatusCode, respBody)
			}
		})
	}
}

// TestEntity_DuplicateKeys_400 asserts duplicate object names are rejected on
// every write path.
//
// Left accepted, a duplicated name is read as the LAST occurrence by schema
// validation and the GET response, and as the FIRST by the criterion evaluator
// and search — so an entity can be reported as holding one value while its
// workflow transition was decided on another. Measured before this fix: a
// criterion `amount == 5` did not fire for an entity the API reports as
// amount=5, leaving it in the wrong state with nothing logged.
func TestEntity_DuplicateKeys_400(t *testing.T) {
	const model = "e2e-dup-keys"
	setupUnstorableModel(t, model)

	entityID := createEntityE2E(t, model, 1, `{"name":"ok","amount":1,"status":"new"}`)
	dup := `{"name":"x","amount":"not-a-number","amount":5,"status":"new"}`

	writes := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create", http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), dup},
		{"create-batch-array", http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), "[" + dup + "]"},
		{"create-collection", http.MethodPost, "/api/entity/JSON",
			fmt.Sprintf(`[{"model":{"name":%q,"version":1},"payload":%q}]`, model, dup)},
		{"update", http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", entityID), dup},
		{"update-collection", http.MethodPut, "/api/entity/JSON",
			fmt.Sprintf(`[{"id":%q,"payload":%q}]`, entityID, dup)},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			resp := doAuth(t, w.method, w.path, w.body)
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 — a duplicated name is read differently by different subsystems; body: %s",
					resp.StatusCode, body)
			}
			assertErrorCode(t, body, "BAD_REQUEST")
		})
	}

}

// TestEntity_RepeatedNamesInDifferentObjectsAccepted guards the duplicate-key
// rejection from over-reaching. A name repeated in sibling objects, across
// array elements, or at different depths is perfectly ordinary JSON — only a
// name repeated within ONE object is ambiguous.
//
// This needs its own model whose sample declares the nested shapes; on a model
// that does not declare them the write is rejected by schema validation before
// the guard is reached, which would make the test pass for the wrong reason.
func TestEntity_RepeatedNamesInDifferentObjectsAccepted(t *testing.T) {
	const model = "e2e-dup-keys-ok"
	resp := doAuth(t, http.MethodPost,
		fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", model),
		`{"name":"x","a":{"k":1},"b":{"k":2},"xs":[{"k":1}],"k":{"k":1}}`)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	resp = doAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", model), "")
	readBody(t, resp)
	resp = doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), `{
		"importMode":"REPLACE",
		"workflows":[{"version":"1.1","name":"dupok-wf","initialState":"NONE","active":true,
			"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},"CREATED":{}}}]}`)
	readBody(t, resp)

	ok := map[string]string{
		"sibling-objects":  `{"name":"x","a":{"k":1},"b":{"k":2},"xs":[{"k":1}],"k":{"k":1}}`,
		"array-elements":   `{"name":"x","a":{"k":1},"b":{"k":1},"xs":[{"k":1},{"k":2}],"k":{"k":1}}`,
		"different-depths": `{"name":"x","a":{"k":9},"b":{"k":9},"xs":[{"k":9}],"k":{"k":9}}`,
	}
	for name, body := range ok {
		t.Run(name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), body)
			respBody := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200 — no name is repeated within one object; body: %s",
					resp.StatusCode, respBody)
			}
		})
	}
}

// TestEntity_DuplicateKeys_NoEntityLeftBehind confirms the rejected writes
// persisted nothing, so the corruption path is closed rather than merely
// reported.
func TestEntity_DuplicateKeys_NoEntityLeftBehind(t *testing.T) {
	const model = "e2e-dup-keys-clean"
	setupUnstorableModel(t, model)

	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model),
		`{"name":"x","amount":"not-a-number","amount":5,"status":"new"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate-key create: status=%d, want 400; body: %s", resp.StatusCode, body)
	}

	resp = doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s/1", model), "")
	listBody := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status=%d; body: %s", resp.StatusCode, listBody)
	}
	var entities []map[string]any
	if err := json.Unmarshal([]byte(listBody), &entities); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("expected no entities after the rejected write, got %d", len(entities))
	}
}
