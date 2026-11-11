package multinode

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
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
//   - The processor records the crossed-back answer outside T (a standalone
//     create, no token) and succeeds.
//   - The refusal is a non-retryable failure of a joined write: T is made
//     rollback-only, and node 1 refuses its owner 409 TRANSACTION_ROLLBACK_ONLY,
//     naming the callback's code; nothing of T is stored, the refused
//     secondary nowhere.
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
						"type": "externalized", "name": "noop", "executionMode": "COMMIT_BEFORE_DISPATCH",
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
	const record = "cbjc-fwdhop-record"
	const primarySample = `{"name":"Test","amount":10,"status":"new","tokenWasEmpty":false,"hopStatus":0,"hopBody":""}`

	parity.SetupHopRecordModel(t, cSetup, record)
	cbRouteSetupModel(t, cSetup, secondary, cbRouteSampleSecondary, cbRouteCommitBeforeDispatchWorkflow("cbjc-fwdhop-secondary-wf"))
	cbRouteSetupModel(t, cSetup, primary, primarySample,
		cbRoutePrimaryWorkflow("cbjc-fwdhop-wf", "cb-create-secondary-record", parity.HopRecordContext(secondary, marker, record)))

	const ownerIdx = 1
	cOwner := client.NewClient(urls[ownerIdx], tenant.Token)
	status, raw, err := cOwner.CreateEntityRaw(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create via owner node %d: %v", ownerIdx, err)
	}
	var pd struct {
		Detail     string `json:"detail"`
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("primary create answered %d, not a problem document: %v (body: %s)", status, err, raw)
	}
	if status != http.StatusConflict || pd.Properties.ErrorCode != "TRANSACTION_ROLLBACK_ONLY" || pd.Properties.Retryable {
		t.Fatalf("primary create = %d %s retryable=%t; want 409 TRANSACTION_ROLLBACK_ONLY, not retryable (body: %s)",
			status, pd.Properties.ErrorCode, pd.Properties.Retryable, raw)
	}
	if !strings.Contains(pd.Detail, "COMMIT_IN_JOINED_TRANSACTION") {
		t.Fatalf("detail = %q; want it to name the crossed-back COMMIT_IN_JOINED_TRANSACTION", pd.Detail)
	}
	if all, err := cOwner.ListEntitiesByModel(t, primary, 1); err != nil {
		t.Fatalf("list primary: %v", err)
	} else if len(all) != 0 {
		t.Fatalf("%d primary entities committed; the owner's refused operation commits nothing", len(all))
	}

	// The joined create's answer, as it crossed back through node 0's proxy.
	hop := parity.ReadHopRecord(t, cSetup, record, marker)
	if hop.TokenWasEmpty {
		t.Fatal("forwarded SYNC dispatch: tokenWasEmpty=true; the owner's token must survive the forward")
	}
	if hop.Status != http.StatusConflict || hop.ErrorCode != "COMMIT_IN_JOINED_TRANSACTION" || hop.Retryable {
		t.Fatalf("joined create across the hop = %d %s retryable=%t; want 409 COMMIT_IN_JOINED_TRANSACTION, not retryable (body: %s)",
			hop.Status, hop.ErrorCode, hop.Retryable, hop.Body)
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
