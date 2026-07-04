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
