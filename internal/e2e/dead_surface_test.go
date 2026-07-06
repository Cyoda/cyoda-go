package e2e_test

import (
	"net/http"
	"testing"
)

// TestDeadSurfaceUnrouted asserts that operations published in the OpenAPI
// contract under excluded tags ("SQL-Schema" and "Stream Data") are not routed
// by the server. Each must return 404. This is a characterization test: it
// passes immediately because the ops are unrouted, and must stay green through
// every future edit — catching any accidental routing of a dead-surface op
// before the x-cyoda-status marker is removed.
func TestDeadSurfaceUnrouted(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"sql-schema listAll", http.MethodGet, "/api/sql/schema/listAll"},
		{"stream-data config list", http.MethodGet, "/api/platform-api/stream-data/config/list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doAuth(t, tc.method, tc.path, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s: got %d, want 404 (op must remain unrouted)", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}
