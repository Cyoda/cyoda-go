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
// docs/superpowers/specs/2026-10-01-624-m2m-only-access-design.md §7.4).
//
// A scheduled transition is armed by whoever's write ATTRIBUTES to at arm time
// (spi.AttributionFor -> ScheduledTask.ArmedBy, arm.go) — not the transaction's
// origin (spi.ResolveOrigin): the two differ for a user-kind caller writing
// inside a transaction begun by someone else, and for an on-behalf-of
// principal, which arms with the OBO user and never inherits a transaction's
// origin. The task is fired later by the platform scheduler. The claimed task
// carries ArmedBy, and FireScheduledTransition stamps the fired anchor's
// attributed principal from it and its EXECUTOR as the system principal
// {id:"system", kind:"system"} — never the literal string "scheduler". These
// tests drive real timers (short DelayMs / fireAfterMs) through the full
// HTTP+gRPC stack on a stack with a live scheduler and a database of its own
// (newSchedulerCallbackHarness), wait for the scheduler to fire, and assert the
// recorded {attributed, executor} pair on GET /entity/{id}/changes.
//
// Reused helpers: mintUserToken / createEntityAs / getChanges /
// findChangeByType / assertAttribution (attribution_test.go),
// newSchedulerCallbackHarness / schedDB (scheduler_harness_test.go),
// newCallbackHarness (callback_harness_test.go), awaitCallbackEntityState /
// RegisterFunction / scheduleFunctionWorkflowJSON (scheduled_function_test.go).

// firedSchedExecKind/firedSchedExecID are the executor identity every
// scheduled fire records (common.SystemPrincipal).
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
	h, _ := newSchedulerCallbackHarness(t, nil)

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

// --- Scenario 1b: an OBO request arms a timer directly ------------------------

// TestAttribution_ScheduledOBOArmed: alice's on-behalf-of token DIRECTLY
// creates an entity (not joined to anyone else's transaction) whose init
// cascade lands it in a state with a short-DelayMs scheduled transition.
// spi.AttributionFor never lets an OBO write inherit a transaction's origin,
// so the timer arms with alice, never the OBO client. The fired anchor must
// attribute to alice, executed by the system principal — the "directly" half
// of spec §13's "scheduled fire armed by an OBO request, directly and inside
// its own joined transaction" row; TestAttribution_D3_OBOKeepsOwnUser covers
// the joined half (an OBO callback joining its own transaction).
func TestAttribution_ScheduledOBOArmed(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)

	const model = "attr-sched-obo-armed"
	wf := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-sched-obo-armed-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 300}}]},
				"Closed": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, model, wf)

	alice := oboTokenOn(t, h.baseURL, h.token(t), "alice")
	xID, status, body := h.createEntityAs(t, alice, model, 1, `{"name":"x","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create as alice (OBO): %d %s", status, body)
	}

	anchor := awaitFiredAnchor(t, h, xID, "Closed")
	assertAttribution(t, anchor, "scheduled fire anchor (OBO-armed)", "alice", "user", firedSchedExecKind, firedSchedExecID)
	assertNoSchedulerString(t, h, xID)
}

// --- Scenario 1c: a compute write-back in alice's transaction arms a timer ----

// TestAttribution_ScheduledArmedByWriteBack: alice creates the primary; its
// SYNC processor writes a secondary back through the compute client's own
// (service-kind, no OBO Executor) token, joined to alice's transaction. The
// secondary's init cascade lands it in a state with a short-DelayMs scheduled
// transition, armed inside that same joined transaction. ArmedBy must be
// alice — the transaction's origin spi.AttributionFor inherits for a plain
// service executor — never the compute client that staged the write. Spec
// §13: "scheduled fire armed by a compute write-back in alice's transaction:
// ArmedBy alice".
func TestAttribution_ScheduledArmedByWriteBack(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)

	const primary = "attr-sched-wb-primary"
	const secondary = "attr-sched-wb-secondary"
	secondaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-sched-wb-y-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 300}}]},
				"Closed": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWF)

	compute := h.computeBearer(t)
	yIDs := make(chan string, 1)
	h.RegisterProc("attr-sched-wb-proc", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntityAs(compute, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("write-back create Y: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("write-back create Y status=%d body=%s", res.StatusCode, res.Body)
		}
		yIDs <- res.EntityID
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-sched-wb", "attr-sched-wb-proc", "SYNC", ""))

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice: %d %s", status, body)
	}

	var yID string
	select {
	case yID = <-yIDs:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: write-back processor did not create Y")
	}

	// The write-back CREATE itself attributes to alice, executed by the
	// compute client — a different principal from alice.
	created := findChangeByType(h.getChanges(t, yID), "CREATE")
	assertAttribution(t, created, "Y write-back create", "alice", "user", "service", "")
	if ex, _ := created["executedBy"].(map[string]any); ex != nil {
		if id, _ := ex["id"].(string); id == "alice" {
			t.Errorf("Y write-back create: executedBy.id = %q; want the compute client, not alice", id)
		}
	}

	// The fired anchor must carry alice (ArmedBy = the transaction's origin),
	// never the compute client that staged the write-back.
	anchor := awaitFiredAnchor(t, h, yID, "Closed")
	assertAttribution(t, anchor, "Y fired anchor (write-back armed, ArmedBy alice)", "alice", "user", firedSchedExecKind, firedSchedExecID)
	assertNoSchedulerString(t, h, yID)
}

// --- Scenario 2: a fire that arms a further hop stays user-rooted -------------

// TestAttribution_ScheduledChain: alice arms hop 1 (Open -[AutoClose]-> Mid);
// firing hop 1 arms hop 2 (Mid -[AutoNext]-> Done) inside the fire's cascade,
// seeded with the SAME chain origin (fire_scheduled.go's WithAmbientOrigin);
// firing hop 2 must still attribute to alice. Every version in the history —
// the create and both fired anchors — is alice-rooted; the executor of each
// fire is the system principal.
func TestAttribution_ScheduledChain(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)

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

// TestAttribution_ScheduledAnchorStamped: regression for arming falling back
// to the CHAIN origin instead of the write's own spi.AttributionFor result
// (the behavior spi.ResolveOrigin-based arming had, and the exact thing this
// task's switch to spi.AttributionFor eliminates — see arm.go). The entity Y
// is CREATED by bob (a processor callback presenting bob's own plain user
// token — NOT on-behalf-of — joined into alice's transaction: a user-kind
// executor records itself, so Y's create version writer is bob), and the
// AutoClose timer armed on Y in that SAME write must carry bob too — never
// alice, the transaction's origin / the chain that launched the cascade. A
// regression that reintroduced spi.ResolveOrigin at the arm site would make
// this fail by stamping alice instead. When the timer fires, the anchor must
// carry bob (ArmedBy), executed by the system principal.
func TestAttribution_ScheduledAnchorStamped(t *testing.T) {
	h, _ := newSchedulerCallbackHarness(t, nil)

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
		// Joined (rc.token present) presenting bob's own plain user token: a
		// user-kind executor records itself (spi.AttributionFor never lets it
		// inherit the transaction's origin), so both Y's create version AND the
		// AutoClose timer armed in the same write carry bob, not alice.
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

	// Y's CREATE version writer is bob (a user-kind executor records itself).
	created := findChangeByType(h.getChanges(t, yID), "CREATE")
	assertAttribution(t, created, "Y create (joined, user executor bob)", "bob", "user", "user", "bob")

	// The fired anchor must carry bob (ArmedBy, from the SAME write as the
	// create above), NEVER alice — the transaction's origin / the chain that
	// launched the cascade, and what spi.ResolveOrigin-based arming would have
	// stamped here before this task's switch to spi.AttributionFor.
	anchor := awaitFiredAnchor(t, h, yID, "Closed")
	assertAttribution(t, anchor, "Y fired anchor (stamped from ArmedBy)", "bob", "user", firedSchedExecKind, firedSchedExecID)
	if u, _ := anchor["user"].(string); u == "alice" {
		t.Errorf("chain-origin regression: fired anchor attributes to alice (the transaction's origin), not the arming write's own attributed user bob")
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
		h, _ := newSchedulerCallbackHarness(t, nil)
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
	// attributed to (arm.go's armViaFunction: ArmedBy = AttributionFor(ctx)'s
	// attributed return, never the Function result).
	t.Run("ValidFunctionResultArmsTrueOrigin", func(t *testing.T) {
		h, s := newSchedulerCallbackHarness(t, nil)
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
		if err := s.pool.QueryRow(context.Background(),
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
