package multinode

import (
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(NamedTest{Name: "ComputeClient_AttachToNode", Fn: RunComputeClient_AttachToNode})
}

// ComputeClientCapable is the OPTIONAL capability a cluster fixture implements
// to start a further compute client attached to a chosen pnode. Like
// AttributionCapable it is not folded into MultiNodeFixture: a cluster-capable
// backend that has not wired it still consumes the shared registry, and the
// scenarios that need it skip there.
type ComputeClientCapable interface {
	// StartComputeClient starts a compute client attached to pnode node (its
	// gRPC endpoint for the stream, its HTTP base for callbacks) and returns
	// once it has joined. Implementations MUST call t.Helper() and t.Fatal on
	// failure, including an out-of-range node.
	StartComputeClient(t *testing.T, node int, spec parity.ComputeClientSpec) parity.ComputeClient
}

// StartComputeClientOrSkip starts a compute client on pnode node for this
// scenario, or skips the scenario when the fixture lacks the capability. The
// client is stopped when the scenario ends.
func StartComputeClientOrSkip(t *testing.T, fixture MultiNodeFixture, node int, spec parity.ComputeClientSpec) parity.ComputeClient {
	t.Helper()
	cap, ok := fixture.(ComputeClientCapable)
	if !ok {
		t.Skip("cluster fixture cannot start further compute clients; scenario pending on this backend")
	}
	cc := cap.StartComputeClient(t, node, spec)
	t.Cleanup(cc.Stop)
	return cc
}

// RunComputeClient_AttachToNode proves a compute client started on one pnode
// is given work that arrives at another pnode and work that arrives at its own.
func RunComputeClient_AttachToNode(t *testing.T, fixture MultiNodeFixture) {
	if _, ok := fixture.(ComputeClientCapable); !ok {
		t.Skip("cluster fixture cannot start further compute clients; scenario pending on this backend")
	}
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("needs at least 2 pnodes, got %d", len(urls))
	}
	tenant := fixture.NewTenant(t)
	const model, tag = "h-mn-cc-attach", "h-mn-cc"
	host := len(urls) - 1

	cbRouteSetupModel(t, client.NewClient(urls[0], tenant.Token), model, cbRouteSampleNoWriteback,
		parity.ComputeClientWorkflow("h-mn-cc-wf", "noop", tag, "", nil))

	cc := StartComputeClientOrSkip(t, fixture, host, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})

	// Arrives at pnode 0, which has no compute client for this tenant: the
	// work crosses to the hosting pnode. The wait for the tag to reach pnode 0
	// is the server's own (CYODA_DISPATCH_WAIT_TIMEOUT).
	if _, err := client.NewClient(urls[0], tenant.Token).CreateEntity(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("create via pnode 0 (work crosses to pnode %d): %v", host, err)
	}
	if got := parity.AwaitReceived(t, cc, 1, 5*time.Second); got[0].Name != "noop" || !got[0].PassPresent {
		t.Errorf("record = %+v", got[0])
	}

	// Arrives at the hosting pnode itself: served locally.
	if _, err := client.NewClient(urls[host], tenant.Token).CreateEntity(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("create via the hosting pnode %d: %v", host, err)
	}
	if got := parity.AwaitReceived(t, cc, 2, 5*time.Second); got[1].Seq != 2 {
		t.Errorf("second record = %+v", got[1])
	}
}
