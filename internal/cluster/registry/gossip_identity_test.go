package registry_test

import (
	"bytes"
	"context"
	"log/slog"
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
	for _, want := range []string{"CYODA_NODE_ID", "dup-id-node", "127.0.0.1:29946"} {
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
	// that refused the exchange establishing the duplicate. Neither is silent.
	eventually(t, 5*time.Second, "both nodes log the duplicate at ERROR", func() bool {
		return strings.Count(logged.String(), `msg="`+wantRefusalLogMsg+`"`) == 2
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

	witnessed := func() int {
		return strings.Count(logged.String(), `msg="`+wantConflictLogMsg+`"`)
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

// The ERROR messages a node writes about a duplicate id: one when it refuses
// the exchange that would establish it, one when it only witnesses it. They
// are matched as text because the log line is the operator-facing contract.
const (
	wantConflictLogMsg = "two nodes hold the same CYODA_NODE_ID"
	wantRefusalLogMsg  = "refusing a cluster merge: two nodes hold the same CYODA_NODE_ID"
)
