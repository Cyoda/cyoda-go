package multinode

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

// RunMembership_ManyTenantsStayVisible attaches cnodes of
// membershipManyTenantsCount further tenants, three 40-character tags each,
// to node 0 — comfortably past the old 512-byte metadata cap on tenant ids
// and tags alone. A callout driven from node 1 for the shared compute
// tenant's computeMemberTag must still be handed over to node 0, and node 0
// must still see itself.
func RunMembership_ManyTenantsStayVisible(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("needs ≥2 nodes, got %d", len(urls))
	}

	for i := range membershipManyTenantsCount {
		tags := make([]string, 3)
		for j := range tags {
			tags[j] = fmt.Sprintf("membership-%d-%d-%s", i, j, strings.Repeat("x", 25))
		}
		StartComputeClientOrSkip(t, fixture, 0, parity.ComputeClientSpec{
			TenantID: uuid.NewString(),
			Tags:     tags,
		})
	}

	tenant := fixture.ComputeTenant(t)
	const model = "membership-many-tenants"
	cbRouteSetupModel(t, client.NewClient(urls[0], tenant.Token), model,
		cbRouteSampleNoWriteback, membershipWorkflow("membership-many-tenants-wf"))

	// Node 1 hosts no cnode for computeMemberTag: the callout succeeds only if
	// node 1 still sees node 0 and the tag it hosts.
	owner := client.NewClient(urls[1], tenant.Token)
	id, err := owner.CreateEntity(t, model, 1, `{"name":"e","amount":1,"status":"new"}`)
	if err != nil {
		t.Fatalf("create via node 1 with %d tenants attached to node 0: %v — node 0 has dropped out of node 1's view", membershipManyTenantsCount, err)
	}
	got, err := owner.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "ACTIVE" {
		t.Fatalf("state = %q, want ACTIVE (the callout was not handed to node 0)", got.Meta.State)
	}

	// And node 0 must still see itself: a create on it runs the callout
	// locally too.
	if _, err := client.NewClient(urls[0], tenant.Token).CreateEntity(t, model, 1, `{"name":"e0","amount":1,"status":"new"}`); err != nil {
		t.Fatalf("create via node 0: %v", err)
	}
}
