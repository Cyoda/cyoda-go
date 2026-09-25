package multinode

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_joined_commit.go — the refusal of a callback's COMMIT_BEFORE_DISPATCH
// holds when the callback crosses a cluster hop to reach the transaction's node.
//
// Topology (as in RunTxControlParam_RejectedAcrossForwardedHop):
//   - The primary is created on node 1, which owns the transaction T and has no
//     compute member, so its SYNC processor is handed over to node 0.
//   - Node 0's member makes a joined create callback to node 0. Node 0 is not
//     T's node, so it proxies the request to node 1, which joins T.
//   - The secondary's workflow reaches a COMMIT_BEFORE_DISPATCH processor. On
//     node 1 the joined write is refused with 409 COMMIT_IN_JOINED_TRANSACTION
//     before T is flushed or committed, and the answer crosses back.
//   - The processor records the answer and succeeds, so node 1 commits T: the
//     primary reaches ACTIVE, and the refused secondary is stored nowhere.
//
// The COMMIT_BEFORE_DISPATCH processor carries a tag no compute member serves:
// the refusal comes before any dispatch, so when it holds the tag never
// matters, and when it does not, the dispatch fails at once instead of waiting
// on the compute test client, which serves one callout at a time.

func init() {
	Register(
		NamedTest{Name: "Callback_CommitBeforeDispatchRefusedAcrossForwardedHop", Fn: RunCallback_CommitBeforeDispatchRefusedAcrossForwardedHop},
	)
}

// cbRouteCommitBeforeDispatchWorkflow gives the secondary model a
// COMMIT_BEFORE_DISPATCH processor on its create's automated transition.
func cbRouteCommitBeforeDispatchWorkflow(wfName string) string {
	b, _ := json.Marshal(map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{map[string]any{
			"version": "1.1", "name": wfName, "initialState": "NONE", "active": true,
			"states": map[string]any{
				"NONE": map[string]any{"transitions": []any{map[string]any{
					"name": "store", "next": "STORED", "manual": false,
					"processors": []any{map[string]any{
						"type": "calculator", "name": "noop", "executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": map[string]any{"attachEntity": true, "calculationNodesTags": "cbjc-fwdhop-no-member-serves-this"},
					}},
				}}},
				"STORED": map[string]any{},
			},
		}},
	})
	return string(b)
}

func RunCallback_CommitBeforeDispatchRefusedAcrossForwardedHop(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("the forwarded-hop refusal needs ≥2 nodes, got %d", len(urls))
	}
	tenant := fixture.ComputeTenant(t)
	cSetup := client.NewClient(urls[0], tenant.Token)

	const secondary = "cbjc-fwdhop-secondary"
	const primary = "cbjc-fwdhop-primary"
	const marker = "cbjc-fwdhop-marker"
	const primarySample = `{"name":"Test","amount":10,"status":"new","tokenWasEmpty":false,"hopStatus":0,"hopBody":""}`

	cbRouteSetupModel(t, cSetup, secondary, cbRouteSampleSecondary, cbRouteCommitBeforeDispatchWorkflow("cbjc-fwdhop-secondary-wf"))
	cbRouteSetupModel(t, cSetup, primary, primarySample,
		cbRoutePrimaryWorkflow("cbjc-fwdhop-wf", "cb-create-secondary-record", cbRouteContext(secondary, marker)))

	const ownerIdx = 1
	cOwner := client.NewClient(urls[ownerIdx], tenant.Token)
	primaryID, err := cOwner.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create via owner node %d: %v — the owner must still commit T after its callback was refused", ownerIdx, err)
	}
	prim, err := cOwner.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary via owner: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE (the owner committed T): data=%+v", prim.Meta.State, prim.Data)
	}
	if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
		t.Fatal("forwarded SYNC dispatch: tokenWasEmpty=true; the owner's token must survive the forward")
	}

	status, _ := prim.Data["hopStatus"].(float64)
	body, _ := prim.Data["hopBody"].(string)
	if int(status) != http.StatusConflict {
		t.Fatalf("joined create across the hop: status = %v; want 409 (body: %s)", status, body)
	}
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("crossed-back answer is not a problem document: %v (body: %s)", err, body)
	}
	if pd.Properties.ErrorCode != "COMMIT_IN_JOINED_TRANSACTION" || pd.Properties.Retryable {
		t.Fatalf("errorCode=%q retryable=%t; want COMMIT_IN_JOINED_TRANSACTION, not retryable (body: %s)",
			pd.Properties.ErrorCode, pd.Properties.Retryable, body)
	}

	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		hits, err := c.SyncSearch(t, secondary, 1, cbRouteStatusEquals(marker))
		if err != nil {
			t.Errorf("search secondary via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 0 {
			t.Errorf("refused secondary stored %d time(s), seen via node %d; want 0", len(hits), nodeIdx)
		}
	}
}
