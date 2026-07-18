// Package scheduledfunction provides cross-backend parity scenarios for the
// scheduled-transition Function runtime (issue #419, design
// docs/superpowers/specs/2026-07-17-scheduled-transition-function-design.md):
// the `schedule.function` transition shape, which computes its
// scheduledTime/timeoutMs by dispatching a generic Function callout to a
// gRPC calculation member rather than reading a static delayMs — must be
// byte-consistent across memory, sqlite, postgres, and (on its next
// dependency bump) the commercial backend.
//
// Unlike e2e/parity/scheduledtransition (the static-delayMs sibling package,
// which needs no compute node at all), every scenario here dispatches to
// the parity harness's fixed gRPC compute-test-client member
// (cmd/compute-test-client) via the "sched-fn-resolve" catalog entry —
// see that entry's doc comment (cmd/compute-test-client/catalog.go) for the
// five schedMode values it serves. Because compute-test-client is a single,
// process-lifetime catalog shared by every parity scenario (there is no
// per-test closure registration the way internal/e2e's callback harness
// allows), "sched-fn-resolve" is driven entirely by entity DATA
// (schedMode/offsetMs/expireOffsetMs, read via attachEntity:true) rather
// than by scenario-specific server state — the same "config read from the
// request" idiom this package's compute-test-client already uses for
// select-premium/amount-gt-100/set-field.
//
// Determinism note: like scheduledtransition, BackendFixture is API-only
// (e2e/parity/contracts.go) and every backend runs cyoda-go as a real OS
// subprocess, so there is no in-process clock to inject. The
// ExpiryElapsedExpiresNoFire scenario in particular cannot rely on pausing
// the scheduler (internal/e2e's TestScheduledFunction_ExpiryElapsedBeforeScan_ExpiresNoFire
// approach) — instead it arms with an ALREADY-past absolute fireAt/expireAt
// pair whose lateness already exceeds timeoutMs+grace at arm time, so the
// very first scan (whatever its cadence) sees an expired task deterministically.
// See "sched-fn-resolve"'s expiryElapsed case for the arithmetic.
package scheduledfunction

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
		parity.NamedTest{Name: "ScheduledFunction_ArmAbsoluteFireAt", Fn: RunScheduledFunction_ArmAbsoluteFireAt},
		parity.NamedTest{Name: "ScheduledFunction_ArmRelativeFireAfterMs", Fn: RunScheduledFunction_ArmRelativeFireAfterMs},
		parity.NamedTest{Name: "ScheduledFunction_BornExpiredCancelsPriorRow", Fn: RunScheduledFunction_BornExpiredCancelsPriorRow},
		parity.NamedTest{Name: "ScheduledFunction_PastFireAtFiresPromptly", Fn: RunScheduledFunction_PastFireAtFiresPromptly},
		parity.NamedTest{Name: "ScheduledFunction_ExpiryElapsedExpiresNoFire", Fn: RunScheduledFunction_ExpiryElapsedExpiresNoFire},
	)
}

// StateMachine audit event-type strings (design doc §8), duplicated from
// e2e/parity/scheduledtransition's own copy — that package's doc comment
// explains why: keeping each package's dependency surface at HTTP + the
// parity client only, matching the existing e2e/parity/audit.go convention
// of asserting literal wire values rather than importing typed constants.
const (
	eventArmed     = "SCHEDULED_TRANSITION_ARM"
	eventFired     = "SCHEDULED_TRANSITION_FIRE"
	eventExpired   = "SCHEDULED_TRANSITION_EXPIRE"
	eventCancelled = "SCHEDULED_TRANSITION_CANCEL"
)

// functionTag is the calculationNodesTags value every schedule.function
// config in this file uses — it must match the compute-test-client's join
// tags (cmd/compute-test-client/dispatch.go's joinPayload: ["compute-test-client"]).
const functionTag = "compute-test-client"

// functionName is the compute-test-client catalog entry every scenario in
// this file dispatches to (cmd/compute-test-client/catalog.go).
const functionName = "sched-fn-resolve"

// fireTimeout bounds every poll loop in this file — sized generously above
// the parity fixtures' tuned-down CYODA_SCHEDULER_SCAN_INTERVAL (50ms —
// see each backend's fixture.go) while still tolerating slow-CI /
// postgres-subprocess overhead, mirroring scheduledtransition.go's
// identically-named, identically-reasoned constant.
const fireTimeout = 15 * time.Second

// pollInterval is the sleep between polls in awaitEntityState /
// awaitStateMachineEvent.
const pollInterval = 50 * time.Millisecond

// setupModelWithWorkflow imports a model from sampleDoc, locks it, and
// imports the given workflow JSON — this package's copy of the
// scheduledtransition-package helper of the same name (not reusable across
// packages; see that package's doc comment).
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

// scheduleFunctionWorkflowJSON builds a REPLACE-mode workflow import payload
// (schema version 1.3 — schedule.function requires it, per
// docs/workflow-schema-versioning.md) with a single non-manual
// Open -[AutoClose]-> Closed transition dispatching to "sched-fn-resolve"
// (attachEntity:true, so the Function reads schedMode/offsetMs/
// expireOffsetMs off the entity's own data).
func scheduleFunctionWorkflowJSON(wfName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.3", "name": %q, "initialState": "Open", "active": true,
			"states": {
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false,
					"schedule": {"function": {
						"name": %q,
						"resultKind": "Schedule",
						"calculationNodesTags": %q,
						"attachEntity": true
					}}
				}]},
				"Closed": {}
			}
		}]
	}`, wfName, functionName, functionTag)
}

// entityJSON builds the create/update payload for a given schedMode, with
// optional offsetMs/expireOffsetMs overrides (0 lets "sched-fn-resolve" use
// its own default for that mode — see catalog.go).
func entityJSON(schedMode string, offsetMs, expireOffsetMs int64) string {
	return fmt.Sprintf(`{"k":1,"schedMode":%q,"offsetMs":%d,"expireOffsetMs":%d}`, schedMode, offsetMs, expireOffsetMs)
}

const sampleDoc = `{"k":1,"schedMode":"none","offsetMs":0,"expireOffsetMs":0}`

// RunScheduledFunction_ArmAbsoluteFireAt verifies the absolute-fireAt arm
// path end-to-end: "sched-fn-resolve" returns a Schedule result with fireAt
// set to a fixed near-future unix-millis timestamp; the transition arms and
// (once the real scheduler picks it up) fires, landing the entity in
// Closed with the expected audit trail.
func RunScheduledFunction_ArmAbsoluteFireAt(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "schedfn-arm-absolute"
	const modelVersion = 1

	setupModelWithWorkflow(t, c, modelName, modelVersion, sampleDoc, scheduleFunctionWorkflowJSON("schedfn-abs-wf"))

	entityID, err := c.CreateEntity(t, modelName, modelVersion, entityJSON("absolute", 300, 0))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventArmed, "Open") {
		t.Errorf("expected a %s audit event for state Open after entity creation; got events: %+v", eventArmed, events)
	}

	awaitEntityState(t, c, entityID, "Closed", fireTimeout)

	events = stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "Closed") {
		t.Errorf("expected a %s audit event with state Closed; got events: %+v", eventFired, events)
	}
}

// RunScheduledFunction_ArmRelativeFireAfterMs verifies the relative-
// fireAfterMs arm path: "sched-fn-resolve" returns a Schedule result with
// only fireAfterMs set, so scheduledTime = armMs + fireAfterMs; the
// transition still fires and lands the entity in Closed.
func RunScheduledFunction_ArmRelativeFireAfterMs(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "schedfn-arm-relative"
	const modelVersion = 1

	setupModelWithWorkflow(t, c, modelName, modelVersion, sampleDoc, scheduleFunctionWorkflowJSON("schedfn-rel-wf"))

	entityID, err := c.CreateEntity(t, modelName, modelVersion, entityJSON("relative", 300, 0))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventArmed, "Open") {
		t.Errorf("expected a %s audit event for state Open after entity creation; got events: %+v", eventArmed, events)
	}

	awaitEntityState(t, c, entityID, "Closed", fireTimeout)

	events = stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "Closed") {
		t.Errorf("expected a %s audit event with state Closed; got events: %+v", eventFired, events)
	}
}

// RunScheduledFunction_BornExpiredCancelsPriorRow verifies the re-arm path
// of resolveSchedule's born-expired branch (design: "expiry <= sched"):
// creation arms a real, far-future task; a subsequent data-only loopback
// update whose recomputed Function result is now born-expired must cancel
// the PRIOR armed row synchronously in the same write — no stale fire is
// possible once the update returns — and record EXPIRE, not a silent no-op.
func RunScheduledFunction_BornExpiredCancelsPriorRow(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "schedfn-born-expired-rearm"
	const modelVersion = 1

	setupModelWithWorkflow(t, c, modelName, modelVersion, sampleDoc, scheduleFunctionWorkflowJSON("schedfn-bornexpired-wf"))

	// First arm: far in the future (600s) — proves a real row existed
	// before being superseded; comfortably outlives this scenario, so any
	// FIRE observed later would be a genuine bug, not a race.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, entityJSON("relative", 600_000, 0))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}
	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventArmed, "Open") {
		t.Fatalf("expected the first arm's %s audit event for state Open; got events: %+v", eventArmed, events)
	}

	// Re-arm via a data-only loopback (still Open, no transition): the new
	// Function result is born-expired (expireAfterMs:0 resolves to
	// expiry == the fire time exactly).
	if err := c.UpdateEntityData(t, entityID, entityJSON("bornExpired", 1000, 0)); err != nil {
		t.Fatalf("UpdateEntityData (born-expired re-arm): %v", err)
	}

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "Open" {
		t.Fatalf("expected the entity to remain in Open (never armed, so it can never fire); got %q", got.Meta.State)
	}

	events = stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventExpired, "Open") {
		t.Errorf("expected a %s audit event after the born-expired re-arm; got events: %+v", eventExpired, events)
	}
	if hasStateMachineEvent(events, eventFired, "") {
		t.Errorf("expected no %s event — the prior row was cancelled before any scanner could pick it up; got events: %+v", eventFired, events)
	}
}

// RunScheduledFunction_PastFireAtFiresPromptly verifies that a past
// absolute fireAt is not an error — resolveSchedule's doc comment: "a past
// fireAt is not an error — the transition simply arms to fire immediately".
// The row is armed verbatim with the past timestamp, and the real
// scheduler fires it on its very next scan (already due at arm time).
func RunScheduledFunction_PastFireAtFiresPromptly(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "schedfn-past-fire"
	const modelVersion = 1

	setupModelWithWorkflow(t, c, modelName, modelVersion, sampleDoc, scheduleFunctionWorkflowJSON("schedfn-pastfire-wf"))

	// pastFire mode resolves fireAt = now - 5000ms (see catalog.go) —
	// already due the instant the row is created.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, entityJSON("pastFire", 5000, 0))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	awaitEntityState(t, c, entityID, "Closed", fireTimeout)

	events := stateMachineEvents(t, c, entityID)
	if !hasStateMachineEvent(events, eventFired, "Closed") {
		t.Errorf("expected the already-due transition to record a %s audit event with state Closed; got events: %+v", eventFired, events)
	}
}

// RunScheduledFunction_ExpiryElapsedExpiresNoFire verifies the settled-
// interval "expiry elapsed before any scanner ever reads the row" path
// (design §5.5's grace-band gate): "sched-fn-resolve" in expiryElapsed mode
// returns a VALID (not born-expired: expiry > fire time) arm whose fireAt
// and expireAt are both already deep in the past at arm time, so lateness
// already exceeds timeoutMs+grace before the very first scan — the task
// resolves Expired and never Fired, deterministically regardless of scan
// cadence (see the package doc comment and catalog.go's expiryElapsed case
// for why no scheduler-pausing trick is needed here).
func RunScheduledFunction_ExpiryElapsedExpiresNoFire(t *testing.T, fixture parity.BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "schedfn-expiry-elapsed"
	const modelVersion = 1

	setupModelWithWorkflow(t, c, modelName, modelVersion, sampleDoc, scheduleFunctionWorkflowJSON("schedfn-expiryelapsed-wf"))

	// expiryElapsed mode resolves fireAt = now-5000ms, expireAt = fireAt+110ms
	// (see catalog.go) — a valid, non-born-expired arm (expiry > fire time)
	// whose lateness (~5000ms) already vastly exceeds timeoutMs(110ms)+grace(100ms)
	// at arm time.
	entityID, err := c.CreateEntity(t, modelName, modelVersion, entityJSON("expiryElapsed", 5000, 110))
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	awaitStateMachineEvent(t, c, entityID, eventExpired, "Open", fireTimeout)

	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Meta.State != "Open" {
		t.Errorf("expected the entity to remain in Open (expired, not fired); got %q", got.Meta.State)
	}

	events := stateMachineEvents(t, c, entityID)
	if hasStateMachineEvent(events, eventFired, "") {
		t.Errorf("expected no %s event — lateness already exceeded timeout+grace before any scanner ran; got events: %+v", eventFired, events)
	}
}
