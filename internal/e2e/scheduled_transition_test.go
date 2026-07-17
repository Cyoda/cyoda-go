package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster"
	"github.com/cyoda-platform/cyoda-go/internal/scheduler"
)

// --- Polling / audit helpers (running-backend e2e — Task E2, design §11 "E"
// column). Mirrors the awaitEntityState/awaitStateMachineEvent idiom already
// established in e2e/parity/scheduledtransition/scheduledtransition.go:
// small Schedule.DelayMs plus generous, bounded polling — never a bare
// time.Sleep as the sole detector of a positive outcome (this repo's #1 CI
// flake source is wall-clock e2e timing).

// scheduledFireTimeout bounds every poll loop in this file. Sized generously
// above the untuned default CYODA_SCHEDULER_SCAN_INTERVAL (1s — this shared
// harness's TestMain does not tune it down, unlike the parity fixtures) plus
// slow-CI overhead, so a real bug — not scan cadence — trips the timeout.
const scheduledFireTimeout = 15 * time.Second

// scheduledPollInterval is the sleep between polls.
const scheduledPollInterval = 75 * time.Millisecond

// awaitEntityStateE2E polls getEntityState until it equals wantState, or
// fails the test once timeout elapses.
func awaitEntityStateE2E(t *testing.T, entityID, wantState string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		last = getEntityState(t, entityID)
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

// hasSMEventType reports whether events contains one whose eventType equals
// wantType. If wantState is non-empty, the event's state must also match.
func hasSMEventType(events []map[string]any, wantType, wantState string) bool {
	for _, ev := range events {
		et, _ := ev["eventType"].(string)
		if et != wantType {
			continue
		}
		if wantState != "" {
			st, _ := ev["state"].(string)
			if st != wantState {
				continue
			}
		}
		return true
	}
	return false
}

// awaitSMEventType polls getSMAuditEvents until an event matching wantType
// (and, if wantState is non-empty, state too) appears, or fails the test
// once timeout elapses.
func awaitSMEventType(t *testing.T, entityID, wantType, wantState string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		events := getSMAuditEvents(t, entityID)
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

// TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound exercises
// the explicit-fire-of-a-scheduled-transition rejection path end-to-end through
// the full HTTP stack (#259). The validator accepts the shape-coherent
// scheduled transition (Schedule.DelayMs > 0, manual=false), cascade silently
// skips it on entity creation (no other automated exit), and a client-issued
// PUT against the transition by name returns 400 TRANSITION_NOT_FOUND with a
// message containing "is scheduled and fires automatically; it is not
// manually fireable" (reworded once the scheduled-transition runtime existed
// to fire it — see engine.go's scheduledReason). The delay (1000ms) comfortably
// outlives this test, so the entity stays in the source state throughout.
//
// Mirrors the Disabled-transition precedent (TestEntityLifecycle_DisabledTransition):
// the transition exists in config but is not currently dispatchable from the
// caller's POV.
func TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound(t *testing.T) {
	const model = "e2e-scheduled-explicit-fire"

	// 1+2: Import model and workflow with a single scheduled, non-manual
	// transition out of the initial state. setupModelWithWorkflow asserts
	// 200 on workflow import — pins that the validator accepts shape-coherent
	// scheduled transitions.
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-explicit-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 1000}}]},
				"Closed": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	// 3: Create entity instance. Cascade silently skips the scheduled
	// transition (no other automated exit), so the entity rests in Open.
	entityID := createEntityE2E(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if s := getEntityState(t, entityID); s != "Open" {
		t.Fatalf("expected entity to rest in initial state Open (cascade skips scheduled); got %q", s)
	}

	// 4: Fire AutoClose by name. Expect 400 TRANSITION_NOT_FOUND with the
	// message naming the rejection cause.
	path := fmt.Sprintf("/api/entity/JSON/%s/AutoClose", entityID)
	resp := doAuth(t, http.MethodPut, path, `{"name":"Test Order","amount":100,"status":"draft"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 TRANSITION_NOT_FOUND firing scheduled transition; got %d: %s", resp.StatusCode, body)
	}

	// Error response is RFC 9457 problem+json with properties.errorCode
	// (per common.WriteError) — same shape as TestGroupedStats_E2E_ValidationError.
	var pd struct {
		Status     int            `json:"status"`
		Detail     string         `json:"detail"`
		Title      string         `json:"title"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("decode problem detail: %v\nbody: %s", err, body)
	}
	if pd.Status != http.StatusBadRequest {
		t.Errorf("ProblemDetail.status: got %d, want 400", pd.Status)
	}
	if code, _ := pd.Properties["errorCode"].(string); code != "TRANSITION_NOT_FOUND" {
		t.Errorf("properties.errorCode: got %q, want TRANSITION_NOT_FOUND; body=%s", code, body)
	}
	// The rejection-cause substring may appear in detail, title, or another
	// problem-detail field — assert against the raw body so we're robust to
	// where common.WriteError surfaces the wrapped error string.
	const wantCause = "is scheduled and fires automatically; it is not manually fireable"
	if !strings.Contains(body, wantCause) {
		t.Errorf("expected response body to contain rejection cause %q; got: %s", wantCause, body)
	}

	// 5: Entity must remain in the source state after rejection.
	if s := getEntityState(t, entityID); s != "Open" {
		t.Errorf("expected entity to remain in Open after rejected explicit-fire; got %q", s)
	}
}

// TestE2E_ScheduledTransition_FiresThroughHTTPStack proves the real scan
// loop fires a no-criterion scheduled transition end-to-end through the
// full HTTP stack (design §5.1/§5.2): create lands the entity in a state
// with a scheduled transition, the running server's scheduler.Service scans
// real Postgres on its own cadence (default CYODA_SCHEDULER_SCAN_INTERVAL,
// 1s — this harness does not tune it down), fires it, and the entity
// advances. The 200ms delay is small; the 15s poll bound is generous
// (§11 "Time control": e2e covers coarse happy-path firing, never exact
// thresholds).
func TestE2E_ScheduledTransition_FiresThroughHTTPStack(t *testing.T) {
	const model = "e2e-scheduled-fires-http"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-fires-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 200}}]},
				"Closed": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)
	if s := getEntityState(t, entityID); s != "Open" {
		t.Fatalf("expected entity to rest in Open immediately after creation (200ms delay hasn't elapsed); got %q", s)
	}

	awaitEntityStateE2E(t, entityID, "Closed", scheduledFireTimeout)

	events := getSMAuditEvents(t, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Errorf("expected a SCHEDULED_TRANSITION_FIRE audit event with state Closed; got events: %+v", events)
	}
}

// TestE2E_ScheduledTransition_LoopbackDefersTimer documents the
// settled-interval semantic (design §5.4/F6): a data-only loopback write
// re-arms the pending scheduled task's fire time (design §5.1 step 1's
// upsert), so an entity kept busy by writes faster than DelayMs never
// fires — only once it settles does the last-armed timer run out. This
// proves "a busy entity never fires; a settled one does" through the real
// HTTP stack, not just the unit-level arm/re-arm math.
//
// The 500ms delay and 90ms write cadence (≈5.5x headroom) keep every gap
// between writes comfortably below DelayMs, so the "did not fire" window is
// not racing the scheduler regardless of scan cadence. The final positive
// assertion still uses bounded polling, never a bare sleep.
func TestE2E_ScheduledTransition_LoopbackDefersTimer(t *testing.T) {
	const model = "e2e-scheduled-loopback-defers"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-loopback-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 500}}]},
				"Closed": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test Order","amount":0,"status":"draft"}`)

	// Keep the entity busy — a same-state, data-only loopback write every
	// 90ms, for a window (8 writes ≈ 720ms) that comfortably exceeds the
	// 500ms delay. Each write re-arms the task 500ms further out, so it
	// should never come due while this loop runs.
	loopbackPath := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	for i := 1; i <= 8; i++ {
		time.Sleep(90 * time.Millisecond)
		payload := fmt.Sprintf(`{"name":"Test Order","amount":%d,"status":"draft"}`, i)
		resp := doAuth(t, http.MethodPut, loopbackPath, payload)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("loopback update #%d: expected 200, got %d: %s", i, resp.StatusCode, body)
		}
	}

	// Immediately after the busy window: the entity must still be resting
	// in Open (the last write pushed the fire time ~500ms further out), and
	// no fire must have happened yet.
	if s := getEntityState(t, entityID); s != "Open" {
		t.Fatalf("expected entity to still be in Open right after the busy loopback window (timer kept getting deferred); got %q", s)
	}
	events := getSMAuditEvents(t, entityID)
	if hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "") {
		t.Errorf("expected no SCHEDULED_TRANSITION_FIRE event while the entity was kept busy; got events: %+v", events)
	}

	// Now stop updating and let the last-armed timer run out. Bounded,
	// generous poll — not a bare sleep — detects the eventual fire.
	awaitEntityStateE2E(t, entityID, "Closed", scheduledFireTimeout)

	events = getSMAuditEvents(t, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Errorf("expected a SCHEDULED_TRANSITION_FIRE audit event with state Closed once the entity settled; got events: %+v", events)
	}
}

// TestE2E_ScheduledTransition_RestartDurability exercises the strongest
// durable-survival variant available in this shared-harness package: the
// package-level testApp (internal/e2e/e2e_test.go's TestMain) constructs and
// starts exactly one scheduler.Service, kept as an unexported field with no
// public Stop/Start accessor (app/app.go's `a.scheduler`), and it is shared
// by every test in this package — stopping it here would break every other
// test running against the same server. A full second `app.New` against the
// same Postgres container was considered and rejected: it would duplicate
// the entire app (its own JWT/JWKS wiring, HTTP handler, etc.) purely to
// exercise one collaborator, and per Task D4's finding any e2e test that
// starts its own app instance must call Shutdown() — extra teardown
// surface for no additional signal over the approach below.
//
// Instead this test:
//  1. Arms a task via the shared production HTTP stack, then reads the
//     scheduled_tasks row directly out of Postgres (design §5.1: the arm is
//     atomic with the entity write) — proving the pending task is durable,
//     persisted storage, not in-memory scheduler state, well before the
//     delay elapses.
//  2. Constructs a brand-new scheduler.Service from scratch — zero prior
//     state, wired only to the app's already-exported collaborators
//     (StoreFactory, NodeRegistry, WorkflowEngine) via the same
//     cluster.NewSchedulerEngine/NewClusterExecutor adapters app.go itself
//     uses — the same shape a freshly restarted process's scheduler would
//     take, reading only from the durable store (internal/scheduler's Deps
//     godoc: "the scan loop calls factory.ScheduledTaskStore... fresh on
//     each tick" — no cached in-memory task state to lose across restarts
//     by design).
//  3. Bounded-polls for the entity to reach the fired state and asserts the
//     FIRE audit event.
//
// Limitation (reported, not silently skipped): the shared production
// scheduler is also running throughout and may independently observe and
// fire the same task first — the design's guard (§5.3, fire-time re-read +
// first-flush CAS) makes that race safe (at most one fire either way), but
// it means this test cannot cryptographically prove *which* scheduler
// instance performed the fire. What it does prove end-to-end is that the
// durably-persisted task is discoverable and fireable by a scheduler
// instance with no relationship to whichever one armed it — the essence of
// restart durability — without modifying production code to add a
// test-only Stop/Start hook on the app's own scheduler.
func TestE2E_ScheduledTransition_RestartDurability(t *testing.T) {
	const model = "e2e-scheduled-restart-durability"

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-restart-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 800}}]},
				"Closed": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, model, wf)

	entityID := createEntityE2E(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft"}`)

	// 1: The armed task must already be durably persisted in Postgres, well
	// before the 800ms delay elapses — proof it does not depend on any
	// scheduler process's in-memory state.
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 1 {
		t.Fatalf("expected exactly one durably-persisted scheduled_tasks row for entity %s (source_state=Open, transition=AutoClose); found %d", entityID, rows)
	}

	// 2: A brand-new scheduler.Service — the closest available proxy for "a
	// freshly restarted process's scheduler" in this shared-app harness —
	// wired only to the app's already-exported collaborators.
	schedEngine := cluster.NewSchedulerEngine(testApp.WorkflowEngine())
	freshExecutor := cluster.NewClusterExecutor(schedEngine, "local", testApp.NodeRegistry(), nil)
	freshScheduler := scheduler.NewService(
		scheduler.Config{
			Enabled:           true,
			ScanInterval:      100 * time.Millisecond,
			RedispatchBackoff: 5 * time.Second,
			BatchSize:         100,
		},
		scheduler.Deps{
			Store:        testApp.StoreFactory(),
			Registry:     testApp.NodeRegistry(),
			Coordinator:  scheduler.LowestLiveNodeID{},
			Distribution: scheduler.Self{},
			Clock:        scheduler.NewRealClock(),
			Executor:     freshExecutor,
			SelfID:       "local",
		},
	)
	freshScheduler.Start()
	defer freshScheduler.Stop()

	// 3: The durably-persisted task must still be discoverable and fireable.
	awaitEntityStateE2E(t, entityID, "Closed", scheduledFireTimeout)

	events := getSMAuditEvents(t, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Errorf("expected a SCHEDULED_TRANSITION_FIRE audit event with state Closed after the simulated-restart scheduler ran; got events: %+v", events)
	}
}
