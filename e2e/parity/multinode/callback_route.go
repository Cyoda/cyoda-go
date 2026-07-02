package multinode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_route.go — feature #287 CROSS-NODE callback routing scenarios.
//
// These exercise the two cluster hops a compute-node callback can take when the
// transaction owner, the compute member, and the callback's landing node are all
// different cyoda-go nodes:
//
//  1. Forwarded dispatch A→B: the owner node (A) has no local compute member for
//     the processor's tag, so the ClusterDispatcher forwards the processor
//     dispatch to the peer (B) that advertises the tag. B's member runs while A's
//     transaction T is open, and the engine-minted cyodatxtoken carries A's
//     NodeID as owner.
//  2. Callback routing back to the owner: the member fires a callback presenting
//     that token. The callback lands on a NON-owner node, which reads the owner
//     NodeID from the token and reverse-proxies (HTTP) / forwards (gRPC) the
//     request to A, where it joins T.
//
// The cluster fixture wires this: the compute-test-client registers its member on
// node 0 under the tag "compute-test-client" (which only node 0 advertises), and
// its HTTP callback base points at node 0. Driving a transition from a NON-member
// node therefore forces both hops: dispatch forwards node→node-0, and the
// callback (to node 0, a non-owner for that transaction) proxies back to the
// owner. The fixture enables CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING so the
// dispatch forwarder accepts the loopback peers the fixture runs on.
//
// The token value is never logged or asserted (Gate 3); scenarios observe only
// entity state and derived data.

func init() {
	Register(
		NamedTest{Name: "Callback_ForwardedDispatch_HTTP", Fn: RunCallback_ForwardedDispatch_HTTP},
		NamedTest{Name: "Callback_ForwardedDispatch_GRPC", Fn: RunCallback_ForwardedDispatch_GRPC},
	)
}

// computeMemberTag is the tag the compute-test-client's member advertises (see
// cmd/compute-test-client/dispatch.go join event). Only the member-hosting node
// advertises it, so a processor requiring this tag routes specifically to that
// node — never to a memberless peer that would fail with no-matching-member.
const computeMemberTag = "compute-test-client"

// cbRouteContext builds the pass-through ProcessorConfig.context JSON-string the
// callback processors decode into {secondaryModel, secondaryVersion, marker}.
// Mirrors parity.cbContext (unexported there) for the multinode package.
func cbRouteContext(secondaryModel, marker string) string {
	inner, _ := json.Marshal(map[string]any{
		"secondaryModel":   secondaryModel,
		"secondaryVersion": 1,
		"marker":           marker,
	})
	quoted, _ := json.Marshal(string(inner))
	return string(quoted)
}

// cbRouteSecondaryWorkflow is a trivial NONE→STORED workflow (no processors) so a
// secondary entity created inside a joined callback runs a minimal cascade within
// T and never dispatches a nested processor.
const cbRouteSecondaryWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "cbroute-secondary-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false}]},
			"STORED": {}
		}
	}]
}`

// cbRoutePrimaryWorkflow builds a NONE→ACTIVE auto-transition workflow whose
// transition carries one callback processor pinned to computeMemberTag, so the
// dispatch is forwarded to the member-hosting node when driven from any other node.
func cbRoutePrimaryWorkflow(wfName, procName, contextValue string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": %q, "context": %s}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, wfName, procName, computeMemberTag, contextValue)
}

// cbRouteSetupModel imports+locks a model with the given sample doc, then imports
// the workflow. Setup is routed through node 0 (any node works — the model store
// is cluster-shared).
func cbRouteSetupModel(t *testing.T, c *client.Client, modelName, sampleDoc, workflowJSON string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, 1, sampleDoc); err != nil {
		t.Fatalf("ImportModel %s: %v", modelName, err)
	}
	if err := c.LockModel(t, modelName, 1); err != nil {
		t.Fatalf("LockModel %s: %v", modelName, err)
	}
	if err := c.ImportWorkflow(t, modelName, 1, workflowJSON); err != nil {
		t.Fatalf("ImportWorkflow %s: %v", modelName, err)
	}
}

// RunCallback_ForwardedDispatch_HTTP proves the full cross-node callback path over
// HTTP:
//
//   - The primary transition is driven from node 1 (the OWNER), which hosts no
//     compute member. Its processor requires the "compute-test-client" tag, so the
//     ClusterDispatcher forwards the dispatch to node 0 (A→B forward).
//   - Node 0's member fires an HTTP callback (X-Tx-Token owner=node 1) to node 0's
//     HTTP base. Node 0 is NOT the owner, so the tx-token proxy reverse-proxies the
//     request to node 1, where it joins T and writes the secondary.
//   - Success branch: the secondary is durable and visible from EVERY node (the
//     cross-node callback write committed atomically with node 1's transition).
//   - Failure branch (atomicity proof): the processor creates a secondary then
//     fails, so node 1's T rolls back and the secondary is gone cluster-wide. If
//     the callback had run in its own transaction, the doomed secondary would
//     survive — the marker search would return 1 instead of 0.
func RunCallback_ForwardedDispatch_HTTP(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("forwarded-dispatch callback routing needs ≥2 nodes, got %d", len(urls))
	}
	tenant := fixture.ComputeTenant(t)

	// Setup via node 0 (model store is cluster-shared).
	cSetup := client.NewClient(urls[0], tenant.Token)

	const secondary = "cbroute-http-secondary"
	const primaryOK = "cbroute-http-primary-ok"
	const primaryFail = "cbroute-http-primary-fail"
	const okMarker = "cbroute-http-ok"
	const doomedMarker = "cbroute-http-doomed"

	cbRouteSetupModel(t, cSetup, secondary, `{"name":"child","amount":1,"status":"new"}`, cbRouteSecondaryWorkflow)
	cbRouteSetupModel(t, cSetup, primaryOK, `{"name":"Test","amount":10,"status":"new"}`,
		cbRoutePrimaryWorkflow("cbroute-http-ok-wf", "cb-create-secondary", cbRouteContext(secondary, okMarker)))
	cbRouteSetupModel(t, cSetup, primaryFail, `{"name":"Test","amount":10,"status":"new"}`,
		cbRoutePrimaryWorkflow("cbroute-http-fail-wf", "cb-create-then-fail", cbRouteContext(secondary, doomedMarker)))

	// OWNER = node 1 (no local compute member → dispatch forwards to node 0).
	const ownerIdx = 1
	cOwner := client.NewClient(urls[ownerIdx], tenant.Token)

	// --- success branch ---
	primaryID, err := cOwner.CreateEntity(t, primaryOK, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create via owner node %d (forwarded dispatch + cross-node callback): %v", ownerIdx, err)
	}

	prim, err := cOwner.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary via owner: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE (cross-node cascade did not complete)", prim.Meta.State)
	}
	if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
		t.Errorf("forwarded SYNC dispatch: tokenWasEmpty=true; want false (owner token must survive the A→B forward)")
	}
	secID, _ := prim.Data["secondaryId"].(string)
	if secID == "" {
		t.Fatalf("primary missing secondaryId (cross-node callback did not create secondary): data=%+v", prim.Data)
	}

	// The secondary must be durable and identical from EVERY node — proves the
	// callback's write, proxied back to the owner and committed with T, is visible
	// cluster-wide via shared storage + gossip.
	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		hits, err := c.SyncSearch(t, secondary, 1, cbRouteStatusEquals(okMarker))
		if err != nil {
			t.Errorf("search ok-marker via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 1 {
			t.Errorf("ok-marker secondary search via node %d = %d; want 1 (cross-node callback write not durable/visible)", nodeIdx, len(hits))
		}
	}

	// --- failure branch (cross-node atomicity proof) ---
	status, body, err := cOwner.CreateEntityRaw(t, primaryFail, 1, `{"name":"parent2","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create (failure branch) transport: %v", err)
	}
	if status == http.StatusOK {
		t.Fatalf("primary create (failure branch) unexpectedly succeeded: %s", body)
	}

	// The doomed secondary the cross-node callback created inside T must be gone
	// from EVERY node — the owner's T rolled back and took it. A hit anywhere means
	// the callback write was NOT atomic with T across the node boundary.
	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		hits, err := c.SyncSearch(t, secondary, 1, cbRouteStatusEquals(doomedMarker))
		if err != nil {
			t.Errorf("search doomed-marker via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 0 {
			t.Fatalf("doomed secondary search via node %d = %d; want 0 — cross-node callback write was NOT rolled back with T", nodeIdx, len(hits))
		}
	}
}

// RunCallback_ForwardedDispatch_GRPC proves the full cross-node callback path
// over gRPC — the FIRST real two-node exercise of the Task 8/8b EntityManage
// forward transport and advertise-or-derive gRPC endpoint resolution:
//
//   - Same forwarded dispatch as the HTTP scenario: node 1 (OWNER, no member)
//     forwards the tagged processor dispatch to node 0 (A→B).
//   - Node 0's member fires a gRPC EntityManage(EntityCreateRequest) callback
//     presenting the tx-token (owner=node 1) as "tx-token" metadata. The call
//     lands on node 0's gRPC (a non-owner for this transaction); node 0's
//     txRouteInterceptor resolves the owner and FORWARDS the EntityManage to
//     node 1 over gRPC (B→A). Node 1 joins T and writes the secondary.
//   - Success branch: the secondary is durable and visible cluster-wide.
//   - Failure branch: the processor fails after the gRPC callback write, so
//     node 1's T rolls back and the secondary is gone from every node — the
//     cross-node gRPC-callback atomicity proof.
func RunCallback_ForwardedDispatch_GRPC(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("forwarded-dispatch gRPC callback routing needs ≥2 nodes, got %d", len(urls))
	}
	tenant := fixture.ComputeTenant(t)

	cSetup := client.NewClient(urls[0], tenant.Token)

	const secondary = "cbroute-grpc-secondary"
	const primaryOK = "cbroute-grpc-primary-ok"
	const primaryFail = "cbroute-grpc-primary-fail"
	const okMarker = "cbroute-grpc-ok"
	const doomedMarker = "cbroute-grpc-doomed"

	cbRouteSetupModel(t, cSetup, secondary, `{"name":"child","amount":1,"status":"new"}`, cbRouteSecondaryWorkflow)
	cbRouteSetupModel(t, cSetup, primaryOK, `{"name":"Test","amount":10,"status":"new"}`,
		cbRoutePrimaryWorkflow("cbroute-grpc-ok-wf", "cb-grpc-create-secondary", cbRouteContext(secondary, okMarker)))
	cbRouteSetupModel(t, cSetup, primaryFail, `{"name":"Test","amount":10,"status":"new"}`,
		cbRoutePrimaryWorkflow("cbroute-grpc-fail-wf", "cb-grpc-create-then-fail", cbRouteContext(secondary, doomedMarker)))

	// OWNER = node 1 (no local compute member → dispatch forwards to node 0).
	const ownerIdx = 1
	cOwner := client.NewClient(urls[ownerIdx], tenant.Token)

	// --- success branch ---
	primaryID, err := cOwner.CreateEntity(t, primaryOK, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create via owner node %d (forwarded dispatch + cross-node gRPC callback): %v", ownerIdx, err)
	}

	prim, err := cOwner.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary via owner: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE (cross-node gRPC cascade did not complete)", prim.Meta.State)
	}
	if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
		t.Errorf("forwarded SYNC dispatch: tokenWasEmpty=true; want false (owner token must survive A→B forward)")
	}
	secID, _ := prim.Data["secondaryId"].(string)
	if secID == "" {
		t.Fatalf("primary missing secondaryId (cross-node gRPC callback did not create secondary): data=%+v", prim.Data)
	}

	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		hits, err := c.SyncSearch(t, secondary, 1, cbRouteStatusEquals(okMarker))
		if err != nil {
			t.Errorf("search ok-marker via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 1 {
			t.Errorf("ok-marker secondary search via node %d = %d; want 1 (gRPC B→A callback write not durable/visible)", nodeIdx, len(hits))
		}
	}

	// --- failure branch (cross-node gRPC atomicity proof) ---
	status, body, err := cOwner.CreateEntityRaw(t, primaryFail, 1, `{"name":"parent2","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create (failure branch) transport: %v", err)
	}
	if status == http.StatusOK {
		t.Fatalf("primary create (failure branch) unexpectedly succeeded: %s", body)
	}

	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		hits, err := c.SyncSearch(t, secondary, 1, cbRouteStatusEquals(doomedMarker))
		if err != nil {
			t.Errorf("search doomed-marker via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 0 {
			t.Fatalf("doomed secondary search via node %d = %d; want 0 — cross-node gRPC callback write was NOT rolled back with T", nodeIdx, len(hits))
		}
	}
}

// cbRouteStatusEquals builds a simple search condition matching data.status.
func cbRouteStatusEquals(value string) string {
	return fmt.Sprintf(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":%q}`, value)
}
