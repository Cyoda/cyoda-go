// Package scheduledtransition provides cross-backend parity scenarios for
// the scheduled-transition runtime (design doc §11, "P" column): the
// ScheduledTaskStore arm/upsert/reconcile/delete contract and the engine's
// fire/decline/expire/cancel behavior must be byte-consistent across
// memory, sqlite, postgres, and (on its next dependency bump) the
// commercial backend.
//
// Determinism note: BackendFixture is deliberately API-only (see
// e2e/parity/contracts.go — "no storage handle... verification is
// API-only") and every backend under it runs cyoda-go as a real OS
// subprocess (e2e/parity/fixtureutil.LaunchCyodaAndCompute), so there is
// no in-process engine or clock to inject — WithScheduledClock (used by
// internal/domain/workflow's unit tests) is not reachable from here, and
// never will be for any backend under this harness, including memory.
// Exact scheduledTime/lateness arithmetic and grace-band boundaries are
// therefore Unit-only (see internal/domain/workflow/arm_test.go and
// fire_scheduled_test.go) — never asserted here.
//
// What IS asserted here is OBSERVABLE, coarse behavior: does a task get
// armed (a SCHEDULED_TRANSITION_ARM audit event appears), does it fire
// and land the entity in the expected state, does a state exit cancel it,
// does a loopback re-arm it without a spurious cancel. Scenarios that
// need the runtime scan loop to actually pick up a due task use a small
// Schedule.DelayMs plus generous, bounded polling (the same
// non-boundary, non-flaky methodology the design doc prescribes for
// internal/e2e — see its §11 "Time control" note) rather than any exact
// timing assertion. The one exception (ReArmClockReset) needs a
// wall-clock comparison to prove the timer was pushed forward rather
// than left alone; it uses wide, documented margins for exactly that
// reason.
package scheduledtransition

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity"
	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "ScheduledTransition_ArmsOnEntry", Fn: RunScheduledTransition_ArmsOnEntry},
		parity.NamedTest{Name: "ScheduledTransition_FiresOnTime", Fn: RunScheduledTransition_FiresOnTime},
		parity.NamedTest{Name: "ScheduledTransition_DeclineCriterionFalse", Fn: RunScheduledTransition_DeclineCriterionFalse},
		parity.NamedTest{Name: "ScheduledTransition_NoExpireWhenNilTimeout", Fn: RunScheduledTransition_NoExpireWhenNilTimeout},
		parity.NamedTest{Name: "ScheduledTransition_CancelOnStateExit", Fn: RunScheduledTransition_CancelOnStateExit},
		parity.NamedTest{Name: "ScheduledTransition_LoopbackReArmsNoCancel", Fn: RunScheduledTransition_LoopbackReArmsNoCancel},
		parity.NamedTest{Name: "ScheduledTransition_ReArmClockReset", Fn: RunScheduledTransition_ReArmClockReset},
		parity.NamedTest{Name: "ScheduledTransition_CascadeAfterFire", Fn: RunScheduledTransition_CascadeAfterFire},
	)
}

// StateMachine audit event-type strings (design doc §8). Scenarios assert
// against these literal wire values — matching the convention already
// used by e2e/parity/audit.go (e.g. "STATE_MACHINE_START") — rather than
// importing the spi package's typed constants, keeping this package's
// dependency surface at HTTP + the parity client only.
const (
	eventArmed            = "SCHEDULED_TRANSITION_ARM"
	eventFired            = "SCHEDULED_TRANSITION_FIRE"
	eventExpired          = "SCHEDULED_TRANSITION_EXPIRE"
	eventCancelled        = "SCHEDULED_TRANSITION_CANCEL"
	eventCriterionNoMatch = "TRANSITION_NOT_MATCH_CRITERION"
	eventTransitionMade   = "TRANSITION_MAKE"
)

// fireTimeout bounds every poll loop in this file. It is sized generously
// above the worst case a tuned-down scan interval should need (the parity
// fixtures set CYODA_SCHEDULER_SCAN_INTERVAL=50ms — see each backend's
// fixture.go) while still tolerating the untuned 1s default plus slow-CI /
// postgres-subprocess startup overhead, so a real bug (not scheduler
// cadence) is what trips this timeout.
const fireTimeout = 15 * time.Second

// pollInterval is the sleep between polls in awaitEntityState /
// awaitStateMachineEvent.
const pollInterval = 50 * time.Millisecond

// setupModelWithWorkflow imports a model from sampleDoc, locks it, and
// imports the given workflow JSON — the scheduledtransition-package
// equivalent of the unexported helper of the same name in e2e/parity (not
// reusable across packages). sampleDoc must declare every field any
// scenario entity subsequently sends — the model's schema is inferred
// from it and is strict about unknown fields.
func setupModelWithWorkflow(t *testing.T, c *client.Client, modelName string, modelVersion int, sampleDoc, workflowJSON string) {
	t.Helper()
	if err := c.ImportModel(t, modelName, modelVersion, sampleDoc); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, workflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}
}

// awaitEntityState polls GetEntity until Meta.State == wantState, or fails
// the test once timeout elapses.
func awaitEntityState(t *testing.T, c *client.Client, id uuid.UUID, wantState string, timeout time.Duration) client.EntityResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last client.EntityResult
	for {
		got, err := c.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity: %v", err)
		}
		last = got
		if got.Meta.State == wantState {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for entity %s to reach state %q; last observed state %q",
				timeout, id, wantState, last.Meta.State)
		}
		time.Sleep(pollInterval)
	}
}

// stateMachineEvents fetches and decodes every StateMachine-typed audit
// event for the entity.
func stateMachineEvents(t *testing.T, c *client.Client, id uuid.UUID) []client.StateMachineAuditEvent {
	t.Helper()
	resp, err := c.GetAuditEvents(t, id)
	if err != nil {
		t.Fatalf("GetAuditEvents: %v", err)
	}
	out := make([]client.StateMachineAuditEvent, 0, len(resp.Items))
	for i := range resp.Items {
		if resp.Items[i].AuditEventType != "StateMachine" {
			continue
		}
		sm, err := resp.Items[i].AsStateMachine()
		if err != nil {
			t.Fatalf("AsStateMachine: %v", err)
		}
		out = append(out, *sm)
	}
	return out
}

// hasStateMachineEvent reports whether events contains one whose EventType
// equals eventType. If state is non-empty, the event's State must also
// match.
func hasStateMachineEvent(events []client.StateMachineAuditEvent, eventType, state string) bool {
	for _, ev := range events {
		if ev.EventType != eventType {
			continue
		}
		if state != "" && ev.State != state {
			continue
		}
		return true
	}
	return false
}

// awaitStateMachineEvent polls GetAuditEvents until an event matching
// eventType (and, if state is non-empty, State too) appears, or fails the
// test once timeout elapses.
func awaitStateMachineEvent(t *testing.T, c *client.Client, id uuid.UUID, eventType, state string, timeout time.Duration) client.StateMachineAuditEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		events := stateMachineEvents(t, c, id)
		for _, ev := range events {
			if ev.EventType == eventType && (state == "" || ev.State == state) {
				return ev
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for StateMachine event %q (state=%q) on entity %s; got events: %+v",
				timeout, eventType, state, id, events)
		}
		time.Sleep(pollInterval)
	}
}

// RunScheduledTransition_ArmsOnEntry verifies that an entity landing in a
// state with a scheduled transition gets a SCHEDULED_TRANSITION_ARM audit
// event recorded atomically with the entity write (design §5.1), across
// every backend. The 5s delay comfortably outlives this scenario, so a
// positive ARM read is not racing a fire that already happened, and the
// entity is expected to still be resting in OPEN.
func RunScheduledTransition_ArmsOnEntry(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-arms-on-entry"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "arms-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 5000}}]},
				"CLOSED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventArmed, "OPEN") {
		t.Errorf("expected a %s audit event for state OPEN after entity creation; got events: %+v", eventArmed, events)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "OPEN" {
		t.Errorf("expected entity to rest in OPEN (5s delay hasn't elapsed); got %q", got.Meta.State)
	}
}

// RunScheduledTransition_FiresOnTime verifies a no-criterion scheduled
// transition eventually fires: the entity advances to Next and a
// SCHEDULED_TRANSITION_FIRE audit event is recorded.
func RunScheduledTransition_FiresOnTime(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-fires-on-time"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "fires-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 150}}]},
				"CLOSED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	awaitEntityState(t, c, entityID, "CLOSED", fireTimeout)

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "CLOSED") {
		t.Errorf("expected a %s audit event with state CLOSED; got events: %+v", eventFired, events)
	}
}

// RunScheduledTransition_DeclineCriterionFalse verifies that a scheduled
// transition guarded by a criterion that evaluates false is Declined
// (one-shot, no retry, design §5.4): the entity stays in its source state
// and the task is resolved (no fire).
func RunScheduledTransition_DeclineCriterionFalse(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-decline-criterion-false"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "decline-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 150},
					"criterion": {"type": "simple", "jsonPath": "$.flag", "operatorType": "EQUALS", "value": true}
				}]},
				"CLOSED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"flag":true}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"flag":false}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	awaitStateMachineEvent(t, c, entityID, eventCriterionNoMatch, "OPEN", fireTimeout)

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "OPEN" {
		t.Errorf("expected entity to remain in OPEN after a one-shot decline; got %q", got.Meta.State)
	}

	events := stateMachineEvents(t, c, entityID)
	if hasStateMachineEvent(events, eventFired, "") {
		t.Errorf("expected no %s event on a declined scheduled transition; got events: %+v", eventFired, events)
	}
}

// RunScheduledTransition_NoExpireWhenNilTimeout verifies that a scheduled
// transition with no TimeoutMs never expires, no matter how much lateness
// accrues before it is picked up (design §5.5: "timeoutMs nil -> never
// expires"). A deliberate sleep lets real lateness build up past the
// delay before polling even starts.
func RunScheduledTransition_NoExpireWhenNilTimeout(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-no-expire-nil-timeout"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "no-expire-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 150}}]},
				"CLOSED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Let real lateness accrue well past the 150ms delay before even
	// starting to poll — a nil TimeoutMs task never expires (design §5.5),
	// so arriving "late" to the check must not matter.
	time.Sleep(500 * time.Millisecond)

	awaitEntityState(t, c, entityID, "CLOSED", fireTimeout)

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "CLOSED") {
		t.Errorf("expected a %s audit event despite accrued lateness (nil TimeoutMs never expires); got events: %+v", eventFired, events)
	}
	if hasStateMachineEvent(events, eventExpired, "") {
		t.Errorf("expected no %s event when TimeoutMs is nil; got events: %+v", eventExpired, events)
	}
}

// RunScheduledTransition_CancelOnStateExit verifies that an entity
// explicitly leaving a state via a DIFFERENT (manual) transition cancels
// that state's pending scheduled task: a SCHEDULED_TRANSITION_CANCEL
// audit event is recorded (design §5.1) and the abandoned task never
// fires. This scenario needs no wall-clock wait at all — the cancel is
// synchronous within the manual-transition request, and the 10s schedule
// delay comfortably outlives the test regardless.
func RunScheduledTransition_CancelOnStateExit(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-cancel-on-exit"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cancel-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [
					{"name": "AutoClose", "next": "CLOSED", "manual": false, "schedule": {"delayMs": 10000}},
					{"name": "ManualLeave", "next": "LEFT", "manual": true}
				]},
				"CLOSED": {},
				"LEFT": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	if err := c.UpdateEntity(t, entityID, "ManualLeave", `{"k":1}`); err != nil {
		t.Fatalf("UpdateEntity(ManualLeave): %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "LEFT" {
		t.Fatalf("expected entity in LEFT after the manual exit; got %q", got.Meta.State)
	}

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventCancelled, "OPEN") {
		t.Errorf("expected a %s audit event for the abandoned AutoClose task (state OPEN); got events: %+v", eventCancelled, events)
	}
	// The 10s delay comfortably outlives this scenario, so any FIRE would be
	// a genuine bug (cancel didn't actually remove the row), not a race.
	if hasStateMachineEvent(events, eventFired, "") {
		t.Errorf("expected no %s event — the scheduled task should have been cancelled, not fired; got events: %+v", eventFired, events)
	}
}

// RunScheduledTransition_LoopbackReArmsNoCancel verifies that a data-only
// update (Loopback — same state, no transition) does NOT emit a spurious
// SCHEDULED_TRANSITION_CANCEL (design §5.1 / §15 F5). This isolates the
// "no cancel" half of the loopback contract so it stays fast and free of
// timing margins; the "re-armed to a later time" half is
// RunScheduledTransition_ReArmClockReset.
func RunScheduledTransition_LoopbackReArmsNoCancel(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-loopback-no-cancel"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "loopback-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": 3000}}]},
				"CLOSED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Data-only update while still in OPEN, well before the 3s delay
	// elapses — a Loopback, not a state exit.
	time.Sleep(100 * time.Millisecond)
	if err := c.UpdateEntityData(t, entityID, `{"k":2}`); err != nil {
		t.Fatalf("UpdateEntityData (loopback): %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "OPEN" {
		t.Fatalf("expected the loopback to leave the entity in OPEN; got %q", got.Meta.State)
	}

	events := stateMachineEvents(t, c, entityID)
	if hasStateMachineEvent(events, eventCancelled, "") {
		t.Errorf("expected no %s event on a same-state loopback; got events: %+v", eventCancelled, events)
	}
}

// RunScheduledTransition_ReArmClockReset verifies that a loopback actually
// pushes the scheduled task's fire time later, rather than leaving the
// original arm untouched (design §5.1: "loopback... re-arms via upsert").
//
// Timing rationale: if reconcile were a no-op on the loopback path, the
// task would still fire ~delayMs after creation (the original arm). If it
// correctly re-arms, it fires ~loopbackOffset+delayMs after creation
// instead. minElapsedForCorrectReArm sits well clear of both marks —
// comfortably above the "no-op" mark and comfortably below the "correct"
// mark — so the assertion tolerates scan cadence and CI scheduling
// jitter without needing exact clock control (which this HTTP-only,
// subprocess-per-backend harness cannot provide — see the package doc).
func RunScheduledTransition_ReArmClockReset(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-rearm-clock-reset"
	const modelVersion = 1
	const delayMs = 700
	const loopbackOffset = 250 * time.Millisecond
	const minElapsedForCorrectReArm = 820 * time.Millisecond

	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "rearm-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "CLOSED", "manual": false,
					"schedule": {"delayMs": %d}}]},
				"CLOSED": {}
			}
		}]
	}`, delayMs)
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	createdAt := time.Now()
	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	time.Sleep(loopbackOffset)
	if err := c.UpdateEntityData(t, entityID, `{"k":2}`); err != nil {
		t.Fatalf("UpdateEntityData (loopback): %v", err)
	}

	awaitEntityState(t, c, entityID, "CLOSED", fireTimeout)
	elapsed := time.Since(createdAt)
	if elapsed < minElapsedForCorrectReArm {
		t.Errorf("entity fired %s after creation — too soon: expected the loopback to re-arm the task to fire "+
			"around %s (loopback offset + delay), not the original %dms", elapsed, loopbackOffset+delayMs*time.Millisecond, delayMs)
	}

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "CLOSED") {
		t.Errorf("expected a %s audit event; got events: %+v", eventFired, events)
	}
}

// RunScheduledTransition_CascadeAfterFire verifies that firing a scheduled
// transition continues the ordinary automated cascade past its landing
// state, and that the settled state's own scheduled transitions get
// armed by the same reconcile (design §5.1/§5.2): OPEN fires into MID
// (scheduled), MID's unconditional automated transition immediately
// advances to DONE, and DONE's own scheduled transition (AutoFinal) is
// armed once the entity settles there.
func RunScheduledTransition_CascadeAfterFire(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "sched-cascade-after-fire"
	const modelVersion = 1

	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cascade-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [{"name": "AutoClose", "next": "MID", "manual": false,
					"schedule": {"delayMs": 150}}]},
				"MID": {"transitions": [{"name": "AutoAdvance", "next": "DONE", "manual": false}]},
				"DONE": {"transitions": [{"name": "AutoFinal", "next": "ARCHIVED", "manual": false,
					"schedule": {"delayMs": 10000}}]},
				"ARCHIVED": {}
			}
		}]
	}`
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"k":1}`, wf)

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"k":1}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	awaitEntityState(t, c, entityID, "DONE", fireTimeout)

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "DONE") {
		t.Errorf("expected a %s audit event (state DONE, the post-cascade settled state) for the OPEN->MID scheduled fire; got events: %+v", eventFired, events)
	}
	if !hasStateMachineEvent(events, eventTransitionMade, "MID") {
		t.Errorf("expected an ordinary %s event (state MID) for the automated MID->DONE cascade hop; got events: %+v", eventTransitionMade, events)
	}
	if !hasStateMachineEvent(events, eventArmed, "DONE") {
		t.Errorf("expected a %s event for DONE's own scheduled transition (AutoFinal) armed after the cascade settled; got events: %+v", eventArmed, events)
	}
	if hasStateMachineEvent(events, eventCancelled, "") {
		t.Errorf("expected no %s event — the fired task's own resolution must not be double-counted as a cancel; got events: %+v", eventCancelled, events)
	}
}
