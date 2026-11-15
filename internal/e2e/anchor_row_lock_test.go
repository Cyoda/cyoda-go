package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// anchor_row_lock_test.go — on postgres the save before a callout writes the
// anchor's row in the request's transaction, so the row stays locked until
// that transaction ends. Another client that writes the anchor, or another
// transaction that read it and commits, during the callout waits for the
// cascade to end and is then checked as it would be at any other time. These
// tests assert a consistent outcome, never a precise interleave.
//
// Isolated single-backend tests (they hold a callout open while another
// request runs), never parity scenarios.

// stillRunningAfter is how long the test lets a request that must wait run
// before it checks that the request has not finished.
const stillRunningAfter = 1500 * time.Millisecond

// holdWorkflowJSON is a READY -go(manual)-> DONE workflow whose transition runs
// the given SYNC processors in order, each with a short answer limit so a
// test that went wrong ends in seconds.
func holdWorkflowJSON(wfName string, procs ...string) string {
	list := make([]any, 0, len(procs))
	for _, p := range procs {
		list = append(list, map[string]any{"type": "externalized", "name": p, "executionMode": "SYNC",
			"config": map[string]any{"attachEntity": true, "calculationNodesTags": "", "responseTimeoutMs": 10000}})
	}
	return workflowDocJSON(wfName, map[string]any{
		"NONE":  map[string]any{"transitions": []any{map[string]any{"name": "init", "next": "READY", "manual": false}}},
		"READY": map[string]any{"transitions": []any{map[string]any{"name": "go", "next": "DONE", "manual": true, "processors": list}}},
		"DONE":  map[string]any{},
	})
}

// heldCallout is a processor that tells the test it is running and answers
// only when the test lets it. It never waits on anything else.
type heldCallout struct {
	running chan struct{}
	release chan struct{}
	first   atomic.Bool
}

func newHeldCallout() *heldCallout {
	return &heldCallout{running: make(chan struct{}), release: make(chan struct{})}
}

func (c *heldCallout) proc(rc *reqCtx) (map[string]any, error) {
	if c.first.CompareAndSwap(false, true) {
		close(c.running)
	}
	select {
	case <-c.release:
	case <-time.After(8 * time.Second):
	}
	return nil, nil
}

func (c *heldCallout) awaitRunning(t *testing.T) {
	t.Helper()
	select {
	case <-c.running:
	case <-time.After(15 * time.Second):
		t.Fatal("the callout never started")
	}
}

// asyncResult runs fn on a goroutine and hands its outcome back.
func asyncRequest(fn func() (callbackResult, error)) <-chan callbackResult {
	out := make(chan callbackResult, 1)
	go func() {
		res, err := fn()
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		out <- res
	}()
	return out
}

func awaitRequest(t *testing.T, what string, ch <-chan callbackResult) callbackResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish", what)
		return callbackResult{}
	}
}

// requireNotFinished asserts a request has not answered after
// stillRunningAfter; when it has, it returns that answer for the report.
func requireNotFinished(t *testing.T, what string, ch <-chan callbackResult) {
	t.Helper()
	select {
	case res := <-ch:
		t.Fatalf("%s answered %d %s while the cascade's callout was still running; want it to wait for the cascade to end",
			what, res.StatusCode, res.Body)
	case <-time.After(stillRunningAfter):
	}
}

func problemCode(body string) (code string, retryable bool) {
	var pd struct {
		Properties struct {
			ErrorCode string `json:"errorCode"`
			Retryable bool   `json:"retryable"`
		} `json:"properties"`
	}
	_ = json.Unmarshal([]byte(body), &pd)
	return pd.Properties.ErrorCode, pd.Properties.Retryable
}

// TestAnchorRowLock_OtherClientWaits_OneWinner: while an update's processor
// callout runs, another client writes the same entity. The other client waits
// until the cascade ends; then the cascade has committed and the other
// client's write, made from the older snapshot, is refused with a retryable
// 409 CONFLICT. The entity holds the cascade's data in DONE.
func TestAnchorRowLock_OtherClientWaits_OneWinner(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "rowlock-other-client"
	held := newHeldCallout()
	h.RegisterProc("rowlock-hold", held.proc)
	h.SetupModelWithWorkflow(t, model, holdWorkflowJSON(model+"-wf", "rowlock-hold"))

	id, status, body := h.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"old"}`)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}

	cascade := asyncRequest(func() (callbackResult, error) {
		return h.updateTransitionHTTP(id, "go", `{"name":"anchor","amount":2,"status":"cascade"}`)
	})
	held.awaitRunning(t)
	rival := asyncRequest(func() (callbackResult, error) {
		return h.callback(http.MethodPut, "/api/entity/JSON/"+id, `{"name":"anchor","amount":3,"status":"rival"}`, "")
	})
	requireNotFinished(t, "the other client's write", rival)
	close(held.release)

	// The other client read the entity before the cascade committed, so a
	// write from that snapshot would overwrite the committed transition: on
	// postgres (REPEATABLE READ) it is refused when the row lock is released.
	if c := awaitRequest(t, "the cascade", cascade); c.StatusCode != http.StatusOK {
		t.Fatalf("cascade: %d %s; want 200", c.StatusCode, c.Body)
	}
	r := awaitRequest(t, "the other client's write", rival)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("other client: %d %s; want 409 — it wrote from a snapshot the cascade then replaced", r.StatusCode, r.Body)
	}
	if code, retryable := problemCode(r.Body); code != "CONFLICT" || !retryable {
		t.Errorf("other client refused with %s retryable=%v; want a retryable CONFLICT: %s", code, retryable, r.Body)
	}
	if got := h.GetEntityData(t, id)["status"]; got != "cascade" {
		t.Errorf("entity status = %v; want the cascade's", got)
	}
	if st, _ := h.GetEntityState(t, id); st != "DONE" {
		t.Errorf("entity state = %q; want DONE", st)
	}
}

// TestAnchorRowLock_ReaderCommitWaitsThenValidates: while an update's processor
// callout runs, another transaction reads the entity (a processor of a second
// entity's create reads it under its own token) and commits. Its commit waits
// for the cascade to end, then validates what it read: the cascade changed the
// entity, so the reader's commit is refused with a retryable 409.
func TestAnchorRowLock_ReaderCommitWaitsThenValidates(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "rowlock-reader-anchor"
	const readerModel = "rowlock-reader"
	held := newHeldCallout()
	h.RegisterProc("rowlock-reader-hold", held.proc)
	h.SetupModelWithWorkflow(t, model, holdWorkflowJSON(model+"-wf", "rowlock-reader-hold"))

	var readStatus atomic.Int32
	var readMarker atomic.Value
	h.RegisterProc("rowlock-read-target", func(rc *reqCtx) (map[string]any, error) {
		target, _ := rc.entityData["targetId"].(string)
		res, err := rc.GetEntity(target)
		if err != nil {
			return nil, err
		}
		readStatus.Store(int32(res.StatusCode))
		readMarker.Store(entityDataField(res.Body, "status"))
		return nil, nil
	})
	h.setupModelSampleWithWorkflow(t, readerModel, workflowSampleWith(`"targetId": ""`),
		chainWorkflowJSON(readerModel+"-wf", procSpec{"rowlock-read-target", "SYNC", map[string]any{"calculationNodesTags": ""}}))

	id, status, body := h.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"old"}`)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}

	cascade := asyncRequest(func() (callbackResult, error) {
		return h.updateTransitionHTTP(id, "go", `{"name":"anchor","amount":2,"status":"cascade"}`)
	})
	held.awaitRunning(t)
	reader := asyncRequest(func() (callbackResult, error) {
		res := h.CreateEntityRaw(readerModel, 1, fmt.Sprintf(`{"name":"reader","amount":1,"status":"r","targetId":%q}`, id))
		return callbackResult{StatusCode: res.status, Body: res.body}, res.err
	})
	requireNotFinished(t, "the reader's commit", reader)
	close(held.release)

	if c := awaitRequest(t, "the cascade", cascade); c.StatusCode != http.StatusOK {
		t.Fatalf("cascade: %d %s; want 200", c.StatusCode, c.Body)
	}
	r := awaitRequest(t, "the reader's commit", reader)
	if got := readStatus.Load(); got != http.StatusOK {
		t.Fatalf("the reader's GET of the entity answered %d; want 200", got)
	}
	if got, _ := readMarker.Load().(string); got != "old" {
		t.Fatalf("the reader read status %q; want the committed %q (the cascade had not committed)", got, "old")
	}
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("reader: %d %s; want 409 — it read a version the cascade then replaced", r.StatusCode, r.Body)
	}
	if code, retryable := problemCode(r.Body); code != "CONFLICT" || !retryable {
		t.Errorf("reader refused with %s retryable=%v; want a retryable CONFLICT: %s", code, retryable, r.Body)
	}
	if got := h.GetEntityData(t, id)["status"]; got != "cascade" {
		t.Errorf("entity status = %v; want the cascade's", got)
	}
}

// TestPreCalloutSave_Conflict_Retryable409: a transition (no new data, so
// nothing is saved before its first processor) runs two processors. While the
// first one's callout runs, another client commits a write of the entity. The
// first processor returns new data, so the engine saves the entity before the
// second callout, and that save meets the other client's commit: the request
// is refused with a retryable 409 CONFLICT, the second processor is never
// called, and the other client's write stands.
func TestPreCalloutSave_Conflict_Retryable409(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "presave-conflict"
	held := newHeldCallout()
	h.RegisterProc("presave-first", func(rc *reqCtx) (map[string]any, error) {
		if _, err := held.proc(rc); err != nil {
			return nil, err
		}
		out := cloneData(rc.entityData)
		out["status"] = "first"
		return out, nil
	})
	var secondCalls atomic.Int32
	h.RegisterProc("presave-second", func(rc *reqCtx) (map[string]any, error) {
		secondCalls.Add(1)
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, model, holdWorkflowJSON(model+"-wf", "presave-first", "presave-second"))

	id, status, body := h.CreateEntity(t, model, 1, `{"name":"anchor","amount":1,"status":"old"}`)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}

	type envResult struct {
		env txEnvelope
		err error
	}
	transition := make(chan envResult, 1)
	go func() {
		env, err := h.joinedManageGRPC(internalgrpc.EntityTransitionRequest, map[string]any{
			"id": "presave-conflict", "entityId": id, "transition": "go",
		}, "")
		transition <- envResult{env, err}
	}()
	held.awaitRunning(t)
	rival, err := h.callback(http.MethodPut, "/api/entity/JSON/"+id, `{"name":"anchor","amount":3,"status":"rival"}`, "")
	close(held.release)
	if err != nil || rival.StatusCode != http.StatusOK {
		t.Fatalf("the other client's write: %v %d %s; want 200 (nothing held the entity yet)", err, rival.StatusCode, rival.Body)
	}

	var got envResult
	select {
	case got = <-transition:
	case <-time.After(30 * time.Second):
		t.Fatal("the transition did not finish")
	}
	if got.err != nil {
		t.Fatalf("transition: %v", got.err)
	}
	env := got.env
	if env.Success || env.Error == nil {
		t.Fatalf("transition succeeded; want a retryable 409 CONFLICT")
	}
	if env.Error.Code != "CLIENT_ERROR" || !strings.HasPrefix(env.Error.Message, "CONFLICT:") ||
		env.Error.Retryable == nil || !*env.Error.Retryable {
		t.Errorf("envelope error = %+v; want a retryable CLIENT_ERROR carrying CONFLICT", *env.Error)
	}
	if n := secondCalls.Load(); n != 0 {
		t.Errorf("the second processor was called %d time(s); want none — the save before it failed", n)
	}
	if got := h.GetEntityData(t, id)["status"]; got != "rival" {
		t.Errorf("entity status = %v; want the other client's write to stand", got)
	}
}
