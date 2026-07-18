package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
	"github.com/cyoda-platform/cyoda-go/internal/cluster"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
)

// scheduled_function_test.go is Task 9.1's E2E layer for the scheduled-
// transition Function feature (issue #419, design
// docs/superpowers/specs/2026-07-17-scheduled-transition-function-design.md):
// import-time validation of the schedule.function shape, and arm-time happy
// paths through the full HTTP stack.
//
// Import-validation cases (no compute-node dispatch involved — validate.go
// rejects these shapes statically before any Function callout is attempted)
// run against the shared package-level testApp via doAuth/importWorkflowE2E,
// same as every other workflow-import E2E test in this package.
//
// Arm happy-path cases need a real Function callout, so they use the
// callback-harness's gRPC compute member (callback_harness_test.go),
// extended in this change with RegisterFunction/handleFunctionRequest to
// answer EntityFunctionCalculationRequest with a configurable
// resultKind/result — the shared, reusable "fake Function-serving compute
// node" tasks 9.2/9.3 build on. The member joins with tags ["sched-fn"] so
// a schedule.function config's calculationNodesTags (validated non-empty at
// import, unlike the processor/criteria tests' calculationNodesTags:"")
// resolves to it via MemberRegistry.FindByTags.
//
// The shared harness's scheduler runs with its default cadence
// (CYODA_SCHEDULER_SCAN_INTERVAL, 1s — see scheduled_transition_test.go's
// scheduledFireTimeout/awaitEntityStateE2E, which this file reuses/mirrors
// for the callback-harness stack via awaitCallbackEntityState), so the
// "fires at the expected time" assertions poll for the real fire, not just
// the armed row. Storage-level assertions (exact scheduled_time/timeout_ms)
// additionally query scheduled_tasks directly, mirroring
// TestE2E_ScheduledTransition_RestartDurability's durability check.

// scheduledFnTag is the calculationNodesTags value used by every
// schedule.function config in this file — must match the callback-harness
// compute member's join tags (callback_harness_test.go).
const scheduledFnTag = "sched-fn"

// scheduleFunctionWorkflowJSON builds a REPLACE-mode workflow import payload
// (schema version 1.3 — schedule.function requires it, per
// docs/workflow-schema-versioning.md) with a single non-manual
// Open -[AutoClose]-> Closed transition carrying the given schedule.function
// shape. fnJSON is the raw JSON object literal for "function".
func scheduleFunctionWorkflowJSON(wfName, fnJSON string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": %q, "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": {"function": %s}
				}]},
				"Closed": {}
			}
		}]
	}`, wfName, fnJSON)
}

// validScheduleFunctionJSON is a shape-valid schedule.function config
// literal, reused by the arm happy-path tests below (each overrides "name"
// to bind a distinct callback).
func validScheduleFunctionJSON(functionName string) string {
	return fmt.Sprintf(`{"name":%q,"resultKind":"Schedule","calculationNodesTags":%q}`, functionName, scheduledFnTag)
}

// assertImportRejected asserts a workflow import fails with 400
// VALIDATION_FAILED — the pattern already established by
// workflow_annotations_test.go for shape-validation rejections.
func assertImportRejected(t *testing.T, status int, body, why string) {
	t.Helper()
	if status != http.StatusBadRequest || !strings.Contains(body, "VALIDATION_FAILED") {
		t.Fatalf("expected 400 VALIDATION_FAILED (%s), got %d: %s", why, status, body)
	}
}

// --- Import validation (no compute-node dispatch) ---

func TestScheduledFunction_Import_ValidAccepted(t *testing.T) {
	const model = "e2e-schedfn-import-valid"
	wf := scheduleFunctionWorkflowJSON("schedfn-valid-wf", validScheduleFunctionJSON("calcFire"))
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	if status != http.StatusOK {
		t.Fatalf("expected 200 for valid schedule.function import, got %d: %s", status, body)
	}
}

func TestScheduledFunction_Import_DelayMsAndFunctionBothSet_400(t *testing.T) {
	const model = "e2e-schedfn-import-delay-and-fn"
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": "schedfn-delay-and-fn-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": {"delayMs": 1000, "function": %s}
				}]},
				"Closed": {}
			}
		}]
	}`, validScheduleFunctionJSON("calcFire"))
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	assertImportRejected(t, status, body, "delayMs and function both set")
}

func TestScheduledFunction_Import_ManualAndFunction_400(t *testing.T) {
	const model = "e2e-schedfn-import-manual-and-fn"
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": "schedfn-manual-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": true,
					"schedule": {"function": %s}
				}]},
				"Closed": {}
			}
		}]
	}`, validScheduleFunctionJSON("calcFire"))
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	assertImportRejected(t, status, body, "manual and function both set")
}

func TestScheduledFunction_Import_ResultKindNotSchedule_400(t *testing.T) {
	const model = "e2e-schedfn-import-badkind"
	fn := `{"name":"calcFire","resultKind":"NotSchedule","calculationNodesTags":"sched-fn"}`
	wf := scheduleFunctionWorkflowJSON("schedfn-badkind-wf", fn)
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	assertImportRejected(t, status, body, `resultKind != "Schedule"`)
}

func TestScheduledFunction_Import_MissingName_400(t *testing.T) {
	const model = "e2e-schedfn-import-noname"
	fn := `{"name":"","resultKind":"Schedule","calculationNodesTags":"sched-fn"}`
	wf := scheduleFunctionWorkflowJSON("schedfn-noname-wf", fn)
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	assertImportRejected(t, status, body, "missing function.name")
}

func TestScheduledFunction_Import_MissingCalculationNodesTags_400(t *testing.T) {
	const model = "e2e-schedfn-import-notags"
	fn := `{"name":"calcFire","resultKind":"Schedule","calculationNodesTags":""}`
	wf := scheduleFunctionWorkflowJSON("schedfn-notags-wf", fn)
	importModelE2E(t, model, 1)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, wf)
	assertImportRejected(t, status, body, "missing function.calculationNodesTags")
}

// --- Arm happy paths (fake Function-serving compute node, callback harness) ---

// awaitCallbackEntityState is awaitEntityStateE2E's callback-harness
// counterpart (scheduled_transition_test.go): polls until entityID reaches
// wantState on h's stack, or fails the test once timeout elapses.
func awaitCallbackEntityState(t *testing.T, h *callbackHarness, entityID, wantState string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		last, _ = h.GetEntityState(t, entityID)
		if last == wantState {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for entity %s to reach state %q; last observed state %q",
				timeout, entityID, wantState, last)
		}
		time.Sleep(scheduledPollInterval)
	}
}

// TestScheduledFunction_ArmAbsoluteFireAt_FiresThroughHTTPStack proves the
// absolute-fireAt arm path end-to-end: the fake compute node returns a
// Schedule result with fireAt set to a fixed future unix-millis timestamp;
// resolveSchedule uses it verbatim (no armMs offset), so the durably-armed
// row's scheduled_time must equal it exactly. The real scheduler then fires
// the transition once it comes due.
func TestScheduledFunction_ArmAbsoluteFireAt_FiresThroughHTTPStack(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-arm-abs"

	var wantFireAt atomic.Int64
	h.RegisterFunction("calcAbsFire", func(rc *reqCtx) (string, map[string]any, error) {
		fireAt := time.Now().Add(300 * time.Millisecond).UnixMilli()
		wantFireAt.Store(fireAt)
		return "Schedule", map[string]any{"fireAt": fireAt}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-abs-wf", validScheduleFunctionJSON("calcAbsFire"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}

	// The create POST only returns after the create-cascade (including the
	// arm-time Function dispatch) has committed, so the callback has
	// necessarily already run and stored fireAt by this point.
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND scheduled_time = $4",
		entityID, "Open", "AutoClose", wantFireAt.Load())
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row with scheduled_time=%d for entity %s; found %d",
			wantFireAt.Load(), entityID, rows)
	}

	if st := awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout); st != "Closed" {
		t.Fatalf("expected entity to fire to Closed, got %q", st)
	}
}

// TestScheduledFunction_ArmRelativeFireAfterMs_FiresThroughHTTPStack proves
// the relative-fireAfterMs arm path: the fake compute node returns a
// Schedule result with only fireAfterMs set, so
// scheduled_time = armMs + fireAfterMs where armMs is the server's "now" at
// dispatch — bounded here by the client-observed request window (no clock
// skew: client and server share a machine/process in this harness). No
// expiry is configured, so timeout_ms must be NULL.
func TestScheduledFunction_ArmRelativeFireAfterMs_FiresThroughHTTPStack(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-arm-rel"
	const fireAfterMs = int64(300)

	h.RegisterFunction("calcRelFire", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAfterMs": fireAfterMs}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-rel-wf", validScheduleFunctionJSON("calcRelFire"))
	h.SetupModelWithWorkflow(t, model, wf)

	beforeCreateMs := time.Now().UnixMilli()
	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	afterCreateMs := time.Now().UnixMilli()
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}

	wantMin := beforeCreateMs + fireAfterMs
	wantMax := afterCreateMs + fireAfterMs
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND scheduled_time BETWEEN $4 AND $5 AND timeout_ms IS NULL",
		entityID, "Open", "AutoClose", wantMin, wantMax)
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row with scheduled_time in [%d,%d] and timeout_ms NULL for entity %s; found %d",
			wantMin, wantMax, entityID, rows)
	}

	if st := awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout); st != "Closed" {
		t.Fatalf("expected entity to fire to Closed, got %q", st)
	}
}

// TestScheduledFunction_ArmWithExpiry_TimeoutMsStoredAndStillFires proves the
// expiry-bearing arm path: the fake node returns fireAfterMs+expireAfterMs.
// resolveSchedule stores timeoutMs = expiry - sched = expireAfterMs (both
// measured relative to the resolved fire time). The expiry is set well past
// the fire time, so the task is not born-expired and must still fire — the
// born-expired rejection path itself is Task 9.2's concern, not this one's.
func TestScheduledFunction_ArmWithExpiry_TimeoutMsStoredAndStillFires(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-arm-expiry"
	const fireAfterMs = int64(300)
	const expireAfterMs = int64(60_000)

	h.RegisterFunction("calcExpiry", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAfterMs": fireAfterMs, "expireAfterMs": expireAfterMs}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-expiry-wf", validScheduleFunctionJSON("calcExpiry"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}

	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND timeout_ms = $4",
		entityID, "Open", "AutoClose", expireAfterMs)
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row with timeout_ms=%d for entity %s; found %d",
			expireAfterMs, entityID, rows)
	}

	if st := awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout); st != "Closed" {
		t.Fatalf("expected entity to fire to Closed despite a configured (not-yet-elapsed) expiry, got %q", st)
	}
}

// --- Task 9.2: Function error surfaces + born-expired + settled-interval ---
//
// Extends the harness above with the scenarios NOT yet covered by 9.1's
// import-validation and arm-happy-path tests: compute-infra dispatch
// failures (no member / timeout), a malformed compute-node result, the
// born-expired arm path (both first-arm and re-arm), a past-due fireAt,
// expiry elapsed before any scanner sees the row, re-arm-on-plain-update
// recompute, and the inherited explicit-fire rejection.
//
// scheduledFnProblem/decodeProblem give this file's error-surface scenarios
// a single, minimal RFC 9457 problem+json decode (status/detail/ticket/
// properties) — the same shape scheduled_transition_test.go's explicit-fire
// test asserts inline; factored out here because Task 9.2 asserts it
// repeatedly (timeout/malformed-result/explicit-fire).

type scheduledFnProblem struct {
	Status     int            `json:"status"`
	Detail     string         `json:"detail"`
	Ticket     string         `json:"ticket"`
	Properties map[string]any `json:"properties"`
}

func decodeProblem(t *testing.T, body string) scheduledFnProblem {
	t.Helper()
	var pd scheduledFnProblem
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, body)
	}
	return pd
}

func (pd scheduledFnProblem) errorCode() string {
	code, _ := pd.Properties["errorCode"].(string)
	return code
}

func (pd scheduledFnProblem) retryable() bool {
	r, _ := pd.Properties["retryable"].(bool)
	return r
}

// scheduleFunctionJSONWithTimeout is validScheduleFunctionJSON with an
// explicit responseTimeoutMs (spi.ScheduleFunction.ResponseTimeoutMs) —
// used by the DISPATCH_TIMEOUT scenario to force a short dispatch budget
// against a deliberately slow fake Function callback.
func scheduleFunctionJSONWithTimeout(functionName string, timeoutMs int) string {
	return fmt.Sprintf(`{"name":%q,"resultKind":"Schedule","calculationNodesTags":%q,"responseTimeoutMs":%d}`,
		functionName, scheduledFnTag, timeoutMs)
}

// awaitCallbackSMEventType is awaitSMEventType's callback-harness
// counterpart (mirrors awaitCallbackEntityState's relationship to
// awaitEntityStateE2E): polls h's own audit endpoint until an event
// matching wantType (and, if wantState is non-empty, state too) appears, or
// fails the test once timeout elapses.
func awaitCallbackSMEventType(t *testing.T, h *callbackHarness, entityID, wantType, wantState string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		events := h.GetSMAuditEvents(t, entityID)
		if hasSMEventType(events, wantType, wantState) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for StateMachine event %q (state=%q) on entity %s; got events: %+v",
				timeout, wantType, wantState, entityID, events)
		}
		time.Sleep(scheduledPollInterval)
	}
}

// TestScheduledFunction_NoMember_Returns503 lives in
// dispatch_infra_error_test.go (TestScheduledFunctionNoMember_Returns503) —
// it needs a harness with ZERO connected compute members (this file's
// callback harness always has one, joined on scheduledFnTag), so it reuses
// newDispatchInfraErrorServer's empty-MemberRegistry stack instead.

// TestScheduledFunction_DispatchTimeout_Returns503 proves a Function
// callback that blows through its configured responseTimeoutMs surfaces as
// a retryable 503 DISPATCH_TIMEOUT — the same compute-infra classification
// the processor/criteria dispatch paths already have (dispatch.go's
// dispatchCalloutToMember, shared by all three callout kinds) — rather than
// hanging or a client-attributable 4xx.
func TestScheduledFunction_DispatchTimeout_Returns503(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-timeout"
	const responseTimeoutMs = 150

	h.RegisterFunction("calcSlow", func(rc *reqCtx) (string, map[string]any, error) {
		// Comfortably past responseTimeoutMs so dispatchCalloutToMember's
		// <-time.After(timeout) branch wins the select — DISPATCH_TIMEOUT,
		// not a late-but-successful dispatch.
		time.Sleep(600 * time.Millisecond)
		return "Schedule", map[string]any{"fireAfterMs": int64(60_000)}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-timeout-wf", scheduleFunctionJSONWithTimeout("calcSlow", responseTimeoutMs))
	h.SetupModelWithWorkflow(t, model, wf)

	_, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("create entity with a slow Function callout: expected 503, got %d: %s", status, body)
	}
	pd := decodeProblem(t, body)
	if pd.errorCode() != "DISPATCH_TIMEOUT" {
		t.Errorf("properties.errorCode: got %q, want DISPATCH_TIMEOUT; body=%s", pd.errorCode(), body)
	}
	if !pd.retryable() {
		t.Errorf("properties.retryable: got false, want true; body=%s", body)
	}
}

// TestScheduledFunction_MalformedResult_Returns500 proves a structurally
// invalid Schedule result (here: both fireAt and fireAfterMs set —
// resolveSchedule requires exactly one) fails the write with a 500
// SCHEDULE_FUNCTION_INVALID_RESULT — the fault is in the compute node's
// response, not the caller's request — and the response body follows the
// sanitized-5xx contract (Gate 3 / .claude/rules/error-handling.md): a
// generic detail plus a correlation ticket, never the internal cause text.
func TestScheduledFunction_MalformedResult_Returns500(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-badresult"

	h.RegisterFunction("calcBadResult", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAt": time.Now().UnixMilli(), "fireAfterMs": int64(1000)}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-badresult-wf", validScheduleFunctionJSON("calcBadResult"))
	h.SetupModelWithWorkflow(t, model, wf)

	_, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("create entity with a malformed Schedule result (fireAt AND fireAfterMs both set): expected 500, got %d: %s", status, body)
	}
	pd := decodeProblem(t, body)
	if pd.errorCode() != "SCHEDULE_FUNCTION_INVALID_RESULT" {
		t.Errorf("properties.errorCode: got %q, want SCHEDULE_FUNCTION_INVALID_RESULT; body=%s", pd.errorCode(), body)
	}
	if pd.Ticket == "" {
		t.Errorf("expected a correlation ticket UUID on the sanitized 500 response; body=%s", body)
	}
	if strings.Contains(body, "exactly one of fireAt") {
		t.Errorf("sanitized-5xx contract violated: internal cause text leaked into the client-facing body=%s", body)
	}
}

// TestScheduledFunction_BornExpired_SucceedsNotArmedRecordsExpire proves the
// born-expired arm path (design: resolveSchedule's expiry<=fire branch):
// the entity write itself SUCCEEDS (the transition simply isn't armed — no
// error, no rollback), no scheduled_tasks row is ever created, and a
// SCHEDULED_TRANSITION_EXPIRE audit event records why.
func TestScheduledFunction_BornExpired_SucceedsNotArmedRecordsExpire(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-bornexpired"

	h.RegisterFunction("calcBornExpired", func(rc *reqCtx) (string, map[string]any, error) {
		// expireAfterMs:0 resolves to expiry == the fire time exactly —
		// resolveSchedule's "expiry <= sched" born-expired branch.
		return "Schedule", map[string]any{"fireAfterMs": int64(1000), "expireAfterMs": int64(0)}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-bornexpired-wf", validScheduleFunctionJSON("calcBornExpired"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity with a born-expired Function result: expected 200 (write succeeds; only the arm is skipped), got %d: %s", status, body)
	}

	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 0 {
		t.Fatalf("expected NO scheduled_tasks row for a born-expired arm; found %d", rows)
	}

	events := h.GetSMAuditEvents(t, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_EXPIRE", "Open") {
		t.Errorf("expected a SCHEDULED_TRANSITION_EXPIRE audit event with state Open; got events: %+v", events)
	}
	if st, _ := h.GetEntityState(t, entityID); st != "Open" {
		t.Errorf("expected entity to remain in Open (never armed, so it can never fire); got %q", st)
	}
}

// TestScheduledFunction_PastFireAt_FiresPromptly proves a past absolute
// fireAt is not an error — resolveSchedule's doc comment: "a past fireAt is
// not an error — the transition simply arms to fire immediately". The row
// is armed verbatim with the past timestamp, and the real scheduler fires
// it on its very next scan (already due at arm time).
func TestScheduledFunction_PastFireAt_FiresPromptly(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-pastfire"

	pastFireAt := time.Now().Add(-5 * time.Second).UnixMilli()
	h.RegisterFunction("calcPastFire", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAt": pastFireAt}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-pastfire-wf", validScheduleFunctionJSON("calcPastFire"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}

	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND scheduled_time = $4",
		entityID, "Open", "AutoClose", pastFireAt)
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row armed verbatim with the past fireAt=%d; found %d", pastFireAt, rows)
	}

	if st := awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout); st != "Closed" {
		t.Fatalf("expected the already-due transition to fire promptly, got %q", st)
	}
	events := h.GetSMAuditEvents(t, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Errorf("expected a SCHEDULED_TRANSITION_FIRE audit event with state Closed; got events: %+v", events)
	}
}

// TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire proves the
// settled-interval "expiry elapsed before any scanner ever reads the row"
// path (design §5.5's grace-band gate in fire_scheduled.go): once lateness
// exceeds TimeoutMs+grace, the task resolves Expired and never Fired.
//
// A live default-cadence (1s) scheduler ticking mid-flight would create an
// unavoidable race window between "due" and "expired" here (the
// TimeoutMs+grace band is only ~110ms) — a tick landing inside that window
// would legitimately FIRE instead of expire, flaking the test purely on
// cadence phase. This test sidesteps the race by disabling the harness's
// own scheduler at construction (cfg.Scheduler.Enabled=false) and only
// starting a bespoke scheduler.Service (mirrors
// TestE2E_ScheduledTransition_RestartDurability's fresh-instance pattern)
// AFTER sleeping comfortably past the expiry deadline — so the very FIRST
// scan any scheduler ever performs against this row already sees it
// expired, deterministically, no matter the scan cadence.
func TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.Scheduler.Enabled = false
	})
	const model = "e2e-schedfn-expiry-elapsed"
	const fireAfterMs = int64(50)
	const expireAfterMs = int64(60) // resolves to timeoutMs = 10ms

	h.RegisterFunction("calcExpireElapsed", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAfterMs": fireAfterMs, "expireAfterMs": expireAfterMs}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-expireelapsed-wf", validScheduleFunctionJSON("calcExpireElapsed"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}

	// Comfortably past the fire(50ms)+timeout(10ms)+default-grace(100ms) =
	// 160ms deadline, with generous CI-jitter headroom — BEFORE any
	// scheduler is even running (the harness's own is disabled above, and
	// the bespoke one below hasn't started yet).
	time.Sleep(500 * time.Millisecond)

	schedEngine := cluster.NewSchedulerEngine(h.app.WorkflowEngine())
	freshExecutor := cluster.NewClusterExecutor(schedEngine, "local", h.app.NodeRegistry(), nil)
	freshScheduler := scheduler.NewService(
		scheduler.Config{
			Enabled:           true,
			ScanInterval:      100 * time.Millisecond,
			RedispatchBackoff: 5 * time.Second,
			BatchSize:         100,
		},
		scheduler.Deps{
			Store:        h.app.StoreFactory(),
			Registry:     h.app.NodeRegistry(),
			Coordinator:  scheduler.LowestLiveNodeID{},
			Distribution: scheduler.Self{},
			Clock:        scheduler.NewRealClock(),
			Executor:     freshExecutor,
			SelfID:       "local",
		},
	)
	freshScheduler.Start()
	defer freshScheduler.Stop()

	awaitCallbackSMEventType(t, h, entityID, "SCHEDULED_TRANSITION_EXPIRE", "Open", scheduledFireTimeout)

	events := h.GetSMAuditEvents(t, entityID)
	if hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "") {
		t.Errorf("expected no SCHEDULED_TRANSITION_FIRE event — lateness already exceeded timeout+grace before any scanner ran; got events: %+v", events)
	}
	if st, _ := h.GetEntityState(t, entityID); st != "Open" {
		t.Errorf("expected entity to remain in Open (expired, not fired); got %q", st)
	}
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 0 {
		t.Fatalf("expected the expired scheduled_tasks row to be deleted; found %d", rows)
	}
}

// TestScheduledFunction_ReArmOnPlainUpdate_Recomputes proves a plain
// data-only loopback write (no state change) still runs
// reconcileScheduledTasks for the CURRENT state — which re-invokes the
// Function and re-arms with its freshly recomputed result — rather than
// leaving the first arm's stale scheduled_time in place.
func TestScheduledFunction_ReArmOnPlainUpdate_Recomputes(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-rearm-recompute"

	var calls atomic.Int64
	h.RegisterFunction("calcRearm", func(rc *reqCtx) (string, map[string]any, error) {
		n := calls.Add(1)
		// Minutes in the future either way (never fires mid-test); only the
		// magnitude differs between calls, so a re-armed scheduled_time
		// provably reflects the SECOND call's own arm window, not a stale
		// copy of the first.
		fireAfterMs := int64(600_000) + n*1000
		return "Schedule", map[string]any{"fireAfterMs": fireAfterMs}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-rearm-wf", validScheduleFunctionJSON("calcRearm"))
	h.SetupModelWithWorkflow(t, model, wf)

	beforeCreateMs := time.Now().UnixMilli()
	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	afterCreateMs := time.Now().UnixMilli()
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly one Function call after create, got %d", got)
	}
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND scheduled_time BETWEEN $4 AND $5",
		entityID, "Open", "AutoClose", beforeCreateMs+601_000, afterCreateMs+601_000)
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row for the FIRST arm's window; found %d", rows)
	}

	// Plain data-only loopback write (no state change) — must still
	// re-invoke the Function for the current state's schedule.function
	// transition.
	beforeUpdateMs := time.Now().UnixMilli()
	resp := h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", entityID), `{"name":"Test Order","amount":200,"status":"draft"}`, "")
	updateBody := h.readBody(t, resp)
	afterUpdateMs := time.Now().UnixMilli()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("data-only loopback update: expected 200, got %d: %s", resp.StatusCode, updateBody)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected the Function to be called AGAIN on a plain data update (re-arm recomputes), got %d total calls", got)
	}

	rows = queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND scheduled_time BETWEEN $4 AND $5",
		entityID, "Open", "AutoClose", beforeUpdateMs+602_000, afterUpdateMs+602_000)
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row reflecting the SECOND arm's recomputed window; found %d", rows)
	}
}

// TestScheduledFunction_BornExpiredOnReArm_CancelsPriorArmedRow proves the
// re-arm path (not just the first-arm path above) also honours
// born-expired: a plain data-only loopback update whose Function result is
// now born-expired must cancel (delete) the PRIOR armed row — no stale
// fire is possible once the write returns, since the row is gone in the
// same transaction — and record EXPIRE, not a silent no-op.
func TestScheduledFunction_BornExpiredOnReArm_CancelsPriorArmedRow(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-rearm-bornexpired"

	var calls atomic.Int64
	h.RegisterFunction("calcRearmBornExpired", func(rc *reqCtx) (string, map[string]any, error) {
		if calls.Add(1) == 1 {
			// First arm (create): far in the future — proves a real row
			// existed before being superseded.
			return "Schedule", map[string]any{"fireAfterMs": int64(600_000)}, nil
		}
		// Re-arm (the loopback update below): born expired.
		return "Schedule", map[string]any{"fireAfterMs": int64(1000), "expireAfterMs": int64(0)}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-rearm-bornexpired-wf", validScheduleFunctionJSON("calcRearmBornExpired"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 1 {
		t.Fatalf("expected the first arm's scheduled_tasks row to exist; found %d", rows)
	}

	resp := h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", entityID), `{"name":"Test Order","amount":200,"status":"draft"}`, "")
	updateBody := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("data-only loopback update (triggers a born-expired re-arm): expected 200, got %d: %s", resp.StatusCode, updateBody)
	}

	// The prior armed row must be gone — deleted atomically with the same
	// write that just committed 200 above, so no stale fire is possible.
	rows = queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 0 {
		t.Fatalf("expected the prior armed row to be cancelled by the born-expired re-arm; found %d remaining", rows)
	}

	events := h.GetSMAuditEvents(t, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_EXPIRE", "Open") {
		t.Errorf("expected a SCHEDULED_TRANSITION_EXPIRE audit event after the born-expired re-arm; got events: %+v", events)
	}
	if hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "") {
		t.Errorf("expected no SCHEDULED_TRANSITION_FIRE event — the row was cancelled before any scanner could ever pick it up; got events: %+v", events)
	}
	if st, _ := h.GetEntityState(t, entityID); st != "Open" {
		t.Errorf("expected entity to remain in Open (never fires — the row is gone); got %q", st)
	}
}

// TestScheduledFunction_ExplicitFire_ReturnsTransitionNotFound proves the
// inherited explicit-fire rejection (TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound's
// legacy-delayMs precedent) holds identically for a schedule.function
// transition: engine.go's manual-transition path checks transition.Schedule
// != nil regardless of which Schedule variant is configured, so a
// client-issued PUT against the transition by name is rejected 400
// TRANSITION_NOT_FOUND — it is never manually fireable.
func TestScheduledFunction_ExplicitFire_ReturnsTransitionNotFound(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-explicit-fire"

	h.RegisterFunction("calcExplicit", func(rc *reqCtx) (string, map[string]any, error) {
		return "Schedule", map[string]any{"fireAfterMs": int64(600_000)}, nil // far future — never fires mid-test
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-explicit-wf", validScheduleFunctionJSON("calcExplicit"))
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}
	if st, _ := h.GetEntityState(t, entityID); st != "Open" {
		t.Fatalf("expected entity to rest in Open (armed, not yet due); got %q", st)
	}

	resp := h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s/AutoClose", entityID), `{"name":"Test Order","amount":100,"status":"draft"}`, "")
	fireBody := h.readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 TRANSITION_NOT_FOUND firing a schedule.function transition explicitly; got %d: %s", resp.StatusCode, fireBody)
	}
	pd := decodeProblem(t, fireBody)
	if pd.errorCode() != "TRANSITION_NOT_FOUND" {
		t.Errorf("properties.errorCode: got %q, want TRANSITION_NOT_FOUND; body=%s", pd.errorCode(), fireBody)
	}
	const wantCause = "is scheduled and fires automatically; it is not manually fireable"
	if !strings.Contains(fireBody, wantCause) {
		t.Errorf("expected response body to contain rejection cause %q; got: %s", wantCause, fireBody)
	}
	if st, _ := h.GetEntityState(t, entityID); st != "Open" {
		t.Errorf("expected entity to remain in Open after rejected explicit-fire; got %q", st)
	}
}
