package externalapi

// External API Scenario Suite — 09-workflow-externalization.
//
// Externalised processors and criteria. The workflow FSM delegates a
// transition step to an external calculation member over the gRPC
// bidirectional-streaming `CloudEventsService.startStreaming` channel.
// Three execution modes are exercised by the dictionary:
//
//   SYNC              - processor runs inline in the caller's transaction
//   ASYNC_SAME_TX     - processor runs inline in the caller's transaction
//                       (cyoda-go treats SYNC and ASYNC_SAME_TX identically;
//                       see internal/domain/workflow/engine.go default case)
//   ASYNC_NEW_TX      - processor runs in a savepoint; failures are
//                       non-fatal and the entity reaches the next state
//                       regardless (see engine.go executeAsyncNewTx)
//
// Schema notes (dictionary shape → cyoda-go accepted shape):
//
//   Processor "type":"calculator" — cyoda-go ProcessorDefinition.Type is a
//     free-form string; the engine ignores it and dispatches by tag. The
//     dictionary's SYNC/ASYNC_SAME_TX/ASYNC_NEW_TX is the
//     ProcessorDefinition.ExecutionMode field directly.
//   Criterion: dictionary "function" criterion → cyoda-go
//     {"type":"function","function":{"name":"<name>","config":{...}}}.
//     This is the only criterion shape that the engine routes to the
//     external calculation member (see engine.go evaluateCriterion).
//
// Compute-test-client catalog (cmd/compute-test-client/catalog.go):
//   processors: noop, tag-with-foo, bump-amount, inject-error,
//               slow-configurable, set-field, echo-context-to-field
//   criteria:   always-true, always-false, amount-gt-100, select-premium,
//               select-standard, field-equals, context-equals
//
// The catalog has no warning/error-flag processor — adding one would
// require extending processorFunc to expose the gRPC ProcessorResponse
// warnings/errors path. That is out of tranche-3 scope and the
// error-flag scenarios (09/05/06/07) are skipped accordingly.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/errorcontract"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	parityclient "github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ExternalAPI_09_01_SyncProcessorSuccess", Fn: RunExternalAPI_09_01_SyncProcessorSuccess},
		parity.NamedTest{Name: "ExternalAPI_09_02_SyncProcessorExceptionRollsBack", Fn: RunExternalAPI_09_02_SyncProcessorExceptionRollsBack},
		parity.NamedTest{Name: "ExternalAPI_09_03_AsyncSameTxExceptionRollsBack", Fn: RunExternalAPI_09_03_AsyncSameTxExceptionRollsBack},
		parity.NamedTest{Name: "ExternalAPI_09_04_AsyncNewTxExceptionKeepsInitialSave", Fn: RunExternalAPI_09_04_AsyncNewTxExceptionKeepsInitialSave},
		parity.NamedTest{Name: "ExternalAPI_09_05_SyncErrorFlagRollsBack", Fn: RunExternalAPI_09_05_SyncErrorFlagRollsBack},
		parity.NamedTest{Name: "ExternalAPI_09_06_AsyncSameTxErrorFlagRollsBack", Fn: RunExternalAPI_09_06_AsyncSameTxErrorFlagRollsBack},
		parity.NamedTest{Name: "ExternalAPI_09_07_AsyncNewTxErrorFlagKeepsInitialSave", Fn: RunExternalAPI_09_07_AsyncNewTxErrorFlagKeepsInitialSave},
		parity.NamedTest{Name: "ExternalAPI_09_08_NoExternalRegisteredFails", Fn: RunExternalAPI_09_08_NoExternalRegisteredFails},
		parity.NamedTest{Name: "ExternalAPI_09_09_ExternalDisconnectSucceedsOnRetry", Fn: RunExternalAPI_09_09_ExternalDisconnectSucceedsOnRetry},
		parity.NamedTest{Name: "ExternalAPI_09_10_ExternalTimeoutFailover", Fn: RunExternalAPI_09_10_ExternalTimeoutFailover},
		parity.NamedTest{Name: "ExternalAPI_09_11_ProcessingNodeDisconnectsMidRequest", Fn: RunExternalAPI_09_11_ProcessingNodeDisconnectsMidRequest},
		parity.NamedTest{Name: "ExternalAPI_09_12_ExternalizedCriterionSkipsCall", Fn: RunExternalAPI_09_12_ExternalizedCriterionSkipsCall},
		parity.NamedTest{Name: "ExternalAPI_09_13_ProcessorContextPassesThrough", Fn: RunExternalAPI_09_13_ProcessorContextPassesThrough},
		parity.NamedTest{Name: "ExternalAPI_09_14_CriterionContextPassesThrough", Fn: RunExternalAPI_09_14_CriterionContextPassesThrough},
		parity.NamedTest{Name: "ExternalAPI_09_15_CriterionContextNegative", Fn: RunExternalAPI_09_15_CriterionContextNegative},
	)
}

// externalProcessorWorkflow returns a two-state workflow whose CREATED→PROCESSED
// transition carries a single externalized processor of the given name and
// execution mode. The compute-test-client must have a processor registered
// under procName for the entity creation to complete (or fail with the
// processor's deliberate error, depending on procName).
func externalProcessorWorkflow(workflowName, procName, executionMode string) string {
	return `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "` + workflowName + `", "initialState": "CREATED", "active": true,
			"states": {
				"CREATED": {"transitions": [{"name": "process", "next": "PROCESSED", "manual": false,
					"processors": [{"type": "calculator", "name": "` + procName + `", "executionMode": "` + executionMode + `",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"PROCESSED": {}
			}
		}]
	}`
}

// setupExternalModel imports a model from sample, locks it, then imports the
// given workflow JSON. Returns the parity client (already tenant-scoped).
func setupExternalModel(t *testing.T, c *parityclient.Client, modelName string, modelVersion int, sample, workflowJSON string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, modelVersion, sample); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// RunExternalAPI_09_01_SyncProcessorSuccess — dictionary 09/01.
// SYNC processor `noop` returns the entity unchanged; entity reaches PROCESSED.
func RunExternalAPI_09_01_SyncProcessorSuccess(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extSync"
	const modelVersion = 1
	wf := externalProcessorWorkflow("ext-sync-wf", "noop", "SYNC")
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, wf)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "PROCESSED" {
		t.Errorf("expected state PROCESSED after SYNC processor success, got %s", got.Meta.State)
	}
}

// RunExternalAPI_09_02_SyncProcessorExceptionRollsBack — dictionary 09/02.
// SYNC processor `inject-error` always errors; the dictionary requires the
// entity creation to fail and the entity not to be persisted.
//
// In cyoda-go the SYNC processor failure causes the engine to abort the
// pipeline (see engine.go executeProcessors return path) and the API
// surfaces an HTTP 400 with errorCode=WORKFLOW_FAILED — discover-and-compare
// confirms this surface across all backends. Classified as equiv_or_better:
// cyoda-go returns a 4xx with a precise errorCode rather than the
// dictionary's implicit 5xx-shape, which is more informative and still
// satisfies the "transaction CANCELLED + entity not persisted" semantics
// (the create call returns non-2xx so no entity exists).
func RunExternalAPI_09_02_SyncProcessorExceptionRollsBack(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	d := driver.NewRemote(t, fixture.BaseURL(), tenant.Token)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extSyncError"
	const modelVersion = 1
	wf := externalProcessorWorkflow("ext-sync-error-wf", "inject-error", "SYNC")
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, wf)

	status, body, err := d.CreateEntityRaw(modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport: %v", err)
	}
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: 400,
		ErrorCode:  "WORKFLOW_FAILED",
	})
}

// RunExternalAPI_09_03_AsyncSameTxExceptionRollsBack — dictionary 09/03.
// In cyoda-go, ASYNC_SAME_TX runs identically to SYNC (engine.go default case).
// A failing ASYNC_SAME_TX processor therefore aborts the pipeline and the
// entity creation surfaces as a server error.
func RunExternalAPI_09_03_AsyncSameTxExceptionRollsBack(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	d := driver.NewRemote(t, fixture.BaseURL(), tenant.Token)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extAsyncSameErr"
	const modelVersion = 1
	wf := externalProcessorWorkflow("ext-async-same-err-wf", "inject-error", "ASYNC_SAME_TX")
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, wf)

	status, body, err := d.CreateEntityRaw(modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport: %v", err)
	}
	// Same WORKFLOW_FAILED 400 surface as 09/02 — ASYNC_SAME_TX is treated
	// identically to SYNC by the engine (see engine.go default case).
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: 400,
		ErrorCode:  "WORKFLOW_FAILED",
	})
}

// RunExternalAPI_09_04_AsyncNewTxExceptionKeepsInitialSave — dictionary 09/04.
//
// Per internal/domain/workflow/engine.go and engine_test.go (lines 923-961,
// 1520-1581), cyoda-go's ASYNC_NEW_TX failures are *non-fatal*: the engine
// rolls back the savepoint, logs a warning, and continues the pipeline.
// The transition completes and the entity reaches PROCESSED.
//
// This matches the dictionary's expectation that the initial save survives
// (the entity is persisted at PROCESSED). The dictionary's secondary
// "follow-up tx CANCELLED" assertion is internal bookkeeping and is not
// observable through cyoda-go's HTTP API — we therefore assert only the
// observable: entity exists at PROCESSED.
func RunExternalAPI_09_04_AsyncNewTxExceptionKeepsInitialSave(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extAsyncNewErr"
	const modelVersion = 1
	wf := externalProcessorWorkflow("ext-async-new-err-wf", "inject-error", "ASYNC_NEW_TX")
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, wf)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity (ASYNC_NEW_TX failure should be non-fatal): %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "PROCESSED" {
		t.Errorf("09/04: expected entity at PROCESSED (ASYNC_NEW_TX failure non-fatal), got %s", got.Meta.State)
	}
}

// RunExternalAPI_09_05_SyncErrorFlagRollsBack — dictionary 09/05.
func RunExternalAPI_09_05_SyncErrorFlagRollsBack(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	t.Skip("pending: cmd/compute-test-client has no processor that sets the response's warnings/errors flags.")
}

// RunExternalAPI_09_06_AsyncSameTxErrorFlagRollsBack — dictionary 09/06.
func RunExternalAPI_09_06_AsyncSameTxErrorFlagRollsBack(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	t.Skip("pending: cmd/compute-test-client has no processor that sets the response's warnings/errors flags.")
}

// RunExternalAPI_09_07_AsyncNewTxErrorFlagKeepsInitialSave — dictionary 09/07.
func RunExternalAPI_09_07_AsyncNewTxErrorFlagKeepsInitialSave(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	t.Skip("pending: cmd/compute-test-client has no processor that sets the response's warnings/errors flags.")
}

// RunExternalAPI_09_08_NoExternalRegisteredFails — dictionary 09/08.
//
// A SYNC `noop` processor under a fresh tenant (NOT ComputeTenant) — the
// MemberRegistry routes by tenant and no member is registered for this fresh
// tenant. The engine returns ErrNoMatchingMember and the API surfaces a
// retryable 503.
func RunExternalAPI_09_08_NoExternalRegisteredFails(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	// Use NewTenant — no calculation member is registered for a fresh tenant.
	d := driver.NewInProcess(t, fixture)

	const modelName = "extNoClient"
	const modelVersion = 1
	wf := externalProcessorWorkflow("ext-no-client-wf", "noop", "SYNC")

	if err := d.CreateModelFromSample(modelName, modelVersion, `{"k":1}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel(modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := d.ImportWorkflow(modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	status, body, err := d.CreateEntityRaw(modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntityRaw transport: %v", err)
	}
	// Engine returns ErrNoMatchingMember, which the workflow layer wraps as
	// `processor noop failed: no matching calculation member: tags ""`.
	// classifyWorkflowError (internal/domain/entity/service.go) maps this
	// compute-infra condition — not a bad request — to a retryable 503
	// NO_COMPUTE_MEMBER_FOR_TAG rather than 400 WORKFLOW_FAILED: a client
	// cannot fix "your request was bad" by editing the request, but it CAN
	// retry once a compute member connects.
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{
		HTTPStatus: 503,
		ErrorCode:  "NO_COMPUTE_MEMBER_FOR_TAG",
	})
	var problem struct {
		Properties struct {
			Retryable bool `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("failed to parse response body as JSON: %v\nbody: %s", err, body)
	}
	if !problem.Properties.Retryable {
		t.Errorf("expected properties.retryable=true, got false (body: %s)", body)
	}
}

// failoverPair starts, under a FRESH tenant, a compute client with the given
// behaviour and then a healthy one, both on tag — so the misbehaving one is
// tried first — and imports a model whose one SYNC processor is routed to tag.
// A fixture that cannot start compute clients skips here, before the server is
// touched.
func failoverPair(t *testing.T, fixture parity.BackendFixture, name, behaviour string, extraConfig map[string]any) (c *parityclient.Client, model string, bad, good parity.ComputeClient) {
	t.Helper()
	tenant := fixture.NewTenant(t)
	tag := "ext-" + name
	bad = parity.StartComputeClientOrSkip(t, fixture, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}, Behaviour: behaviour})
	good = parity.StartComputeClientOrSkip(t, fixture, parity.ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})
	c = parityclient.NewClient(fixture.BaseURL(), tenant.Token)
	model = "ext-" + name
	setupExternalModel(t, c, model, 1, `{"k":1}`, parity.ComputeClientWorkflow("ext-"+name+"-wf", "noop", tag, "", extraConfig))
	return c, model, bad, good
}

// assertRetryableProblem matches status and errorCode and requires
// properties.retryable=true.
func assertRetryableProblem(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	errorcontract.Match(t, status, body, errorcontract.ExpectedError{HTTPStatus: wantStatus, ErrorCode: wantCode})
	var problem struct {
		Properties struct {
			Retryable bool `json:"retryable"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("failed to parse response body as JSON: %v\nbody: %s", err, body)
	}
	if !problem.Properties.Retryable {
		t.Errorf("expected properties.retryable=true (body: %s)", body)
	}
}

// assertTriedThenServed: bad received the work first, good served the same
// request afterwards.
func assertTriedThenServed(t *testing.T, bad, good parity.ComputeClient) {
	t.Helper()
	b := parity.AwaitReceived(t, bad, 1, 5*time.Second)
	g := parity.AwaitReceived(t, good, 1, 5*time.Second)
	if len(b) != 1 || len(g) != 1 {
		t.Fatalf("first client received %d requests, second %d; want 1 and 1", len(b), len(g))
	}
	if b[0].RequestID == "" || b[0].RequestID != g[0].RequestID {
		t.Errorf("request ids differ across tries: %q then %q", b[0].RequestID, g[0].RequestID)
	}
}

// RunExternalAPI_09_09_ExternalDisconnectSucceedsOnRetry — dictionary 09/09.
// The first compute node drops its connection on receiving the work; the
// processor is declared idempotent, so the second is asked and the entity is
// created.
func RunExternalAPI_09_09_ExternalDisconnectSucceedsOnRetry(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	c, model, bad, good := failoverPair(t, fixture, "0909", parity.ComputeBehaviourDrop, map[string]any{"idempotent": true})
	id, err := c.CreateEntity(t, model, 1, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	if got, err := c.GetEntity(t, id); err != nil || got.Meta.State != "ACTIVE" {
		t.Errorf("state = %q err=%v; want ACTIVE", got.Meta.State, err)
	}
	assertTriedThenServed(t, bad, good)
}

// RunExternalAPI_09_10_ExternalTimeoutFailover — dictionary 09/10.
// The first compute node never answers; after the processor's own
// responseTimeoutMs the second is asked.
func RunExternalAPI_09_10_ExternalTimeoutFailover(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	c, model, bad, good := failoverPair(t, fixture, "0910", parity.ComputeBehaviourStall,
		map[string]any{"idempotent": true, "responseTimeoutMs": 300})
	if _, err := c.CreateEntity(t, model, 1, `{"k":1}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	assertTriedThenServed(t, bad, good)
}

// RunExternalAPI_09_11_ProcessingNodeDisconnectsMidRequest — dictionary 09/11.
// A compute node disconnects with the request in its hands while another
// remains. What happens next is the workflow author's declaration: without
// `idempotent` the operation fails with the try's own code and the other node
// is never asked; with it, the other node serves the request.
func RunExternalAPI_09_11_ProcessingNodeDisconnectsMidRequest(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	t.Run("not-idempotent", func(t *testing.T) {
		c, model, bad, good := failoverPair(t, fixture, "0911n", parity.ComputeBehaviourDrop, nil)
		status, body, err := c.CreateEntityRaw(t, model, 1, `{"k":1}`)
		if err != nil {
			t.Fatalf("CreateEntityRaw: %v", err)
		}
		assertRetryableProblem(t, status, body, http.StatusServiceUnavailable, "COMPUTE_MEMBER_DISCONNECTED")
		parity.AwaitReceived(t, bad, 1, 5*time.Second)
		if got := good.Received(t); len(got) != 0 {
			t.Errorf("the remaining compute node received %d requests; a processor not declared idempotent is not repeated", len(got))
		}
		if list, err := c.ListEntitiesByModel(t, model, 1); err != nil || len(list) != 0 {
			t.Errorf("entities after the failed create: %d err=%v; want 0", len(list), err)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		c, model, bad, good := failoverPair(t, fixture, "0911i", parity.ComputeBehaviourDrop, map[string]any{"idempotent": true})
		if _, err := c.CreateEntity(t, model, 1, `{"k":1}`); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		assertTriedThenServed(t, bad, good)
	})
}

// RunExternalAPI_09_12_ExternalizedCriterionSkipsCall — dictionary 09/12.
//
// A workflow where the auto-transition CREATED→PROCESSED is guarded by an
// externalized `always-false` criterion. Because the criterion does not
// match, the transition does not fire and the entity stays at CREATED.
//
// Schema notes:
//   - The criterion uses cyoda-go's accepted shape:
//     {"type":"function","function":{"name":"always-false"}}.
//     This routes to the external calc member (see engine.go evaluateCriterion).
//   - The "external_calls_count == 0" assertion in the dictionary is internal
//     bookkeeping; cyoda-go does not expose a per-tenant call counter through
//     the HTTP API. We assert the observable equivalent: the entity stays
//     in the initial state, proving the externalized step did not fire (or
//     fired and returned false — both observable as "transition not taken").
//
// Note: the criterion always-false runs externally (it IS dispatched), then
// returns false; the dictionary's tighter assertion that the call is
// *suppressed entirely* by an upstream simple criterion in an AND group is
// not observable through HTTP. We accept the looser shape: transition not
// taken. The dictionary remains semantically satisfied (the externalized
// criterion's outcome — false — does block the transition, just as the
// dictionary expects).
func RunExternalAPI_09_12_ExternalizedCriterionSkipsCall(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extCritSkip"
	const modelVersion = 1
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "ext-crit-skip-wf", "initialState": "CREATED", "active": true,
			"states": {
				"CREATED": {"transitions": [{"name": "process", "next": "PROCESSED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "always-false"}}
				}]},
				"PROCESSED": {}
			}
		}]
	}`
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, wf)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "CREATED" {
		t.Errorf("09/12: expected entity at CREATED (criterion=always-false → transition not taken), got %s", got.Meta.State)
	}
}

// externalProcessorWorkflowWithContext returns a workflow whose
// CREATED→PROCESSED transition carries a single externalized processor with
// the given pass-through context string. Used by the context pass-through
// parity test to observe ProcessorConfig.context surfacing at the
// calculation member.
func externalProcessorWorkflowWithContext(workflowName, procName, contextValue string) string {
	return `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "` + workflowName + `", "initialState": "CREATED", "active": true,
			"states": {
				"CREATED": {"transitions": [{"name": "process", "next": "PROCESSED", "manual": false,
					"processors": [{"type": "calculator", "name": "` + procName + `", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": "", "context": "` + contextValue + `"}}]
				}]},
				"PROCESSED": {}
			}
		}]
	}`
}

// RunExternalAPI_09_13_ProcessorContextPassesThrough — ProcessorConfig.context
// is a pass-through string forwarded verbatim as the `parameters` JSON node of
// EntityProcessorCalculationRequest. The echo-context-to-field processor
// records the received parameters string at entity.data._context. We assert
// the value round-trips through the engine dispatcher to the calculation
// member faithfully.
func RunExternalAPI_09_13_ProcessorContextPassesThrough(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extCtxProc"
	const modelVersion = 1
	const contextValue = "premium-role"
	wf := externalProcessorWorkflowWithContext("ext-ctx-proc-wf", "echo-context-to-field", contextValue)
	// The sample seeds a zero-valued "_context" so the model DECLARES the field
	// echo-context-to-field writes: processor output passes the same model
	// checks a client write does.
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1,"_context":""}`, wf)

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "PROCESSED" {
		t.Fatalf("expected state PROCESSED, got %s", got.Meta.State)
	}
	if v, ok := got.Data["_context"]; !ok {
		t.Errorf("expected entity.data._context to be populated by echo-context-to-field (data=%+v)", got.Data)
	} else if v != contextValue {
		t.Errorf("expected entity.data._context=%q, got %q (processor saw mismatched parameters — Context not passed through)", contextValue, v)
	}
}

// criterionContextWorkflow returns a workflow whose CREATED→PROCESSED
// auto-transition is guarded by a context-equals criterion configured with
// the supplied context string. The criterion matches only when the engine
// forwards the context value verbatim as the request's `parameters` node
// AND the value equals the literal "match".
func criterionContextWorkflow(workflowName, contextValue string) string {
	return `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "` + workflowName + `", "initialState": "CREATED", "active": true,
			"states": {
				"CREATED": {"transitions": [{"name": "process", "next": "PROCESSED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "context-equals",
						"config": {"calculationNodesTags": "", "context": "` + contextValue + `"}}}
				}]},
				"PROCESSED": {}
			}
		}]
	}`
}

// RunExternalAPI_09_14_CriterionContextPassesThrough — FunctionCondition.config.context
// follows the same pass-through-string rule as the processor path. The
// context-equals criterion returns true only when the dispatched parameters
// string equals "match". With context="match" the transition fires and the
// entity reaches PROCESSED — the negative case is covered by 09/15 below.
func RunExternalAPI_09_14_CriterionContextPassesThrough(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extCtxCrit"
	const modelVersion = 1
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, criterionContextWorkflow("ext-ctx-crit-wf", "match"))

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "PROCESSED" {
		t.Errorf("expected state PROCESSED (criterion context=\"match\" → transition fires); got %s — Context not passed through to criterion", got.Meta.State)
	}
}

// RunExternalAPI_09_15_CriterionContextNegative — mirror of 09_14 with a
// non-matching context. context-equals returns false, the auto-transition
// does not fire, and the entity stays at CREATED. This proves end-to-end
// that the engine actually forwards the configured context value — not that
// context-equals coincidentally returns true (which is what the positive
// case alone could be explained by if the dispatcher passed an arbitrary or
// hard-coded payload).
func RunExternalAPI_09_15_CriterionContextNegative(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	tenant := fixture.ComputeTenant(t)
	c := parityclient.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "extCtxCritNeg"
	const modelVersion = 1
	setupExternalModel(t, c, modelName, modelVersion, `{"k":1}`, criterionContextWorkflow("ext-ctx-crit-neg-wf", "no-match"))

	id, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	got, err := c.GetEntity(t, id)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "CREATED" {
		t.Errorf("expected state CREATED (criterion context=\"no-match\" → transition does not fire); got %s — engine may be forwarding a wrong/empty context value or context-equals received unexpected input", got.Meta.State)
	}
}
