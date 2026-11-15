package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/fixtureutil"
)

// scheduled_run_segments_test.go — runs that commit in segments
// (COMMIT_BEFORE_DISPATCH), the PartialCommit rule and a run cut by the
// scheduler, through the full stack on PostgreSQL.

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

	awaitReceive(t, gotWork, scheduledFireTimeout, "p1 dispatched")
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
