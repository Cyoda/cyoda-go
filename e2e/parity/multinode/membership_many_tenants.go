package multinode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// membership_many_tenants.go — a pnode hosting cnodes of many tenants must
// stay visible to its peers. Tenants and tags once rode in memberlist's node
// metadata, capped at 512 bytes; past it the pnode published nothing, every
// peer dropped it, and no callout was ever handed to it — for all its
// tenants, not only the one that tipped it over.

func init() {
	Register(NamedTest{Name: "Membership_ManyTenantsStayVisible", Fn: RunMembership_ManyTenantsStayVisible})
}

// membershipManyTenantsCount is the number of further tenants whose cnodes
// attach to node 0. R§12 sized the old per-node metadata's cap at about 8
// tenants with 36-character (uuid.NewString) tenant ids before the 512-byte
// memberlist limit was exceeded; this doubles that with margin.
const membershipManyTenantsCount = 16

func membershipWorkflow(wfName string) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "init", "next": "ACTIVE", "manual": false,
					"processors": []any{map[string]any{
						"type": "calculator", "name": "noop", "executionMode": "SYNC",
						"config": map[string]any{"calculationNodesTags": computeMemberTag},
					}},
				}}},
				"ACTIVE": map[string]any{},
			},
		}},
	})
	return string(b)
}

// membershipHostNode is the pnode the 16 further tenants' cnodes attach to.
// membershipDriveNode is a peer that hosts none of them: every callout this
// scenario drives is routed there, so it only succeeds if the peer still
// sees membershipHostNode and the tags it advertises.
const (
	membershipHostNode  = 0
	membershipDriveNode = 1
)

// RunMembership_ManyTenantsStayVisible attaches cnodes of
// membershipManyTenantsCount further tenants, three 40-character tags each,
// to node 0 — comfortably past the old 512-byte metadata cap on tenant ids
// and tags alone. Three callouts, each driven from node 1 (which hosts none
// of the attached cnodes), must still be handed over to node 0: one for the
// fixture's own built-in computeMemberTag, and — because a per-node tag-list
// defect could drop exactly one element (e.g. the first or the last) while
// leaving the rest and the built-in tag intact — one for the FIRST tenant
// attached and one for the LAST, each routed by that tenant's own unique tag
// and observed on that tenant's own compute-client handle. Node 0 must also
// still see itself.
func RunMembership_ManyTenantsStayVisible(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("needs ≥2 nodes, got %d", len(urls))
	}

	var first, last membershipAttachedTenant
	for i := range membershipManyTenantsCount {
		tags := make([]string, 3)
		for j := range tags {
			tags[j] = fmt.Sprintf("membership-%d-%d-%s", i, j, strings.Repeat("x", 25))
		}

		switch i {
		case 0, membershipManyTenantsCount - 1:
			// The FIRST and LAST tenant attached must be real fixture
			// tenants, able to authenticate an HTTP create against their
			// own tag below — a bare uuid.NewString() joins the compute
			// client's gRPC member fine but cannot mint an HTTP bearer.
			tenant := fixture.NewTenant(t)
			cc := StartComputeClientOrSkip(t, fixture, membershipHostNode, parity.ComputeClientSpec{
				TenantID: tenant.ID,
				Tags:     tags,
			})
			at := membershipAttachedTenant{tenant: tenant, tag: tags[0], cc: cc}
			if i == 0 {
				first = at
			} else {
				last = at
			}
		default:
			// The middle tenants only need to bulk out node 0's metadata;
			// nothing observes them, so a bare fresh tenant id is enough.
			StartComputeClientOrSkip(t, fixture, membershipHostNode, parity.ComputeClientSpec{
				TenantID: uuid.NewString(),
				Tags:     tags,
			})
		}
	}

	// The fixture's own built-in compute member, under the shared compute
	// tenant, must still be reachable too.
	tenant := fixture.ComputeTenant(t)
	const model = "membership-many-tenants"
	cbRouteSetupModel(t, client.NewClient(urls[membershipHostNode], tenant.Token), model,
		cbRouteSampleNoWriteback, membershipWorkflow("membership-many-tenants-wf"))

	owner := client.NewClient(urls[membershipDriveNode], tenant.Token)
	id, err := owner.CreateEntity(t, model, 1, `{"name":"e","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("create via node %d with %d tenants attached to node %d: %v — node %d has dropped out of node %d's view",
			membershipDriveNode, membershipManyTenantsCount, membershipHostNode, err, membershipHostNode, membershipDriveNode)
	}
	got, err := owner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "ACTIVE" {
		t.Fatalf("state = %q, want ACTIVE (the callout was not handed to node %d)", got.Meta.State, membershipHostNode)
	}

	// And node 0 must still see itself: a create on it runs the callout
	// locally too.
	if _, err := client.NewClient(urls[membershipHostNode], tenant.Token).CreateEntity(t, model, 1, `{"name":"e0","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("create via node %d: %v", membershipHostNode, err)
	}

	// The FIRST and LAST tenant attached must each still be individually
	// reachable by their own tag, not merely the built-in one above.
	membershipAssertOwnTagReachable(t, urls, "first", first)
	membershipAssertOwnTagReachable(t, urls, "last", last)
}

// membershipAttachedTenant is one of the FIRST/LAST tenants this scenario
// observes directly: its own tenant, the unique tag its cnode joined under,
// and the handle on that cnode.
type membershipAttachedTenant struct {
	tenant parity.Tenant
	tag    string
	cc     parity.ComputeClient
}

// membershipAssertOwnTagReachable drives one callout, tagged with at's own
// unique tag, from membershipDriveNode (which hosts no cnode for it) and
// asserts it was handed to at's own compute-client handle — following the
// pattern of RunComputeClient_AttachToNode (compute_client.go).
func membershipAssertOwnTagReachable(t *testing.T, urls []string, label string, at membershipAttachedTenant) {
	t.Helper()
	model := "membership-many-tenants-" + label
	cSetup := client.NewClient(urls[membershipHostNode], at.tenant.Token)
	cbRouteSetupModel(t, cSetup, model, cbRouteSampleNoWriteback,
		parity.ComputeClientWorkflow("membership-many-tenants-"+label+"-wf", "noop", at.tag, "", nil))

	c := client.NewClient(urls[membershipDriveNode], at.tenant.Token)
	if _, err := c.CreateEntity(t, model, 1, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("%s tenant attached: create via node %d routed by its own tag to node %d: %v",
			label, membershipDriveNode, membershipHostNode, err)
	}
	if got := parity.AwaitReceived(t, at.cc, 1, 5*time.Second); got[0].Name != "noop" || !got[0].PassPresent {
		t.Errorf("%s tenant attached: record = %+v", label, got[0])
	}
}
