package multinode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// txctl_joined_forward.go — transaction-control params x cross-node
// callback routing intersection, spec D7/F1:
//
// A transaction-control param (transactionTimeoutMillis, transactionSize,
// timeoutMillis) is rejected with 400 BAD_REQUEST on any request that JOINS
// an open transaction — a participant must not be able to unilaterally
// override the deadline the transaction's owner controls. This scenario
// proves that rejection survives a forwarded cluster hop: a write carrying
// both a valid tx-token (owned by a peer node) AND a transaction-control
// param must have the OWNER's 400 cross back to the caller verbatim, not be
// silently swallowed, downgraded, or replaced by the proxying node.
//
// Topology (mirrors RunCallback_ForwardedDispatch_HTTP in callback_route.go):
//
//   - The primary transition is driven from node 1 (the OWNER of the
//     resulting transaction), which hosts no compute member. Its processor
//     requires the "compute-test-client" tag, so the owner hands the
//     processor over to node 0 (A→B).
//   - Node 0's member fires an HTTP callback presenting the tx-token
//     (owner=node 1) to node 0's HTTP base, with
//     ?transactionTimeoutMillis=5000 appended to the create URL. Node 0 is
//     NOT the owner, so the tx-token proxy reverse-proxies the request,
//     query string intact (TestProxy_PreservesQueryString pins this), to
//     node 1.
//   - Node 1 joins the transaction and, per spec D7/F1
//     (resolveRequestTimeout in internal/domain/entity/handler.go), rejects
//     the request BEFORE any write: 400 BAD_REQUEST naming the param and the
//     joined-transaction reason. That response crosses back through node 0's
//     proxy to the callback client unmodified.
//   - The processor records the crossed-back HTTP status + body outside the
//     transaction (a standalone create, no token), and the Go test driver
//     reads it from that record.
//   - The rejection is a non-retryable failure of a joined write: the
//     callback's scope is discarded and T made rollback-only, so the owner is
//     refused 409 TRANSACTION_ROLLBACK_ONLY, naming BAD_REQUEST, and nothing
//     of T is committed.
//
// If the 400 does not cross the hop cleanly (e.g. gets replaced by a
// generic 502/500, or the param is silently honored), that is a product bug
// in the proxy or the forwarded join path — fail closed, do not weaken this
// assertion (.claude/rules/correctness-over-availability.md).

func init() {
	Register(
		NamedTest{Name: "TxControlParam_RejectedAcrossForwardedHop", Fn: RunTxControlParam_RejectedAcrossForwardedHop},
	)
}

func RunTxControlParam_RejectedAcrossForwardedHop(t *testing.T, fixture MultiNodeFixture) {
	t.Helper()
	urls := fixture.BaseURLs()
	if len(urls) < 2 {
		t.Fatalf("tx-control-param forwarded-hop rejection needs ≥2 nodes, got %d", len(urls))
	}
	tenant := fixture.ComputeTenant(t)

	cSetup := client.NewClient(urls[0], tenant.Token)

	const secondary = "txctl-fwdhop-secondary"
	const primary = "txctl-fwdhop-primary"
	const marker = "txctl-fwdhop-marker"
	const record = "txctl-fwdhop-record"

	// The sample declares hopStatus/hopBody/tokenWasEmpty at zero value — the
	// exact field set cb-tx-control-param-joined writes back (see comment on
	// txctlHopResult). Processor output passes the same strict model checks a
	// client write does, so a mistyped field name here fails loudly instead
	// of silently widening the model.
	const primarySample = `{"name":"Test","amount":10,"status":"new","tokenWasEmpty":false,"hopStatus":0,"hopBody":""}`
	cbRouteSetupModel(t, cSetup, secondary, cbRouteSampleSecondary, cbRouteSecondaryWorkflow)
	parity.SetupHopRecordModel(t, cSetup, record)
	cbRouteSetupModel(t, cSetup, primary, primarySample,
		cbRoutePrimaryWorkflow("txctl-fwdhop-wf", "cb-tx-control-param-joined", parity.HopRecordContext(secondary, marker, record)))

	// OWNER = node 1 (no local compute member → dispatch forwards to node 0,
	// whose member's callback then proxies BACK to node 1 to join T).
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
	// The owner's refusal names the code its callback got: the joined create
	// carrying transactionTimeoutMillis was refused 400 BAD_REQUEST on the
	// owner, across the forwarded hop.
	if status != 409 || pd.Properties.ErrorCode != "TRANSACTION_ROLLBACK_ONLY" || pd.Properties.Retryable {
		t.Fatalf("primary create = %d %s retryable=%t; want 409 TRANSACTION_ROLLBACK_ONLY, not retryable (body: %s)",
			status, pd.Properties.ErrorCode, pd.Properties.Retryable, raw)
	}
	if !strings.Contains(pd.Detail, "BAD_REQUEST") {
		t.Fatalf("detail = %q; want it to name the callback's BAD_REQUEST", pd.Detail)
	}
	if all, err := cOwner.ListEntitiesByModel(t, primary, 1); err != nil {
		t.Fatalf("list primary: %v", err)
	} else if len(all) != 0 {
		t.Fatalf("%d primary entities committed; the owner's refused operation commits nothing", len(all))
	}

	// The core assertion: the OWNER's rejection crossed the forwarded hop
	// verbatim as 400 BAD_REQUEST naming the param and the joined-transaction
	// reason — not a proxy-mangled status, not a silently-honored param.
	hop := parity.ReadHopRecord(t, cSetup, record, marker)
	if hop.TokenWasEmpty {
		t.Fatalf("forwarded SYNC dispatch: tokenWasEmpty=true; want false (owner token must survive the A→B forward)")
	}
	if hop.Status != 400 {
		t.Fatalf("joined create with transactionTimeoutMillis, forwarded across the hop: status = %d, want 400; body: %s", hop.Status, hop.Body)
	}
	if hop.ErrorCode != "BAD_REQUEST" {
		t.Errorf("crossed-back 400 properties.errorCode = %q, want BAD_REQUEST: body=%s", hop.ErrorCode, hop.Body)
	}
	if !strings.Contains(hop.Body, "transactionTimeoutMillis") {
		t.Errorf("crossed-back 400 body does not name transactionTimeoutMillis: %s", hop.Body)
	}
	if !strings.Contains(hop.Body, "joins an open transaction") {
		t.Errorf("crossed-back 400 body does not name the joined-transaction rejection reason: %s", hop.Body)
	}

	// Negative control: the secondary the rejected create would have written
	// must not exist anywhere in the cluster — the owner rejected the write
	// before touching storage, and the rejection must not leave a partial
	// artifact behind on any node.
	for nodeIdx, url := range urls {
		c := client.NewClient(url, tenant.Token)
		hits, err := c.SyncSearch(t, secondary, 1, cbRouteStatusEquals(marker))
		if err != nil {
			t.Errorf("search marker via node %d: %v", nodeIdx, err)
			continue
		}
		if len(hits) != 0 {
			t.Errorf("secondary search via node %d = %d; want 0 — the rejected joined create must not have written anything", nodeIdx, len(hits))
		}
	}
}
