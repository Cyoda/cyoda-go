package multinode

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_location_independent.go — a callback on a backend whose
// transactions are location-independent.
//
// A backend that coordinates transactions itself (the optional SPI capability
// spi.TransactionCoordinationCapable) lets any node serve a transaction's
// callbacks: the passes name no owner, so a callback is served on the node it
// lands on instead of being proxied to the node that began the transaction.
// None of the in-tree backends offers the capability; the scenario skips on
// them, and runs on a capable backend whose cluster fixture says so.

func init() {
	Register(NamedTest{Name: "Callback_LocationIndependent_ServedOnNonOwner", Fn: RunCallback_LocationIndependent_ServedOnNonOwner})
}

// LocationIndependentCapable is the OPTIONAL capability a cluster fixture
// implements when its backend coordinates transactions itself. It is not
// folded into MultiNodeFixture: the scenarios that need it type-assert it and
// skip when it is absent or reports false.
type LocationIndependentCapable interface {
	// LocationIndependent reports whether the cluster's backend offers the
	// transaction-coordination capability.
	LocationIndependent() bool
}

// liRequireCapable skips the scenario unless the fixture's backend is
// location-independent.
func liRequireCapable(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	li, ok := fixture.(LocationIndependentCapable)
	if !ok || !li.LocationIndependent() {
		t.Skip("backend is not location-independent")
	}
}

// RunCallback_LocationIndependent_ServedOnNonOwner drives a SYNC callback
// processor from node 1, the owner, which has no compute member: the callout
// is handed to node 0, whose member sends its callback to node 0. On a
// location-independent backend the callback's pass names no owner, so node 0
// serves it itself, and the callback's write commits with the owner's
// transaction: it is visible from every node and carries the owner's
// transaction id. The scenario would also pass if the callback were proxied
// to node 1: it proves the commit, not where the callback was served. That a
// pass without an owner is served where it lands is pinned by the routing unit
// tests (TestHTTPRouting_PassWithoutOwnerServedLocally,
// TestResolveNodeInfo_PassWithoutOwnerLocal in internal/cluster/proxy) and by
// TestDurable_CallbackServedOnAnotherPnodeCommitsWithOwner in internal/txcoord.
func RunCallback_LocationIndependent_ServedOnNonOwner(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	liRequireCapable(t, fixture)
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("a callback served on a non-owner needs ≥2 nodes, got %d", len(urls))
	}
	tenant := fixture.ComputeTenant(t)
	cSetup := client.NewClient(urls[0], tenant.Token)

	const secondary = "cbli-secondary"
	const primary = "cbli-primary"
	const marker = "cbli-ok"
	cbRouteSetupModel(t, cSetup, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)
	cbRouteSetupModel(t, cSetup, primary, cbRouteSampleCreateSecondary,
		cbRoutePrimaryWorkflow("cbli-wf", "cb-create-secondary", cbRouteContext(secondary, marker)))

	const ownerIdx = 1
	cOwner := client.NewClient(urls[ownerIdx], tenant.Token)
	primaryID, err := cOwner.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create via owner node %d: %v", ownerIdx, err)
	}
	prim, err := cOwner.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary via owner: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE", prim.Meta.State)
	}
	secID, _ := prim.Data["secondaryId"].(string)
	if secID == "" {
		t.Fatalf("primary missing secondaryId (the callback served on node 0 did not create the secondary): data=%+v", prim.Data)
	}

	for nodeIdx, url := range urls {
		hits, err := client.NewClient(url, tenant.Token).SyncSearch(t, secondary, 1, cbRouteStatusEquals(marker))
		if err != nil {
			t.Errorf("search marker via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 1 {
			t.Errorf("secondary search via node %d = %d; want 1 (the callback's write did not commit with the owner)", nodeIdx, len(hits))
		}
	}
	secParsed, err := uuid.Parse(secID)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secID, err)
	}
	cbRouteSameTxID(t, cOwner, primaryID, secParsed)
}
