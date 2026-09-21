package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// callout_fencing_wait_test.go is the owner's wait: a joined request that was
// already in progress when the work moved on is neither overtaken nor
// interrupted. The fence shuts the earlier pass out and then takes the
// transaction's lock, which the request in progress still holds, so nothing of
// the next try — and nothing the engine does after a callout — can start until
// that request has finished.
//
// "In progress" is made deterministic inside PostgreSQL rather than by waiting.
// A joined WRITE to a committed row the test holds FOR UPDATE waits in the
// database, on the operation's own connection, while the join layer holds the
// transaction's lock for it. A joined READ cannot be blocked by a row lock, so
// the read scenarios read a message while the test holds the messages table
// ACCESS EXCLUSIVE. That table, and only it: a table lock on entities would
// stall the shared stack's background loops (storage_ceilings_e2e_test.go's
// holdRowLock says why), while nothing in this package's background loops
// touches messages, no test in it runs in parallel, and the lock is held for
// about two seconds.

const (
	victimUpdate = `{"name": "Test Order", "amount": 100, "status": "held"}`
	lateChild    = `{"name":"late-child","amount":1,"status":"late"}`
)

// awaitBlockedStatement returns once some backend is waiting on a lock while
// running a statement that names table, so a scenario never guesses whether the
// joined request has reached the database.
func awaitBlockedStatement(t *testing.T, table string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		err := dbPool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE wait_event_type = 'Lock' AND state = 'active' AND query ILIKE '%' || $1 || '%'`, table).Scan(&n)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement on %s is waiting on a lock after 10s; the joined request never reached the database", table)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdMessagesTableLock holds ACCESS EXCLUSIVE on messages from a connection of
// the test's own until the returned func is called, so a joined message read
// waits inside PostgreSQL. See the file header for why this table, and only it.
// The release is also registered as a cleanup, so a failing assertion can never
// leave the table locked for the rest of the package.
func holdMessagesTableLock(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := pgxpool.New(ctx, withAppName(t, pgURLFromEnv(t), "callout-fence-locker"))
	if err != nil {
		cancel()
		t.Fatalf("open locker pool: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`)
	}
	if err != nil {
		pool.Close()
		cancel()
		t.Fatalf("lock messages: %v", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(ctx)
			pool.Close()
			cancel()
		})
	}
	t.Cleanup(release)
	return release
}

// seedVictim commits one entity on a processor-free workflow and returns its id.
// It is the row a joined request is made to wait on, and it exists outside the
// transaction under test so that what the request wrote can be read afterwards.
func (h *callbackHarness) seedVictim(t *testing.T, model string) string {
	t.Helper()
	h.SetupModelWithWorkflow(t, model, workflowV1)
	id, status, body := h.CreateEntity(t, model, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("seed victim: %d %s", status, body)
	}
	return id
}

// goJoined runs one joined HTTP request off the script's goroutine and delivers
// its outcome.
func goJoined(h *callbackHarness, method, path, body, pass string) <-chan callbackResult {
	out := make(chan callbackResult, 1)
	go func() {
		res, err := h.callback(method, path, body, pass)
		if err != nil {
			res = callbackResult{StatusCode: -1, Body: err.Error()}
		}
		out <- res
	}()
	return out
}

func awaitResult(t *testing.T, what string, ch <-chan callbackResult) callbackResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not complete within 15s", what)
		return callbackResult{}
	}
}

// TestCalloutFence_OwnerWaitsForARequestInProgress (F-replaced + F-blocked):
// the first cnode has one joined write inside PostgreSQL and a second queued
// for the transaction's lock when its answer limit passes. The second cnode is
// not given the work until the first write has finished; that write lands and
// is answered 200; the queued one is refused on taking the lock and writes
// nothing.
func TestCalloutFence_OwnerWaitsForARequestInProgress(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const model, secondary, tag = "s8-wait", "s8-wait-secondary", "s8-wait"
	victim := h.seedVictim(t, "s8-wait-victim")
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-wait-wf", procSpec{"s8-proc", "SYNC",
		map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 1500, "idempotent": true}}))

	unlock := holdRowLock(t, victim)
	defer unlock()

	queueNow := make(chan struct{})
	queueNext := closeOnce(queueNow)
	t.Cleanup(queueNext)
	inProgress := make(chan (<-chan callbackResult), 1)
	queued := make(chan callbackResult, 1)
	first := h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag},
		script: func(ctx context.Context, call receivedCallout, rc *reqCtx) cnodeReply {
			inProgress <- goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass())
			select {
			case <-queueNow:
			case <-ctx.Done():
				return neverAnswer()
			}
			res, err := rc.CreateEntity(secondary, 1, lateChild) // queues behind the write in progress
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			queued <- res
			return neverAnswer()
		}})
	second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	writeRes := <-inProgress
	awaitBlockedStatement(t, "entities")
	queueNext()

	time.Sleep(2 * time.Second) // the first try's 1.5s answer limit has passed
	if got := second.Received(); len(got) != 0 {
		t.Fatalf("the second cnode was given the work while a request of the first is in progress: %v", got)
	}

	unlock()
	if res := awaitResult(t, "the write in progress", writeRes); res.StatusCode != http.StatusOK {
		t.Errorf("the write in progress: %d %s; want 200 — it made its check before the number rose, and it lands", res.StatusCode, res.Body)
	}
	res := awaitResult(t, "the queued write", queued)
	assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)

	awaitCnodeReceived(t, second, 1, 15*time.Second)
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	if got := h.GetEntityData(t, victim)["status"]; got != "held" {
		t.Errorf("victim.status = %v; want \"held\" — the write in progress landed in the transaction that committed", got)
	}
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; the queued write must have written nothing", n)
	}
	if n := len(first.Received()); n != 1 {
		t.Errorf("the first cnode received %d callouts; want 1", n)
	}
	if n := len(second.Received()); n != 1 {
		t.Errorf("the second cnode received %d callouts; want 1", n)
	}
}

// TestCalloutFence_AsyncNewTxFailedWriteIsNotCommitted (F-blocked): an
// ASYNC_NEW_TX processor's cnode has a write inside PostgreSQL when its try
// times out. The engine does not carry on until that write has finished, and
// the write is then undone with the processor's savepoint.
func TestCalloutFence_AsyncNewTxFailedWriteIsNotCommitted(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const model, tagA, tagB = "s8-async", "s8-async-a", "s8-async-b"
	victim := h.seedVictim(t, "s8-async-victim")
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-async-wf",
		procSpec{"s8-proc-a", "ASYNC_NEW_TX", map[string]any{"calculationNodesTags": tagA, "responseTimeoutMs": 1000}},
		procSpec{"s8-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	unlock := holdRowLock(t, victim)
	defer unlock()

	inProgress := make(chan (<-chan callbackResult), 1)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA},
		script: func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
			inProgress <- goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass())
			return neverAnswer()
		}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	writeRes := <-inProgress
	awaitBlockedStatement(t, "entities")
	time.Sleep(1500 * time.Millisecond) // A's answer limit has passed; its callout has failed
	if got := b.Received(); len(got) != 0 {
		t.Fatalf("the engine carried on to processor B while A's write is in progress: %v", got)
	}

	unlock()
	if res := awaitResult(t, "A's write in progress", writeRes); res.StatusCode != http.StatusOK {
		t.Errorf("A's write in progress: %d %s; want 200", res.StatusCode, res.Body)
	}
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 — an ASYNC_NEW_TX failure does not fail the operation", res.status, res.body)
	}
	if got := h.GetEntityData(t, victim)["status"]; got != "draft" {
		t.Errorf("victim.status = %v; want \"draft\" — the failed processor's write is undone with its savepoint", got)
	}
	if n := len(a.Received()); n != 1 {
		t.Errorf("processor A's cnode received %d callouts; want 1 — a lost answer ends a callout that is not repeat-safe", n)
	}
	if n := len(b.Received()); n != 1 {
		t.Errorf("processor B ran %d times; want 1", n)
	}
}

// TestCalloutFence_CallbackPastItsLastCheckLandsWhole (F-blocked): a cnode
// answers while its own joined collection update is still inside PostgreSQL.
// The engine does not carry on until the collection has finished; the
// collection lands whole and is answered 200.
func TestCalloutFence_CallbackPastItsLastCheckLandsWhole(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const model, victimModel, tagA, tagB = "s8-whole", "s8-whole-victim", "s8-whole-a", "s8-whole-b"
	v1 := h.seedVictim(t, victimModel)
	v2, status, body := h.CreateEntity(t, victimModel, 1, workflowSampleModel)
	if status != http.StatusOK {
		t.Fatalf("seed second victim: %d %s", status, body)
	}
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-whole-wf",
		procSpec{"s8-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s8-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	items, err := json.Marshal([]map[string]any{ // the locked row FIRST: v2 is written after the callout ended
		{"id": v1, "payload": victimUpdate, "transition": "approve"},
		{"id": v2, "payload": victimUpdate, "transition": "approve"},
	})
	if err != nil {
		t.Fatalf("build the collection update: %v", err)
	}

	unlock := holdRowLock(t, v1)
	defer unlock()

	answerNow := make(chan struct{})
	answerNext := closeOnce(answerNow)
	t.Cleanup(answerNext)
	inProgress := make(chan (<-chan callbackResult), 1)
	h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA},
		script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
			inProgress <- goJoined(h, http.MethodPut, "/api/entity/JSON", string(items), call.Pass())
			select {
			case <-answerNow:
				return answerOK() // answers before its own callback has finished
			case <-ctx.Done():
				return neverAnswer()
			}
		}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

	collRes := <-inProgress
	awaitBlockedStatement(t, "entities")
	answerNext()
	time.Sleep(700 * time.Millisecond)
	if got := b.Received(); len(got) != 0 {
		t.Fatalf("the engine carried on to processor B while A's collection is in progress: %v", got)
	}

	unlock()
	if res := awaitResult(t, "the collection in progress", collRes); res.StatusCode != http.StatusOK {
		t.Fatalf("the collection: %d %s; want 200 — what it wrote is in the transaction", res.StatusCode, res.Body)
	}
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200", res.status, res.body)
	}
	for _, id := range []string{v1, v2} {
		if st, _ := h.GetEntityState(t, id); st != "APPROVED" {
			t.Errorf("victim %s state = %q; want APPROVED — the collection lands whole", id, st)
		}
	}
	if n := len(b.Received()); n != 1 {
		t.Errorf("processor B ran %d times; want 1", n)
	}
}

// TestCalloutFence_JoinedReadInProgress (F-replaced + F-blocked, a read): the
// first cnode has a joined read inside PostgreSQL when it is replaced — because
// its answer limit passed, or because it disconnected and abandoned the read
// mid-statement. Either way the statement is not interrupted, the owner waits
// for it, and the owner's operation succeeds.
func TestCalloutFence_JoinedReadInProgress(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := map[bool]string{false: "answer-limit-passes", true: "cnode-disconnects-mid-callback"}[disconnect]
		t.Run(name, func(t *testing.T) {
			h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
			model, tag := "s8-read-"+name, "s8-read-"+name
			h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s8-read-wf", procSpec{"s8-proc", "SYNC",
				map[string]any{"calculationNodesTags": tag, "responseTimeoutMs": 1000, "idempotent": true}}))

			resp := h.DoAuth(t, http.MethodPost, "/api/message/new/s8-read", `{"payload":{"x":1}}`, "")
			msgBody := h.readBody(t, resp)
			var created []map[string]any
			if err := json.Unmarshal([]byte(msgBody), &created); err != nil || resp.StatusCode != http.StatusOK || len(created) == 0 {
				t.Fatalf("seed message: %d %s", resp.StatusCode, msgBody)
			}
			ids, _ := created[0]["entityIds"].([]any)
			msgID, _ := ids[0].(string)

			unlock := holdMessagesTableLock(t)

			readCtx, abandonRead := context.WithCancel(context.Background())
			defer abandonRead()
			dropNow := make(chan struct{})
			dropStream := closeOnce(dropNow)
			t.Cleanup(dropStream)
			readRes := make(chan callbackResult, 1)
			h.AttachCnode(t, cnodeSpec{name: "first", tags: []string{tag},
				script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
					go func() {
						res, err := h.joinedRequest(readCtx, http.MethodGet, "/api/message/"+msgID, "", call.Pass())
						if err != nil {
							res = callbackResult{StatusCode: -1, Body: err.Error()}
						}
						readRes <- res
					}()
					if !disconnect {
						return neverAnswer()
					}
					select {
					case <-dropNow:
						return closeStream()
					case <-ctx.Done():
						return neverAnswer()
					}
				}})
			second := h.AttachCnode(t, cnodeSpec{name: "second", tags: []string{tag}})

			done := make(chan createEntityResult, 1)
			go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()

			awaitBlockedStatement(t, "messages")
			if disconnect {
				abandonRead() // the cnode walks away from its callback mid-statement …
				dropStream()  // … and drops its stream
				time.Sleep(700 * time.Millisecond)
			} else {
				time.Sleep(1500 * time.Millisecond) // the answer limit has passed
			}
			if got := second.Received(); len(got) != 0 {
				t.Fatalf("the second cnode was given the work while the first's read is in progress: %v", got)
			}

			unlock()
			res := awaitResult(t, "the read in progress", readRes)
			if !disconnect && res.StatusCode != http.StatusOK {
				t.Errorf("the read in progress: %d %s; want 200 — it completes", res.StatusCode, res.Body)
			}
			awaitCnodeReceived(t, second, 1, 15*time.Second)
			if cr := awaitCreate(t, done, 15*time.Second); cr.status != http.StatusOK {
				t.Fatalf("create: %d %s; want 200 — no statement of the operation's connection was interrupted", cr.status, cr.body)
			}
			if n := len(second.Received()); n != 1 {
				t.Errorf("the second cnode received %d callouts; want 1", n)
			}
		})
	}
}

// TestCalloutFence_WaitDoesNotDeadlock (F-nested + F-blocked): out1's callback
// made a callout of its own, and that inner cnode's callback holds the
// transaction's lock inside PostgreSQL when out1 is replaced. Everything
// unwinds once the database lets the write through.
func TestCalloutFence_WaitDoesNotDeadlock(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	const outer, inner, tagOut, tagIn = "s8-dl-outer", "s8-dl-inner", "s8-dl-out", "s8-dl-in"
	victim := h.seedVictim(t, "s8-dl-victim")
	h.SetupModelWithWorkflow(t, inner, chainWorkflowJSON("s8-dl-inner-wf",
		procSpec{"s8-in", "SYNC", map[string]any{"calculationNodesTags": tagIn}}))
	h.SetupModelWithWorkflow(t, outer, chainWorkflowJSON("s8-dl-outer-wf",
		procSpec{"s8-out", "SYNC", map[string]any{"calculationNodesTags": tagOut, "responseTimeoutMs": 1500, "idempotent": true}}))

	unlock := holdRowLock(t, victim)
	defer unlock()

	holdIn := make(chan struct{})
	t.Cleanup(closeOnce(holdIn))
	out1Res := make(chan callbackResult, 1)
	inWrite := make(chan (<-chan callbackResult), 1)
	h.AttachCnode(t, cnodeSpec{name: "out1", tags: []string{tagOut},
		script: func(_ context.Context, _ receivedCallout, rc *reqCtx) cnodeReply {
			res, err := rc.CreateEntity(inner, 1, workflowSampleModel)
			if err != nil {
				res = callbackResult{StatusCode: -1, Body: err.Error()}
			}
			out1Res <- res
			return neverAnswer()
		}})
	h.AttachCnode(t, cnodeSpec{name: "in1", tags: []string{tagIn},
		script: func(ctx context.Context, call receivedCallout, _ *reqCtx) cnodeReply {
			inWrite <- goJoined(h, http.MethodPut, "/api/entity/JSON/"+victim, victimUpdate, call.Pass())
			select {
			case <-holdIn:
			case <-ctx.Done():
			}
			return neverAnswer()
		}})
	out2 := h.AttachCnode(t, cnodeSpec{name: "out2", tags: []string{tagOut}})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(outer, 1, workflowSampleModel) }()

	writeRes := <-inWrite
	awaitBlockedStatement(t, "entities")
	time.Sleep(2 * time.Second) // OUT's 1.5s answer limit has passed
	if got := out2.Received(); len(got) != 0 {
		t.Fatalf("out2 was given the work while the inner cnode's write holds the transaction: %v", got)
	}

	unlock()
	if res := awaitResult(t, "the inner cnode's write", writeRes); res.StatusCode != http.StatusOK {
		t.Errorf("the inner cnode's write: %d %s; want 200 — it was in progress and lands", res.StatusCode, res.Body)
	}
	res := awaitResult(t, "out1's callback", out1Res)
	assertProblem(t, res.StatusCode, res.Body, http.StatusGone, "CALLOUT_SUPERSEDED", false)
	awaitCnodeReceived(t, out2, 1, 15*time.Second)
	if cr := awaitCreate(t, done, 15*time.Second); cr.status != http.StatusOK {
		t.Fatalf("outer create: %d %s; want 200", cr.status, cr.body)
	}
	if n := h.countEntities(t, inner); n != 0 {
		t.Errorf("%d inner entities committed; want 0", n)
	}
	if n := len(out2.Received()); n != 1 {
		t.Errorf("out2 received %d callouts; want 1", n)
	}
}
