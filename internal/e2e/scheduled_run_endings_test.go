package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// scheduled_run_endings_test.go — every ending of a scheduled run through the
// full stack on PostgreSQL (spec §4), and the text a failed attempt records
// (§5.8). Each test builds a stack with a scheduler on a database of its own.

// firstAttempt waits for the first recorded attempt of the task and returns
// the row as it was then. The retry delay is 1s and the poll 20ms, so the
// first row with attempts >= 1 has attempts 1.
func firstAttempt(t *testing.T, s *schedDB, entityID, transition string) taskRow {
	t.Helper()
	return s.awaitTask(t, entityID, transition, scheduledFireTimeout, "a recorded attempt",
		func(r taskRow, ok bool) bool { return ok && r.Attempts >= 1 })
}

// awaitFailed waits for the task to be FAILED and asserts that the FAILED
// row carries its failedTime.
func awaitFailed(t *testing.T, s *schedDB, entityID, transition string) taskRow {
	t.Helper()
	r := s.awaitTask(t, entityID, transition, scheduledFireTimeout, "FAILED",
		func(r taskRow, ok bool) bool { return ok && r.Status == "FAILED" })
	if n := s.count(t, `SELECT count(*) FROM scheduled_tasks
		 WHERE tenant_id = $1 AND id = $2 AND status = 'FAILED' AND failed_time IS NOT NULL`, harnessTenant, r.ID); n != 1 {
		t.Errorf("FAILED task %s has no failedTime", r.ID)
	}
	return r
}

// awaitSchedulerLiveFor returns once a sentinel task, armed now with delay d
// on another model of h's stack, has fired: proof that the scheduler kept
// scanning and claiming for at least d after the call. It stands in for a
// fixed sleep when a test asserts that something did not happen in that time.
func awaitSchedulerLiveFor(t *testing.T, h *callbackHarness, d time.Duration) {
	t.Helper()
	model := uniq("sr-sentinel")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-sentinel-wf", d.Milliseconds(), 0))
	id := createOpen(t, h, model, workflowSampleModel)
	awaitCallbackEntityState(t, h, id, "Done", d+scheduledFireTimeout)
}

// receivedFor counts the callouts in recs that carry entityID.
func receivedFor(recs []receivedCallout, entityID string) int {
	n := 0
	for _, r := range recs {
		if r.EntityID == entityID {
			n++
		}
	}
	return n
}

// createOpen creates one entity of model and asserts 200.
func createOpen(t *testing.T, h *callbackHarness, model, payload string) string {
	t.Helper()
	id, status, body := h.CreateEntity(t, model, 1, payload)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	return id
}

// requireState asserts the entity's state.
func requireState(t *testing.T, h *callbackHarness, id, want string) {
	t.Helper()
	if st, _ := h.GetEntityState(t, id); st != want {
		t.Errorf("state = %q; want %q", st, want)
	}
}

// TestSchedRun_NoComputeNodeThenFires: an unsafe processor whose tag has no
// cnode. MarkUnsafe writes a mark, the dispatch returns the NotHandedOff
// proof, and RecordAttempt{ClearOwnMark} removes the mark in the same write:
// WAITING, attempts 1, no mark, the NO_COMPUTE_MEMBER_FOR_TAG text (§5.5,
// §5.6, §5.8). A cnode attached later receives the retry and the task fires.
func TestSchedRun_NoComputeNodeThenFires(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-nocn"), uniq("sr-nocn-tag")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-nocn-wf", 100, 0, sProc("p", "SYNC", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.FailureReason != "" || r.Marked || r.ClaimToken != "" {
		t.Fatalf("after the first attempt: %+v; want WAITING, attempts 1, no mark, no claim", r)
	}
	if !strings.HasPrefix(r.LastError, "NO_COMPUTE_MEMBER_FOR_TAG: ") {
		t.Errorf("lastError = %q; want the NO_COMPUTE_MEMBER_FOR_TAG text", r.LastError)
	}

	cn := h.AttachCnode(t, cnodeSpec{name: "late", tags: []string{tag}})
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the cnode received %d callouts; want 1", n)
	}
	if len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")) != 1 {
		t.Error("want exactly one SCHEDULED_TRANSITION_FIRE")
	}
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the fired task is still stored")
	}
}

// TestSchedRun_Declined: a criterion answers false. The task is removed with
// TRANSITION_NOT_MATCH_CRITERION; the entity stays (§4). Not a failure.
func TestSchedRun_Declined(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-decl"), uniq("sr-decl-tag")
	h.AttachCnode(t, cnodeSpec{name: "crit", tags: []string{tag}, script: scriptAlways(answerMatches(false))})
	h.SetupModelWithWorkflow(t, model, schedDoc("sr-decl-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name": "c", "config": map[string]any{"calculationNodesTags": tag, "attachEntity": true}}},
		}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitCallbackSMEventType(t, h, id, "TRANSITION_NOT_MATCH_CRITERION", "Open", scheduledFireTimeout)
	s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
	requireState(t, h, id, "Open")
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("a declined task recorded a failure")
	}
}

// TestSchedRun_SelfLoopReArmsNewLife: Open -[Tick]-> Open fires and re-arms
// the same id as a new life: a new arm token, attempts 0 (§5.2). Tick carries
// a criterion that always holds: workflow import refuses an unguarded
// automated self-loop, and counts a scheduled transition as automated.
func TestSchedRun_SelfLoopReArmsNewLife(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model := uniq("sr-loop")
	h.SetupModelWithWorkflow(t, model, schedDoc("sr-loop-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Tick", "next": "Open", "manual": false, "schedule": map[string]any{"delayMs": 300},
			"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "draft"},
		}}},
	}))
	id := createOpen(t, h, model, workflowSampleModel)
	t.Cleanup(func() { h.DoAuth(t, http.MethodDelete, "/api/entity/"+id, "", "").Body.Close() })

	first, ok := s.task(t, id, "Tick")
	if !ok {
		t.Fatal("no task after the create")
	}
	awaitDBCondition(t, scheduledFireTimeout, "two fires", func() bool {
		return len(smEventsOfType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FIRE")) >= 2
	})
	later := s.awaitTask(t, id, "Tick", scheduledFireTimeout, "a new life",
		func(r taskRow, ok bool) bool { return ok && r.ArmToken != first.ArmToken })
	if later.ID != first.ID || later.Attempts != 0 || later.LostOwners != 0 || later.Marked || later.PartialCommit {
		t.Errorf("re-armed task = %+v (first %+v); want the same id as a fresh life", later, first)
	}
}

// TestSchedRun_CriterionErrorRetried: a criterion callout fails; criteria are
// repeat-safe: WAITING, attempts 1, the cnode's own message (§5.8, MemberFailed).
func TestSchedRun_CriterionErrorRetried(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-crit"), uniq("sr-crit-tag")
	h.AttachCnode(t, cnodeSpec{name: "crit", tags: []string{tag}, script: scriptAlways(answerFail("crit boom"))})
	h.SetupModelWithWorkflow(t, model, schedDoc("sr-crit-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Done", "manual": false, "schedule": map[string]any{"delayMs": 100},
			"criterion": map[string]any{"type": "function", "function": map[string]any{
				"name": "c", "config": map[string]any{"calculationNodesTags": tag, "attachEntity": true}}},
		}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.FailureReason != "" || r.LastError != "crit boom" {
		t.Errorf("after the failed criterion: %+v; want WAITING, attempts 1, lastError \"crit boom\"", r)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_IdempotentFailureRetried: an idempotent processor fails: a safe
// failure, WAITING attempts 1, no mark, and the processor is sent again.
func TestSchedRun_IdempotentFailureRetried(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-idem"), uniq("sr-idem-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("proc boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-idem-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.Attempts != 1 || r.Marked || r.LastError != "proc boom" {
		t.Errorf("after the failed idempotent processor: %+v; want WAITING, attempts 1, no mark, \"proc boom\"", r)
	}
	awaitDBCondition(t, scheduledFireTimeout, "a second send", func() bool { return len(cn.Received()) >= 2 })
}

// TestSchedRun_LateAfterFailedAttemptsFails: timeoutMs 1500, an idempotent
// processor that always fails: FAILED EXPIRED_AFTER_FAILED_ATTEMPTS (§5.1
// step 4, §5.6), with its audit event; the entity stays.
func TestSchedRun_LateAfterFailedAttemptsFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-late"), uniq("sr-late-tag")
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("late boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-late-wf", 100, 1500, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := s.awaitTask(t, id, "Fire", 20*time.Second, "FAILED", func(r taskRow, ok bool) bool { return ok && r.Status == "FAILED" })
	if r.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || r.Attempts < 1 || r.ClaimToken != "" {
		t.Errorf("failed task = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS, attempts >= 1, no claim", r)
	}
	if data := failEvent(t, h, id); data["reason"] != "EXPIRED_AFTER_FAILED_ATTEMPTS" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_UnsafeFailureFails: an unsafe processor reached its cnode and
// failed: FAILED UNSAFE_WORK_NOT_COMPLETED with SCHEDULED_TRANSITION_FAIL
// {transition, sourceState, reason, attempts, lostOwners}; never sent again.
func TestSchedRun_UnsafeFailureFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-unsafe"), uniq("sr-unsafe-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("unsafe boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-unsafe-wf", 100, 0, sProc("p", "SYNC", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" || r.LastError != "unsafe boom" || r.ClaimToken != "" {
		t.Errorf("failed task = %+v; want UNSAFE_WORK_NOT_COMPLETED, \"unsafe boom\", no claim", r)
	}
	data := failEvent(t, h, id)
	for k, want := range map[string]any{"transition": "Fire", "sourceState": "Open", "reason": "UNSAFE_WORK_NOT_COMPLETED",
		"attempts": float64(0), "lostOwners": float64(0)} {
		if data[k] != want {
			t.Errorf("SCHEDULED_TRANSITION_FAIL data[%q] = %v; want %v (data %v)", k, data[k], want, data)
		}
	}
	awaitSchedulerLiveFor(t, h, 3*fixtureutil.TunedRetryDelay)
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the unsafe processor was sent %d times; want 1", n)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_LaterStepFailsAfterUnsafeHandOff: the unsafe first processor
// succeeds, the idempotent second fails: unsafe work reached a cnode and the
// run did not commit — FAILED UNSAFE_WORK_NOT_COMPLETED; neither is re-sent.
func TestSchedRun_LaterStepFailsAfterUnsafeHandOff(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sr-later"), uniq("sr-later-a"), uniq("sr-later-b")
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptAlways(answerFail("b boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-later-wf", 100, 0,
		sProc("pa", "SYNC", tagA, false), sProc("pb", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	if r := awaitFailed(t, s, id, "Fire"); r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", r.FailureReason)
	}
	awaitSchedulerLiveFor(t, h, 3*fixtureutil.TunedRetryDelay)
	if na, nb := len(a.Received()), len(b.Received()); na != 1 || nb != 1 {
		t.Errorf("sent: a %d, b %d; want 1 and 1", na, nb)
	}
}

// TestSchedRun_FailureAfterUnsafeDispatchSameStep: the unsafe processor's
// dispatch succeeds but its answer carries a field the locked model does not
// declare, so the step fails after the dispatch (engine_processors.go:239).
// No NotHandedOff proof: FAILED UNSAFE_WORK_NOT_COMPLETED.
func TestSchedRun_FailureAfterUnsafeDispatchSameStep(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-same"), uniq("sr-same-tag")
	h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerData(map[string]any{
		"name": "Test Order", "amount": 100, "status": "draft", "undeclared": "x"}))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-same-wf", 100, 0, sProc("p", "SYNC", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	if r := awaitFailed(t, s, id, "Fire"); r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
		t.Errorf("failureReason = %q; want UNSAFE_WORK_NOT_COMPLETED", r.FailureReason)
	}
	requireState(t, h, id, "Open")
}

// TestSchedRun_UnsafeAsyncNewTxFailureStillCompletes: an unsafe ASYNC_NEW_TX
// processor fails; its failure does not fail the transition, the run commits,
// and the task is completed (§5.5 "If the run commits, the task is completed").
func TestSchedRun_UnsafeAsyncNewTxFailureStillCompletes(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sr-async"), uniq("sr-async-tag")
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("async boom"))})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-async-wf", 100, 0, sProc("p", "ASYNC_NEW_TX", tag, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the completed task is still stored")
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("a committed run recorded a failure")
	}
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the ASYNC_NEW_TX processor was sent %d times; want 1", n)
	}
}

// TestSchedRun_FireTimeCancel: the two CANCEL endings a run decides itself
// (§4): the selected workflow no longer schedules the transition, and the
// stored entity carries no transaction id to guard the fire.
func TestSchedRun_FireTimeCancel(t *testing.T) {
	t.Run("NotScheduledInSelectedWorkflow", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model := uniq("sr-cancel")
		doc := func(scheduledIn string) string {
			wf := func(name, flavor string, scheduled bool) map[string]any {
				fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
				if scheduled {
					fire["schedule"] = map[string]any{"delayMs": 2000}
				}
				return map[string]any{"version": "1.5", "name": name, "initialState": "Open", "active": true,
					"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": flavor},
					"states":    map[string]any{"Open": map[string]any{"transitions": []any{fire}}, "Done": map[string]any{}}}
			}
			return string(mustJSON(t, map[string]any{"importMode": "REPLACE", "workflows": []any{
				wf("sr-cancel-w1", "draft", scheduledIn == "w1"), wf("sr-cancel-w2", "final", scheduledIn == "w2")}}))
		}
		h.SetupModelWithWorkflow(t, model, doc("w1"))
		id := createOpen(t, h, model, workflowSampleModel)
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), doc("w2"), "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("re-import: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); !ok {
			t.Fatal("the re-import removed the task; it is scheduled in W2")
		}
		awaitCallbackSMEventType(t, h, id, "SCHEDULED_TRANSITION_CANCEL", "Open", scheduledFireTimeout)
		s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
		requireState(t, h, id, "Open")
	})

	t.Run("NoTransactionID", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model := uniq("sr-notx")
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-notx-wf", 1500, 0))
		id := createOpen(t, h, model, workflowSampleModel)
		// Legacy data: the API never writes an entity without a transaction id.
		if _, err := s.pool.Exec(context.Background(),
			`UPDATE entities SET doc = doc #- '{_meta,transaction_id}' WHERE tenant_id = $1 AND entity_id = $2`,
			harnessTenant, id); err != nil {
			t.Fatalf("strip the transaction id: %v", err)
		}
		awaitCallbackSMEventType(t, h, id, "SCHEDULED_TRANSITION_CANCEL", "Open", scheduledFireTimeout)
		s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
		requireState(t, h, id, "Open")
	})
}

// TestSchedRun_LastErrorText: what a failed attempt records (§5.8). Every
// case uses an idempotent processor, so each failure is a safe one and the
// row is WAITING with attempts 1. On a scheduled run an Operational AppError
// reaches the scheduler inside a CalloutFailure (DISPATCH_TIMEOUT,
// NO_COMPUTE_MEMBER_FOR_TAG), whose Message is the AppError's "CODE: detail".
func TestSchedRun_LastErrorText(t *testing.T) {
	ticketOnly := regexp.MustCompile(`^internal error \[ticket: [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\]$`)

	run := func(t *testing.T, script cnodeScript, attach bool, check func(t *testing.T, r taskRow)) {
		t.Helper()
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-err"), uniq("sr-err-tag")
		if attach {
			h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: script})
		}
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-err-wf", 100, 0, sProc("p", "SYNC", tag, true)))
		id := createOpen(t, h, model, workflowSampleModel)
		check(t, firstAttempt(t, s, id, "Fire"))
	}

	t.Run("MemberFailedMessage", func(t *testing.T) {
		run(t, scriptAlways(answerFail("the cnode's own words")), true, func(t *testing.T, r taskRow) {
			if r.LastError != "the cnode's own words" {
				t.Errorf("lastError = %q; want the cnode's message", r.LastError)
			}
		})
	})
	t.Run("CalloutTimeout", func(t *testing.T) {
		// sProc sets a 60s answer limit; this case lowers it to 300ms so the
		// silent cnode's one try times out quickly.
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-to"), uniq("sr-to-tag")
		h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(neverAnswer())})
		p := sProc("p", "SYNC", tag, true)
		p["config"].(map[string]any)["responseTimeoutMs"] = 300
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-to-wf", 100, 0, p))
		id := createOpen(t, h, model, workflowSampleModel)
		r := firstAttempt(t, s, id, "Fire")
		if !strings.HasPrefix(r.LastError, "DISPATCH_TIMEOUT: ") {
			t.Errorf("lastError = %q; want DISPATCH_TIMEOUT: <detail>", r.LastError)
		}
	})
	t.Run("NoComputeMember", func(t *testing.T) {
		run(t, nil, false, func(t *testing.T, r taskRow) {
			if !strings.HasPrefix(r.LastError, "NO_COMPUTE_MEMBER_FOR_TAG: ") {
				t.Errorf("lastError = %q; want NO_COMPUTE_MEMBER_FOR_TAG: <detail>", r.LastError)
			}
		})
	})
	t.Run("NonSentinelStoreErrorIsTicketOnly", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-tk"), uniq("sr-tk-tag")
		gotWork, release := make(chan struct{}, 1), make(chan struct{})
		rel := closeOnce(release)
		t.Cleanup(rel)
		var calls atomic.Int32
		h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(ctx context.Context, _ receivedCallout, _ *reqCtx) cnodeReply {
			if calls.Add(1) > 1 {
				return neverAnswer()
			}
			gotWork <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return neverAnswer()
			}
			return answerOK()
		}})
		h.SetupModelWithWorkflow(t, model, fireOpenToDone("sr-tk-wf", 100, 0, sProc("p", "SYNC", tag, true)))
		id := createOpen(t, h, model, workflowSampleModel)

		<-gotWork
		// The run's transaction is the only one open in this database while
		// its processor is held.
		var n int
		if err := s.pool.QueryRow(context.Background(), `
			SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
			 WHERE datname = current_database() AND state = 'idle in transaction' AND pid <> pg_backend_pid()`,
		).Scan(&n); err != nil || n != 1 {
			t.Fatalf("terminated %d backends (err %v); want exactly the run's", n, err)
		}
		rel()
		r := firstAttempt(t, s, id, "Fire")
		if !ticketOnly.MatchString(r.LastError) {
			t.Errorf("lastError = %q; want exactly \"internal error [ticket: <uuid>]\"", r.LastError)
		}
	})
}

// TestSchedRun_FailedTaskUnderEntityWrites: the two ways an entity write ends
// a FAILED task (§4), and the removal of a task its workflow no longer
// schedules at the next write (§7).
func TestSchedRun_FailedTaskUnderEntityWrites(t *testing.T) {
	failedStack := func(t *testing.T) (*callbackHarness, *schedDB, *scriptedCnode, string, taskRow) {
		t.Helper()
		h, s := newSchedulerHarness(t, nil)
		model, tag := uniq("sr-fw"), uniq("sr-fw-tag")
		cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: scriptAlways(answerFail("fw boom"))})
		h.SetupModelWithWorkflow(t, model, schedDoc("sr-fw-wf", map[string]any{
			"Open": map[string]any{"transitions": []any{
				map[string]any{"name": "Fire", "next": "Done", "manual": false,
					"schedule":   map[string]any{"delayMs": 1500},
					"processors": []any{sProc("p", "SYNC", tag, false)}},
				map[string]any{"name": "Leave", "next": "Left", "manual": true},
			}},
			"Done": map[string]any{},
			"Left": map[string]any{},
		}))
		id := createOpen(t, h, model, workflowSampleModel)
		r := awaitFailed(t, s, id, "Fire")
		if r.FailureReason != "UNSAFE_WORK_NOT_COMPLETED" {
			t.Fatalf("failureReason = %q", r.FailureReason)
		}
		return h, s, cn, id, r
	}

	t.Run("ReArmedByUpdateInTheState", func(t *testing.T) {
		h, s, cn, id, failed := failedStack(t)
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":5,"status":"draft"}`, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("update: %d %s", resp.StatusCode, body)
		}
		r, ok := s.task(t, id, "Fire")
		if !ok || r.Status != "WAITING" || r.ArmToken == failed.ArmToken || r.Attempts != 0 || r.LostOwners != 0 ||
			r.FailureReason != "" || r.LastError != "" || r.Marked || r.PartialCommit || r.ClaimToken != "" {
			t.Fatalf("after the update: %+v present=%t; want a new life with no mark", r, ok)
		}
		// The new life runs and sends its processor: no old-life mark stops it.
		awaitDBCondition(t, scheduledFireTimeout, "the new life's send", func() bool { return len(cn.Received()) >= 2 })
	})

	t.Run("CancelledWhenEntityLeaves", func(t *testing.T) {
		h, s, _, id, _ := failedStack(t)
		resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/Leave", workflowSampleModel, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("Leave: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); ok {
			t.Error("the FAILED task outlived the state exit")
		}
		if !hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_CANCEL", "Open") {
			t.Error("no SCHEDULED_TRANSITION_CANCEL for the FAILED task")
		}
		requireState(t, h, id, "Left")
	})

	t.Run("NoLongerScheduledRemovedAtNextWrite", func(t *testing.T) {
		h, s := newSchedulerHarness(t, nil)
		model := uniq("sr-nw")
		doc := func(scheduledIn string) string {
			wf := func(name, flavor string, scheduled bool) map[string]any {
				fire := map[string]any{"name": "Fire", "next": "Done", "manual": !scheduled}
				if scheduled {
					fire["schedule"] = map[string]any{"delayMs": 3600000}
				}
				return map[string]any{"version": "1.5", "name": name, "initialState": "Open", "active": true,
					"criterion": map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": flavor},
					"states":    map[string]any{"Open": map[string]any{"transitions": []any{fire}}, "Done": map[string]any{}}}
			}
			return string(mustJSON(t, map[string]any{"importMode": "REPLACE", "workflows": []any{
				wf("sr-nw-w1", "draft", scheduledIn == "w1"), wf("sr-nw-w2", "final", scheduledIn == "w2")}}))
		}
		h.SetupModelWithWorkflow(t, model, doc("w1"))
		id := createOpen(t, h, model, workflowSampleModel)
		resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), doc("w2"), "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("re-import: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); !ok {
			t.Fatal("the re-import removed the task; it is scheduled in W2")
		}
		resp = h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id, `{"name":"Test Order","amount":6,"status":"draft"}`, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("update: %d %s", resp.StatusCode, body)
		}
		if _, ok := s.task(t, id, "Fire"); ok {
			t.Error("the write kept a task its workflow no longer schedules")
		}
		if !hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_CANCEL", "Open") {
			t.Error("no SCHEDULED_TRANSITION_CANCEL for the removed task")
		}
	})
}
