package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help"
)

// TestHTTP_ConfigAll_JSON verifies GET /help/config/all over the real
// HTTP help-route stack: 200 + application/json with a non-empty vars
// list for GET, and 405 for non-GET.
//
// This in-process test does not blank-import the storage plugins, so
// spi.RegisteredPlugins() returns only whatever the internal/api test
// binary happens to register — likely just root vars. Assert only
// len(vars) > 0 and the presence of a root var (CYODA_HTTP_PORT); do
// not assert plugin-specific names here (covered by the help-package
// tests: TestBuildConfigRegistry, TestConfigAll_Complete).
func TestHTTP_ConfigAll_JSON(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHelpRoutes(mux, help.DefaultTree, "", "v0.0.0")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/help/config/all")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type=%q", ct)
	}
	var env struct {
		Vars []map[string]any `json:"vars"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || len(env.Vars) == 0 {
		t.Fatalf("decode/vars: %v len=%d", err, len(env.Vars))
	}
	found := false
	for _, v := range env.Vars {
		if v["name"] == "CYODA_HTTP_PORT" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected root var CYODA_HTTP_PORT in vars: %v", env.Vars)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/help/config/all", nil)
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d want 405", pr.StatusCode)
	}
}
