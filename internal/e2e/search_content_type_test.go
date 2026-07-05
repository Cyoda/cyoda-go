package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestSearchEntities_ContentTypeIsNdjson verifies that a valid synchronous
// entity search returns Content-Type: application/x-ndjson. The spec now
// documents x-ndjson as the only response content type for the 200 case;
// this test pins that server behaviour before the spec edit so regressions
// are caught immediately.
func TestSearchEntities_ContentTypeIsNdjson(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-ct-ndjson"
	setupSearchModel(t, model)

	path := fmt.Sprintf("/api/search/direct/%s/1", model)
	resp := doAuth(t, http.MethodPost, path, `{"type":"group","operator":"AND","conditions":[]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
}

// TestSearchEntities_LimitOver10000_Returns400 verifies that a synchronous
// entity search with limit > MaxPageSize (10000) is rejected with HTTP 400.
// The spec documents this as an explicit rejection (not silent clamping).
func TestSearchEntities_LimitOver10000_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-ct-limit"
	setupSearchModel(t, model)

	path := fmt.Sprintf("/api/search/direct/%s/1?limit=10001", model)
	resp := doAuth(t, http.MethodPost, path, `{"type":"group","operator":"AND","conditions":[]}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
	}
}
