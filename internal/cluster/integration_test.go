package cluster_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/lifecycle"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestEndToEnd_ProxyRouting(t *testing.T) {
	secret := []byte("integration-test-secret-32bytes!!")
	signer, _ := token.NewSigner(secret)

	// Create "Node A" HTTP server
	nodeAHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"served_by": "node-a",
			"path":      r.URL.Path,
		})
	})
	nodeA := httptest.NewServer(nodeAHandler)
	defer nodeA.Close()

	// Registry that knows both nodes — use the full URL (with scheme) so the
	// reverse proxy can parse the target address correctly.
	reg := &testRegistry{nodes: map[string]string{
		"node-a": nodeA.URL,
		"node-b": "http://localhost:0",
	}}

	// Create "Node B" with routing middleware
	nodeBLocal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"served_by": "node-b-local"})
	})
	nodeBHandler := proxy.HTTPRouting(signer, reg, "node-b", 5*time.Second, true)(nodeBLocal)
	nodeB := httptest.NewServer(nodeBHandler)
	defer nodeB.Close()

	// Issue a token for node-a
	tok, err := signer.Issue("node-a", "tx-123", time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Send request to Node B with node-a's token
	req, _ := http.NewRequest("GET", nodeB.URL+"/api/entity/456", nil)
	req.Header.Set("X-Tx-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	if result["served_by"] != "node-a" {
		t.Errorf("served_by = %q, want %q", result["served_by"], "node-a")
	}
	if result["path"] != "/api/entity/456" {
		t.Errorf("path = %q, want %q", result["path"], "/api/entity/456")
	}
}

func TestEndToEnd_NoToken_ServesLocally(t *testing.T) {
	secret := []byte("integration-test-secret-32bytes!!")
	signer, _ := token.NewSigner(secret)
	reg := registry.NewLocal("node-b", "localhost:0")

	localHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"served_by": "node-b-local"})
	})
	handler := proxy.HTTPRouting(signer, reg, "node-b", 5*time.Second, true)(localHandler)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/entity/789")
	if err != nil {
		t.Fatalf("HTTP request: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	if result["served_by"] != "node-b-local" {
		t.Errorf("served_by = %q, want %q", result["served_by"], "node-b-local")
	}
}

func TestEndToEnd_LifecycleTracking(t *testing.T) {
	mgr := lifecycle.NewManager(5 * time.Minute)
	ctx := context.Background()

	mgr.Register(ctx, "tx-1", "node-a", 100*time.Millisecond)

	alive, nodeID, _ := mgr.IsAlive(ctx, "tx-1")
	if !alive || nodeID != "node-a" {
		t.Fatalf("expected alive on node-a, got alive=%v nodeID=%q", alive, nodeID)
	}

	time.Sleep(150 * time.Millisecond)

	reaped, _ := mgr.ReapExpired(ctx)
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
	}

	outcome, found := mgr.GetOutcome(ctx, "tx-1")
	if !found {
		t.Fatal("expected outcome to be recorded")
	}
	if outcome != lifecycle.OutcomeRolledBack {
		t.Errorf("outcome = %v, want RolledBack", outcome)
	}
}

type testRegistry struct {
	nodes map[string]string
}

func (r *testRegistry) Register(_ context.Context, _ string, _ string) error { return nil }
func (r *testRegistry) Deregister(_ context.Context, _ string) error         { return nil }
func (r *testRegistry) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	addr, ok := r.nodes[nodeID]
	return addr, ok, nil
}
func (r *testRegistry) List(_ context.Context) ([]contract.NodeInfo, error) { return nil, nil }
