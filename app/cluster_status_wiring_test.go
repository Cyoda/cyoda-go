package app_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/app"
)

type clusterAnswer struct {
	Operational bool `json:"operational"`
	Counts      []struct {
		Reachability string `json:"reachability"`
		State        string `json:"state"`
		Count        int    `json:"count"`
	} `json:"counts"`
	Nodes []struct {
		ID           string `json:"id"`
		Reachability string `json:"reachability"`
		State        string `json:"state"`
		Database     *bool  `json:"database"`
		Version      string `json:"version"`
		Clock        *struct {
			Controllable bool   `json:"controllable"`
			Version      int64  `json:"version"`
			Writer       string `json:"writer"`
		} `json:"clock"`
	} `json:"nodes"`
}

func getCluster(t *testing.T, srv *httptest.Server) (int, clusterAnswer) {
	t.Helper()
	st, raw := call(t, srv, http.MethodGet, "/platform/cluster", "")
	var c clusterAnswer
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("GET /platform/cluster answered %d %s: %v", st, raw, err)
	}
	return st, c
}

// Without clustering the one entry is local, read in process: UP, NORMAL,
// the database check, the binary's version and the clock setting.
func TestClusterStatusWiring_SingleNode(t *testing.T) {
	srv := clockNode(t, func(cfg *app.Config) { cfg.Version = "wiring-v1" })
	st, c := getCluster(t, srv)
	if st != http.StatusOK || !c.Operational || len(c.Nodes) != 1 {
		t.Fatalf("status %d, answer %+v", st, c)
	}
	n := c.Nodes[0]
	if n.ID != "local" || n.Reachability != "UP" || n.State != "NORMAL" ||
		n.Database == nil || !*n.Database || n.Version != "wiring-v1" || n.Clock == nil || n.Clock.Controllable {
		t.Fatalf("entry %+v", n)
	}
}

// With clock control on, the entry carries the test clock's setting.
func TestClusterStatusWiring_SingleNodeClockControl(t *testing.T) {
	srv := clockNode(t, func(cfg *app.Config) { cfg.ClockControlEnabled = true })
	if st, raw := call(t, srv, http.MethodPost, "/platform/clock", `{"rate":2}`); st != http.StatusOK {
		t.Fatalf("set rate: %d %s", st, raw)
	}
	st, c := getCluster(t, srv)
	if st != http.StatusOK || len(c.Nodes) != 1 || c.Nodes[0].Clock == nil {
		t.Fatalf("status %d, answer %+v", st, c)
	}
	if ck := c.Nodes[0].Clock; !ck.Controllable || ck.Version == 0 || ck.Writer != "local" {
		t.Fatalf("clock %+v, want the change's setting", ck)
	}
}

// In cluster mode the node asks itself over the sealed peer channel, so its
// own entry is UP only when its status route serves.
func TestClusterStatusWiring_ClusterAsksOverPeerChannel(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.Cluster.Enabled = true
	cfg.Cluster.NodeID = "status-node"
	cfg.Cluster.NodeAddr = "http://" + l.Addr().String()
	cfg.Cluster.GossipAddr = "127.0.0.1:0"
	cfg.Cluster.StabilityWindow = 0
	cfg.Cluster.HMACSecret = clusterSecret
	cfg.Cluster.DispatchAllowLoopback = true
	a := app.New(cfg)
	srv := httptest.NewUnstartedServer(a.Handler())
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		_ = a.NodeRegistry().Deregister(context.Background(), cfg.Cluster.NodeID)
		_ = a.Close()
	})

	st, c := getCluster(t, srv)
	if st != http.StatusOK || !c.Operational || len(c.Nodes) != 1 {
		t.Fatalf("status %d, answer %+v", st, c)
	}
	if n := c.Nodes[0]; n.ID != "status-node" || n.Reachability != "UP" || n.State != "NORMAL" {
		t.Fatalf("entry %+v", n)
	}
}
