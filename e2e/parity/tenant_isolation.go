package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunTenantIsolationEntities verifies that entities created by tenant A
// are invisible, unmodifiable, and audit-invisible to tenant B.
//
// Ports three internal/e2e tests:
//   - TestTenantIsolation_EntitiesInvisible   (GET cross-tenant -> 404)
//   - TestTenantIsolation_DeleteReturns404    (DELETE cross-tenant -> 404, no leakage)
//   - TestTenantIsolation_AuditInvisible      (GET audit cross-tenant -> 404 or empty)
//
// After all cross-tenant operations by B, verifies tenant A can still
// retrieve the entity (200) — the entity was not damaged by B's attempts.
func RunTenantIsolationEntities(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-entity-test"
	const modelVersion = 1

	// Tenant A: set up model + workflow + entity.
	setupSimpleWorkflow(t, clientA, modelName, modelVersion)

	entityID, err := clientA.CreateEntity(t, modelName, modelVersion,
		`{"name":"TenantA","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity (tenant A): %v", err)
	}

	// Tenant B: GET entity by ID -> expect 404 (entity invisible).
	status, _ := clientB.GetEntityRaw(t, entityID)
	if status != http.StatusNotFound {
		t.Errorf("tenant B GET entity: expected 404, got %d", status)
	}

	// Tenant B: DELETE entity -> expect 404 (no existence leakage).
	status, _ = clientB.DeleteEntityRaw(t, entityID)
	if status != http.StatusNotFound {
		t.Errorf("tenant B DELETE entity: expected 404, got %d", status)
	}

	// Tenant B: GET audit events for entity -> expect 404 or 200 with empty items.
	status, err = clientB.GetAuditEventsRaw(t, entityID)
	if status == http.StatusOK {
		// If 200, verify items are empty by doing a full decode.
		auditResp, auditErr := clientB.GetAuditEvents(t, entityID)
		if auditErr != nil {
			t.Fatalf("tenant B GetAuditEvents decode: %v", auditErr)
		}
		if len(auditResp.Items) > 0 {
			t.Error("tenant B should not see tenant A's audit events")
		}
	} else if status != http.StatusNotFound {
		t.Errorf("tenant B GET audit: expected 404 or 200-with-empty-items, got %d (err: %v)", status, err)
	}
	// 404 is acceptable — no existence leakage.

	// Verify tenant A can still GET the entity (200) — not damaged by B.
	got, err := clientA.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("tenant A GET entity after B's attempts: %v", err)
	}
	if got.Data["name"] != "TenantA" {
		t.Errorf("tenant A entity data.name: got %v, want \"TenantA\"", got.Data["name"])
	}
}

// RunTenantIsolationModels verifies that models created by tenant A are
// invisible to tenant B, and that tenant B can independently create and
// lock a model with the same name.
//
// Ports two internal/e2e tests:
//   - TestTenantIsolation_ModelsInvisible          (export cross-tenant -> 404)
//   - TestTenantIsolation_SameModelNameIndependent  (same name, independent lifecycle)
func RunTenantIsolationModels(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-model-test"
	const modelVersion = 1

	// Tenant A: import and lock model.
	if err := clientA.ImportModel(t, modelName, modelVersion, `{"name":"TenantA","amount":100,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel (tenant A): %v", err)
	}
	if err := clientA.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel (tenant A): %v", err)
	}

	// Tenant B: export tenant A's model -> expect 404 (model invisible).
	_, err := clientB.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err == nil {
		t.Error("tenant B ExportModel: expected error (404), got success — model should be invisible")
	}

	// Tenant B: import a model with the SAME name -> expect 200 (independent lifecycle).
	if err := clientB.ImportModel(t, modelName, modelVersion, `{"name":"TenantB","amount":99,"status":"new"}`); err != nil {
		t.Fatalf("ImportModel (tenant B, same name): %v", err)
	}

	// Tenant B: lock same-name model -> expect 200 (independent).
	if err := clientB.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel (tenant B): %v", err)
	}

	// Verify tenant A can still export their own model (200).
	_, err = clientA.ExportModel(t, "SIMPLE_VIEW", modelName, modelVersion)
	if err != nil {
		t.Errorf("tenant A ExportModel after B's operations: %v", err)
	}
}

// problemBodyHasErrorCode reports whether a 4xx Problem-Details body
// carries the expected `properties.errorCode`. Tolerant decode to keep
// the assertion focused on the contract bit (errorCode), not the full
// envelope shape.
func problemBodyHasErrorCode(t *testing.T, body []byte, want string) bool {
	t.Helper()
	type envelope struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
		} `json:"properties"`
	}
	var pd envelope
	if err := json.Unmarshal(body, &pd); err != nil {
		t.Errorf("decode Problem-Details body: %v (body=%s)", err, string(body))
		return false
	}
	return pd.Properties.ErrorCode == want
}

// RunTenantIsolationTransactionIDInvisible pins the contract that the
// `?transactionId=` temporal query parameter cannot be used as an
// existence oracle across tenants. Tenant A creates an entity and
// captures the transactionId from the create envelope. Tenant B then
// asks for the entity by ID with that transactionId — and again with a
// bogus transactionId — and the responses must be byte-equal 404s.
//
// Tenant isolation here is structurally guaranteed (the entity store
// factory derives tenant from request context before any history scan).
// This test pins the invariant so a future refactor that introduced an
// existence oracle on the temporal path would fail loudly. Companion to
// GetOneEntity honouring transactionId.
func RunTenantIsolationTransactionIDInvisible(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-txid-test"
	const modelVersion = 1

	// Tenant A: set up model + workflow + entity, capturing the txID from
	// the create envelope.
	setupSimpleWorkflow(t, clientA, modelName, modelVersion)
	entityID, txIDA, err := clientA.CreateEntityWithTxID(t, modelName, modelVersion,
		`{"name":"TenantA","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID (tenant A): %v", err)
	}
	if txIDA == "" {
		t.Fatal("tenant A create returned empty transactionId — needed to drive cross-tenant lookup")
	}

	// (1) Tenant B asks for tenant A's entity using tenant A's real txID.
	statusReal, bodyReal, err := clientB.GetEntityByTransactionIDBodyRaw(t, entityID, txIDA)
	if err != nil {
		t.Fatalf("tenant B GET ?transactionId=<txID_A>: transport error: %v", err)
	}
	if statusReal != http.StatusNotFound {
		t.Errorf("tenant B GET ?transactionId=<txID_A>: status got %d, want 404 (body=%s)", statusReal, string(bodyReal))
	}
	if !problemBodyHasErrorCode(t, bodyReal, "ENTITY_NOT_FOUND") {
		t.Errorf("tenant B GET ?transactionId=<txID_A>: errorCode != ENTITY_NOT_FOUND (body=%s)", string(bodyReal))
	}

	// (2) Tenant B asks for the same entity with a bogus txID that exists
	// in no tenant. The handler reaches the same tenant-scoped lookup and
	// must produce a byte-identical 404 — no existence oracle.
	bogusTxID := uuid.New().String()
	statusBogus, bodyBogus, err := clientB.GetEntityByTransactionIDBodyRaw(t, entityID, bogusTxID)
	if err != nil {
		t.Fatalf("tenant B GET ?transactionId=<bogus>: transport error: %v", err)
	}
	if statusBogus != http.StatusNotFound {
		t.Errorf("tenant B GET ?transactionId=<bogus>: status got %d, want 404 (body=%s)", statusBogus, string(bodyBogus))
	}
	if !problemBodyHasErrorCode(t, bodyBogus, "ENTITY_NOT_FOUND") {
		t.Errorf("tenant B GET ?transactionId=<bogus>: errorCode != ENTITY_NOT_FOUND (body=%s)", string(bodyBogus))
	}

	// Byte-equality: an existence oracle would surface as a difference
	// between the two response bodies (e.g. distinct error code, distinct
	// detail wording, or one body that hints the entity exists in another
	// tenant). The contract is: the responses are indistinguishable.
	if !bytes.Equal(bodyReal, bodyBogus) {
		t.Errorf("existence oracle: response bodies differ between real and bogus transactionId\n  real:  %s\n  bogus: %s",
			string(bodyReal), string(bodyBogus))
	}
}

// RunTenantIsolationTransitionsTransactionIDRejected pins the contract
// that the transitions endpoint's `?transactionId=` parameter cannot be
// used to resolve another tenant's transaction. Unlike the entity GET
// (which scans tenant-scoped history), the transitions handler resolves
// the txID to a submit time via TransactionManager.GetSubmitTime BEFORE
// any entity lookup — so this pins the tenant check inside GetSubmitTime
// itself: a cross-tenant txID and a nonexistent txID must both yield the
// same 400 status, never a resolved point-in-time (and never a status
// that distinguishes "exists in another tenant" from "doesn't exist").
func RunTenantIsolationTransitionsTransactionIDRejected(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-txid-transitions"
	const modelVersion = 1

	setupSimpleWorkflow(t, clientA, modelName, modelVersion)
	entityID, txIDA, err := clientA.CreateEntityWithTxID(t, modelName, modelVersion,
		`{"name":"TenantA","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID (tenant A): %v", err)
	}
	if txIDA == "" {
		t.Fatal("tenant A create returned empty transactionId — needed to drive cross-tenant lookup")
	}

	// Sanity: tenant A resolves its own txID on its own entity.
	statusOwn, bodyOwn, err := clientA.GetTransitionsByTransactionIDBodyRaw(t, entityID, txIDA)
	if err != nil {
		t.Fatalf("tenant A GET transitions?transactionId=<own>: transport error: %v", err)
	}
	if statusOwn != http.StatusOK {
		t.Errorf("tenant A GET transitions?transactionId=<own>: status got %d, want 200 (body=%s)", statusOwn, string(bodyOwn))
	}

	// (1) Tenant B supplies tenant A's real txID. The submit-time lookup
	// must reject it — 400, not 200 (which would leak that the txID is
	// committed) and not any tenant-A data.
	statusReal, bodyReal, err := clientB.GetTransitionsByTransactionIDBodyRaw(t, entityID, txIDA)
	if err != nil {
		t.Fatalf("tenant B GET transitions?transactionId=<txID_A>: transport error: %v", err)
	}
	if statusReal != http.StatusBadRequest {
		t.Errorf("tenant B GET transitions?transactionId=<txID_A>: status got %d, want 400 (body=%s)", statusReal, string(bodyReal))
	}

	// (2) Tenant B supplies a txID that exists in no tenant — the status
	// must be the same 400, so the status code is not an existence oracle.
	statusBogus, bodyBogus, err := clientB.GetTransitionsByTransactionIDBodyRaw(t, entityID, uuid.New().String())
	if err != nil {
		t.Fatalf("tenant B GET transitions?transactionId=<bogus>: transport error: %v", err)
	}
	if statusBogus != http.StatusBadRequest {
		t.Errorf("tenant B GET transitions?transactionId=<bogus>: status got %d, want 400 (body=%s)", statusBogus, string(bodyBogus))
	}
}

// RunTenantIsolationPointInTimeInvisible pins the contract that the
// `?pointInTime=` temporal query parameter cannot be used as an
// existence oracle across tenants. Tenant A creates an entity at time
// t1; tenant B asks for it at t1+epsilon (when it exists in A's tenant)
// and again at a clearly-bogus point in time before A created it. Both
// responses must be byte-equal 404s.
//
// Companion to the parity helpers and the propagated pointInTime in
// GetEntityChangesMetadata.
func RunTenantIsolationPointInTimeInvisible(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-pit-test"
	const modelVersion = 1

	// Tenant A: set up model + workflow + entity.
	setupSimpleWorkflow(t, clientA, modelName, modelVersion)
	entityID, err := clientA.CreateEntity(t, modelName, modelVersion,
		`{"name":"TenantA","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntity (tenant A): %v", err)
	}

	// afterCreate must be a PIT at which the entity genuinely EXISTS for
	// tenant A — that is what makes probe (1) below distinct from the bogus-PIT
	// probe (2). Derived from the server's clock so skew cannot silently
	// collapse the two probes into the same test (see pit_time.go).
	afterCreate := LatestChangeTime(t, clientA, entityID)
	beforeCreate := afterCreate.Add(-1 * time.Hour)

	// (1) Tenant B asks for tenant A's entity at t1+epsilon (when it
	// exists in A's tenant). Must be 404 ENTITY_NOT_FOUND.
	statusReal, bodyReal, err := clientB.GetEntityAtBodyRaw(t, entityID, afterCreate)
	if err != nil {
		t.Fatalf("tenant B GET ?pointInTime=<afterCreate>: transport error: %v", err)
	}
	if statusReal != http.StatusNotFound {
		t.Errorf("tenant B GET ?pointInTime=<afterCreate>: status got %d, want 404 (body=%s)", statusReal, string(bodyReal))
	}
	if !problemBodyHasErrorCode(t, bodyReal, "ENTITY_NOT_FOUND") {
		t.Errorf("tenant B GET ?pointInTime=<afterCreate>: errorCode != ENTITY_NOT_FOUND (body=%s)", string(bodyReal))
	}

	// (2) Tenant B asks for the same entity at a clearly-bogus PIT before
	// A even created it. The entity does not exist for any tenant at this
	// time; the response must be byte-identical to (1) — no oracle.
	statusBogus, bodyBogus, err := clientB.GetEntityAtBodyRaw(t, entityID, beforeCreate)
	if err != nil {
		t.Fatalf("tenant B GET ?pointInTime=<bogus>: transport error: %v", err)
	}
	if statusBogus != http.StatusNotFound {
		t.Errorf("tenant B GET ?pointInTime=<bogus>: status got %d, want 404 (body=%s)", statusBogus, string(bodyBogus))
	}
	if !problemBodyHasErrorCode(t, bodyBogus, "ENTITY_NOT_FOUND") {
		t.Errorf("tenant B GET ?pointInTime=<bogus>: errorCode != ENTITY_NOT_FOUND (body=%s)", string(bodyBogus))
	}

	if !bytes.Equal(bodyReal, bodyBogus) {
		t.Errorf("existence oracle: response bodies differ between real and bogus pointInTime\n  real:  %s\n  bogus: %s",
			string(bodyReal), string(bodyBogus))
	}
}

// RunTenantIsolationChangesAtPITInvisible pins the contract that the
// `?pointInTime=` query parameter on the change-history endpoint
// (`/api/entity/{id}/changes`) cannot be used as an existence oracle
// across tenants. Tenant A creates and updates an entity; tenant B
// asks for its change history at a recent PIT and at a bogus PIT —
// both must return byte-equal 404s.
//
// Companion to the propagated pointInTime in GetEntityChangesMetadata.
func RunTenantIsolationChangesAtPITInvisible(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-changes-pit-test"
	const modelVersion = 1

	// Tenant A: set up the temporal workflow (NONE->CREATED auto,
	// CREATED->CREATED manual UPDATE) so we can produce multiple changes.
	if err := clientA.ImportModel(t, modelName, modelVersion, `{"name":"Temporal","amount":0,"status":"init"}`); err != nil {
		t.Fatalf("ImportModel (tenant A): %v", err)
	}
	if err := clientA.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel (tenant A): %v", err)
	}
	if err := clientA.ImportWorkflow(t, modelName, modelVersion, temporalWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow (tenant A): %v", err)
	}

	entityID, err := clientA.CreateEntity(t, modelName, modelVersion,
		`{"name":"TenantA","amount":1,"status":"v1"}`)
	if err != nil {
		t.Fatalf("CreateEntity (tenant A): %v", err)
	}
	if err := clientA.UpdateEntity(t, entityID, "UPDATE",
		`{"name":"TenantA","amount":2,"status":"v2"}`); err != nil {
		t.Fatalf("UpdateEntity v2 (tenant A): %v", err)
	}

	// A PIT at which the history genuinely EXISTS for tenant A, on the server's
	// clock — see pit_time.go. beforeCreate is the far-past bogus probe.
	afterUpdates := LatestChangeTime(t, clientA, entityID)
	beforeCreate := afterUpdates.Add(-1 * time.Hour)

	// (1) Tenant B asks for the change history of tenant A's entity at a
	// PIT after the updates landed. Must be 404 ENTITY_NOT_FOUND.
	statusReal, bodyReal, err := clientB.GetEntityChangesAtBodyRaw(t, entityID, afterUpdates)
	if err != nil {
		t.Fatalf("tenant B GET /changes?pointInTime=<afterUpdates>: transport error: %v", err)
	}
	if statusReal != http.StatusNotFound {
		t.Errorf("tenant B GET /changes?pointInTime=<afterUpdates>: status got %d, want 404 (body=%s)", statusReal, string(bodyReal))
	}
	if !problemBodyHasErrorCode(t, bodyReal, "ENTITY_NOT_FOUND") {
		t.Errorf("tenant B GET /changes?pointInTime=<afterUpdates>: errorCode != ENTITY_NOT_FOUND (body=%s)", string(bodyReal))
	}

	// (2) Tenant B asks for the same entity's change history at a bogus
	// PIT before any tenant created the entity. The response must be
	// byte-identical to (1).
	statusBogus, bodyBogus, err := clientB.GetEntityChangesAtBodyRaw(t, entityID, beforeCreate)
	if err != nil {
		t.Fatalf("tenant B GET /changes?pointInTime=<bogus>: transport error: %v", err)
	}
	if statusBogus != http.StatusNotFound {
		t.Errorf("tenant B GET /changes?pointInTime=<bogus>: status got %d, want 404 (body=%s)", statusBogus, string(bodyBogus))
	}
	if !problemBodyHasErrorCode(t, bodyBogus, "ENTITY_NOT_FOUND") {
		t.Errorf("tenant B GET /changes?pointInTime=<bogus>: errorCode != ENTITY_NOT_FOUND (body=%s)", string(bodyBogus))
	}

	if !bytes.Equal(bodyReal, bodyBogus) {
		t.Errorf("existence oracle: response bodies differ between real and bogus pointInTime on /changes\n  real:  %s\n  bogus: %s",
			string(bodyReal), string(bodyBogus))
	}
}

// RunTenantIsolationWorkflowFinishedInvisible pins the contract that the
// workflow-finished door (GET /api/audit/entity/{id}/workflow/{txId}/finished)
// is tenant-isolated the same way as the search door already pinned by
// RunTenantIsolationEntities' audit check. Tenant A creates an entity
// through a workflow that finishes (setupSimpleWorkflow — an auto
// transition to CREATED), producing a STATE_MACHINE_FINISH event for its
// transaction. Tenant B then asks for that same entity/transaction pair
// on the finished endpoint and must get 404 — never tenant A's event.
//
// The body is fetched with DoJSONBodyRaw rather than the typed
// GetWorkflowFinished client method: that method returns a nil result map
// on any non-2xx status (it never decodes the body — see its doc comment),
// so a "resultB has no eventId" check against it would be vacuously true
// for EVERY response, not just a correctly-isolated one. Reading the raw
// body and checking it for tenant A's real eventId (captured from tenant
// A's own successful call) and the STATE_MACHINE_FINISH event-type string
// makes the assertion capable of failing.
func RunTenantIsolationWorkflowFinishedInvisible(t *testing.T, fixture BackendFixture) {
	tenantA := fixture.NewTenant(t)
	tenantB := fixture.NewTenant(t)
	clientA := client.NewClient(fixture.BaseURL(), tenantA.Token)
	clientB := client.NewClient(fixture.BaseURL(), tenantB.Token)

	const modelName = "iso-workflow-finished-test"
	const modelVersion = 1

	// Tenant A: set up model + workflow + entity, capturing the txID that
	// produced the STATE_MACHINE_FINISH event.
	setupSimpleWorkflow(t, clientA, modelName, modelVersion)
	entityID, txIDA, err := clientA.CreateEntityWithTxID(t, modelName, modelVersion,
		`{"name":"TenantA","amount":10,"status":"new"}`)
	if err != nil {
		t.Fatalf("CreateEntityWithTxID (tenant A): %v", err)
	}
	if txIDA == "" {
		t.Fatal("tenant A create returned empty transactionId — needed to drive cross-tenant lookup")
	}

	finishedPath := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID.String(), txIDA)

	// Sanity: tenant A can resolve its own finished event (200), and its
	// body carries a real eventId — the value tenant B's response must
	// never contain.
	statusOwn, bodyOwn, err := clientA.DoJSONBodyRaw(t, http.MethodGet, finishedPath, nil)
	if err != nil {
		t.Fatalf("tenant A GET workflow finished (own): transport error: %v", err)
	}
	if statusOwn != http.StatusOK {
		t.Fatalf("tenant A GET workflow finished (own): status got %d, want 200 (body=%s)", statusOwn, string(bodyOwn))
	}
	var ownResult map[string]any
	if err := json.Unmarshal(bodyOwn, &ownResult); err != nil {
		t.Fatalf("decode tenant A's own finished response: %v (body=%s)", err, string(bodyOwn))
	}
	ownEventID, _ := ownResult["eventId"].(string)
	if ownEventID == "" {
		t.Fatalf("tenant A's own finished response has no eventId: %s", string(bodyOwn))
	}

	// Tenant B: same entity/transaction pair -> 404, and the raw body must
	// contain neither tenant A's real eventId nor the STATE_MACHINE_FINISH
	// event-type string.
	statusB, bodyB, err := clientB.DoJSONBodyRaw(t, http.MethodGet, finishedPath, nil)
	if err != nil {
		t.Fatalf("tenant B GET workflow finished (tenant A's entity/tx): transport error: %v", err)
	}
	assertTenantBFinishedResponseIsolated(t, statusB, bodyB, ownEventID)
}

// assertTenantBFinishedResponseIsolated asserts that a cross-tenant response
// to the workflow-finished endpoint is a 404 carrying neither the owning
// tenant's real eventId nor the STATE_MACHINE_FINISH event-type string.
// Factored out so the non-vacuousness of this check can be demonstrated by
// calling it with the OWNING tenant's own 200 status/body (see the
// proof-of-non-vacuousness note in the security fix report): that call
// must fail, since the owner's own body necessarily contains its own
// eventId.
func assertTenantBFinishedResponseIsolated(t *testing.T, status int, body []byte, ownerEventID string) {
	t.Helper()
	if status != http.StatusNotFound {
		t.Errorf("tenant B GET workflow finished (tenant A's entity/tx): status got %d, want 404 (body=%s)", status, string(body))
	}
	if bytes.Contains(body, []byte(ownerEventID)) {
		t.Errorf("tenant B's response contains tenant A's real eventId %q: body=%s", ownerEventID, string(body))
	}
	if bytes.Contains(body, []byte("STATE_MACHINE_FINISH")) {
		t.Errorf("tenant B's response contains STATE_MACHINE_FINISH: body=%s", string(body))
	}
}
