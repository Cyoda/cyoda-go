package registry_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
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
// second node answering to one id can open the first one's hand-overs. The
// node that finds the id taken must not start, and must say which setting is
// wrong; the node already holding it keeps serving.
func TestGossipRegistry_DuplicateNodeIDRefusesToStart(t *testing.T) {
	logged := captureErrors(t)

	const (
		incumbentPort = 29946
		duplicatePort = 29947
		seed          = "127.0.0.1:29946"
	)

	incumbent := startGossip(t, gossipCfg("dup-id-node", incumbentPort))

	duplicate, err := registry.NewGossip(gossipCfg("dup-id-node", duplicatePort, seed))
	if err != nil {
		t.Fatalf("NewGossip duplicate: %v", err)
	}
	t.Cleanup(func() { _ = duplicate.Deregister(context.Background(), "dup-id-node") })

	// A generous deadline: the point is that Register does not spend it. A
	// duplicate id is not a condition the next join attempt clears.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	err = duplicate.Register(ctx, "dup-id-node", "http://dup-id-node.test:8080")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Register: expected the duplicate to refuse to start, got nil after %v", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Register took %v; a duplicate id must be refused at once, not retried", elapsed)
	}
	// The setting, the address, the seed — and the other cause of the same
	// refusal, since nothing separates an impostor from a record of this
	// node's own previous life left behind by a crash.
	for _, want := range []string{"CYODA_NODE_ID", "dup-id-node", "127.0.0.1:29946", "crash"} {
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
		return countLines(logged.String(), refusalBy("127.0.0.1:29946")...) == 1 &&
			countLines(logged.String(), refusalBy("127.0.0.1:29947")...) == 1
	})
}

// TestGossipRegistry_RestartUnderDepartedNodeID covers the exclusion that
// makes the check safe: a node that left gracefully does not hold its name, so
// a restart under the same id is the documented, intended case.
func TestGossipRegistry_RestartUnderDepartedNodeID(t *testing.T) {
	const (
		firstPort   = 29948
		watcherPort = 29949
		secondPort  = 29950
	)

	first := startGossip(t, gossipCfg("restarting-node", firstPort))
	watcher := startGossip(t, gossipCfg("restart-watcher", watcherPort, "127.0.0.1:29948"))

	eventually(t, 5*time.Second, "the watcher sees the restarting node", func() bool {
		_, ok := nodeIn(t, watcher, "restarting-node")
		return ok
	})

	if err := first.Deregister(context.Background(), "restarting-node"); err != nil {
		t.Fatalf("Deregister first: %v", err)
	}
	eventually(t, 5*time.Second, "the watcher sees the node leave", func() bool {
		_, ok := nodeIn(t, watcher, "restarting-node")
		return !ok
	})

	// The watcher still holds the departed record, now in state "left"; the
	// same id coming back at a new address must be admitted.
	second, err := registry.NewGossip(gossipCfg("restarting-node", secondPort, "127.0.0.1:29949"))
	if err != nil {
		t.Fatalf("NewGossip second: %v", err)
	}
	t.Cleanup(func() { _ = second.Deregister(context.Background(), "restarting-node") })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := second.Register(ctx, "restarting-node", "http://restarting-node.test:8080"); err != nil {
		t.Fatalf("Register second: a restart under the id of a node that left must be admitted: %v", err)
	}
}

// TestGossipRegistry_NoSeedsStartsWithoutJoin pins the other end of the check:
// with no seeds there is no join exchange and nothing to check against, and
// the node is a cluster of one.
func TestGossipRegistry_NoSeedsStartsWithoutJoin(t *testing.T) {
	r, err := registry.NewGossip(gossipCfg("lone-node", 29951))
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
	const (
		holderPort  = 29952
		watcherPort = 29953
		claimerPort = 29954
	)

	startGossip(t, gossipCfg("witnessed-id", holderPort))
	watcher := startGossip(t, gossipCfg("conflict-watcher", watcherPort, "127.0.0.1:29952"))

	eventually(t, 5*time.Second, "the watcher sees the id's holder", func() bool {
		_, ok := nodeIn(t, watcher, "witnessed-id")
		return ok
	})

	// Only now: the watcher's own ERROR lines are what the test counts.
	logged := captureErrors(t)

	claim := func() {
		t.Helper()
		claimer, err := registry.NewGossip(gossipCfg("witnessed-id", claimerPort, "127.0.0.1:29953"))
		if err != nil {
			t.Fatalf("NewGossip claimer: %v", err)
		}
		defer func() { _ = claimer.Deregister(context.Background(), "witnessed-id") }()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
			"heldBy=127.0.0.1:29952",
			"claimedBy=127.0.0.1:29954",
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
}

// TestGossipRegistry_DuplicateFoundByInboundExchangeRefusesToStart covers a
// node that learns the id is taken from an exchange a peer started, while its
// own join to a healthy seed succeeds. Nothing that join returns carries the
// finding, so a check that only reads it when the join failed starts the node
// under a duplicated id.
func TestGossipRegistry_DuplicateFoundByInboundExchangeRefusesToStart(t *testing.T) {
	logged := captureErrors(t)

	const (
		seedPort     = 29955
		joinerPort   = 29956
		impostorPort = 29957
	)

	// A seed under an id of its own: the joiner's own join has nothing to
	// object to.
	startGossip(t, gossipCfg("inbound-seed", seedPort))

	// Listening, but not joined: Register has not been called yet.
	joiner, err := registry.NewGossip(gossipCfg("inbound-dup", joinerPort, "127.0.0.1:29955"))
	if err != nil {
		t.Fatalf("NewGossip joiner: %v", err)
	}
	t.Cleanup(func() { _ = joiner.Deregister(context.Background(), "inbound-dup") })

	impostor, err := registry.NewGossip(gossipCfg("inbound-dup", impostorPort, "127.0.0.1:29956"))
	if err != nil {
		t.Fatalf("NewGossip impostor: %v", err)
	}
	t.Cleanup(func() { _ = impostor.Deregister(context.Background(), "inbound-dup") })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := impostor.Register(ctx, "inbound-dup", "http://inbound-dup.test:8080"); err == nil {
		t.Fatal("Register impostor: expected the second holder of the id to refuse to start")
	}

	// The joiner has run its merge delegate on that inbound exchange.
	eventually(t, 5*time.Second, "the joiner refuses the inbound exchange", func() bool {
		return countLines(logged.String(), refusalBy("127.0.0.1:29956")...) == 1
	})

	err = joiner.Register(ctx, "inbound-dup", "http://inbound-dup.test:8080")
	if err == nil {
		t.Fatal("Register joiner: it started although another node holds its id")
	}
	if !strings.Contains(err.Error(), "CYODA_NODE_ID") {
		t.Errorf("Register joiner err = %v; want it to name CYODA_NODE_ID", err)
	}
}

// TestGossipRegistry_DuplicateFoundWhileJoinSucceedsRefusesToStart covers the
// other way a join returns success over a refused exchange: one seed name that
// resolves to several addresses. memberlist contacts each of them and clears
// the errors it collected as soon as one answers, so the exchange that refused
// leaves no trace in what Join returns.
func TestGossipRegistry_DuplicateFoundWhileJoinSucceedsRefusesToStart(t *testing.T) {
	const port = 29961

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

	// The duplicate's holder answers on one of the two addresses.
	holderCfg := gossipCfg("resolved-dup", port)
	holderCfg.BindAddr = "::1"
	holder, err := registry.NewGossip(holderCfg)
	if err != nil {
		t.Skipf("no IPv6 loopback to bind: %v", err)
	}
	t.Cleanup(func() { _ = holder.Deregister(context.Background(), "resolved-dup") })

	// A node under an id of its own answers on the other, so the join as a
	// whole succeeds.
	startGossip(t, gossipCfg("resolved-seed", port))

	joiner, err := registry.NewGossip(gossipCfg("resolved-dup", 29962, "localhost:29961"))
	if err != nil {
		t.Fatalf("NewGossip joiner: %v", err)
	}
	t.Cleanup(func() { _ = joiner.Deregister(context.Background(), "resolved-dup") })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	const (
		seedPort     = 29958
		joinerPort   = 29959
		impostorPort = 29960
	)

	seed := startGossip(t, gossipCfg("stability-seed", seedPort))

	cfg := gossipCfg("stability-dup", joinerPort, "127.0.0.1:29958")
	cfg.StabilityWindow = 5 * time.Second
	joiner, err := registry.NewGossip(cfg)
	if err != nil {
		t.Fatalf("NewGossip joiner: %v", err)
	}
	t.Cleanup(func() { _ = joiner.Deregister(context.Background(), "stability-dup") })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	impostor, err := registry.NewGossip(gossipCfg("stability-dup", impostorPort, "127.0.0.1:29959"))
	if err != nil {
		t.Fatalf("NewGossip impostor: %v", err)
	}
	t.Cleanup(func() { _ = impostor.Deregister(context.Background(), "stability-dup") })
	if err := impostor.Register(ctx, "stability-dup", "http://stability-dup.test:8080"); err == nil {
		t.Fatal("Register impostor: expected the second holder of the id to refuse to start")
	}

	select {
	case err := <-registered:
		if err == nil {
			t.Fatal("Register joiner: it started although another node claimed its id while it waited")
		}
		if !strings.Contains(err.Error(), "CYODA_NODE_ID") {
			t.Errorf("Register joiner err = %v; want it to name CYODA_NODE_ID", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("Register joiner never returned")
	}
}

// The ERROR messages a node writes about a duplicate id: one when it refuses
// the exchange that would establish it, one when it only witnesses it. They
// are matched as text because the log line is the operator-facing contract.
const (
	wantConflictLogMsg = "two nodes hold the same CYODA_NODE_ID"
	wantRefusalLogMsg  = "refusing a cluster merge: two nodes hold the same CYODA_NODE_ID"
)
