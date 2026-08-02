package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestCreate_BatchArrayAccepted asserts the create endpoint accepts a JSON
// array of entity objects (batch form documented by E4).
func TestCreate_BatchArrayAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-create-batch"
	importModel(t, model, 1)
	body := `[{"x":1},{"x":2}]`
	resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch create: expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

// TestCreateCollection_RejectsNonArray asserts the collection endpoint rejects
// a non-array body (documented as an array of {model,payload}).
func TestCreateCollection_RejectsNonArray(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuth(t, http.MethodPost, "/api/entity/JSON", `{"not":"an array"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-array collection: expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

// TestCreate_MalformedJSON_400 asserts a syntactically invalid body is
// rejected at the boundary with an RFC 9457 400 — not persisted, not a 500.
func TestCreate_MalformedJSON_400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-create-malformed"
	importModel(t, model, 1)

	bodies := map[string]string{
		"truncated-object": `{"x":1`,
		"not-json":         `this is not json`,
		"trailing-garbage": `{"x":1}}}`,
		"bare-empty":       ``,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), body)
			respBody := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("malformed create: expected 400, got %d: %s", resp.StatusCode, respBody)
			}
			assertErrorCode(t, respBody, "BAD_REQUEST")
		})
	}

	// Nothing may have been persisted by any of the rejected requests.
	if count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model); count != 0 {
		t.Errorf("expected 0 entities after malformed creates, got %d", count)
	}
}
