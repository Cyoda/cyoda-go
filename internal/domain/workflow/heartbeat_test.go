package workflow

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestHeartbeat_UnconditionalScheduledCycleFiresRepeatedly is a runtime proof
// of design §5.4's "infinite heartbeat" claim: an unconditional scheduled
// cycle (S1 --schedule(delayMs)--> S2 --schedule(delayMs)--> S1, no
// criteria — the accepted `allowCycles` "polling" shape) genuinely re-arms
// and re-fires hop after hop at runtime, not merely that the importer
// accepts the cycle shape (`validate_import_test.go:606` only proves that).
//
// Drives three hops deterministically via FireScheduledTransition on the
// task ID read back from the store after each hop, advancing the injected
// steppable clock between hops — matching the idioms in
// fire_scheduled_test.go (setupEngineWithSteppableClock, seedFireEntity,
// armTask, getTask, getEntityState, countAuditEvents).
func TestHeartbeat_UnconditionalScheduledCycleFiresRepeatedly(t *testing.T) {
	const armMs = int64(1_700_000_000_000)
	const delayMs = int64(1000)
	engine, factory, advance := setupEngineWithSteppableClock(t, armMs)
	ctx := ctxWithTenant(testTenant)
	modelRef := spi.ModelRef{EntityName: "heartbeat-order", ModelVersion: "1.0"}

	wf := spi.WorkflowDefinition{
		Version: "1.1", Name: "HeartbeatWF", InitialState: "S1", Active: true,
		States: map[string]spi.StateDefinition{
			"S1": {Transitions: []spi.TransitionDefinition{
				{Name: "PingToS2", Next: "S2", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
			"S2": {Transitions: []spi.TransitionDefinition{
				{Name: "PingToS1", Next: "S1", Schedule: &spi.TransitionSchedule{DelayMs: delayMs}},
			}},
		},
	}
	saveWorkflow(t, factory, ctx, modelRef, []spi.WorkflowDefinition{wf})
	seedFireEntity(t, factory, ctx, "hb-e1", modelRef, "S1", "seed-tx-1", map[string]any{})

	// The entity starts armed in S1 — seedFireEntity bypasses the engine's
	// own arm-on-entry, so the initial hop's task is armed by hand exactly
	// like every other fire_scheduled_test.go test does.
	s1ToS2 := taskID(testTenant, "hb-e1", "S1", "PingToS2")
	armTask(t, factory, ctx, spi.ScheduledTask{
		ID: s1ToS2, TenantID: testTenant, Type: spi.ScheduledTaskFireTransition,
		ScheduledTime: armMs + delayMs, EntityID: "hb-e1", ModelName: modelRef.EntityName,
		Transition: "PingToS2", SourceState: "S1", ArmedAt: armMs,
	})

	nowMs := armMs

	// --- Hop 1: S1 -> S2 ---
	advance(delayMs)
	nowMs += delayMs
	outcome, err := engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: s1ToS2, TenantID: testTenant})
	if err != nil {
		t.Fatalf("hop 1 FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("hop 1 outcome = %v, want Fired", outcome)
	}
	if got := getEntityState(t, factory, ctx, "hb-e1"); got != "S2" {
		t.Fatalf("hop 1 entity state = %q, want S2", got)
	}

	s2ToS1 := taskID(testTenant, "hb-e1", "S2", "PingToS1")
	task2, found := getTask(t, factory, ctx, s2ToS1)
	if !found {
		t.Fatal("expected S2's PingToS1 task armed after hop 1 (arm-on-entry, atomic with the fire)")
	}
	if task2.ScheduledTime != nowMs+delayMs {
		t.Errorf("hop 1 re-arm ScheduledTime = %d, want %d", task2.ScheduledTime, nowMs+delayMs)
	}

	// --- Hop 2: S2 -> S1 ---
	advance(delayMs)
	nowMs += delayMs
	outcome, err = engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: s2ToS1, TenantID: testTenant})
	if err != nil {
		t.Fatalf("hop 2 FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("hop 2 outcome = %v, want Fired", outcome)
	}
	if got := getEntityState(t, factory, ctx, "hb-e1"); got != "S1" {
		t.Fatalf("hop 2 entity state = %q, want S1", got)
	}

	task1, found := getTask(t, factory, ctx, s1ToS2)
	if !found {
		t.Fatal("expected S1's PingToS2 task re-armed after hop 2 — the cycle keeps ticking")
	}
	if task1.ScheduledTime != nowMs+delayMs {
		t.Errorf("hop 2 re-arm ScheduledTime = %d, want %d", task1.ScheduledTime, nowMs+delayMs)
	}

	// --- Hop 3: S1 -> S2 again — proves the heartbeat keeps firing past a
	// single round trip, not just once around the cycle. ---
	advance(delayMs)
	nowMs += delayMs
	outcome, err = engine.FireScheduledTransition(ctx, spi.ScheduledTask{ID: s1ToS2, TenantID: testTenant})
	if err != nil {
		t.Fatalf("hop 3 FireScheduledTransition: %v", err)
	}
	if outcome != OutcomeFired {
		t.Fatalf("hop 3 outcome = %v, want Fired", outcome)
	}
	if got := getEntityState(t, factory, ctx, "hb-e1"); got != "S2" {
		t.Fatalf("hop 3 entity state = %q, want S2", got)
	}

	if n := countAuditEvents(t, factory, ctx, "hb-e1", spi.SMEventScheduledTransitionFired); n != 3 {
		t.Errorf("SCHEDULED_TRANSITION_FIRE events = %d, want 3 (one per hop — the heartbeat genuinely fires repeatedly, not just imports)", n)
	}
}
