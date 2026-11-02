package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type clusterNodeClock struct {
	Controllable bool   `json:"controllable"`
	Version      int64  `json:"version"`
	Writer       string `json:"writer"`
}

type clusterNodeStatus struct {
	ID           string            `json:"id"`
	Reachability string            `json:"reachability"`
	State        string            `json:"state"`
	Database     *bool             `json:"database"`
	Version      *string           `json:"version"`
	Clock        *clusterNodeClock `json:"clock"`
}

type clusterStateCount struct {
	Reachability string `json:"reachability"`
	State        string `json:"state"`
	Count        int    `json:"count"`
}

type clusterStatus struct {
	Operational bool                `json:"operational"`
	Counts      []clusterStateCount `json:"counts"`
	Nodes       []clusterNodeStatus `json:"nodes"`
}

// getPlatformCluster reads GET /platform/cluster as the platform operator and
// answers the status code and the decoded body, which is a status for both
// 200 and 503.
func getPlatformCluster(t *testing.T, query string) (int, clusterStatus) {
	t.Helper()
	resp := operatorRequest(t, http.MethodGet, "/platform/cluster"+query, nil)
	ct := resp.Header.Get("Content-Type")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /platform/cluster%s: %d %s", query, resp.StatusCode, body)
	}
	if ct != "application/json" {
		t.Fatalf("content-type %q, want application/json; body %s", ct, body)
	}
	var st clusterStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return resp.StatusCode, st
}

// assertLocalEntry checks the single node's one entry: local, UP, NORMAL,
// its database check passing, a version, and the clock setting of a node
// without clock control.
func assertLocalEntry(t *testing.T, st clusterStatus) {
	t.Helper()
	if len(st.Nodes) != 1 {
		t.Fatalf("nodes %+v, want one entry", st.Nodes)
	}
	n := st.Nodes[0]
	if n.ID != "local" || n.Reachability != "UP" || n.State != "NORMAL" {
		t.Fatalf("entry %+v, want local UP NORMAL", n)
	}
	if n.Database == nil || !*n.Database {
		t.Fatalf("entry %+v: database is not true", n)
	}
	if n.Version == nil || *n.Version == "" {
		t.Fatalf("entry %+v: no version", n)
	}
	if n.Clock == nil || *n.Clock != (clusterNodeClock{}) {
		t.Fatalf("clock %+v, want controllable false, version 0, writer empty", n.Clock)
	}
	if len(st.Counts) != 1 || st.Counts[0] != (clusterStateCount{"UP", "NORMAL", 1}) {
		t.Fatalf("counts %+v, want one UP NORMAL 1", st.Counts)
	}
}

func TestPlatformCluster_SingleNode_200(t *testing.T) {
	code, st := getPlatformCluster(t, "")
	if code != http.StatusOK || !st.Operational {
		t.Fatalf("status %d, operational %v; want 200 and true", code, st.Operational)
	}
	assertLocalEntry(t, st)
}

// A single node can never have two nodes UP and NORMAL: the wait runs to its
// timeout and answers 503 with the last round.
func TestPlatformCluster_SingleNode_WaitForTwo_503AtTimeout(t *testing.T) {
	start := time.Now() // realtime: the test measures the wait's real duration
	code, st := getPlatformCluster(t, "?waitForNodes=2&timeoutMs=500")
	// realtime: the test measures the wait's real duration
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("answered after %s, before the 500 ms timeout", elapsed)
	}
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false", code, st.Operational)
	}
	assertLocalEntry(t, st)
}

func TestPlatformCluster_400(t *testing.T) {
	cases := []struct{ query, param string }{
		{"foo=1", "foo"},
		{"waitForNodes=abc", "waitForNodes"},
		{"waitForNodes=2.5", "waitForNodes"},
		{"waitForNodes=0", "waitForNodes"},
		{"waitForNodes=1001", "waitForNodes"},
		{"waitForNodes=99999999999", "waitForNodes"},
		{"waitForNodes=2&waitForNodes=3", "waitForNodes"},
		{"timeoutMs=0&waitForNodes=1", "timeoutMs"},
		{"timeoutMs=600001&waitForNodes=1", "timeoutMs"},
		{"timeoutMs=100", "timeoutMs"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			resp := operatorRequest(t, http.MethodGet, "/platform/cluster?"+tc.query, nil)
			ct := resp.Header.Get("Content-Type")
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest || ct != "application/problem+json" {
				t.Fatalf("status %d, content-type %q; want 400 problem: %s", resp.StatusCode, ct, body)
			}
			var pd struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal([]byte(body), &pd); err != nil {
				t.Fatalf("body %s: %v", body, err)
			}
			if got := fmt.Sprint(pd.Properties["errorCode"]); got != "BAD_REQUEST" {
				t.Fatalf("errorCode %q, want BAD_REQUEST: %s", got, body)
			}
			if got := fmt.Sprint(pd.Properties["parameter"]); got != tc.param {
				t.Fatalf("parameter %q, want %q: %s", got, tc.param, body)
			}
		})
	}
}
