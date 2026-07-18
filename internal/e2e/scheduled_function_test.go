package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
