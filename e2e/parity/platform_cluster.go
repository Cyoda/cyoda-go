package parity

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunPlatformClusterStatus: the platform operator reads the cluster's status
// through GET /platform/cluster and a tenant admin is refused. The fixture may
// run one node or several, so the scenario asserts no node id and no count of
// nodes, only what holds for any healthy cluster on any backend.
func RunPlatformClusterStatus(t *testing.T, fixture BackendFixture) {
	operator := client.NewClient(fixture.BaseURL(), fixture.PlatformOperator(t).Token)
	status, raw, err := operator.DoRaw(t, http.MethodGet, "/api/platform/cluster", "")
	if err != nil {
		t.Fatalf("GET /platform/cluster: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET /platform/cluster as the platform operator: %d %s; want 200", status, raw)
	}
	var body struct {
		Operational bool `json:"operational"`
		Counts      *[]struct {
			Reachability string `json:"reachability"`
			State        string `json:"state"`
			Count        int    `json:"count"`
		} `json:"counts"`
		Nodes *[]struct {
			ID           string `json:"id"`
			Reachability string `json:"reachability"`
			State        string `json:"state"`
			Database     bool   `json:"database"`
			Version      string `json:"version"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode cluster status %s: %v", raw, err)
	}
	if !body.Operational {
		t.Errorf("operational = false; want true: %s", raw)
	}
	if body.Counts == nil || body.Nodes == nil {
		t.Fatalf("counts or nodes is null: %s", raw)
	}
	nodes := *body.Nodes
	if len(nodes) == 0 {
		t.Fatalf("nodes is empty: %s", raw)
	}
	for _, n := range nodes {
		if n.Reachability != "UP" || n.State != "NORMAL" || !n.Database {
			t.Errorf("node %s = %s/%s database=%v; want UP/NORMAL with the database reachable", n.ID, n.Reachability, n.State, n.Database)
		}
		if n.Version == "" || n.Version != nodes[0].Version {
			t.Errorf("node %s version %q; want the same non-empty version as node %s (%q)", n.ID, n.Version, nodes[0].ID, nodes[0].Version)
		}
	}
	total := 0
	for _, c := range *body.Counts {
		total += c.Count
	}
	if total != len(nodes) {
		t.Errorf("counts sum to %d; want %d, the number of nodes: %s", total, len(nodes), raw)
	}

	tenant := client.NewClient(fixture.BaseURL(), fixture.NewTenant(t).Token)
	status, raw, err = tenant.DoRaw(t, http.MethodGet, "/api/platform/cluster", "")
	if err != nil {
		t.Fatalf("GET /platform/cluster as a tenant admin: %v", err)
	}
	if status != http.StatusForbidden || !containsErrorCode(raw, "FORBIDDEN") {
		t.Errorf("GET /platform/cluster as a tenant admin: %d %s; want 403 FORBIDDEN", status, raw)
	}
}
