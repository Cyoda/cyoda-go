package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scheduled_attribution_test.go — single-backend (postgres) e2e coverage for
// attribution of SCHEDULED-fire follow-on actions (spec
// docs/superpowers/specs/2026-07-22-attribute-followon-actions-design.md §5).
//
// A scheduled transition is armed by whoever's causal origin was in effect at
// arm time (spi.ResolveOrigin -> ScheduledTask.ArmedBy, arm.go), and fired
// later by the platform scheduler. FireScheduledTransition
// (internal/domain/workflow/fire_scheduled.go) stamps the fired anchor
// version's attributed principal from the durable ArmedBy (re-verified
// unchanged under the re-read guard) and its EXECUTOR as the system principal
// {id:"system", kind:"system"} — never the literal string "scheduler". These
// tests drive real timers (short DelayMs / fireAfterMs) through the full
// HTTP+gRPC stack via the callback harness, wait for the real scan loop to
// fire, and assert the recorded {attributed, executor} pair on
// GET /entity/{id}/changes.
//
// Reused helpers: newCallbackHarness / mintUserToken / createEntityAs /
// getChanges / findChangeByType / assertAttribution (attribution_test.go),
// awaitCallbackEntityState (scheduled_function_test.go), queryDB / dbPool
// (helpers_test.go / e2e_test.go), RegisterFunction / scheduleFunctionWorkflowJSON
// (scheduled_function_test.go).

// firedSchedExecKind/firedSchedExecID are the executor identity every
// scheduled fire records (fire_scheduled.go's systemPrincipal).
const (
	firedSchedExecKind = "system"
	firedSchedExecID   = "system"
)

// awaitFiredAnchor waits for entityID to reach wantState on h's stack, then
// returns the newest UPDATE change entry — the fired transition's anchor
// version. Fails the test if the fire never lands or no UPDATE is recorded.
func awaitFiredAnchor(t *testing.T, h *callbackHarness, entityID, wantState string) map[string]any {
	t.Helper()
	awaitCallbackEntityState(t, h, entityID, wantState, scheduledFireTimeout)
	anchor := findChangeByType(h.getChanges(t, entityID), "UPDATE")
	if anchor == nil {
		t.Fatalf("entity %s reached %q but no UPDATE change (fired anchor) was recorded; changes=%v",
			entityID, wantState, h.getChanges(t, entityID))
	}
	return anchor
}

// assertNoSchedulerString fails if the string "scheduler" appears anywhere in
// the entity's serialized change history — the fire path attributes to the
// arming principal or the system principal, never the literal "scheduler"
// (fire_scheduled.go).
func assertNoSchedulerString(t *testing.T, h *callbackHarness, entityID string) {
	t.Helper()
	raw, err := json.Marshal(h.getChanges(t, entityID))
	if err != nil {
		t.Fatalf("marshal changes for %s: %v", entityID, err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "scheduler") {
		t.Errorf("change history for %s records the literal \"scheduler\" somewhere; want only the arming/system principal: %s", entityID, raw)
	}
}

// --- Scenario 1: user arms a timer; the fire attributes to that user ----------

// TestAttribution_ScheduledUserArmed: a USER (alice) creates an entity whose
// init cascade lands it in a state with a short-DelayMs scheduled transition —
// arming that timer with alice as the causal origin. The platform scheduler
// fires it later. The fired anchor must attribute to alice (attributedKind
// user), executed by the system principal {system,system}. No version anywhere
// in the entity's history may record the literal "scheduler".
func TestAttribution_ScheduledUserArmed(t *testing.T) {
	h := newCallbackHarness(t)

	const model = "attr-sched-user-armed"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-sched-user-armed-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 300}}]},
				"Closed": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, model, wf)

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	xID, status, body := h.createEntityAs(t, alice, model, 1, `{"name":"x","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create as alice: %d %s", status, body)
	}

	anchor := awaitFiredAnchor(t, h, xID, "Closed")
	assertAttribution(t, anchor, "scheduled fire anchor (user-armed)", "alice", "user", firedSchedExecKind, firedSchedExecID)
	assertNoSchedulerString(t, h, xID)
}

// --- Scenario 2: a fire that arms a further hop stays user-rooted -------------

// TestAttribution_ScheduledChain: alice arms hop 1 (Open -[AutoClose]-> Mid);
// firing hop 1 arms hop 2 (Mid -[AutoNext]-> Done) inside the fire's cascade,
// seeded with the SAME chain origin (fire_scheduled.go's WithAmbientOrigin);
// firing hop 2 must still attribute to alice. Every version in the history —
// the create and both fired anchors — is alice-rooted; the executor of each
// fire is the system principal.
func TestAttribution_ScheduledChain(t *testing.T) {
	h := newCallbackHarness(t)

	const model = "attr-sched-chain"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-sched-chain-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
				"Open": {"transitions": [{"name": "AutoClose", "next": "Mid", "manual": false, "schedule": {"delayMs": 300}}]},
				"Mid":  {"transitions": [{"name": "AutoNext", "next": "Done", "manual": false, "schedule": {"delayMs": 300}}]},
				"Done": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, model, wf)

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	xID, status, body := h.createEntityAs(t, alice, model, 1, `{"name":"x","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create as alice: %d %s", status, body)
	}

	// Hop 2's fire lands the entity in Done — its anchor is the newest UPDATE.
	anchor := awaitFiredAnchor(t, h, xID, "Done")
	assertAttribution(t, anchor, "scheduled chain final anchor", "alice", "user", firedSchedExecKind, firedSchedExecID)

	// Faithful all the way down: every recorded version attributes to alice —
	// the create AND both scheduled fires — none silently degrading to the
	// system principal mid-chain.
	changes := h.getChanges(t, xID)
	updates := 0
	for _, c := range changes {
		if u, _ := c["user"].(string); u != "alice" {
			t.Errorf("chain leak: change %v attributes to %q; want alice throughout", c, u)
		}
		if ct, _ := c["changeType"].(string); ct == "UPDATE" {
			updates++
		}
	}
	if updates < 2 {
		t.Errorf("expected at least 2 fired-anchor UPDATE versions (AutoClose + AutoNext); got %d (changes=%v)", updates, changes)
	}
	assertNoSchedulerString(t, h, xID)
}

// --- Scenario 3: the fired anchor is stamped from ArmedBy, not stale meta -----

// TestAttribution_ScheduledAnchorStamped: regression for the stale-meta bug
// (fire_scheduled.go: the anchor must be stamped from the durable ArmedBy
// BEFORE the cascade, not left carrying whatever principal last wrote the
// entity). The entity Y is CREATED by bob (a processor callback presenting
// bob's OBO user token, joined into alice's transaction — the D3 divergence:
// a user-kind executor records itself, so Y's create version writer is bob),
// while the timer armed on Y in that same transaction carries the CHAIN ORIGIN
// alice (spec §5.2). When the timer fires, the anchor must carry alice
// (ArmedBy) — DIFFERENT from bob, the previous version's writer — proving the
// fire re-stamps rather than inheriting the loaded entity's stale meta.
func TestAttribution_ScheduledAnchorStamped(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "attr-sched-stamp-primary"
	const secondary = "attr-sched-stamp-secondary"
	// Y: init -> Open, with a short-DelayMs AutoClose that really fires.
	secondaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-sched-stamp-y-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 300}}]},
				"Closed": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWF)

	bob := h.mintUserToken(t, "bob", "ROLE_USER")
	yIDs := make(chan string, 1)
	h.RegisterProc("attr-sched-stamp-proc", func(rc *reqCtx) (map[string]any, error) {
		// Joined (rc.token present) BUT presenting bob's user token → D3: Y's
		// create version writer is bob; the AutoClose timer, armed in the same
		// tx, inherits the chain origin alice as ArmedBy.
		res, err := rc.CreateEntityAs(bob, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("stamp create Y: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("stamp create Y status=%d body=%s", res.StatusCode, res.Body)
		}
		yIDs <- res.EntityID
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-sched-stamp", "attr-sched-stamp-proc", "SYNC", ""))

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice: %d %s", status, body)
	}

	var yID string
	select {
	case yID = <-yIDs:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: processor did not create Y")
	}

	// The previous version's writer (Y's CREATE) is bob.
	created := findChangeByType(h.getChanges(t, yID), "CREATE")
	assertAttribution(t, created, "Y create (D3 executor bob)", "bob", "user", "user", "bob")

	// The fired anchor carries alice (ArmedBy / chain origin), NOT bob (the
	// entity's last writer / stale meta), executed by the system principal.
	anchor := awaitFiredAnchor(t, h, yID, "Closed")
	assertAttribution(t, anchor, "Y fired anchor (stamped from ArmedBy)", "alice", "user", firedSchedExecKind, firedSchedExecID)
	if u, _ := anchor["user"].(string); u == "bob" {
		t.Errorf("stale-meta regression: fired anchor attributes to bob (the previous version's writer), not the arming principal alice")
	}
	assertNoSchedulerString(t, h, yID)
}

// --- Scenario 4: a spoofed armedBy is ignored; the true principal wins --------

// TestAttribution_ScheduledArmedBySpoofIgnored: neither the triggering save
// body nor (for schedule.function) the callout RESPONSE can set the arming
// principal. The actually-fired task attributes to the true origin, never the
// spoof (spec §9; arm.go's armViaFunction reads only timing from the Function
// result, never a principal).
func TestAttribution_ScheduledArmedBySpoofIgnored(t *testing.T) {
	// 4a: the save body carries a spoofed armedBy (declared as ordinary data on
	// the model so a locked model doesn't reject it outright) → the fired
	// anchor attributes to alice, the true arming principal, not the spoof.
	t.Run("SaveBodyArmedBySpoofed", func(t *testing.T) {
		h := newCallbackHarness(t)
		const model = "attr-sched-spoof-body"

		// Declare armedBy as ordinary DATA so the spoof is accepted-but-ignored
		// (proving "ignored for attribution", not "rejected") — same technique
		// as TestAttribution_NoRequestFieldSetsOrigin.
		sample := `{"name":"x","amount":100,"status":"new","armedBy":{"id":"","kind":""}}`
		wf := `{
			"importMode": "REPLACE",
			"workflows": [{
				"version": "1.1", "name": "attr-sched-spoof-body-wf", "initialState": "NONE", "active": true,
				"states": {
					"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
					"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 300}}]},
					"Closed": {}
				}
			}]
		}`
		h.setupModelSampleWithWorkflow(t, model, sample, wf)

		alice := h.mintUserToken(t, "alice", "ROLE_USER")
		spoofBody := `{"name":"x","amount":100,"status":"new","armedBy":{"id":"evil","kind":"system"}}`
		xID, status, body := h.createEntityAs(t, alice, model, 1, spoofBody)
		if status != http.StatusOK {
			t.Fatalf("create as alice (spoofed body): %d %s", status, body)
		}

		anchor := awaitFiredAnchor(t, h, xID, "Closed")
		assertAttribution(t, anchor, "fired anchor (save-body armedBy spoof ignored)", "alice", "user", firedSchedExecKind, firedSchedExecID)
		if u, _ := anchor["user"].(string); u == "evil" {
			t.Errorf("save-body armedBy spoof leaked into attribution (user=evil); want alice")
		}
		// Sanity: the spoof WAS accepted as plain data, proving accepted-but-ignored.
		if got, _ := h.GetEntityData(t, xID)["armedBy"].(map[string]any); got != nil {
			if id, _ := got["id"].(string); id != "evil" {
				t.Errorf("spoofed data.armedBy.id = %q; want %q (should be stored as plain data)", id, "evil")
			}
		}
	})

	// 4b: the schedule.function RESULT contract carries only timing — it has no
	// principal field. A compute node that tries to smuggle an armedBy into the
	// Schedule result is rejected outright (strict decode → 500
	// SCHEDULE_FUNCTION_INVALID_RESULT, same fail-closed path as any malformed
	// result): there is simply no channel for the callout response to set the
	// arming principal.
	t.Run("FunctionResultCannotCarryPrincipal", func(t *testing.T) {
		h := newCallbackHarness(t)
		const model = "attr-sched-spoof-fn-reject"

		h.RegisterFunction("calcSpoofArmedBy", func(rc *reqCtx) (string, map[string]any, error) {
			// Well-formed timing PLUS a spoofed armedBy — the extra field has no
			// place in the Schedule result schema and is rejected, not ignored.
			return "Schedule", map[string]any{
				"fireAfterMs": int64(600_000),
				"armedBy":     map[string]any{"id": "evil", "kind": "system"},
			}, nil
		})

		wf := scheduleFunctionWorkflowJSON("attr-sched-spoof-fn-reject-wf", validScheduleFunctionJSON("calcSpoofArmedBy"))
		h.SetupModelWithWorkflow(t, model, wf)

		alice := h.mintUserToken(t, "alice", "ROLE_USER")
		_, status, body := h.createEntityAs(t, alice, model, 1, `{"name":"x","amount":1,"status":"new"}`)
		if status != http.StatusInternalServerError {
			t.Fatalf("expected 500 rejecting a Schedule result carrying a principal field; got %d %s", status, body)
		}
		if code, _ := decodeProblem(t, body).Properties["errorCode"].(string); code != "SCHEDULE_FUNCTION_INVALID_RESULT" {
			t.Errorf("errorCode = %q; want SCHEDULE_FUNCTION_INVALID_RESULT (the result contract has no principal channel); body=%s", code, body)
		}
	})

	// 4c: with a WELL-FORMED (timing-only) schedule.function result, the task
	// still arms to the true origin (alice) and the fire attributes to her —
	// the callout decides WHEN the task fires, the platform decides WHO it is
	// attributed to (arm.go's armViaFunction: ArmedBy = ResolveOrigin(ctx),
	// never the Function result).
	t.Run("ValidFunctionResultArmsTrueOrigin", func(t *testing.T) {
		h := newCallbackHarness(t)
		const model = "attr-sched-spoof-fn-valid"

		h.RegisterFunction("calcTiming", func(rc *reqCtx) (string, map[string]any, error) {
			return "Schedule", map[string]any{"fireAfterMs": int64(300)}, nil
		})

		wf := scheduleFunctionWorkflowJSON("attr-sched-spoof-fn-valid-wf", validScheduleFunctionJSON("calcTiming"))
		h.SetupModelWithWorkflow(t, model, wf)

		alice := h.mintUserToken(t, "alice", "ROLE_USER")
		xID, status, body := h.createEntityAs(t, alice, model, 1, `{"name":"x","amount":1,"status":"new"}`)
		if status != http.StatusOK {
			t.Fatalf("create as alice: %d %s", status, body)
		}

		// The armed row's principal is the platform-resolved origin (alice),
		// sourced from ctx, not from the compute-node-controlled result.
		var armedID, armedKind string
		if err := dbPool.QueryRow(context.Background(),
			`SELECT armed_by_id, armed_by_kind FROM scheduled_tasks WHERE entity_id=$1`, xID,
		).Scan(&armedID, &armedKind); err != nil {
			t.Fatalf("inspect scheduled_task for %s: %v", xID, err)
		}
		if armedID != "alice" || armedKind != "user" {
			t.Errorf("armed principal = {%q,%q}; want {alice,user} (platform origin, not the callout)", armedID, armedKind)
		}

		anchor := awaitFiredAnchor(t, h, xID, "Closed")
		assertAttribution(t, anchor, "fired anchor (function-armed, true origin)", "alice", "user", firedSchedExecKind, firedSchedExecID)
	})
}
