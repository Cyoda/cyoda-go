package parity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_txjoin.go — feature #287 cross-backend parity scenarios for
// compute-node callbacks that JOIN the originating transition's transaction T.
//
// Backend-agnostic invariants proven identically on memory / sqlite / postgres
// (and any out-of-tree backend that runs the parity suite):
//
//	1. SYNC callback WRITE is atomic with the transition — survives on success,
//	   rolled back with T on processor failure.
//	2. SYNC callback READ sees the transition's uncommitted cascade write
//	   (read-your-own-writes across the callback boundary).
//	3. CRITERIA callback read-your-writes joins T — a criteria service GET sees
//	   the entity's uncommitted in-T state.
//	4. Callback UPDATE with If-Match against a version written earlier in the
//	   SAME uncommitted T succeeds.
//	5. Empty-token callback runs STANDALONE (regression guard: no token →
//	   independent Begin/Commit).
//
// The join is driven by the callback-capable compute-test-client (cmd/
// compute-test-client): the engine mints a signed cyodatxtoken on the gRPC
// calc request, the member reads it and echoes it as X-Tx-Token on an HTTP
// callback, and the TxJoin middleware joins T. Every scenario is constructed so
// it FAILS if the callback ran in its own transaction rather than joining T.
//
// The token value is never logged or asserted (Gate 3 / spec §8-H10); scenarios
// only observe derived booleans (e.g. "was the token empty") and entity state.
//
// These scenarios use ComputeTenant (the compute-test-client's tenant); model
// names are globally unique across the compute parity set because that tenant
// and its storage persist for the whole package run.

// cbSecondaryWorkflow is a trivial two-state workflow (NONE -store-> STORED)
// with NO processors, so a secondary entity created from inside a callback runs
// a minimal cascade within the joined T and never dispatches a nested processor
// (which would contend the per-tx gate).
const cbSecondaryWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "cbtj-secondary-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false}]},
			"STORED": {}
		}
	}]
}`

// cbContext builds the pass-through ProcessorConfig.context value the callback
// processors read to learn the secondary model + marker for a scenario. It
// returns a JSON-encoded (quoted, escaped) string literal ready to embed as the
// "context" value inside a workflow JSON document.
func cbContext(secondaryModel string, marker string) string {
	inner, _ := json.Marshal(map[string]any{
		"secondaryModel":   secondaryModel,
		"secondaryVersion": 1,
		"marker":           marker,
	})
	// json.Marshal of the inner-JSON string yields a fully quoted+escaped
	// literal (e.g. "\"{\\\"secondaryModel\\\":...}\"") suitable for direct
	// substitution as a JSON string value.
	quoted, _ := json.Marshal(string(inner))
	return string(quoted)
}

// cbPrimaryProcWorkflow builds a NONE→ACTIVE auto-transition workflow whose
// transition carries a single callback processor with the given name, execution
// mode, and pass-through context.
func cbPrimaryProcWorkflow(wfName, procName, execMode, contextValue string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": %q,
						"config": {"attachEntity": true, "calculationNodesTags": "", "context": %s}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, wfName, procName, execMode, contextValue)
}

// cbSetupModel imports + locks a model with the given sample doc, then imports
// the workflow. Unlike setupModelWithWorkflow it takes an explicit sample doc so
// callers can seed marker-bearing fields.
func cbSetupModel(t *testing.T, c *client.Client, modelName string, sampleDoc, workflowJSON string) {
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

// RunCallbackSyncWriteAtomic proves invariant 1: a SYNC processor whose callback
// CREATES a secondary entity has that write bound to the primary transition's
// transaction T. On success the secondary is durable; on processor failure it is
// rolled back atomically with T.
//
// Failure branch is the join proof: if the callback had opened its OWN
// transaction, the secondary would survive the aborted primary and the
// marker-search would return 1 instead of 0.
func RunCallbackSyncWriteAtomic(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-atomic-secondary"
	const primaryOK = "cbtj-atomic-primary-ok"
	const primaryFail = "cbtj-atomic-primary-fail"
	const okMarker = "cbtj-atomic-ok"
	const doomedMarker = "cbtj-atomic-doomed"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)

	// --- success branch ---
	cbSetupModel(t, c, primaryOK, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-atomic-ok-wf", "cb-create-secondary", "SYNC", cbContext(secondary, okMarker)))

	primaryID, err := c.CreateEntity(t, primaryOK, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create (success branch): %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary (success): %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE", prim.Meta.State)
	}
	if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
		t.Errorf("SYNC dispatch: tokenWasEmpty=true; want false (a real tx-token must be attached)")
	}
	secIDStr, _ := prim.Data["secondaryId"].(string)
	if secIDStr == "" {
		t.Fatalf("primary data missing secondaryId (callback did not create secondary): data=%+v", prim.Data)
	}
	secID, err := uuid.Parse(secIDStr)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secIDStr, err)
	}
	// Secondary is durable: the callback write committed atomically with T.
	sec, err := c.GetEntity(t, secID)
	if err != nil {
		t.Fatalf("secondary GET after success: %v (callback write must be durable)", err)
	}
	if sec.Meta.State != "STORED" {
		t.Errorf("secondary state = %q; want STORED", sec.Meta.State)
	}
	// Control: the ok-marker secondary is searchable (proves create+search work).
	okHits, err := c.SyncSearch(t, secondary, 1, cbStatusEquals(okMarker))
	if err != nil {
		t.Fatalf("search ok-marker: %v", err)
	}
	if len(okHits) != 1 {
		t.Errorf("ok-marker secondary search = %d; want 1", len(okHits))
	}

	// Same-transaction assertion: the primary's processor-launching transition
	// and the secondary create must carry the IDENTICAL transactionId. This is
	// the unambiguous proof that the callback joined T rather than opening a
	// separate transaction — it would FAIL under the pre-#287 behaviour where
	// the callback ran its own Begin/Commit.
	cbAssertSameTxID(t, c, primaryID, secID)

	// --- failure branch (THE ATOMICITY PROOF) ---
	cbSetupModel(t, c, primaryFail, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-atomic-fail-wf", "cb-create-then-fail", "SYNC", cbContext(secondary, doomedMarker)))

	status, body, err := c.CreateEntityRaw(t, primaryFail, 1, `{"name":"parent2","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create (failure branch) transport: %v", err)
	}
	if status == http.StatusOK {
		t.Fatalf("primary create (failure branch) unexpectedly succeeded: %s", body)
	}
	// The doomed secondary the callback created inside T must be gone, because
	// the primary transition aborted and T rolled back. A hit here means the
	// callback write was NOT atomic with T (it ran in its own transaction).
	doomedHits, err := c.SyncSearch(t, secondary, 1, cbStatusEquals(doomedMarker))
	if err != nil {
		t.Fatalf("search doomed-marker: %v", err)
	}
	if len(doomedHits) != 0 {
		t.Fatalf("doomed secondary search = %d; want 0 — callback write was NOT rolled back with T (join broken)", len(doomedHits))
	}
}

// RunCallbackSyncReadYourWrites proves invariant 2: a SYNC processor creates a
// secondary entity inside T (uncommitted), then READS it back through a second
// joined callback and observes it — a read that would 404 outside T. The
// observation is echoed into the primary's committed data and asserted after
// commit.
func RunCallbackSyncReadYourWrites(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-ryw-secondary"
	const primary = "cbtj-ryw-primary"
	const marker = "cbtj-ryw-marker"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-ryw-wf", "cb-read-your-writes", "SYNC", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if found, _ := prim.Data["readbackFound"].(bool); !found {
		t.Fatalf("joined callback read did NOT see the uncommitted secondary write (read-your-writes across the callback boundary is broken): data=%+v", prim.Data)
	}
	if got := prim.Data["readbackMarker"]; got != marker {
		t.Errorf("joined read observed status=%v; want the uncommitted marker %q", got, marker)
	}
}

// RunCallbackGRPCSearchReadYourWrites proves that a callback which SEARCHES over
// the gRPC entity API joins T identically on every backend: a SYNC processor
// creates a secondary inside T (uncommitted), then issues an EntitySearchCollection
// gRPC callback for it both WITH the tx-token (joined) and WITHOUT (standalone).
// The joined search must match the uncommitted row (count >= 1); the standalone
// control must match nothing (count 0). This guards the search-within-T
// visibility the gRPC read-RPC routing depends on — before the search RPCs were
// wired into the txRouteInterceptor, the joined count collapsed to the standalone
// count on every backend.
func RunCallbackGRPCSearchReadYourWrites(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-gsearch-secondary"
	const primary = "cbtj-gsearch-primary"
	const marker = "cbtj-gsearch-marker"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-gsearch-wf", "cb-grpc-search-read-your-writes", "SYNC", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	joined, _ := prim.Data["grpcSearchJoinedCount"].(float64)
	standalone, _ := prim.Data["grpcSearchStandaloneCount"].(float64)
	if joined < 1 {
		t.Fatalf("joined gRPC search matched %v entities; want >= 1 — the tx-token was ignored on the search RPC (read-your-writes broken for gRPC search): data=%+v", prim.Data["grpcSearchJoinedCount"], prim.Data)
	}
	if standalone != 0 {
		t.Errorf("standalone gRPC search (no token) matched %v entities; want 0 — the control must not see the uncommitted secondary", prim.Data["grpcSearchStandaloneCount"])
	}
}

// RunCallbackCriteriaReadYourWrites proves invariant 3: a criteria callback that
// reads an entity written earlier in T joins the transaction and sees the
// uncommitted state.
//
// Workflow: NONE→ENRICHED (auto, processor cb-create-secondary writes a
// secondary into T and stamps secondaryId onto the primary) → APPROVED (auto,
// guarded by a criterion that GETs that secondary through a joined callback and
// matches only when the uncommitted secondary is visible with the marker). A
// joined criteria GET sees the in-T secondary → APPROVED. A standalone GET 404s
// (secondary not yet committed) → no match → stuck at ENRICHED.
func RunCallbackCriteriaReadYourWrites(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-crit-secondary"
	const primary = "cbtj-crit-primary"
	const marker = "cbtj-crit-marker"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)

	ctxVal := cbContext(secondary, marker)
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cbtj-crit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":     {"transitions": [{"name": "init", "next": "ENRICHED", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-create-secondary", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": "", "context": %s}}]
				}]},
				"ENRICHED": {"transitions": [{"name": "approve", "next": "APPROVED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "cb-criterion-reads",
						"config": {"attachEntity": true, "calculationNodesTags": "", "context": %s}}}
				}]},
				"APPROVED": {}
			}
		}]
	}`, ctxVal, ctxVal)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`, wf)

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if prim.Meta.State != "APPROVED" {
		t.Fatalf("primary state = %q; want APPROVED — criteria callback did not see the uncommitted in-T secondary (join broken)", prim.Meta.State)
	}
}

// RunCallbackIfMatchUpdate proves invariant 4: a callback UPDATE carrying an
// If-Match precondition equal to a version stamped earlier in the SAME
// uncommitted T succeeds.
//
// The processor creates a secondary inside T (stamped with T's uncommitted
// txID), then issues a loopback update with If-Match set to that txID. Inside
// the join the secondary is visible AND its version matches → 200. Outside T the
// secondary is invisible (404) or its committed version differs (412) — never
// 200.
func RunCallbackIfMatchUpdate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-ifmatch-secondary"
	const primary = "cbtj-ifmatch-primary"
	const marker = "cbtj-ifmatch-marker"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-ifmatch-wf", "cb-ifmatch-update", "SYNC", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if ok, _ := prim.Data["ifMatchOK"].(bool); !ok {
		t.Fatalf("callback If-Match update did not succeed against the in-T version: ifMatchStatus=%v (join broken)", prim.Data["ifMatchStatus"])
	}
}

// RunCallbackEmptyTokenStandalone proves invariant 5 (regression guard): a
// COMMIT_BEFORE_DISPATCH processor dispatched with no transaction context
// receives an EMPTY token, issues a callback with NO X-Tx-Token, and the write
// commits independently (standalone Begin/Commit) — it does not leak a stale
// token or accidentally join an unrelated transaction.
func RunCallbackEmptyTokenStandalone(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-standalone-secondary"
	const primary = "cbtj-standalone-primary"
	const marker = "cbtj-standalone"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	// COMMIT_BEFORE_DISPATCH default (startNewTxOnDispatch omitted → false):
	// the dispatcher sends no tx-token, so the processor's callback runs
	// standalone.
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-standalone-wf", "cb-create-secondary", "COMMIT_BEFORE_DISPATCH", cbContext(secondary, marker)))

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create: %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	// The processor must have received an EMPTY token (no tx context during
	// CBD-default dispatch). Gate 3: we assert the derived boolean, never the
	// token value.
	if empty, ok := prim.Data["tokenWasEmpty"].(bool); !ok || !empty {
		t.Errorf("CBD-default: tokenWasEmpty=%v; want true (dispatch must carry no tx context)", prim.Data["tokenWasEmpty"])
	}
	secIDStr, _ := prim.Data["secondaryId"].(string)
	if secIDStr == "" {
		t.Fatalf("primary data missing secondaryId (standalone callback did not create secondary): data=%+v", prim.Data)
	}
	secID, err := uuid.Parse(secIDStr)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secIDStr, err)
	}
	// Secondary committed independently (standalone operation must be durable).
	sec, err := c.GetEntity(t, secID)
	if err != nil {
		t.Fatalf("secondary GET after standalone commit: %v", err)
	}
	if sec.Meta.State != "STORED" {
		t.Errorf("secondary state = %q; want STORED", sec.Meta.State)
	}
}

// cbStatusEquals builds a simple search condition matching data.status == value.
func cbStatusEquals(value string) string {
	return fmt.Sprintf(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":%q}`, value)
}

// RunCallback_CBDPostJoinsTxPost proves that COMMIT_BEFORE_DISPATCH with
// startNewTxOnDispatch=true opens TX_post and mints a real tx-token: the
// callback joins TX_post and writes a secondary entity atomically. After success
// the secondary is durable, the primary carries secondaryId, and tokenWasEmpty
// is false (the dispatcher DID mint a real token — unlike the CBD-default branch).
//
// This is the parity counterpart to TestCallback_CBDPost_JoinsTxPost in
// internal/e2e (single-backend, live harness). Cross-backend proof: the engine's
// TX_post plumbing and the TxJoin middleware must behave identically on every
// backend.
func RunCallback_CBDPostJoinsTxPost(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-cbd-post-secondary"
	const primary = "cbtj-cbd-post-primary"
	const marker = "cbtj-cbd-post"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)

	// Build the workflow inline: COMMIT_BEFORE_DISPATCH + startNewTxOnDispatch=true.
	// cbPrimaryProcWorkflow does not support startNewTxOnDispatch, so inline here.
	ctxVal := cbContext(secondary, marker)
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cbtj-cbd-post-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-create-secondary",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": "",
						           "startNewTxOnDispatch": true, "context": %s}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, ctxVal)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`, wf)

	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create (CBD+TX_post): %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE", prim.Meta.State)
	}
	// TX_post must have minted a real token — the processor must NOT see an empty token.
	if empty, _ := prim.Data["tokenWasEmpty"].(bool); empty {
		t.Errorf("CBD+TX_post: tokenWasEmpty=true; want false (TX_post must mint a real routing token)")
	}
	secIDStr, _ := prim.Data["secondaryId"].(string)
	if secIDStr == "" {
		t.Fatalf("primary data missing secondaryId (CBD+TX_post callback did not create secondary): data=%+v", prim.Data)
	}
	secID, err := uuid.Parse(secIDStr)
	if err != nil {
		t.Fatalf("parse secondaryId %q: %v", secIDStr, err)
	}
	// Secondary committed atomically with TX_post.
	sec, err := c.GetEntity(t, secID)
	if err != nil {
		t.Fatalf("secondary GET: %v (callback write must be durable after TX_post commit)", err)
	}
	if sec.Meta.State != "STORED" {
		t.Errorf("secondary state = %q; want STORED", sec.Meta.State)
	}
}

// RunCallback_AsyncNewTxDiscardOnFailure proves ASYNC_NEW_TX savepoint semantics
// for the failure branch:
//
//  1. The secondary entity the callback created inside the savepoint is DISCARDED
//     when the processor fails (savepoint rollback, NOT full T abort).
//  2. The pipeline CONTINUES: ASYNC_NEW_TX failure is non-fatal — primary reaches ACTIVE.
//  3. The primary commits WITHOUT the secondary — the doomed marker search returns 0.
//
// This distinguishes ASYNC_NEW_TX from SYNC: a SYNC failure aborts T entirely
// (primary fails too). Here the primary succeeds while the secondary is silently
// discarded. If the secondary survived the rollback, the doomed-marker search would
// return 1, proving the savepoint semantics are broken.
func RunCallback_AsyncNewTxDiscardOnFailure(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const secondary = "cbtj-anytx-fail-secondary"
	const primary = "cbtj-anytx-fail-primary"
	const marker = "cbtj-anytx-doomed"

	cbSetupModel(t, c, secondary, `{"name":"child","amount":1,"status":"new"}`, cbSecondaryWorkflow)
	cbSetupModel(t, c, primary, `{"name":"Test","amount":10,"status":"new"}`,
		cbPrimaryProcWorkflow("cbtj-anytx-fail-wf", "cb-create-then-fail", "ASYNC_NEW_TX", cbContext(secondary, marker)))

	// Pipeline must continue — ASYNC_NEW_TX failure is non-fatal → primary succeeds.
	primaryID, err := c.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if err != nil {
		t.Fatalf("primary create (ASYNC_NEW_TX non-fatal failure): %v", err)
	}
	prim, err := c.GetEntity(t, primaryID)
	if err != nil {
		t.Fatalf("GetEntity primary: %v", err)
	}
	if prim.Meta.State != "ACTIVE" {
		t.Fatalf("primary state = %q; want ACTIVE (ASYNC_NEW_TX failure is non-fatal, pipeline must continue)", prim.Meta.State)
	}
	// Savepoint rolled back — secondary written inside savepoint must be gone.
	// A hit here means the write survived the rollback → savepoint semantics broken.
	doomedHits, err := c.SyncSearch(t, secondary, 1, cbStatusEquals(marker))
	if err != nil {
		t.Fatalf("search doomed marker: %v", err)
	}
	if len(doomedHits) != 0 {
		t.Fatalf("doomed secondary search = %d; want 0 — savepoint NOT rolled back on ASYNC_NEW_TX failure", len(doomedHits))
	}
}

// cbFirstTxID returns the first non-empty transactionId from an audit
// response, scanning all items in order. The caller decides which event
// type to look for — in practice the first non-empty txID is from the
// transition that wrote the entity, which is the same T for both the
// primary (processor-launching transition) and the secondary (callback
// create within T).
func cbFirstTxID(resp client.EntityAuditEventsResponse) string {
	for _, ev := range resp.Items {
		if ev.TransactionID != "" {
			return ev.TransactionID
		}
	}
	return ""
}

// cbAssertSameTxID queries the audit REST endpoint for both the primary
// and secondary entities and asserts that each carries a non-empty
// transactionId and that both transactionIds are IDENTICAL. A mismatch
// means the callback ran in a separate transaction (pre-#287 behaviour),
// not the expected join into T.
func cbAssertSameTxID(t *testing.T, c *client.Client, primaryID, secondaryID uuid.UUID) {
	t.Helper()
	primAudit, err := c.GetAuditEvents(t, primaryID)
	if err != nil {
		t.Fatalf("GetAuditEvents primary %s: %v", primaryID, err)
	}
	secAudit, err := c.GetAuditEvents(t, secondaryID)
	if err != nil {
		t.Fatalf("GetAuditEvents secondary %s: %v", secondaryID, err)
	}
	primTxID := cbFirstTxID(primAudit)
	secTxID := cbFirstTxID(secAudit)
	if primTxID == "" || secTxID == "" {
		t.Fatalf("expected non-empty transactionIds from audit: primary=%q secondary=%q", primTxID, secTxID)
	}
	if primTxID != secTxID {
		t.Fatalf("primary transition txID %q != secondary create txID %q — the callback did NOT join the originating transaction (separate transactions)",
			primTxID, secTxID)
	}
}
