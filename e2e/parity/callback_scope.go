package parity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// callback_scope.go — a joined callback's request runs in a scope of its own,
// and the scope is all-or-nothing, on every backend.
//
//   - A non-retryable failure of a joined write discards what it wrote and
//     makes the transaction rollback-only: the owner's operation is refused 409
//     TRANSACTION_ROLLBACK_ONLY, naming the callback's code.
//   - A retryable failure discards the scope and leaves the transaction usable:
//     the same callback sent again succeeds, and the owner commits one write.
//   - A read that answered a 4xx keeps its scope: the owner commits.
//   - An ASYNC_NEW_TX processor absorbs the failure of its callbacks' writes:
//     the processor counts as failed, the operation commits, and nothing of the
//     callback exists.
//   - A message posted under the token never joins the transaction: the owner's
//     failure leaves it in place.
//
// Every scenario gives the tenant two compute clients of its own: p serves the
// owner's processor and makes the callbacks, x serves the processor of the
// model the callback writes. The compute-test-client serves one callout at a
// time, so the owner's callout and the callout of the callback's write need two
// clients. A callback's own record of what it was answered is written outside
// the transaction (see HopRecordContext), so a scenario reads the crossed-back
// answer whatever becomes of the transaction.
//
// "Nothing is committed" is checked through the API: the model lists no entity
// of the discarded write, and the entity id the compute client was handed is
// answered 404 by the entity door and by the audit door. The audit door answers
// 404 before it merges StateMachine events when the entity has no committed
// version, so a StateMachine audit row written for an uncommitted entity is
// invisible here; internal/e2e counts those in PostgreSQL itself.

const (
	// cbScopeOwnerSample declares the fields the recording processors write
	// back, at zero value.
	cbScopeOwnerSample = cbJoinedCommitSample

	cbScopeOwnerPayload = `{"name":"parent","amount":100,"status":"new"}`
)

// cbScopeCase is one tenant with its models and its two compute clients.
type cbScopeCase struct {
	c             *client.Client
	owner, x, rec string // model names
	marker        string
	pClient       ComputeClient
	xClient       ComputeClient
}

// newCbScopeCase sets up the models and starts the compute clients. xProc is
// the catalog processor of x's create transition; an empty xProc gives x no
// processor and no client.
func newCbScopeCase(t *testing.T, fixture BackendFixture, name, ownerProc, ownerMode, xProc string) cbScopeCase {
	t.Helper()
	requireComputeClients(t, fixture)
	tenant := fixture.NewTenant(t)
	sfx := uuid.NewString()[:8]
	s := cbScopeCase{
		c:      client.NewClient(fixture.BaseURL(), tenant.Token),
		owner:  "cbsc-" + name + "-p-" + sfx,
		x:      "cbsc-" + name + "-x-" + sfx,
		rec:    "cbsc-" + name + "-r-" + sfx,
		marker: "cbsc-" + name + "-" + sfx,
	}
	tagP, tagX := "cbsc-tp-"+sfx, "cbsc-tx-"+sfx

	SetupHopRecordModel(t, s.c, s.rec)
	if xProc != "" {
		xWorkflow := cbWorkflowDoc(s.x+"-wf", map[string]any{
			"NONE": map[string]any{"transitions": []any{map[string]any{
				"name": "store", "next": "STORED", "manual": false,
				"processors": []any{cbProc(xProc, "SYNC", "", map[string]any{"calculationNodesTags": tagX})},
			}}},
			"STORED": map[string]any{},
		})
		cbSetupModel(t, s.c, s.x, cbSampleSecondary, xWorkflow)
		s.xClient = StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tagX}})
	}
	cbSetupModel(t, s.c, s.owner, cbScopeOwnerSample, cbWorkflowDoc(s.owner+"-wf", map[string]any{
		"NONE": map[string]any{"transitions": []any{map[string]any{
			"name": "init", "next": "ACTIVE", "manual": false,
			"processors": []any{cbProc(ownerProc, ownerMode, HopRecordContext(s.x, s.marker, s.rec), map[string]any{"calculationNodesTags": tagP})},
		}}},
		"ACTIVE": map[string]any{},
	}))
	s.pClient = StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tagP}})
	return s
}

// cbScopeProblem is the part of a problem document the scenarios assert on.
type cbScopeProblem struct {
	Detail           string
	ErrorCode        string
	Retryable        bool
	RetryablePresent bool
}

// cbDecodeProblem decodes a problem document, failing the test unless it is
// one with wantStatus and wantCode.
func cbDecodeProblem(t *testing.T, status int, raw []byte, wantStatus int, wantCode string) cbScopeProblem {
	t.Helper()
	var pd struct {
		Detail     string         `json:"detail"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("answered %d, not a problem document: %v (body: %s)", status, err, raw)
	}
	code, _ := pd.Properties["errorCode"].(string)
	retryable, has := pd.Properties["retryable"].(bool)
	if status != wantStatus || code != wantCode {
		t.Fatalf("answered %d %s; want %d %s (body: %s)", status, code, wantStatus, wantCode, raw)
	}
	return cbScopeProblem{Detail: pd.Detail, ErrorCode: code, Retryable: retryable, RetryablePresent: has}
}

// assertNothingCommitted asserts that no entity of model is committed, and that
// each id in ids, the entities the compute clients were handed, is answered
// 404 by the entity door and by the audit door.
func assertNothingCommitted(t *testing.T, c *client.Client, model string, ids ...string) {
	t.Helper()
	all, err := c.ListEntitiesByModel(t, model, 1)
	if err != nil {
		t.Fatalf("list %s: %v", model, err)
	}
	if len(all) != 0 {
		t.Errorf("%d %s entities committed; want none", len(all), model)
	}
	for _, id := range ids {
		assertEntityTraceless(t, c, id)
	}
}

// assertEntityTraceless asserts that the entity door and the audit door both
// answer 404 for id.
func assertEntityTraceless(t *testing.T, c *client.Client, id string) {
	t.Helper()
	eid, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("the compute client was handed entity id %q: %v", id, err)
	}
	if status, _ := c.GetEntityRaw(t, eid); status != http.StatusNotFound {
		t.Errorf("entity %s: GET answered %d; want 404 — a discarded write is not committed", id, status)
	}
	if status, _ := c.GetAuditEventsRaw(t, eid); status != http.StatusNotFound {
		t.Errorf("entity %s: audit answered %d; want 404 — a discarded write leaves no version", id, status)
	}
}

// receivedEntityIDs returns the entity id of each callout cc received, waiting
// for want of them.
func receivedEntityIDs(t *testing.T, cc ComputeClient, want int) []string {
	t.Helper()
	got := AwaitReceived(t, cc, want, calloutAwaitWithin)
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.EntityID)
	}
	return ids
}

// RunCallbackScope_FailedJoinedWriteDoomsOwner: the callback's create of x runs
// x's processor, which fails, not retryably. The callback is answered its own
// 400 WORKFLOW_FAILED, nothing of x is kept, and the owner, whose processor
// then succeeds, is refused 409 TRANSACTION_ROLLBACK_ONLY naming the callback's
// code. Nothing of the owner is committed.
func RunCallbackScope_FailedJoinedWriteDoomsOwner(t *testing.T, fixture BackendFixture) {
	s := newCbScopeCase(t, fixture, "fw", "cb-create-secondary-record", "SYNC", "inject-error-not-retryable")

	status, raw, err := s.c.CreateEntityRaw(t, s.owner, 1, cbScopeOwnerPayload)
	if err != nil {
		t.Fatalf("owner create: %v", err)
	}
	pd := cbDecodeProblem(t, status, raw, http.StatusConflict, "TRANSACTION_ROLLBACK_ONLY")
	if pd.RetryablePresent {
		t.Errorf("owner refusal carries retryable=%t; want the flag absent (body: %s)", pd.Retryable, raw)
	}
	if !strings.Contains(pd.Detail, "WORKFLOW_FAILED") {
		t.Errorf("owner detail = %q; want it to name the callback's WORKFLOW_FAILED", pd.Detail)
	}

	hop := ReadHopRecord(t, s.c, s.rec, s.marker)
	if hop.TokenWasEmpty {
		t.Fatal("tokenWasEmpty=true; the callback must have joined the transaction")
	}
	if hop.Status != http.StatusBadRequest || hop.ErrorCode != "WORKFLOW_FAILED" || hop.Retryable {
		t.Errorf("joined create = %d %s retryable=%t; want 400 WORKFLOW_FAILED, not retryable (body: %s)", hop.Status, hop.ErrorCode, hop.Retryable, hop.Body)
	}

	assertNothingCommitted(t, s.c, s.x, receivedEntityIDs(t, s.xClient, 1)...)
	assertNothingCommitted(t, s.c, s.owner, receivedEntityIDs(t, s.pClient, 1)...)
}

// RunCallbackScope_RetryableFailureRetriedInTx: x's processor fails retryably
// the first time and succeeds after. The first callback is answered 400
// WORKFLOW_FAILED, retryable, and its write is discarded; the transaction stays
// usable, and the same callback sent again succeeds. The owner commits exactly
// one x, the retried one.
func RunCallbackScope_RetryableFailureRetriedInTx(t *testing.T, fixture BackendFixture) {
	s := newCbScopeCase(t, fixture, "rt", "cb-create-secondary-retry-record", "SYNC", "inject-error-retryable-once")

	ownerID, err := s.c.CreateEntity(t, s.owner, 1, cbScopeOwnerPayload)
	if err != nil {
		t.Fatalf("owner create: %v; want it committed — a retryable failure leaves the transaction usable", err)
	}
	owner, err := s.c.GetEntity(t, ownerID)
	if err != nil {
		t.Fatalf("GetEntity owner: %v", err)
	}
	if owner.Meta.State != "ACTIVE" {
		t.Errorf("owner state = %q; want ACTIVE", owner.Meta.State)
	}

	first := ReadHopRecord(t, s.c, s.rec, s.marker+"-attempt-1")
	if first.TokenWasEmpty {
		t.Fatal("tokenWasEmpty=true; the callback must have joined the transaction")
	}
	if first.Status != http.StatusBadRequest || first.ErrorCode != "WORKFLOW_FAILED" || !first.Retryable {
		t.Errorf("first joined create = %d %s retryable=%t; want 400 WORKFLOW_FAILED, retryable (body: %s)", first.Status, first.ErrorCode, first.Retryable, first.Body)
	}
	second := ReadHopRecord(t, s.c, s.rec, s.marker+"-attempt-2")
	if second.Status != http.StatusOK {
		t.Fatalf("the retried joined create = %d (body: %s); want 200 — a retryable failure leaves the transaction usable", second.Status, second.Body)
	}

	ids := receivedEntityIDs(t, s.xClient, 2)
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("x's processor ran on %v; want two runs on two entities", ids)
	}
	assertEntityTraceless(t, s.c, ids[0])
	xs, err := s.c.ListEntitiesByModel(t, s.x, 1)
	if err != nil {
		t.Fatalf("list x: %v", err)
	}
	if len(xs) != 1 || xs[0].Meta.ID != ids[1] || xs[0].Meta.State != "STORED" {
		t.Fatalf("committed x entities = %+v; want exactly the retried one (%s) in STORED", xs, ids[1])
	}
	xid, _ := uuid.Parse(ids[1])
	if audit, err := s.c.GetAuditEvents(t, xid); err != nil || len(audit.Items) == 0 {
		t.Errorf("the retried x has no audit events (err: %v)", err)
	}
}

// RunCallbackScope_FailedReadKeepsTx: a joined read of an unknown entity is
// answered 404 ENTITY_NOT_FOUND and keeps its scope; the owner commits.
func RunCallbackScope_FailedReadKeepsTx(t *testing.T, fixture BackendFixture) {
	s := newCbScopeCase(t, fixture, "rd", "cb-get-unknown-record", "SYNC", "")

	ownerID, err := s.c.CreateEntity(t, s.owner, 1, cbScopeOwnerPayload)
	if err != nil {
		t.Fatalf("owner create: %v; want 200 — a read's 404 does not touch the transaction", err)
	}
	hop := ReadHopRecord(t, s.c, s.rec, s.marker)
	if hop.TokenWasEmpty {
		t.Fatal("tokenWasEmpty=true; the callback must have joined the transaction")
	}
	if hop.Status != http.StatusNotFound || hop.ErrorCode != "ENTITY_NOT_FOUND" {
		t.Errorf("joined read = %d %s; want 404 ENTITY_NOT_FOUND (body: %s)", hop.Status, hop.ErrorCode, hop.Body)
	}
	owner, err := s.c.GetEntity(t, ownerID)
	if err != nil {
		t.Fatalf("GetEntity owner: %v", err)
	}
	if owner.Meta.State != "ACTIVE" {
		t.Errorf("owner state = %q; want ACTIVE", owner.Meta.State)
	}
	if all, err := s.c.ListEntitiesByModel(t, s.owner, 1); err != nil || len(all) != 1 {
		t.Errorf("owner entities committed = %d (err: %v); want 1", len(all), err)
	}
}

// RunCallbackScope_AsyncNewTxAbsorbs: the owner's ASYNC_NEW_TX processor's
// callback fails a write. The processor's scope is marked and discarded with
// it: the processor counts as failed, the operation commits, and nothing of the
// callback exists.
func RunCallbackScope_AsyncNewTxAbsorbs(t *testing.T, fixture BackendFixture) {
	s := newCbScopeCase(t, fixture, "an", "cb-create-secondary-record", "ASYNC_NEW_TX", "inject-error-not-retryable")

	ownerID, err := s.c.CreateEntity(t, s.owner, 1, cbScopeOwnerPayload)
	if err != nil {
		t.Fatalf("owner create: %v; want 200 — an ASYNC_NEW_TX processor absorbs its callbacks' failures", err)
	}
	hop := ReadHopRecord(t, s.c, s.rec, s.marker)
	if hop.TokenWasEmpty {
		t.Fatal("tokenWasEmpty=true; the callback must have joined the processor's transaction")
	}
	if hop.Status != http.StatusBadRequest || hop.ErrorCode != "WORKFLOW_FAILED" || hop.Retryable {
		t.Errorf("joined create = %d %s retryable=%t; want 400 WORKFLOW_FAILED, not retryable (body: %s)", hop.Status, hop.ErrorCode, hop.Retryable, hop.Body)
	}
	owner, err := s.c.GetEntity(t, ownerID)
	if err != nil {
		t.Fatalf("GetEntity owner: %v", err)
	}
	if owner.Meta.State != "ACTIVE" {
		t.Errorf("owner state = %q; want ACTIVE — the pipeline goes on", owner.Meta.State)
	}
	var result map[string]any
	for _, ev := range stateMachineEvents(t, s.c, ownerID) {
		if ev.eventType == "STATE_PROCESS_RESULT" && ev.data["processorName"] == "cb-create-secondary-record" {
			result = ev.data
		}
	}
	if result == nil {
		t.Fatal("no STATE_PROCESS_RESULT for the ASYNC_NEW_TX processor")
	}
	if success, ok := result["success"].(bool); !ok || success {
		t.Errorf("STATE_PROCESS_RESULT = %v; want the processor failed", result)
	}
	assertNothingCommitted(t, s.c, s.x, receivedEntityIDs(t, s.xClient, 1)...)
}

// RunCallbackScope_MessageOutsideTx: a message posted under the token never
// joins the transaction. The owner's processor posts one and then fails; the
// owner is refused, and the message is still there.
func RunCallbackScope_MessageOutsideTx(t *testing.T, fixture BackendFixture) {
	s := newCbScopeCase(t, fixture, "msg", "cb-post-message-then-fail", "SYNC", "")

	status, raw, err := s.c.CreateEntityRaw(t, s.owner, 1, cbScopeOwnerPayload)
	if err != nil {
		t.Fatalf("owner create: %v", err)
	}
	cbDecodeProblem(t, status, raw, http.StatusBadRequest, "WORKFLOW_FAILED")
	if all, err := s.c.ListEntitiesByModel(t, s.owner, 1); err != nil || len(all) != 0 {
		t.Errorf("owner entities committed = %d (err: %v); want none", len(all), err)
	}

	hop := ReadHopRecord(t, s.c, s.rec, s.marker)
	if hop.TokenWasEmpty {
		t.Fatal("tokenWasEmpty=true; the callback must have carried the token")
	}
	if hop.Status != http.StatusOK {
		t.Fatalf("message post under the token = %d (body: %s); want 200", hop.Status, hop.Body)
	}
	var posted []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal([]byte(hop.Body), &posted); err != nil || len(posted) == 0 || len(posted[0].EntityIDs) == 0 {
		t.Fatalf("message post answered %q: no message id (%v)", hop.Body, err)
	}
	msg, err := s.c.GetMessage(t, posted[0].EntityIDs[0])
	if err != nil {
		t.Fatalf("GetMessage %s after the owner failed: %v; want it kept — a message is outside the transaction", posted[0].EntityIDs[0], err)
	}
	if msg == nil {
		t.Fatal("GetMessage returned nothing")
	}
}
