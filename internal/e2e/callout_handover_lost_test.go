package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// callout_handover_lost_test.go covers the one hand-over outcome no
// running-backend test reached: the owner hands a callout over to a peer node,
// the request IS delivered and read there, and the ANSWER IS LOST — it does
// not authenticate, or the connection is closed with no reply at all. The peer
// may have given the work to a compute member, so the try counts; what the
// client is told is `DISPATCH_FORWARD_FAILED` when that was the only try, and
// a `CALLOUT_FAILED` listing it as `member<->` when another try failed too.
//
// The peer is a stand-in: a node of the owner's cluster registry, advertising
// the tag, that opens the sealed hand-over (proving it arrived and was read)
// and then loses the answer. Nothing is killed and no seam is added to
// production code — losing an answer is what a peer behind a dying sidecar,
// an intermediary answering 502, or a node whose clock has drifted does.

// dispatchCalloutPathForTest mirrors the route
// internal/cluster/dispatch/handler.go registers for a hand-over ("POST
// /internal/dispatch/callout"). Duplicated rather than exported across the
// package boundary for a one-line route string — the precedent
// schedulerTaskPathForTest (tx_lifecycle_e2e_test.go) sets.
const dispatchCalloutPathForTest = "/internal/dispatch/callout"

// forwardFailedDetail is the whole client-visible text of a lost hand-over,
// as cmd/cyoda/help/content/errors/DISPATCH_FORWARD_FAILED.md documents it.
const forwardFailedDetail = "DISPATCH_FORWARD_FAILED: forwarding the callout to a peer node failed"

// forwardFailedEntry is the entry a lost hand-over contributes to a
// CALLOUT_FAILED message, as errors/CALLOUT_FAILED.md documents it: the member
// is not known, so it is "-".
const forwardFailedEntry = "[member<->: " + forwardFailedDetail + "]"

// gatewayRefusalText is what the stand-in peer's non-2xx answer says. Nothing
// a peer or an intermediary writes may reach the client, so it is asserted
// absent from every body.
const gatewayRefusalText = "the node behind this gateway is gone"

// peerAnswer is what a stand-in peer does with a hand-over it has opened.
type peerAnswer int

const (
	// peerAnswerDoesNotOpen replies 200 with an envelope that does not
	// authenticate. The owner cannot believe it: the answer is lost.
	peerAnswerDoesNotOpen peerAnswer = iota
	// peerAnswerDropsConnection closes the connection with no reply, as a peer
	// that died between reading the request and answering it does.
	peerAnswerDropsConnection
	// peerAnswerRefusedStatus replies with a non-2xx status and a body of its
	// own, as an intermediary in front of a peer that has gone does. It is not
	// sealed, so it proves nothing: the answer is lost.
	peerAnswerRefusedStatus
	// peerAnswerNoComputeMember is a genuine, sealed "nothing was handed to a
	// compute member": no try is used and the owner goes on to the next peer.
	peerAnswerNoComputeMember
)

// handOverSeen is one hand-over a stand-in peer opened, in the fields the
// scenarios assert on. The pass a hand-over lets the peer mint is never
// recorded: this peer mints none.
type handOverSeen struct {
	peer       string
	kind       string
	requestID  string
	tenantID   string
	tags       string
	triesLeft  int
	major      uint32
	repeatSafe bool
}

// standInPeer is a peer node of the owner's cluster that never runs a callout.
// It joins the node registry as a node advertising the tag, opens the
// hand-over the owner sealed for it, records it, and then answers as its mode
// says.
type standInPeer struct {
	nodeID     string
	gossipAddr string
	srv        *httptest.Server
	gossip     *registry.Gossip

	mu     sync.Mutex
	answer peerAnswer
	seen   []handOverSeen
	// faults are what this peer saw that it should not have — a hand-over that
	// did not authenticate, a body that did not parse, a request on another
	// route. Recorded rather than failed on: the handler runs off the test
	// goroutine. Never the underlying error's text: it is crypto and peer
	// detail.
	faults []string
}

// newStandInPeer starts a stand-in peer: an HTTP dispatch door and a cluster
// registry membership advertising tags for tenantID. seeds is the gossip
// address of a node already in the cluster ("" for the first one).
func newStandInPeer(t *testing.T, nodeID, tenantID string, tags []string, seeds []string, answer peerAnswer) *standInPeer {
	t.Helper()
	auth, err := dispatch.NewAEADPeerAuth(clusterHMACSecret32, nodeID, 30*time.Second)
	if err != nil {
		t.Fatalf("peer auth for %s: %v", nodeID, err)
	}
	p := &standInPeer{nodeID: nodeID, answer: answer}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+dispatchCalloutPathForTest, func(w http.ResponseWriter, r *http.Request) {
		p.serveHandOver(auth, w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.fault(fmt.Sprintf("a request arrived on %s %s", r.Method, r.URL.Path))
		w.WriteHeader(http.StatusNotFound)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)

	// The gossip port has to be known before the node binds it, so that the
	// nodes that seed from it can be configured. A port free a moment ago can
	// be taken by then, which is a retry, not a failure.
	for attempt := 1; ; attempt++ {
		port := freeLoopbackPort(t)
		g, gerr := registry.NewGossip(registry.GossipConfig{
			NodeID:           nodeID,
			NodeAddr:         p.srv.URL,
			BindAddr:         "127.0.0.1",
			BindPort:         port,
			Seeds:            seeds,
			SecretKey:        clusterHMACSecret32,
			StabilityWindow:  0,
			ListScanInterval: 100 * time.Millisecond,
		})
		if gerr == nil {
			p.gossip, p.gossipAddr = g, net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
			break
		}
		if attempt == 5 {
			t.Fatalf("stand-in peer %s could not bind a gossip port: %v", nodeID, gerr)
		}
	}
	t.Cleanup(func() { _ = p.gossip.Deregister(context.Background(), nodeID) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.gossip.Register(ctx, nodeID, p.srv.URL); err != nil {
		t.Fatalf("stand-in peer %s could not join the cluster: %v", nodeID, err)
	}
	if err := p.gossip.UpdateTags(map[string][]string{tenantID: tags}); err != nil {
		t.Fatalf("stand-in peer %s could not advertise its tags: %v", nodeID, err)
	}
	return p
}

// serveHandOver opens the hand-over and answers as the peer's mode says.
func (p *standInPeer) serveHandOver(auth dispatch.PeerAuth, w http.ResponseWriter, r *http.Request) {
	body, _, binding, err := auth.Verify(r)
	if err != nil {
		p.fault("a hand-over did not authenticate")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var req dispatch.DispatchCalloutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.fault("a hand-over's body did not parse")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.record(handOverSeen{
		peer: p.nodeID, kind: req.Kind, requestID: req.RequestID, tenantID: req.TenantID,
		tags: req.Tags, triesLeft: req.TriesLeft, major: req.Major, repeatSafe: req.RepeatSafe,
	})

	switch p.currentAnswer() {
	case peerAnswerDropsConnection:
		// Closes the connection with no reply; net/http neither logs nor
		// answers. The owner read of a request it wrote in full fails.
		panic(http.ErrAbortHandler)
	case peerAnswerRefusedStatus:
		http.Error(w, gatewayRefusalText, http.StatusBadGateway)
	case peerAnswerNoComputeMember:
		zero := 0
		plain, merr := json.Marshal(dispatch.DispatchCalloutResponse{
			Outcome: contract.NoHandOff.String(), TriesUsed: &zero,
		})
		if merr != nil {
			p.fault("the answer could not be built")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		wire, serr := auth.SealResponse(w.Header(), binding, plain)
		if serr != nil {
			p.fault("the answer could not be sealed")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(wire)
	default:
		// Envelope-shaped and long enough to be one, sealed under no key: the
		// owner's OpenResponse refuses it.
		w.Header().Set("Content-Type", dispatch.DispatchContentType)
		_, _ = w.Write(bytes.Repeat([]byte{0x2a}, 64))
	}
}

func (p *standInPeer) setAnswer(a peerAnswer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = a
}

func (p *standInPeer) currentAnswer() peerAnswer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.answer
}

func (p *standInPeer) record(h handOverSeen) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, h)
}

func (p *standInPeer) fault(what string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults = append(p.faults, what)
}

// take returns the hand-overs this peer opened since the last take and
// forgets them, so every scenario counts only its own.
func (p *standInPeer) take() []handOverSeen {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.seen
	p.seen = nil
	return out
}

func (p *standInPeer) faultList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.faults...)
}

// addr is the host:port of this peer's dispatch door — text no client-facing
// message may carry.
func (p *standInPeer) addr() string {
	return p.srv.Listener.Addr().String()
}

// freeLoopbackPort returns a loopback port that was free a moment ago.
// memberlist listens on it for both TCP and UDP.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a loopback port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// newLostHandOverHarness stands up the owner: a cluster node seeded at the
// stand-in peers, with the loopback guard opened as every multi-node fixture
// opens it. Four tries, and no patience at all — a pass is never retried, so
// what each peer and each compute member received is exact.
func newLostHandOverHarness(t *testing.T, nodeID string, seeds []string) *callbackHarness {
	t.Helper()
	gossipPort := freeLoopbackPort(t)
	return newCalloutHarness(t, func(cfg *app.Config) {
		calloutTuning(3, 0)(cfg)
		// Nothing of this file is scheduled; a scan loop would be the one other
		// thing that talks to a peer.
		cfg.Scheduler.Enabled = false
		cfg.Cluster.Enabled = true
		cfg.Cluster.NodeID = nodeID
		cfg.Cluster.NodeAddr = fmt.Sprintf("http://127.0.0.1:%d", cfg.HTTPPort)
		cfg.Cluster.GossipAddr = net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", gossipPort))
		cfg.Cluster.SeedNodes = seeds
		cfg.Cluster.StabilityWindow = 0
		cfg.Cluster.HMACSecret = clusterHMACSecret32
		cfg.Cluster.DispatchAllowLoopback = true
	})
}

// awaitPeerTag returns once the owner's node registry holds want peers
// advertising tag for tenantID. It is the membership handshake, not a sleep:
// a hand-over cannot be made before the owner knows a peer serves the tag.
func awaitPeerTag(t *testing.T, h *callbackHarness, tenantID, tag string, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		nodes, err := h.app.NodeRegistry().List(context.Background())
		if err != nil {
			t.Fatalf("list cluster nodes: %v", err)
		}
		got := 0
		for _, n := range nodes {
			if n.Alive && slices.Contains(n.Tags[tenantID], tag) {
				got++
			}
		}
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the owner sees %d peers advertising %q for %q; want %d (nodes: %+v)", got, tag, tenantID, want, nodes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCalloutHandOver_AnswerLost_NotRepeatSafe: the owner has no compute
// member for the tag, hands the callout over to a peer, and the answer is lost
// after the request was delivered and read. The processor is not declared
// idempotent, so the work must not be given to anyone else, however many tries
// are left: the client gets that one try's own code, the second peer is never
// asked, and no compute member of the owner saw the work at all.
func TestCalloutHandOver_AnswerLost_NotRepeatSafe(t *testing.T) {
	const tenantID = "test-tenant"
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	tag := "s-lost-" + sfx
	peerA := newStandInPeer(t, "lost-peer-a-"+sfx, tenantID, []string{tag}, nil, peerAnswerDoesNotOpen)
	peerB := newStandInPeer(t, "lost-peer-b-"+sfx, tenantID, []string{tag}, []string{peerA.gossipAddr}, peerAnswerDoesNotOpen)
	ownerNodeID := "lost-owner-" + sfx
	h := newLostHandOverHarness(t, ownerNodeID, []string{peerA.gossipAddr, peerB.gossipAddr})
	awaitPeerTag(t, h, tenantID, tag, 2)

	// Nothing that names a node, an address, or the transport's own words may
	// appear in a client-facing body (§8.2, Gate 3).
	forbidden := []string{
		peerA.nodeID, peerB.nodeID, ownerNodeID, peerA.addr(), peerB.addr(),
		"127.0.0.1", "localhost", "dispatch forward", "AEAD", "EOF", dispatchCalloutPathForTest,
		gatewayRefusalText,
	}

	for _, mode := range []struct {
		name   string
		answer peerAnswer
	}{
		{"answer-does-not-open", peerAnswerDoesNotOpen},
		{"connection-dropped", peerAnswerDropsConnection},
		{"non-2xx-status", peerAnswerRefusedStatus},
	} {
		t.Run(mode.name, func(t *testing.T) {
			peerA.setAnswer(mode.answer)
			peerB.setAnswer(mode.answer)
			for _, door := range []string{"http", "grpc"} {
				t.Run(door, func(t *testing.T) {
					model := fmt.Sprintf("lost-%s-%s-%s", sfx, mode.name, door)
					h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("lost-wf-"+mode.name+"-"+door,
						procSpec{"lost-proc", "SYNC", map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 300}}))
					peerA.take()
					peerB.take()

					switch door {
					case "http":
						_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
						pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "DISPATCH_FORWARD_FAILED", true)
						if pd.Detail != forwardFailedDetail {
							t.Errorf("detail = %q; want the literal %q", pd.Detail, forwardFailedDetail)
						}
						assertNoLeak(t, door, body, forbidden)
					case "grpc":
						env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
						assertEnvelope(t, door, env, err, "DISPATCH_FORWARD_FAILED", true)
						if env.Error != nil && env.Error.Message != forwardFailedDetail {
							t.Errorf("message = %q; want the literal %q", env.Error.Message, forwardFailedDetail)
						}
						if env.Error != nil {
							assertNoLeak(t, door, env.Error.Message, forbidden)
						}
					}

					seen := append(peerA.take(), peerB.take()...)
					if len(seen) != 1 {
						t.Fatalf("the peers opened %d hand-overs; want exactly 1 — the work is not repeat-safe: %+v", len(seen), seen)
					}
					got := seen[0]
					if got.kind != "processor" || got.repeatSafe || got.tags != tag || got.tenantID != tenantID {
						t.Errorf("hand-over = %+v; want a non-repeat-safe processor callout for tenant %q at tag %q", got, tenantID, tag)
					}
					if got.triesLeft != 4 || got.major != 1 {
						t.Errorf("hand-over = %+v; want triesLeft 4 (no try was made here) and major 1", got)
					}
					if got.requestID == "" {
						t.Error("the hand-over carries no request id")
					}
					if recs := h.ReceivedCallouts(); len(recs) != 0 {
						t.Errorf("a compute member of the owner received %d callouts; the owner has none for this tag: %v", len(recs), recs)
					}
					if n := h.countEntities(t, model); n != 0 {
						t.Errorf("%d entities committed by a failed SYNC create; want 0", n)
					}
					assertNoPeerFaults(t, peerA, peerB)
				})
			}
		})
	}
}

// TestCalloutHandOver_AnswerLost_RepeatSafe_CalloutFailed: an idempotent
// processor, one compute member of the owner that answers nothing, and a peer
// whose answer is lost. Two tries failed, so the client gets CALLOUT_FAILED —
// and the lost hand-over is listed as `member<->` carrying its own code, the
// form errors/CALLOUT_FAILED.md documents. The local pass necessarily runs
// first, so the lost hand-over is the second of the two entries; the second
// peer answers, under seal, that it has no compute member, which uses no try
// and contributes no entry.
func TestCalloutHandOver_AnswerLost_RepeatSafe_CalloutFailed(t *testing.T) {
	const tenantID = "test-tenant"
	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	tag := "s-lostrs-" + sfx
	peerLost := newStandInPeer(t, "lostrs-peer-a-"+sfx, tenantID, []string{tag}, nil, peerAnswerDoesNotOpen)
	peerNoMember := newStandInPeer(t, "lostrs-peer-b-"+sfx, tenantID, []string{tag}, []string{peerLost.gossipAddr}, peerAnswerNoComputeMember)
	ownerNodeID := "lostrs-owner-" + sfx
	h := newLostHandOverHarness(t, ownerNodeID, []string{peerLost.gossipAddr, peerNoMember.gossipAddr})
	awaitPeerTag(t, h, tenantID, tag, 2)

	silent := h.AttachCnode(t, cnodeSpec{name: "silent", tags: []string{tag}, script: scriptAlways(neverAnswer())})
	defer silent.Detach(t)

	forbidden := []string{
		peerLost.nodeID, peerNoMember.nodeID, ownerNodeID, peerLost.addr(), peerNoMember.addr(),
		"127.0.0.1", "localhost", "dispatch forward", "AEAD", "EOF", dispatchCalloutPathForTest,
	}
	// The whole message, character for character (§8.2's shape): the local
	// try's own code and text, then the lost hand-over under the member id
	// "-", each entry bracketed, N counted before collapsing.
	wantDetail := fmt.Sprintf("CALLOUT_FAILED: the callout could not be completed, got 2 failures: [member<%s>: DISPATCH_TIMEOUT: processor dispatch timed out after 300ms: no response], %s",
		silent.MemberID(), forwardFailedEntry)

	for i, door := range []string{"http", "grpc"} {
		t.Run(door, func(t *testing.T) {
			model := fmt.Sprintf("lostrs-%s-%s", sfx, door)
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("lostrs-wf-"+door,
				procSpec{"lostrs-proc", "SYNC", map[string]any{
					"calculationNodesTags": tag, "responseTimeoutMs": 300, "idempotent": true}}))
			peerLost.take()
			peerNoMember.take()

			switch door {
			case "http":
				_, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
				pd := assertProblem(t, status, body, http.StatusServiceUnavailable, "CALLOUT_FAILED", true)
				if pd.Detail != wantDetail {
					t.Errorf("detail = %q; want the literal %q", pd.Detail, wantDetail)
				}
				assertNoLeak(t, door, body, forbidden)
			case "grpc":
				env, err := h.createEntityGRPC(model, 1, workflowSampleModel)
				assertEnvelope(t, door, env, err, "CALLOUT_FAILED", true)
				if env.Error != nil && env.Error.Message != wantDetail {
					t.Errorf("message = %q; want the literal %q", env.Error.Message, wantDetail)
				}
				if env.Error != nil {
					assertNoLeak(t, door, env.Error.Message, forbidden)
				}
			}

			if lost := peerLost.take(); len(lost) != 1 {
				t.Fatalf("the peer that loses its answer opened %d hand-overs; want 1: %+v", len(lost), lost)
			} else if !lost[0].repeatSafe || lost[0].triesLeft != 3 {
				t.Errorf("hand-over = %+v; want repeatSafe true and triesLeft 3 (the local try used one of four)", lost[0])
			}
			if none := peerNoMember.take(); len(none) != 1 {
				t.Errorf("the peer with no compute member opened %d hand-overs; want 1 — an authenticated no-hand-off uses no try, so the peer after it is still asked: %+v", len(none), none)
			}
			if recs := silent.Received(); len(recs) != i+1 {
				t.Errorf("the owner's compute member received %d callouts in total; want %d (one per door): %v", len(recs), i+1, recs)
			}
			if n := h.countEntities(t, model); n != 0 {
				t.Errorf("%d entities committed by a failed SYNC create; want 0", n)
			}
			assertNoPeerFaults(t, peerLost, peerNoMember)
		})
	}
}

// assertNoPeerFaults fails t if a stand-in peer saw anything it should not
// have: a hand-over that did not authenticate or did not parse, or a request
// on another route.
func assertNoPeerFaults(t *testing.T, peers ...*standInPeer) {
	t.Helper()
	for _, p := range peers {
		for _, f := range p.faultList() {
			t.Errorf("stand-in peer %s: %s", p.nodeID, f)
		}
	}
}
