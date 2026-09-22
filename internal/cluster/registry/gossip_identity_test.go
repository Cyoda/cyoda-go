package registry_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/registry"
)

// lockedBuffer collects log output written from memberlist's goroutines while
// the test goroutine reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureErrors routes the default logger's ERROR records to a buffer for the
// rest of the test. Nothing below ERROR is kept — memberlist's own output goes
// to slog at DEBUG and would drown the records under test.
func captureErrors(t *testing.T) *lockedBuffer {
	t.Helper()
	var out lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &out
}

// countLines counts the captured lines carrying every one of parts. The tests
// run several nodes in one process through one default logger, while the
// suppression each node applies is its own, so a count is only meaningful when
// it names the node that wrote the line.
func countLines(logged string, parts ...string) int {
	n := 0
	for _, line := range strings.Split(logged, "\n") {
		if line == "" {
			continue
		}
		matched := true
		for _, p := range parts {
			if !strings.Contains(line, p) {
				matched = false
				break
			}
		}
		if matched {
			n++
		}
	}
	return n
}

// refusalBy matches the ERROR line a node writes when it refuses the exchange
// that would establish a duplicate. The claiming address is the writer's own,
// so it names which node wrote it.
func refusalBy(addr string) []string {
	return []string{`msg="` + wantRefusalLogMsg + `"`, "claimedBy=" + addr}
}

// TestGossipRegistry_DuplicateNodeIDRefusesToStart is the reason the check
// exists: the node id seals a hand-over for the node it is sent to, so a
// second node answering to one id can open the first one's hand-overs. A
// duplicate that is still there when the startup budget runs out must not
// start, and must say which setting is wrong; the node already holding the id
// keeps serving.
func TestGossipRegistry_DuplicateNodeIDRefusesToStart(t *testing.T) {
	logged := captureErrors(t)

	incumbent := startGossip(t, gossipCfg("dup-id-node"))

	duplicate, err := registry.NewGossip(gossipCfg("dup-id-node", addrOf(incumbent)))
	if err != nil {
		t.Fatalf("NewGossip duplicate: %v", err)
	}
	captureAddr(duplicate)
	t.Cleanup(func() { _ = duplicate.Deregister(context.Background(), "dup-id-node") })

	// The incumbent holds the id for the whole of this budget, so every
	// attempt inside it finds the same answer and the last one fails closed.
	const budget = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	err = duplicate.Register(ctx, "dup-id-node", "http://dup-id-node.test:8080")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Register: expected the duplicate to refuse to start, got nil after %v", elapsed)
	}
	// It is refused at the end of the budget, not at the start of it: a record
	// that ages out inside the window is a node that may start.
	if elapsed < budget {
		t.Errorf("Register returned after %v; the whole %v budget must be given to the id coming free", elapsed, budget)
	}
	if elapsed > budget+3*time.Second {
		t.Errorf("Register took %v; it must fail closed once the %v budget is spent", elapsed, budget)
	}
	// The setting, the address, the seed — and the other cause of the same
	// refusal, since nothing separates an impostor from a record of this
	// node's own previous life left behind by a crash.
	for _, want := range []string{"CYODA_NODE_ID", "dup-id-node", addrOf(incumbent), "crash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Register err = %v; want it to name %q", err, want)
		}
	}

	// The incumbent is not the misconfigured node and keeps serving.
	if _, ok := nodeIn(t, incumbent, "dup-id-node"); !ok {
		t.Error("the incumbent dropped itself from its own view")
	}
	addr, alive, err := incumbent.Lookup(context.Background(), "dup-id-node")
	if err != nil {
		t.Fatalf("incumbent Lookup: %v", err)
	}
	if !alive || addr != "http://dup-id-node.test:8080" {
		t.Errorf("incumbent Lookup = %q, %v; want its own address, alive", addr, alive)
	}

	// Both nodes tell the operator: the one that refused to start and the one
	// that refused the exchange establishing the duplicate. Neither is silent,
	// and each writes its own line once.
	eventually(t, 5*time.Second, "both nodes log the duplicate at ERROR", func() bool {
		return countLines(logged.String(), refusalBy(addrOf(incumbent))...) == 1 &&
			countLines(logged.String(), refusalBy(addrOf(duplicate))...) == 1
	})
}

// TestGossipRegistry_RestartUnderDepartedNodeID covers the exclusion that
// makes the check safe: a node that left gracefully does not hold its name, so
// a restart under the same id is the documented, intended case.
func TestGossipRegistry_RestartUnderDepartedNodeID(t *testing.T) {
	first := startGossip(t, gossipCfg("restarting-node"))
	watcher := startGossipSeededBy(t, "restart-watcher", first)

	eventually(t, 5*time.Second, "the watcher sees the restarting node", func() bool {
		_, ok := nodeIn(t, watcher, "restarting-node")
		return ok
	})

	firstAddr := addrOf(first)
	if err := first.Deregister(context.Background(), "restarting-node"); err != nil {
		t.Fatalf("Deregister first: %v", err)
	}
	eventually(t, 5*time.Second, "the watcher sees the node leave", func() bool {
		_, ok := nodeIn(t, watcher, "restarting-node")
		return !ok
	})

	// The watcher still holds the departed record, now in state "left"; the
	// same id coming back at a new address must be admitted.
	second := newGossipAtAnotherAddress(t, gossipCfg("restarting-node", addrOf(watcher)), firstAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := second.Register(ctx, "restarting-node", "http://restarting-node.test:8080"); err != nil {
		t.Fatalf("Register second: a restart under the id of a node that left must be admitted: %v", err)
	}
}

// TestGossipRegistry_StaleRecordAgesOutAndTheNodeStarts is the ordinary case
// the refusal must not turn into a crash loop: a node crashes, comes back at
// another address, and its peers still hold the record of its previous life.
// Nothing tells that record from a second node's, so the first attempts are
// refused — but the peers reap it within the startup budget, and the attempt
// made after that is the one that decides.
func TestGossipRegistry_StaleRecordAgesOutAndTheNodeStarts(t *testing.T) {
	crashing := startGossip(t, gossipCfg("crashing-node"))
	watcher := startGossipSeededBy(t, "crash-watcher", crashing)

	eventually(t, 5*time.Second, "the watcher sees the node that is about to crash", func() bool {
		_, ok := nodeIn(t, watcher, "crashing-node")
		return ok
	})

	crashedAddr := addrOf(crashing)
	crashing.Crash()

	// The watcher was told nothing, so it still holds the record as alive:
	// only its own failure detector will reap it.
	second := newGossipAtAnotherAddress(t, gossipCfg("crashing-node", addrOf(watcher)), crashedAddr)

	// Long enough for memberlist's probe and suspicion timers to run their
	// course, which is what the node is waiting for.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	if err := second.Register(ctx, "crashing-node", "http://crashing-node.test:8080"); err != nil {
		t.Fatalf("Register second: the stale record aged out inside the budget and the node must be let in: %v", err)
	}
	t.Logf("admitted %v after the crash", time.Since(start))
}

// TestGossipRegistry_NoSeedsStartsWithoutJoin pins the other end of the check:
// with no seeds there is no join exchange and nothing to check against, and
// the node is a cluster of one.
func TestGossipRegistry_NoSeedsStartsWithoutJoin(t *testing.T) {
	r, err := registry.NewGossip(gossipCfg("lone-node"))
	if err != nil {
		t.Fatalf("NewGossip: %v", err)
	}
	t.Cleanup(func() { _ = r.Deregister(context.Background(), "lone-node") })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Register(ctx, "lone-node", "http://lone-node.test:8080"); err != nil {
		t.Fatalf("Register: a node with no seeds must start: %v", err)
	}
}

// TestGossipRegistry_ConflictLoggedOnceAtError covers the half the join check
// cannot: a third node that only witnesses the duplicate. It is not the
// misconfigured one, so it keeps serving and says so at ERROR — once per
// address, however many gossip messages carry the record.
func TestGossipRegistry_ConflictLoggedOnceAtError(t *testing.T) {
	holder := startGossip(t, gossipCfg("witnessed-id"))
	watcher := startGossipSeededBy(t, "conflict-watcher", holder)

	eventually(t, 5*time.Second, "the watcher sees the id's holder", func() bool {
		_, ok := nodeIn(t, watcher, "witnessed-id")
		return ok
	})

	// Only now: the watcher's own ERROR lines are what the test counts.
	logged := captureErrors(t)

	// The claimer must answer at the same address both times below — the
	// point of the second claim is that a repeat from that address is not
	// logged again — so its port is discovered once and reused, rather than
	// left to a fresh ephemeral pick on every call.
	claimerPort := freePort(t)
	claimerAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(claimerPort))
	claim := func() {
		t.Helper()
		claimer, err := registry.NewGossip(gossipCfgAt("witnessed-id", claimerPort, addrOf(watcher)))
		if err != nil {
			t.Fatalf("NewGossip claimer: %v", err)
		}
		defer func() { _ = claimer.Deregister(context.Background(), "witnessed-id") }()
		// A short budget: the claimer retries inside it, and the holder is
		// there for all of it, so the verdict is the same at every attempt.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := claimer.Register(ctx, "witnessed-id", "http://witnessed-id.test:8080"); err == nil {
			t.Fatal("Register claimer: expected the second holder of the id to refuse to start")
		}
	}

	// Only the watcher can write this line for this pair of addresses, and the
	// suppression under test is the watcher's own.
	witnessed := func() int {
		return countLines(logged.String(),
			`msg="`+wantConflictLogMsg+`"`,
			"nodeId=witnessed-id",
			"heldBy="+addrOf(holder),
			"claimedBy="+claimerAddr,
		)
	}

	claim()
	eventually(t, 5*time.Second, "the watcher logs the conflict", func() bool {
		return witnessed() == 1
	})

	// A second appearance of the same id at the same address is the same
	// fact, and is not logged again by the node that already reported it.
	claim()
	time.Sleep(2 * time.Second)
	if n := witnessed(); n != 1 {
		t.Errorf("the conflict was logged %d times; a repeat from the same address must be suppressed:\n%s", n, logged.String())
	}

	// Logging it is all the witness does. It was serving before the conflict
	// and is serving after it.
	if _, ok := nodeIn(t, watcher, "conflict-watcher"); !ok {
		t.Error("the witness dropped itself from its own view")
	}
	if _, ok := nodeIn(t, watcher, "witnessed-id"); !ok {
		t.Error("the witness dropped the id's rightful holder over the conflict")
	}
}

// TestGossipRegistry_DuplicateFoundByInboundExchangeRefusesToStart covers a
// node that learns the id is taken from an exchange a peer started, while its
// own join to a healthy seed succeeds. Nothing that join returns carries the
// finding, so a check that only reads it when the join failed starts the node
// under a duplicated id.
func TestGossipRegistry_DuplicateFoundByInboundExchangeRefusesToStart(t *testing.T) {
	logged := captureErrors(t)

	// A seed under an id of its own: the joiner's own join has nothing to
	// object to.
	seedNode := startGossip(t, gossipCfg("inbound-seed"))

	// Listening, but not joined: Register has not been called yet. The window
	// is long enough that the impostor, which keeps trying on a backoff, is
	// certain to claim the id again inside it.
	joinerCfg := gossipCfg("inbound-dup", addrOf(seedNode))
	joinerCfg.StabilityWindow = 2 * time.Second
	joiner, err := registry.NewGossip(joinerCfg)
	if err != nil {
		t.Fatalf("NewGossip joiner: %v", err)
	}
	captureAddr(joiner)
	t.Cleanup(func() { _ = joiner.Deregister(context.Background(), "inbound-dup") })

	impostor, err := registry.NewGossip(gossipCfg("inbound-dup", addrOf(joiner)))
	if err != nil {
		t.Fatalf("NewGossip impostor: %v", err)
	}
	captureAddr(impostor)
	t.Cleanup(func() { _ = impostor.Deregister(context.Background(), "inbound-dup") })

	// The impostor holds the claim open for as long as the joiner is
	// starting. A finding is judged per attempt, so what must keep the joiner
	// out is a claim that is still live, not one that has been and gone.
	impostorCtx, stopImpostor := context.WithCancel(context.Background())
	defer stopImpostor()
	impostorDone := make(chan error, 1)
	go func() {
		impostorDone <- impostor.Register(impostorCtx, "inbound-dup", "http://inbound-dup.test:8080")
	}()

	// The joiner has run its merge delegate on that inbound exchange.
	eventually(t, 5*time.Second, "the joiner refuses the inbound exchange", func() bool {
		return countLines(logged.String(), refusalBy(addrOf(impostor))...) == 1
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err = joiner.Register(ctx, "inbound-dup", "http://inbound-dup.test:8080")
	if err == nil {
		t.Fatal("Register joiner: it started although another node holds its id")
	}
	if !strings.Contains(err.Error(), "CYODA_NODE_ID") {
		t.Errorf("Register joiner err = %v; want it to name CYODA_NODE_ID", err)
	}

	stopImpostor()
	if err := <-impostorDone; err == nil {
		t.Error("Register impostor: expected the second holder of the id to refuse to start")
	}
}

// TestGossipRegistry_DuplicateFoundWhileJoinSucceedsRefusesToStart covers the
// other way a join returns success over a refused exchange: one seed name that
// resolves to several addresses. memberlist contacts each of them and clears
// the errors it collected as soon as one answers, so the exchange that refused
// leaves no trace in what Join returns.
func TestGossipRegistry_DuplicateFoundWhileJoinSucceedsRefusesToStart(t *testing.T) {
	ips, err := net.LookupIP("localhost")
	if err != nil {
		t.Skipf("localhost does not resolve: %v", err)
	}
	var v4, v6 bool
	for _, ip := range ips {
		if ip.To4() != nil {
			v4 = true
		} else {
			v6 = true
		}
	}
	if !v4 || !v6 {
		t.Skipf("localhost resolves to %v; this test needs both loopback families", ips)
	}

	// The two nodes below must share one port across the two address
	// families — discovered, not literal, so two test processes on the same
	// machine cannot collide on it either.
	port := freePort(t)

	// The duplicate's holder answers on one of the two addresses.
	holderCfg := gossipCfgAt("resolved-dup", port)
	holderCfg.BindAddr = "::1"
	holder, err := registry.NewGossip(holderCfg)
	if err != nil {
		t.Skipf("no IPv6 loopback to bind: %v", err)
	}
	t.Cleanup(func() { _ = holder.Deregister(context.Background(), "resolved-dup") })

	// A node under an id of its own answers on the other, so the join as a
	// whole succeeds.
	startGossip(t, gossipCfgAt("resolved-seed", port))

	joiner, err := registry.NewGossip(gossipCfg("resolved-dup", net.JoinHostPort("localhost", strconv.Itoa(port))))
	if err != nil {
		t.Fatalf("NewGossip joiner: %v", err)
	}
	t.Cleanup(func() { _ = joiner.Deregister(context.Background(), "resolved-dup") })

	// The holder answers for the whole budget, so every attempt inside it
	// finds the same answer.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = joiner.Register(ctx, "resolved-dup", "http://resolved-dup.test:8080")
	if err == nil {
		t.Fatal("Register joiner: it started although one of the seed's addresses holds its id")
	}
	if !strings.Contains(err.Error(), "CYODA_NODE_ID") {
		t.Errorf("Register joiner err = %v; want it to name CYODA_NODE_ID", err)
	}
}

// TestGossipRegistry_DuplicateFoundDuringStabilityWindowRefusesToStart covers
// the last stretch of Register: the join is done and the node is waiting for
// the cluster view to settle when a second holder of its id appears.
func TestGossipRegistry_DuplicateFoundDuringStabilityWindowRefusesToStart(t *testing.T) {
	seed := startGossip(t, gossipCfg("stability-seed"))

	cfg := gossipCfg("stability-dup", addrOf(seed))
	cfg.StabilityWindow = 3 * time.Second
	joiner, err := registry.NewGossip(cfg)
	if err != nil {
		t.Fatalf("NewGossip joiner: %v", err)
	}
	captureAddr(joiner)
	t.Cleanup(func() { _ = joiner.Deregister(context.Background(), "stability-dup") })

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	registered := make(chan error, 1)
	go func() {
		registered <- joiner.Register(ctx, "stability-dup", "http://stability-dup.test:8080")
	}()

	// The seed holding the joiner is proof the join itself is through: what
	// follows lands inside the stability window.
	eventually(t, 10*time.Second, "the seed sees the joiner", func() bool {
		_, ok := nodeIn(t, seed, "stability-dup")
		return ok
	})

	impostor, err := registry.NewGossip(gossipCfg("stability-dup", addrOf(joiner)))
	if err != nil {
		t.Fatalf("NewGossip impostor: %v", err)
	}
	t.Cleanup(func() { _ = impostor.Deregister(context.Background(), "stability-dup") })

	// The impostor keeps claiming the id until the joiner has given up, so
	// the claim is live at the end of the joiner's budget and not only inside
	// one of its attempts.
	impostorCtx, stopImpostor := context.WithCancel(context.Background())
	defer stopImpostor()
	impostorDone := make(chan error, 1)
	go func() {
		impostorDone <- impostor.Register(impostorCtx, "stability-dup", "http://stability-dup.test:8080")
	}()

	select {
	case err := <-registered:
		if err == nil {
			t.Fatal("Register joiner: it started although another node claimed its id while it waited")
		}
		if !strings.Contains(err.Error(), "CYODA_NODE_ID") {
			t.Errorf("Register joiner err = %v; want it to name CYODA_NODE_ID", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Register joiner never returned")
	}

	stopImpostor()
	if err := <-impostorDone; err == nil {
		t.Error("Register impostor: expected the second holder of the id to refuse to start")
	}
}

// The ERROR messages a node writes about a duplicate id: one when it refuses
// the exchange that would establish it, one when it only witnesses it. They
// are matched as text because the log line is the operator-facing contract.
const (
	wantConflictLogMsg = "two nodes hold the same CYODA_NODE_ID"
	wantRefusalLogMsg  = "refusing a cluster merge: two nodes hold the same CYODA_NODE_ID"
)
