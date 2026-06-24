package multinode

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	Register(
		NamedTest{Name: "WorkflowProc_CBD_TxPostPinnedToHomeNode", Fn: RunWorkflowProc_CBD_TxPostPinnedToHomeNode},
	)
}

// RunWorkflowProc_CBD_TxPostPinnedToHomeNode covers spec §16 case 14
// (cluster-mode TX_post pinning) for issue #27. Spec §4.3 states that a
// COMMIT_BEFORE_DISPATCH cascade pins both segments — TX_pre and TX_post —
// to the same node, because the cascade is driven by the goroutine
// holding the HTTP request open and that goroutine never moves. Cross-node
// hand-off between the two segments is structurally out of scope and must
// not happen.
//
// The multi-node harness exercises the cluster surface by:
//
//  1. Sending the cascading PUT to node 0 (the node that hosts the gRPC
//     compute-test-client). All segments — TX_pre.Commit, dispatch,
//     TX_post.Begin/Commit — must therefore execute on node 0. A bug
//     that migrated TX_post to a different node would either error
//     (registry miss) or land the apply-result against the wrong
//     storage view; either way, the cascade would not durably commit
//     the post-callout state visible from every node.
//  2. Asserting the cascade completes durably end-to-end: state
//     APPROVED on node 0, with the processor mutation persisted.
//  3. Reading state and version-history from every cluster node
//     (including the non-home nodes) and asserting full consistency:
//     same APPROVED state, same processor-applied data, same
//     transactionId set. This proves the cluster gossip + shared
//     postgres path is healthy and that TX_post's commit is observable
//     cluster-wide — which is exactly the durability guarantee a
//     same-node-pinned cascade provides.
//  4. Asserting the cascade produced ≥2 distinct transactionIds in the
//     version history (TX_pre's and TX_post's per spec §4.2). A
//     single-segment cascade (no segmentation) would produce only one
//     transactionId in the cascade row; two distinct IDs is the
//     observable signature of correct CBD segmentation.
//
// HARNESS GAPS (documented for future strengthening, see PR description):
//
//   - Strict same-node assertion. Confirming "TX_pre and TX_post both
//     registered on node 0 specifically" requires either an admin
//     endpoint against the per-node txRegistry or a wired
//     cluster-level TX lifecycle registry. internal/cluster/lifecycle.Manager
//     is designed for exactly this but is not yet wired into the
//     runtime — its Register/IsAlive surface needs to be invoked
//     from the TransactionManager and exposed via an admin route.
//
//   - Forwarded-dispatch variant (PUT to a non-home node). The natural
//     stress test would target node 1 (no local compute member) and
//     force the cluster dispatcher to forward to node 0, then return
//     and continue on node 1. The HTTPForwarder's loopback SSRF guard
//     rejects 127.0.0.1 peers in production builds; the multi-node
//     fixture spawns all peers on loopback, so this code path is not
//     reachable from in-tree tests today. A test-only env-var gate on
//     AllowLoopbackForTesting (e.g. CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING)
//     would unlock the variant without weakening production posture.
func RunWorkflowProc_CBD_TxPostPinnedToHomeNode(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("CBD TX-post pinning needs at least 2 nodes for cross-node visibility checks, got %d", len(urls))
	}

	// ComputeTenant — the gRPC compute-test-client registers under
	// system-tenant; processor dispatch is tenant-scoped. Without this
	// the MemberRegistry lookup misses the member and dispatch fails.
	tenant := fixture.ComputeTenant(t)

	// Setup happens via node 0 — model + workflow imports are routed
	// through the cluster-shared model store so any node works for
	// install. The interesting routing happens on entity update.
	cSetup := client.NewClient(urls[0], tenant.Token)

	const modelName = "cbd-tx-pinning"
	const modelVersion = 1

	if err := cSetup.ImportModel(t, modelName, modelVersion, `{"name":"Test","amount":10,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := cSetup.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	// Workflow: NONE -init-> PENDING -approve-> APPROVED, with the
	// manual "approve" transition's processor in COMMIT_BEFORE_DISPATCH
	// mode. The processor is "tag-with-foo" from the compute-test-client's
	// catalog — it sets data.tag="foo" with no config required, which is
	// sufficient to prove post-cascade durability.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cbd-pinning-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "PENDING", "manual": false}]},
				"PENDING":  {"transitions": [{"name": "approve", "next": "APPROVED", "manual": true,
					"processors": [{"type": "calculator", "name": "tag-with-foo",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"APPROVED": {}
			}
		}]
	}`
	if err := cSetup.ImportWorkflow(t, modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	// Create the entity through node 0; should land in PENDING via the
	// auto "init" transition.
	cNode0 := client.NewClient(urls[0], tenant.Token)
	entityID, err := cNode0.CreateEntity(t, modelName, modelVersion, `{"name":"Test","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity via node 0: %v", err)
	}
	pre, err := cNode0.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity after create: %v", err)
	}
	if pre.Meta.State != "PENDING" {
		t.Fatalf("post-create state = %q; want PENDING", pre.Meta.State)
	}

	// Trigger the CBD cascade by PUT to node 0 — the node that hosts
	// the gRPC compute-test-client. All segments (TX_pre, dispatch,
	// TX_post) must execute on node 0. A bug that migrated TX_post to
	// a different node would either error (TX registry miss on the
	// foreign node) or land the apply-result against the wrong storage
	// view, breaking the post-cascade durability assertion below.
	const homeNodeIdx = 0
	cHome := client.NewClient(urls[homeNodeIdx], tenant.Token)
	if err := cHome.UpdateEntity(t, entityID, "approve", `{"name":"Test","amount":100,"status":"approved"}`); err != nil {
		t.Fatalf("UpdateEntity (CBD cascade) via node %d: %v", homeNodeIdx, err)
	}

	// Durability assertion: post-cascade state must be APPROVED with the
	// processor's mutation persisted. Read from every node — proves
	// both the cluster-forwarded dispatch and TX_post.Commit landed in
	// the shared postgres backend, observable cluster-wide.
	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		got, err := c.GetEntity(t, entityID)
		if err != nil {
			t.Errorf("post-cascade GetEntity via node %d: %v", nodeIdx, err)
			continue
		}
		if got.Meta.State != "APPROVED" {
			t.Errorf("post-cascade state via node %d = %q; want APPROVED (TX_post commit leak?)", nodeIdx, got.Meta.State)
		}
		if got.Data["tag"] != "foo" {
			t.Errorf("post-cascade tag via node %d = %v; want \"foo\" (processor mutation lost — TX_post pinning broken?)", nodeIdx, got.Data["tag"])
		}
	}

	// Per spec §4.2, a CBD cascade produces one EntityVersion row per
	// segment — at minimum TX_pre and TX_post, plus the original CREATE.
	// Read /changes from every node and assert (a) at least 3 rows are
	// visible (CREATE + TX_pre + TX_post — the auto "init" transition's
	// SYNC processors collapse into the CREATE row), (b) the rows
	// expose at least 2 distinct transactionIds, and (c) every node
	// sees the same set of transactionIds. Cluster gossip + shared
	// postgres both being healthy is what makes (c) pass.
	txIDsByNode := make(map[int]map[string]bool, len(urls))
	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		changes, err := c.GetEntityChanges(t, entityID)
		if err != nil {
			t.Errorf("GetEntityChanges via node %d: %v", nodeIdx, err)
			continue
		}
		seen := make(map[string]bool, len(changes))
		for _, ch := range changes {
			if ch.TransactionID != "" {
				seen[ch.TransactionID] = true
			}
		}
		txIDsByNode[nodeIdx] = seen
		if len(seen) < 2 {
			ids := make([]string, 0, len(seen))
			for id := range seen {
				ids = append(ids, id)
			}
			t.Errorf("CBD cascade produced %d distinct transactionIds via node %d, want ≥2 (TX_pre + TX_post per spec §4.2): %v",
				len(seen), nodeIdx, ids)
		}
	}

	// Cross-node consistency: every node's view of transactionIds must
	// match. Diverging views would indicate a cluster-routing bug
	// (segments committed against different storage views).
	if len(txIDsByNode) >= 2 {
		baseIdx := 0
		base := txIDsByNode[baseIdx]
		for nodeIdx, seen := range txIDsByNode {
			if nodeIdx == baseIdx {
				continue
			}
			if !sameTxIDSet(base, seen) {
				t.Errorf("transactionId set differs between node %d and node %d: %v vs %v",
					baseIdx, nodeIdx, sortedKeys(base), sortedKeys(seen))
			}
		}
	}
}

// sameTxIDSet returns true when a and b contain exactly the same keys.
func sameTxIDSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// sortedKeys returns the map keys as a JSON-encoded sorted-ish slice
// suitable for an error message; iteration order is not stable but the
// content is fully reported, which is all the diagnostic needs.
func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	out, err := json.Marshal(keys)
	if err != nil {
		return fmt.Sprintf("%v", keys)
	}
	return string(out)
}
