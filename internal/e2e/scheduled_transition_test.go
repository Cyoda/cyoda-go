package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// --- Polling / audit helpers (running-backend e2e).
//
// Every test here that needs a fire builds a stack with a live scheduler on a
// database of its own (newSchedulerHarness): a claim is cross-tenant, so a
// scheduler on the shared database would run other tests' tasks. A small
// Schedule.DelayMs plus generous, bounded polling — never a bare time.Sleep as
// the sole detector of a positive outcome.

// scheduledFireTimeout bounds every poll loop that waits for a fire. The
// scheduler stacks claim every 50ms (schedulerTuning); the bound is sized for
// slow CI, so a real bug — not the scan cadence — trips it.
const scheduledFireTimeout = 15 * time.Second

// scheduledPollInterval is the sleep between polls.
const scheduledPollInterval = 75 * time.Millisecond

// Scheduled-transition assertions read their timings from the SERVER's audit
// trail, never from elapsed wall-clock on the test side.
//
// A "the timer has not fired yet" assertion phrased as "peek the state and
// expect the old one" races the delay against an HTTP round-trip: under load a
// round-trip can exceed the delay, the scheduler fires legitimately, and the
// test reports a defect that does not exist. The audit trail records both the
// armed fire time and the instant each event happened, stamped by the engine's
// own clock (`time.Now()` in-process, the same clock the scheduler compares
// against), so assertions built from it are exact and load-independent.
//
// Audit events are stamped from Engine.now() — the same clock the scheduler
// arms and fires against — so the two remain comparable even under an injected
// WithScheduledClock, not merely because these tests inject none.
//
// Where a precondition for an assertion cannot be established (e.g. the machine
// stalled so long that the scheduler was entitled to fire), these tests fail
// loudly with that cause rather than skipping: a skip would hide a genuine
// scheduler regression behind "the box was busy".

// smEventsOfType returns the events whose eventType equals wantType, oldest
// first. getSMAuditEvents returns newest-first, so the slice is reversed.
func smEventsOfType(events []map[string]any, wantType string) []map[string]any {
	var out []map[string]any
	for i := len(events) - 1; i >= 0; i-- {
		if et, _ := events[i]["eventType"].(string); et == wantType {
			out = append(out, events[i])
		}
	}
	return out
}

// smEventTime returns the instant an audit event was recorded.
func smEventTime(t *testing.T, ev map[string]any) time.Time {
	t.Helper()
	raw, _ := ev["utcTime"].(string)
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("audit event has unparseable utcTime %q: %v (event=%+v)", raw, err, ev)
	}
	return ts
}

// smEventScheduledTime returns the fire time an ARM event recorded, i.e. when
// the engine decided the transition should fire.
func smEventScheduledTime(t *testing.T, ev map[string]any) time.Time {
	t.Helper()
	data, ok := ev["data"].(map[string]any)
	if !ok {
		t.Fatalf("audit event has no data map: %+v", ev)
	}
	ms, ok := data["scheduledTime"].(float64)
	if !ok {
		t.Fatalf("audit event data has no numeric scheduledTime: %+v", ev)
	}
	return time.UnixMilli(int64(ms)).UTC()
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

// TestE2E_ExplicitFireOfScheduledTransition_ReturnsTransitionNotFound exercises
// the explicit-fire-of-a-scheduled-transition rejection path end-to-end through
// the full HTTP stack. The validator accepts the shape-coherent
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
// with a scheduled transition, the scheduler of this test's own stack
// claims it from real Postgres, fires it, and the entity advances. The
// 200ms delay is small; the 15s poll bound is generous (§11 "Time control":
// e2e covers coarse happy-path firing, never exact thresholds).
func TestE2E_ScheduledTransition_FiresThroughHTTPStack(t *testing.T) {
	const delayMs = 200
	h, s := newSchedulerHarness(t, nil)
	model := uniq("e2e-scheduled-fires-http")
	h.SetupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-fires-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 200}}]},
				"Closed": {}
			}
		}]
	}`)

	// The in-process server's engine reads this process's clock, so an instant
	// taken before the create is at or before the engine's arm instant.
	beforeCreate := time.Now().Truncate(time.Millisecond)
	entityID, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	arms := smEventsOfType(schedEvents(t, h, entityID), "SCHEDULED_TRANSITION_ARM")
	if len(arms) == 0 {
		t.Fatalf("expected a SCHEDULED_TRANSITION_ARM audit event after creation (transition must be scheduled, not fired inline)")
	}
	scheduledFor := smEventScheduledTime(t, arms[0])
	if min := beforeCreate.Add(delayMs * time.Millisecond); scheduledFor.Before(min) {
		t.Errorf("armed fire time %s is before %s, the create's start plus %dms — the delay was not applied",
			scheduledFor.Format(time.RFC3339Nano), min.Format(time.RFC3339Nano), delayMs)
	}

	awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout)

	events := schedEvents(t, h, entityID)
	fires := smEventsOfType(events, "SCHEDULED_TRANSITION_FIRE")
	if len(fires) != 1 || !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Fatalf("want exactly one SCHEDULED_TRANSITION_FIRE with state Closed; got events: %+v", events)
	}
	if firedAt := smEventTime(t, fires[0]); firedAt.Before(scheduledFor) {
		t.Errorf("fired at %s, before its armed fire time %s", firedAt.Format(time.RFC3339Nano), scheduledFor.Format(time.RFC3339Nano))
	}
	if r, ok := s.task(t, entityID, "AutoClose"); ok {
		t.Errorf("the fired task is still stored: %+v; spec §4 removes it", r)
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
// Timings are read back from the audit trail rather than assumed from the
// test's own sleeps: a write's round-trip is not bounded under load, so a
// nominal 90ms cadence could in reality leave a gap longer than DelayMs, let
// the timer fire legitimately, and make the test report a defect that does not
// exist. The delay is sized so the loop keeps its headroom on a loaded machine,
// and the deferral precondition — every write landing before the fire time then
// in force — is verified from the ARM events before the "did not fire"
// assertion is trusted.
func TestE2E_ScheduledTransition_LoopbackDefersTimer(t *testing.T) {
	const delayMs = 3000
	const writes = 10
	const cadence = 400 * time.Millisecond // busy window 4s > 3s delay

	h, _ := newSchedulerHarness(t, nil)
	model := uniq("e2e-scheduled-loopback-defers")
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-loopback-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 3000}}]},
				"Closed": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, model, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":0,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}

	// Keep the entity busy with same-state, data-only loopback writes for a
	// window that exceeds the delay. Each write re-arms the task further out,
	// so it must never come due while this loop runs.
	loopbackPath := fmt.Sprintf("/api/entity/JSON/%s", entityID)
	for i := 1; i <= writes; i++ {
		time.Sleep(cadence)
		payload := fmt.Sprintf(`{"name":"Test Order","amount":%d,"status":"draft"}`, i)
		resp := h.DoAuth(t, http.MethodPut, loopbackPath, payload, "")
		body := h.readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("loopback update #%d: expected 200, got %d: %s", i, resp.StatusCode, body)
		}
	}

	// Fetch the WHOLE history: each loopback write emits several StateMachine
	// events, so the endpoint's default 20-item page would truncate the older
	// ARM events and hide the first arm this test reasons about.
	events := schedEvents(t, h, entityID)
	arms := smEventsOfType(events, "SCHEDULED_TRANSITION_ARM")

	// The busy window must have outlasted the FIRST armed fire time, or the
	// entity was never kept busy past the point where it would have fired and
	// the assertion below would prove nothing.
	if len(arms) < 2 {
		t.Fatalf("expected the loopback writes to re-arm the task (want >= 2 ARM events, got %d); events: %+v", len(arms), events)
	}
	firstScheduled := smEventScheduledTime(t, arms[0])
	lastArmedAt := smEventTime(t, arms[len(arms)-1])
	if !lastArmedAt.After(firstScheduled) {
		t.Fatalf("busy window ended at %s, before the first armed fire time %s — the entity was never held past the point it would have fired, so deferral is untested",
			lastArmedAt.Format(time.RFC3339Nano), firstScheduled.Format(time.RFC3339Nano))
	}

	// Precondition for deferral: each write landed before the fire time then in
	// force. If a round-trip stalled past it, the scheduler was entitled to fire
	// and this run cannot test deferral — report that cause, not a false defect.
	for i := 1; i < len(arms); i++ {
		prevDue := smEventScheduledTime(t, arms[i-1])
		if armedAt := smEventTime(t, arms[i]); !armedAt.Before(prevDue) {
			t.Fatalf("loopback write %d landed at %s, at/after the fire time %s armed by the previous write — the machine stalled longer than the %dms delay, so this run cannot test deferral (not a scheduler defect)",
				i+1, armedAt.Format(time.RFC3339Nano), prevDue.Format(time.RFC3339Nano), delayMs)
		}
	}

	// With that established, no fire may have happened during the busy window.
	if hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "") {
		t.Errorf("expected no SCHEDULED_TRANSITION_FIRE event while the entity was kept busy; got events: %+v", events)
	}
	if s, _ := h.GetEntityState(t, entityID); s != "Open" {
		t.Errorf("expected entity to still be in Open while the timer kept getting deferred; got %q", s)
	}

	// Now stop updating and let the last-armed timer run out. Bounded,
	// generous poll — not a bare sleep — detects the eventual fire.
	awaitCallbackEntityState(t, h, entityID, "Closed", scheduledFireTimeout)

	events = schedEvents(t, h, entityID)
	if !hasSMEventType(events, "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Errorf("expected a SCHEDULED_TRANSITION_FIRE audit event with state Closed once the entity settled; got events: %+v", events)
	}
}

// TestE2E_ScheduledTransition_RestartDurability: a task armed by one pnode
// process survives that process and is fired by the next one on the same
// storage.
func TestE2E_ScheduledTransition_RestartDurability(t *testing.T) {
	first, s := newSchedulerHarness(t, func(cfg *app.Config) { cfg.Scheduler.Enabled = false })
	model := uniq("e2e-scheduled-restart-durability")
	first.SetupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "sched-restart-wf", "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 800}}]},
				"Closed": {}
			}
		}]
	}`)
	entityID, status, body := first.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	if r, ok := s.task(t, entityID, "AutoClose"); !ok || r.Status != "WAITING" {
		t.Fatalf("armed task = %+v present=%t; want a stored WAITING task before any scheduler ran", r, ok)
	}

	// The first process goes away: its scheduler never ran, nothing of the
	// task lives in its memory.
	first.app.Shutdown()
	if err := first.app.Close(); err != nil {
		t.Fatalf("close the first stack: %v", err)
	}

	second := newStackOn(t, s, nil)
	awaitCallbackEntityState(t, second, entityID, "Closed", scheduledFireTimeout)
	if !hasSMEventType(schedEvents(t, second, entityID), "SCHEDULED_TRANSITION_FIRE", "Closed") {
		t.Error("the restarted stack fired the task without a SCHEDULED_TRANSITION_FIRE event")
	}
}
