package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// scheduled_run_segments_test.go — runs that commit in segments
// (COMMIT_BEFORE_DISPATCH), the PartialCommit rule, a run cut by the
// scheduler, and the callback anti-pattern (a joined callback that writes the
// entity being fired), through the full stack on PostgreSQL.

// entityTxID returns the transaction id the stored entity carries.
func entityTxID(t *testing.T, h *callbackHarness, id string) string {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, "/api/entity/"+id, "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get entity %s: %d %s", id, resp.StatusCode, body)
	}
	var m struct {
		Meta struct {
			TransactionID string `json:"transactionId"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil || m.Meta.TransactionID == "" {
		t.Fatalf("entity %s carries no transactionId (err %v): %s", id, err, body)
	}
	return m.Meta.TransactionID
}

// TestSchedRun_CBDOnFiredTransitionRetriedFromTXPre: a COMMIT_BEFORE_DISPATCH
// processor of the fired transition itself commits TX_pre with the entity
// still in the source state — no PartialCommit (§5.4). A later idempotent
// processor fails once: a safe failure, retried from that committed state,
// and the retry fires.
func TestSchedRun_CBDOnFiredTransitionRetriedFromTXPre(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-txpre"), uniq("sg-txpre-a"), uniq("sg-txpre-b")
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptSequence(answerFail("once"), answerOK())})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-txpre-wf", 100, 0,
		sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)
	createdTx := entityTxID(t, h, id)

	r := firstAttempt(t, s, id, "Fire")
	if r.Status != "WAITING" || r.PartialCommit || r.FailureReason != "" || r.LastError != "once" {
		t.Fatalf("after the first attempt: %+v; want WAITING, no PartialCommit (the fired transition's own segment), \"once\"", r)
	}
	if tx := entityTxID(t, h, id); tx == createdTx {
		t.Errorf("entity still carries the create's transaction %s; TX_pre did not commit", tx)
	}
	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if na, nb := len(a.Received()), len(b.Received()); na != 2 || nb != 2 {
		t.Errorf("sent: p1 %d, p2 %d; want 2 and 2 (the retry started again from TX_pre)", na, nb)
	}
	if hasSMEventType(schedEvents(t, h, id), "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("an all-idempotent run with a fired-transition segment recorded a failure")
	}
	if _, ok := s.task(t, id, "Fire"); ok {
		t.Error("the fired task is still stored")
	}
}

// TestSchedRun_CBDInCascadeStepThenFailureFails: the fired transition lands in
// Mid; Mid's automated step commits the entity in Mid (COMMIT_BEFORE_DISPATCH),
// then its next processor fails. The run stopped after committing the entity
// into another state: FAILED STOPPED_AFTER_PARTIAL_COMMIT, entity in Mid.
func TestSchedRun_CBDInCascadeStepThenFailureFails(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-step"), uniq("sg-step-a"), uniq("sg-step-b")
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptAlways(answerFail("step boom"))})
	h.SetupModelWithWorkflow(t, model, schedDoc("sg-step-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 100}}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Step", "next": "Done", "manual": false,
			"processors": []any{sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)}}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleModel)

	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "STOPPED_AFTER_PARTIAL_COMMIT" || !r.PartialCommit || r.ClaimToken != "" {
		t.Errorf("failed task = %+v; want STOPPED_AFTER_PARTIAL_COMMIT with PartialCommit, no claim", r)
	}
	requireState(t, h, id, "Mid")
	if data := failEvent(t, h, id); data["reason"] != "STOPPED_AFTER_PARTIAL_COMMIT" {
		t.Errorf("SCHEDULED_TRANSITION_FAIL data = %v", data)
	}
	awaitSchedulerLiveFor(t, h, 3*fixtureutil.TunedRetryDelay)
	if n := len(b.Received()); n != 1 {
		t.Errorf("p2 sent %d times; a FAILED task is never run again", n)
	}
}

// TestSchedRun_CascadeLoopBackWithCBDSetsPartialCommit: Fire lands in Mid,
// Mid's step writes looped=true and returns to Open, and Open's Onward step
// (criterion looped == true) commits the entity in Open — the source state —
// before its next processor fails. That segment is a cascade step, not the
// fired transition, so it sets PartialCommit (§5.4): FAILED
// STOPPED_AFTER_PARTIAL_COMMIT, never retried as if nothing had committed.
// Back carries a criterion that always holds: workflow import refuses an
// unguarded automated cycle, and counts the scheduled Fire as automated.
func TestSchedRun_CascadeLoopBackWithCBDSetsPartialCommit(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB, tagC := uniq("sg-loop"), uniq("sg-loop-a"), uniq("sg-loop-b"), uniq("sg-loop-c")
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptAlways(answerFail("onward boom"))})
	h.AttachCnode(t, cnodeSpec{name: "c", tags: []string{tagC}, script: scriptAlways(answerData(map[string]any{
		"name": "Test Order", "amount": 100, "status": "draft", "looped": true}))})
	h.setupModelSampleWithWorkflow(t, model, workflowSampleWith(`"looped":false`), schedDoc("sg-loop-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{
			map[string]any{"name": "Fire", "next": "Mid", "manual": false, "schedule": map[string]any{"delayMs": 100}},
			map[string]any{"name": "Onward", "next": "Done", "manual": false,
				"criterion":  map[string]any{"type": "simple", "jsonPath": "$.looped", "operatorType": "EQUALS", "value": true},
				"processors": []any{sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)}},
		}},
		"Mid": map[string]any{"transitions": []any{map[string]any{
			"name": "Back", "next": "Open", "manual": false,
			"criterion":  map[string]any{"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "draft"},
			"processors": []any{sProc("p0", "SYNC", tagC, true)}}}},
		"Done": map[string]any{},
	}))
	id := createOpen(t, h, model, workflowSampleWith(`"looped":false`))
	requireState(t, h, id, "Open")

	r := awaitFailed(t, s, id, "Fire")
	if r.FailureReason != "STOPPED_AFTER_PARTIAL_COMMIT" || !r.PartialCommit {
		t.Errorf("failed task = %+v; want STOPPED_AFTER_PARTIAL_COMMIT with PartialCommit", r)
	}
	requireState(t, h, id, "Open")
	if looped, _ := h.GetEntityData(t, id)["looped"].(bool); !looped {
		t.Error("the committed segment is not visible: looped is still false")
	}
	awaitSchedulerLiveFor(t, h, 3*fixtureutil.TunedRetryDelay)
	if n := len(b.Received()); n != 1 {
		t.Errorf("p2 sent %d times; want 1", n)
	}
}

// TestSchedRun_CancelAfterTXPreStopsAtNextStep: the run's TX_pre is committed
// and its COMMIT_BEFORE_DISPATCH processor (idempotent) is in flight when the
// scheduler shuts down. After the drain, step 3 cuts it; the next processor is
// never dispatched, nothing unsafe reached a cnode, so the attempt is not
// counted: WAITING, attempts 0, the fixed CANCELLED text (§5.3, §5.6, §5.8).
func TestSchedRun_CancelAfterTXPreStopsAtNextStep(t *testing.T) {
	h, s := newSchedulerHarness(t, nil) // ShutdownDrain 1s
	model, tagA, tagB := uniq("sg-cut"), uniq("sg-cut-a"), uniq("sg-cut-b")
	gotWork, release := make(chan struct{}, 1), make(chan struct{})
	rel := closeOnce(release)
	t.Cleanup(rel)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: holdScript(gotWork, release)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-cut-wf", 100, 0,
		sProc("p1", "COMMIT_BEFORE_DISPATCH", tagA, true), sProc("p2", "SYNC", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)
	createdTx := entityTxID(t, h, id)

	select {
	case <-gotWork:
	case <-time.After(scheduledFireTimeout):
		t.Fatal("p1 was not dispatched")
	}
	// The premise: TX_pre committed before p1 was dispatched.
	if tx := entityTxID(t, h, id); tx == createdTx {
		t.Fatalf("the entity still carries the create's transaction %s; TX_pre did not commit", tx)
	}
	done := make(chan struct{})
	go func() { h.app.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("Shutdown did not return within the drain, the cut and the bookkeeping")
	}

	r, ok := s.task(t, id, "Fire")
	if !ok || r.Status != "WAITING" || r.Attempts != 0 || r.ClaimToken != "" || r.PartialCommit || r.Marked {
		t.Fatalf("after the cut: %+v present=%t; want WAITING, attempts 0, no claim, no PartialCommit, no mark", r, ok)
	}
	if r.LastError != "CANCELLED: the run was stopped by the scheduler" {
		t.Errorf("lastError = %q; want the fixed CANCELLED text", r.LastError)
	}
	if n := len(b.Received()); n != 0 {
		t.Errorf("p2 was dispatched %d times after the cut; want 0", n)
	}
	requireState(t, h, id, "Open")
}

// joinedWriteScript is a processor that, through a joined callback, writes
// the entity being fired (the pattern help/workflows.md advises against) with
// amount 7, and answers with no data. It reports the first callback's status.
func joinedWriteScript(result chan<- int) cnodeScript {
	var calls atomic.Int32
	return func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.UpdateEntity(rc.entityID, `{"name":"Test Order","amount":7,"status":"draft"}`)
		if calls.Add(1) == 1 {
			status := -1
			if err == nil {
				status = res.StatusCode
			}
			result <- status
		}
		return answerOK()
	}
}

// awaitStatus receives the first callback status from ch.
func awaitStatus(t *testing.T, ch <-chan int, what string) int {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(scheduledFireTimeout):
		t.Fatalf("%s: no callback within %s", what, scheduledFireTimeout)
		return 0
	}
}

// TestSchedRun_JoinedCallbackWritesFiredEntity_OrdinaryOutcome: with no unsafe
// processor after it, the anti-pattern ends exactly as the same processor on
// an ordinary automated transition of a stored entity does: both commit, and
// a processor that wrote its entity through the joined callback and returned
// no data keeps that write.
func TestSchedRun_JoinedCallbackWritesFiredEntity_OrdinaryOutcome(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	tag := uniq("sg-jw")
	ordinaryCB, scheduledCB := make(chan int, 1), make(chan int, 1)
	h.AttachCnode(t, cnodeSpec{name: "ordinary", tags: []string{tag + "-o"}, script: joinedWriteScript(ordinaryCB)})
	h.AttachCnode(t, cnodeSpec{name: "scheduled", tags: []string{tag + "-s"}, script: joinedWriteScript(scheduledCB)})

	// Ordinary: a client's manual Start moves the stored entity to Mid, and
	// the cascade runs Mid -[Go, automated]-> Done with the processor.
	ordModel := uniq("sg-jw-ord")
	h.SetupModelWithWorkflow(t, ordModel, schedDoc("sg-jw-ord-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{"name": "Start", "next": "Mid", "manual": true}}},
		"Mid": map[string]any{"transitions": []any{map[string]any{"name": "Go", "next": "Done", "manual": false,
			"processors": []any{sProc("p1", "SYNC", tag+"-o", true)}}}},
		"Done": map[string]any{},
	}))
	ordID := createOpen(t, h, ordModel, workflowSampleModel)
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+ordID+"/Start", workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("the ordinary transition: %d %s; want 200", resp.StatusCode, body)
	}
	if st := awaitStatus(t, ordinaryCB, "ordinary"); st != http.StatusOK {
		t.Fatalf("the ordinary joined write answered %d", st)
	}
	requireState(t, h, ordID, "Done")
	if amount, _ := h.GetEntityData(t, ordID)["amount"].(float64); amount != 7 {
		t.Errorf("ordinary: amount = %v; want 7, the callback's write", amount)
	}

	// Scheduled: the same processor on the fired transition.
	model := uniq("sg-jw-sched")
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-jw-wf", 100, 0, sProc("p1", "SYNC", tag+"-s", true)))
	id := createOpen(t, h, model, workflowSampleModel)
	if st := awaitStatus(t, scheduledCB, "scheduled"); st != http.StatusOK {
		t.Fatalf("the scheduled joined write answered %d", st)
	}

	awaitCallbackEntityState(t, h, id, "Done", scheduledFireTimeout)
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 7 {
		t.Errorf("scheduled: amount = %v; want 7, the callback's write, as on the ordinary transition", amount)
	}
	s.awaitTask(t, id, "Fire", scheduledFireTimeout, "removal", func(_ taskRow, ok bool) bool { return !ok })
	evs := schedEvents(t, h, id)
	if hasSMEventType(evs, "SCHEDULED_TRANSITION_FAIL", "") {
		t.Error("an idempotent run recorded a failure")
	}
	if n := len(smEventsOfType(evs, "SCHEDULED_TRANSITION_FIRE")); n != 1 {
		t.Errorf("%d SCHEDULED_TRANSITION_FIRE events; want 1", n)
	}
}

// TestSchedRun_JoinedCallbackThenUnsafe_TaskBusySafeFailure: the joined
// callback's write holds the task row in the run's transaction, so the
// MarkUnsafe before the unsafe processor gets ErrTaskBusy: no dispatch, a
// counted safe failure, no hang (§5.5). With timeoutMs set, the attempts end
// FAILED EXPIRED_AFTER_FAILED_ATTEMPTS — visible, never stuck.
func TestSchedRun_JoinedCallbackThenUnsafe_TaskBusySafeFailure(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tagA, tagB := uniq("sg-busy"), uniq("sg-busy-a"), uniq("sg-busy-b")
	cb := make(chan int, 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: joinedWriteScript(cb)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-busy-wf", 100, 4000,
		sProc("p1", "SYNC", tagA, true), sProc("p2", "SYNC", tagB, false)))
	id := createOpen(t, h, model, workflowSampleModel)

	if st := awaitStatus(t, cb, "p1"); st != http.StatusOK {
		t.Fatalf("the joined write answered %d; the scenario needs it to succeed", st)
	}
	start := time.Now()
	r := firstAttempt(t, s, id, "Fire")
	if since := time.Since(start); since > 10*time.Second {
		t.Errorf("the first attempt took %s to be recorded; the run must not hang on its own row lock", since)
	}
	if r.Status != "WAITING" || r.Attempts != 1 || r.Marked || r.FailureReason != "" {
		t.Errorf("after ErrTaskBusy: %+v; want WAITING, attempts 1, no mark", r)
	}
	if want := "CONFLICT: the task is being written by another transaction"; r.LastError != want {
		t.Errorf("lastError = %q; want %q, a conflict without a ticket", r.LastError, want)
	}
	f := s.awaitTask(t, id, "Fire", 20*time.Second, "FAILED", func(r taskRow, ok bool) bool { return ok && r.Status == "FAILED" })
	if f.FailureReason != "EXPIRED_AFTER_FAILED_ATTEMPTS" || f.Attempts < 2 {
		t.Errorf("ending = %+v; want EXPIRED_AFTER_FAILED_ATTEMPTS after counted attempts", f)
	}
	if n := len(b.Received()); n != 0 {
		t.Errorf("the unsafe processor was dispatched %d times; want 0", n)
	}
	requireState(t, h, id, "Open")
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 100 {
		t.Errorf("amount = %v; the callback's write rolled back with the run, want 100", amount)
	}
}

// TestSchedRun_JoinedCallbackInSegmentedRun_StampRefused: p0
// (COMMIT_BEFORE_DISPATCH) commits TX_pre in the source state and the run
// goes on in TX_post. There p1's joined callback writes the fired entity,
// which re-arms the task inside TX_post. p2's COMMIT_BEFORE_DISPATCH segment
// then stamps the old life: the stamp is refused, so the segment does not
// commit and p2 is not dispatched. The rollback undoes the re-arm; the
// non-joining re-read finds the life and claim unchanged and classifies an
// ordinary failure (§5.2, §5.5).
//
// p0 is what makes the stamp the refusal. The first COMMIT_BEFORE_DISPATCH
// flush of a scheduled run is a CompareAndSave against the version the run
// read, and a same-transaction callback write already fails that with a
// conflict before the stamp is reached; the second flush is a plain save.
func TestSchedRun_JoinedCallbackInSegmentedRun_StampRefused(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag0, tagA, tagB := uniq("sg-stamp"), uniq("sg-stamp-0"), uniq("sg-stamp-a"), uniq("sg-stamp-b")
	cb := make(chan int, 1)
	p0 := h.AttachCnode(t, cnodeSpec{name: "p0", tags: []string{tag0}})
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}, script: joinedWriteScript(cb)})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-stamp-wf", 100, 4000,
		sProc("p0", "COMMIT_BEFORE_DISPATCH", tag0, true),
		sProc("p1", "SYNC", tagA, true),
		sProc("p2", "COMMIT_BEFORE_DISPATCH", tagB, true)))
	id := createOpen(t, h, model, workflowSampleModel)
	armed := mustTask(t, s, id)

	if st := awaitStatus(t, cb, "p1"); st != http.StatusOK {
		t.Fatalf("the joined write answered %d; the scenario needs it to succeed", st)
	}
	start := time.Now()
	r := firstAttempt(t, s, id, "Fire")
	if since := time.Since(start); since > 10*time.Second {
		t.Errorf("the first attempt took %s to be recorded", since)
	}
	if r.Status != "WAITING" || r.Attempts != 1 || r.PartialCommit || r.FailureReason != "" || r.ArmToken != armed.ArmToken {
		t.Errorf("after the refused stamp: %+v; want WAITING, attempts 1, the same life, no PartialCommit", r)
	}
	if n := len(p0.Received()); n < 1 {
		t.Errorf("p0 was sent %d times; the run must have passed its first segment", n)
	}
	if n := len(b.Received()); n != 0 {
		t.Errorf("the segment's processor was dispatched %d times; a refused stamp stops the segment", n)
	}
	requireState(t, h, id, "Open")
	if amount, _ := h.GetEntityData(t, id)["amount"].(float64); amount != 100 {
		t.Errorf("amount = %v; the callback's write rolled back with the segment, want 100", amount)
	}
}

// TestSchedRun_JoinedCallbackDeletesFiredEntity_RunCommits: the callback
// deletes the entity being fired; the run commits (§5.2, §13): the entity
// stays deleted, its tasks are gone, and the processor is not sent again.
func TestSchedRun_JoinedCallbackDeletesFiredEntity_RunCommits(t *testing.T) {
	h, s := newSchedulerHarness(t, nil)
	model, tag := uniq("sg-del"), uniq("sg-del-tag")
	deleted := make(chan int, 1)
	var calls atomic.Int32
	cn := h.AttachCnode(t, cnodeSpec{name: "p", tags: []string{tag}, script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
		res, err := rc.DeleteEntity(rc.entityID)
		if calls.Add(1) == 1 {
			status := -1
			if err == nil {
				status = res.StatusCode
			}
			deleted <- status
		}
		return answerOK()
	}})
	h.SetupModelWithWorkflow(t, model, fireOpenToDone("sg-del-wf", 100, 0, sProc("p", "SYNC", tag, true)))
	id := createOpen(t, h, model, workflowSampleModel)

	if st := awaitStatus(t, deleted, "p"); st != http.StatusOK {
		t.Fatalf("the joined delete answered %d; want 200", st)
	}
	awaitDBCondition(t, scheduledFireTimeout, "the entity gone", func() bool {
		_, status := h.GetEntityState(t, id)
		return status == http.StatusNotFound
	})
	// A rolled-back run would bring the entity back and send p again.
	awaitSchedulerLiveFor(t, h, 3*fixtureutil.TunedRetryDelay)
	if _, status := h.GetEntityState(t, id); status != http.StatusNotFound {
		t.Errorf("GET after the run = %d; want 404", status)
	}
	if n := s.count(t, "SELECT count(*) FROM scheduled_tasks WHERE tenant_id = $1 AND entity_id = $2", harnessTenant, id); n != 0 {
		t.Errorf("%d task rows remain for the deleted entity", n)
	}
	if n := len(cn.Received()); n != 1 {
		t.Errorf("the processor was sent %d times; want 1", n)
	}
}
