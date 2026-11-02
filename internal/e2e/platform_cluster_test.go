package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
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
	return resp.StatusCode, clusterStatusOf(t, resp.StatusCode, ct, body)
}

// clusterStatusOf requires a 200 or 503 JSON answer and decodes its body.
func clusterStatusOf(t *testing.T, code int, contentType, body string) clusterStatus {
	t.Helper()
	if code != http.StatusOK && code != http.StatusServiceUnavailable {
		t.Fatalf("GET /platform/cluster: %d %s", code, body)
	}
	if contentType != "application/json" {
		t.Fatalf("content-type %q, want application/json; body %s", contentType, body)
	}
	var st clusterStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return st
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

// appGossipAddr returns a loopback address, free a moment ago, for an app
// node's gossip to bind, so that the next nodes can name it as their seed. A
// bind error there is not a test failure: app.New logs it and exits the
// process, taking the whole e2e binary with it (see newLostHandOverHarness).
// So the port is not one freeLoopbackPort returns, which the kernel can hand
// to any port-0 bind in between: it is below every ephemeral range (Linux
// starts at 32768, macOS at 49152), where only an explicit bind of this very
// port can take it.
func appGossipAddr(t *testing.T) string {
	t.Helper()
	for range 50 {
		port := 20000 + mathrand.IntN(12000)
		addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = l.Close()
		// memberlist binds the UDP port of the same number too.
		u, err := net.ListenPacket("udp", addr)
		if err != nil {
			continue
		}
		_ = u.Close()
		return addr
	}
	t.Fatal("no free loopback port for the gossip of an app node")
	return ""
}

// statusCluster is a cluster under test on a database of its own. Its nodes
// sign with key and join through seed; its node ids end in sfx.
type statusCluster struct {
	s    *schedDB
	key  *rsa.PrivateKey
	sfx  string
	seed string
}

func newStatusCluster(t *testing.T) *statusCluster {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &statusCluster{s: newSchedDB(t), key: key, sfx: randSuffix(t)[:8]}
}

// id is the node id of the node called name.
func (c *statusCluster) id(name string) string { return "cs-" + name + "-" + c.sfx }

// first starts node "a" alone, on a gossip address that becomes the seed of
// the nodes that join after it, so no outside member is in the directory.
func (c *statusCluster) first(t *testing.T) *callbackHarness {
	t.Helper()
	addr := appGossipAddr(t)
	h := clusterNode(t, c.s, c.key, c.id("a"), addr, nil, nil)
	c.seed = addr
	return h
}

// join starts the node called name, joined through the cluster's seed.
// configure, when not nil, configures it.
func (c *statusCluster) join(t *testing.T, name string, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	return clusterNode(t, c.s, c.key, c.id(name), "127.0.0.1:0", []string{c.seed}, configure)
}

// clusterOn reads GET /platform/cluster on node h as the platform operator and
// answers the status code, the decoded body and the raw body.
func clusterOn(t *testing.T, h *callbackHarness, query string) (int, clusterStatus, string) {
	t.Helper()
	resp := h.DoAuthBearer(t, http.MethodGet, "/api/platform/cluster"+query, "", "", h.platformToken(t))
	ct := resp.Header.Get("Content-Type")
	body := readBody(t, resp)
	return resp.StatusCode, clusterStatusOf(t, resp.StatusCode, ct, body), body
}

// entryOf returns the entry of node id; the test fails when there is none.
func entryOf(t *testing.T, st clusterStatus, id string) clusterNodeStatus {
	t.Helper()
	for _, n := range st.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no entry for %s in %+v", id, st.Nodes)
	return clusterNodeStatus{}
}

// assertNodeIDs checks the entries are exactly ids, in ascending order.
func assertNodeIDs(t *testing.T, st clusterStatus, ids ...string) {
	t.Helper()
	got := make([]string, len(st.Nodes))
	for i, n := range st.Nodes {
		got[i] = n.ID
	}
	if want := sortedIDs(ids...); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries %v, want %v", got, want)
	}
}

// assertUp checks an entry is UP in state, with its database check passing,
// a version and a clock.
func assertUp(t *testing.T, n clusterNodeStatus, state string) {
	t.Helper()
	if n.Reachability != "UP" || n.State != state {
		t.Fatalf("entry %+v, want UP %s", n, state)
	}
	if n.Database == nil || !*n.Database || n.Version == nil || *n.Version == "" || n.Clock == nil {
		t.Fatalf("entry %+v: an UP entry has its database check passing, a version and a clock", n)
	}
}

// assertDown checks an entry is DOWN in state, without the fields only an
// answer gives.
func assertDown(t *testing.T, n clusterNodeStatus, state string) {
	t.Helper()
	if n.Reachability != "DOWN" || n.State != state {
		t.Fatalf("entry %+v, want DOWN %s", n, state)
	}
	if n.Database != nil || n.Version != nil || n.Clock != nil {
		t.Fatalf("entry %+v: a DOWN entry carries no database, version or clock", n)
	}
}

// assertCounts checks the counts are exactly want, in order.
func assertCounts(t *testing.T, st clusterStatus, want ...clusterStateCount) {
	t.Helper()
	if fmt.Sprint(st.Counts) != fmt.Sprint(want) {
		t.Fatalf("counts %+v, want %+v", st.Counts, want)
	}
}

// assertNoAddress checks a raw answer names no host, port or URL: a member's
// address goes to the log only.
func assertNoAddress(t *testing.T, raw string) {
	t.Helper()
	for _, leak := range []string{"127.0.0.1", "http://"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("answer contains %q: %s", leak, raw)
		}
	}
}

// Two nodes, both UP and NORMAL: the cluster is operational. Node A's own
// entry is UP, so its own peer status route serves.
func TestPlatformClusterMN_TwoNodes_200(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)
	c.join(t, "b", nil)
	awaitMembers(t, a, c.id("a"), c.id("b"))

	code, st, raw := clusterOn(t, a, "")
	if code != http.StatusOK || !st.Operational {
		t.Fatalf("status %d, operational %v; want 200 and true: %s", code, st.Operational, raw)
	}
	assertNodeIDs(t, st, c.id("a"), c.id("b"))
	for _, n := range st.Nodes {
		assertUp(t, n, "NORMAL")
	}
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 2})
}

// A member whose address serves nothing is DOWN with the state its gossip
// metadata names, and the answer does not name its address.
func TestPlatformClusterMN_DeadAddress_503(t *testing.T) {
	c := newStatusCluster(t)
	bare := newClockSeed(t, false)
	c.seed = bare.gossip
	a := c.join(t, "a", nil)
	awaitMembers(t, a, bare.nodeID, c.id("a"))

	code, st, raw := clusterOn(t, a, "")
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false: %s", code, st.Operational, raw)
	}
	assertNoAddress(t, raw)
	assertNodeIDs(t, st, bare.nodeID, c.id("a"))
	assertUp(t, entryOf(t, st, c.id("a")), "NORMAL")
	assertDown(t, entryOf(t, st, bare.nodeID), "STARTING")
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 1}, clusterStateCount{"DOWN", "STARTING", 1})
}

// A node that leaves the cluster leaves the status: once its leave reaches
// node A, A lists only itself and is operational.
func TestPlatformClusterMN_LeftNodeAbsent(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)
	b := c.join(t, "b", nil)
	awaitMembers(t, a, c.id("a"), c.id("b"))

	b.app.Shutdown()
	if err := b.app.Close(); err != nil {
		t.Fatalf("close node B: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second) // realtime: the bound on the leave reaching node A
	for {
		code, st, raw := clusterOn(t, a, "")
		if code == http.StatusOK && len(st.Nodes) == 1 {
			assertNodeIDs(t, st, c.id("a"))
			assertUp(t, st.Nodes[0], "NORMAL")
			if !st.Operational {
				t.Fatalf("200 without operational: %s", raw)
			}
			return
		}
		if time.Now().After(deadline) { // realtime: the bound on the leave reaching node A
			t.Fatalf("node A still answers %d %s 5 s after node B left", code, raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A draining node answers DRAINING, which makes the cluster not operational;
// its gossip metadata announces DRAINING to node A too.
func TestPlatformClusterMN_Draining_503(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)
	b := c.join(t, "b", nil)
	awaitMembers(t, a, c.id("a"), c.id("b"))

	b.app.DrainScheduler(context.Background())

	code, st, raw := clusterOn(t, a, "")
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false: %s", code, st.Operational, raw)
	}
	assertUp(t, entryOf(t, st, c.id("a")), "NORMAL")
	assertUp(t, entryOf(t, st, c.id("b")), "DRAINING")
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 1}, clusterStateCount{"UP", "DRAINING", 1})

	// The lifecycle's change reaches node A's gossip directory: the state a
	// DOWN entry would carry.
	g, ok := a.app.NodeRegistry().(*registry.Gossip)
	if !ok {
		t.Fatalf("node A's registry is %T, want *registry.Gossip", a.app.NodeRegistry())
	}
	deadline := time.Now().Add(10 * time.Second) // realtime: the bound on the gossip reaching node A
	for {
		var got lifecycle.State
		for _, m := range g.Members() {
			if m.NodeID == c.id("b") {
				got = m.State
			}
		}
		if got == lifecycle.Draining {
			return
		}
		if time.Now().After(deadline) { // realtime: the bound on the gossip reaching node A
			t.Fatalf("node A's directory names node B's state %q 10 s after the drain; want DRAINING", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Two nodes of different versions are both UP and NORMAL, but the cluster is
// not operational.
func TestPlatformClusterMN_VersionMismatch_503(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)
	c.join(t, "b", func(cfg *app.Config) { cfg.Version = "other" })
	awaitMembers(t, a, c.id("a"), c.id("b"))

	code, st, raw := clusterOn(t, a, "")
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false: %s", code, st.Operational, raw)
	}
	na, nb := entryOf(t, st, c.id("a")), entryOf(t, st, c.id("b"))
	assertUp(t, na, "NORMAL")
	assertUp(t, nb, "NORMAL")
	if *nb.Version != "other" || *na.Version == "other" {
		t.Fatalf("versions A %q, B %q; want B other and A not", *na.Version, *nb.Version)
	}
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 2})
}

// A node with clock control among nodes without it: both are UP and NORMAL,
// but the cluster is not operational.
func TestPlatformClusterMN_ClockMismatch_503(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)
	c.join(t, "b", func(cfg *app.Config) { cfg.ClockControlEnabled = true })
	awaitMembers(t, a, c.id("a"), c.id("b"))

	code, st, raw := clusterOn(t, a, "")
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false: %s", code, st.Operational, raw)
	}
	na, nb := entryOf(t, st, c.id("a")), entryOf(t, st, c.id("b"))
	assertUp(t, na, "NORMAL")
	assertUp(t, nb, "NORMAL")
	if na.Clock.Controllable || !nb.Clock.Controllable {
		t.Fatalf("clocks A %+v, B %+v; want A not controllable and B controllable", *na.Clock, *nb.Clock)
	}
	if *na.Version != *nb.Version {
		t.Fatalf("versions A %q, B %q differ: the clock is not the only difference", *na.Version, *nb.Version)
	}
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 2})
}

// clusterAnswer is an answer read off the test goroutine.
type clusterAnswer struct {
	code              int
	contentType, body string
	err               error
}

// A wait for two nodes, sent while node A is alone, answers 200 once node B
// joins: its answer lists node B, which started after the request was sent.
func TestPlatformClusterMN_WaitForTwo_200OnJoin(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)

	req, err := e2eNewRequest(t, http.MethodGet, a.baseURL+"/api/platform/cluster?waitForNodes=2&timeoutMs=60000", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.platformToken(t))
	wrote := make(chan struct{})
	var once sync.Once
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { once.Do(func() { close(wrote) }) },
	}))
	answer := make(chan clusterAnswer, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answer <- clusterAnswer{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		answer <- clusterAnswer{code: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body), err: err}
	}()
	select {
	case <-wrote:
	case got := <-answer:
		t.Fatalf("the wait answered before it was sent in full: %+v", got)
	case <-time.After(10 * time.Second): // realtime: the bound on sending the request
		t.Fatal("the wait's request was not sent within 10 s")
	}

	c.join(t, "b", nil)

	var got clusterAnswer
	select {
	case got = <-answer:
	case <-time.After(70 * time.Second): // realtime: past the wait's own 60 s timeout
		t.Fatal("the wait did not answer")
	}
	if got.err != nil {
		t.Fatalf("the wait: %v", got.err)
	}
	st := clusterStatusOf(t, got.code, got.contentType, got.body)
	if got.code != http.StatusOK || !st.Operational {
		t.Fatalf("status %d, operational %v; want 200 and true: %s", got.code, st.Operational, got.body)
	}
	assertNodeIDs(t, st, c.id("a"), c.id("b"))
	for _, n := range st.Nodes {
		assertUp(t, n, "NORMAL")
	}
}

// Two nodes UP and NORMAL meet waitForNodes=2 at once, but a third member is
// DOWN: the answer is 503 without waiting for the timeout.
func TestPlatformClusterMN_WaitMetWithExtraDown_503(t *testing.T) {
	c := newStatusCluster(t)
	bare := newClockSeed(t, false)
	c.seed = bare.gossip
	a := c.join(t, "a", nil)
	c.join(t, "b", nil)
	awaitMembers(t, a, bare.nodeID, c.id("a"), c.id("b"))

	start := time.Now() // realtime: the test measures the wait's real duration
	code, st, raw := clusterOn(t, a, "?waitForNodes=2")
	// realtime: the test measures the wait's real duration
	if elapsed := time.Since(start); elapsed >= 15*time.Second {
		t.Fatalf("answered after %s; want at once, well under the 30 s default timeout", elapsed)
	}
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false: %s", code, st.Operational, raw)
	}
	assertNoAddress(t, raw)
	assertNodeIDs(t, st, bare.nodeID, c.id("a"), c.id("b"))
	assertUp(t, entryOf(t, st, c.id("a")), "NORMAL")
	assertUp(t, entryOf(t, st, c.id("b")), "NORMAL")
	assertDown(t, entryOf(t, st, bare.nodeID), "STARTING")
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 2}, clusterStateCount{"DOWN", "STARTING", 1})
}

// A wait for two nodes on a node that stays alone runs to its timeout and
// answers 503 with the last round.
func TestPlatformClusterMN_WaitNotMet_503LastRound(t *testing.T) {
	c := newStatusCluster(t)
	a := c.first(t)

	start := time.Now() // realtime: the test measures the wait's real duration
	code, st, raw := clusterOn(t, a, "?waitForNodes=2&timeoutMs=1000")
	elapsed := time.Since(start) // realtime: the test measures the wait's real duration
	if elapsed < time.Second || elapsed >= 20*time.Second {
		t.Fatalf("answered after %s; want at the 1 s timeout", elapsed)
	}
	if code != http.StatusServiceUnavailable || st.Operational {
		t.Fatalf("status %d, operational %v; want 503 and false: %s", code, st.Operational, raw)
	}
	assertNodeIDs(t, st, c.id("a"))
	assertUp(t, st.Nodes[0], "NORMAL")
	assertCounts(t, st, clusterStateCount{"UP", "NORMAL", 1})
}
