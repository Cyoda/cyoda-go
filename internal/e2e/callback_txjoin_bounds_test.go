package e2e_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// callback_txjoin_bounds_test.go — the running-backend rows for the two bounds
// the join layer puts on a compute member's callbacks, proven end-to-end over
// the full HTTP+gRPC stack against real Postgres:
//
//	answer over CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES → 413 JOINED_RESPONSE_TOO_LARGE
//	more than CYODA_CALLOUT_JOINED_MAX_WAITERS queued    → 503 TOO_MANY_JOINED_REQUESTS
//
// Both settings reach the join layer through app.New, and neither is reachable
// in a test at its shipped default, so each cell lowers its own and leaves the
// other alone. That makes them wiring rows as much as contract rows: a Joiner
// built from the wrong argument answers 200 here.
//
// Each cell asserts the transaction is unharmed by the refusal — the transition
// commits and the entity is readable afterwards — because a bound that took the
// transaction down with the callback would be worse than no bound.

// boundsWorkflow is a two-state workflow whose one transition runs procName as
// a SYNC processor on the compute member, which is where the callbacks below
// are made from.
func boundsWorkflow(wfName, procName string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "externalized", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, wfName, procName)
}

// A callback's answer is held in memory while the transaction is held, under
// CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES. Past it the member is told 413
// JOINED_RESPONSE_TOO_LARGE, with the ceiling named, and nothing of the answer
// is sent. The ceiling here is lowered far below any entity, so an ordinary
// joined read reaches it.
func TestCallbackBounds_AnswerOverTheCeiling_413(t *testing.T) {
	const ceiling = 64 // bytes; smaller than any entity this stack can return
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.Callout.JoinedResponseMaxBytes = ceiling
	})

	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	primary, secondary := "cbb-ceiling-primary-"+sfx, "cbb-ceiling-secondary-"+sfx
	proc := "cbb-read-over-ceiling-" + sfx
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	// A committed entity for the callback to read. Created without a token, so
	// it is an ordinary request and the ceiling does not bear on it.
	readID, status, body := h.CreateEntity(t, secondary, 1, `{"name":"target","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("seed create: status=%d body=%s", status, body)
	}

	type readResult struct {
		status int
		body   string
	}
	got := make(chan readResult, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.GetEntity(readID)
		if err != nil {
			return nil, fmt.Errorf("callback read failed: %w", err)
		}
		got <- readResult{res.StatusCode, res.Body}
		// The refusal is the member's to handle: swallowing it here proves the
		// callout and its transaction carry on.
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, boundsWorkflow("cbb-ceiling-wf-"+sfx, proc))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	var r readResult
	select {
	case r = <-got:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: the processor's callback never returned")
	}
	if r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("joined read status = %d; want 413 (body: %s)", r.status, r.body)
	}
	if code := problemErrorCode(r.body); code != "JOINED_RESPONSE_TOO_LARGE" {
		t.Fatalf("errorCode = %q; want JOINED_RESPONSE_TOO_LARGE (body: %s)", code, r.body)
	}
	// The caller is told the ceiling it passed — the one thing it can act on.
	if !strings.Contains(r.body, strconv.Itoa(ceiling)) {
		t.Errorf("the refusal does not name the %d-byte ceiling (body: %s)", ceiling, r.body)
	}

	// The transaction is unharmed: the refused answer was dropped before it was
	// sent, nothing of the transaction was touched, and the transition
	// committed.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state = %q (http %d); want ACTIVE — a refused answer must not harm T", st, code)
	}
	if st, code := h.GetEntityState(t, readID); code != http.StatusOK || st != "STORED" {
		t.Fatalf("read target state = %q (http %d); want STORED", st, code)
	}
}

// queueOutcome is what the processor reports back to the test goroutine: it
// cannot call t.Fatal from the member's handler goroutine.
type queueOutcome struct {
	served, queued, refused callbackResult
	err                     error
}

// Callbacks of one transaction are served one at a time, and the queue behind
// the one being served is bounded by CYODA_CALLOUT_JOINED_MAX_WAITERS. Past it
// the member is told 503 TOO_MANY_JOINED_REQUESTS, retryably, and neither the
// callback holding the transaction nor the transaction itself is harmed.
//
// The cap is lowered to one, and the callback holding the transaction is held
// inside PostgreSQL on a row lock of the test's own, so the sequence is
// fixed rather than raced: one callback is served and stuck, the next two
// arrive while it is, one of them takes the single queue place and the other
// is refused at once. Releasing the lock lets the served one and the queued
// one finish, and the transition commits.
func TestCallbackBounds_TooManyQueuedCallbacks_503(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.Callout.JoinedMaxWaiters = 1 // whoever holds the transaction, plus one queued
	})

	sfx := randSuffix(t) // repeated runs (go test -count=N) share this package's Postgres testcontainer
	primary, secondary := "cbb-waiters-primary-"+sfx, "cbb-waiters-secondary-"+sfx
	proc := "cbb-burst-" + sfx
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	readID, status, body := h.CreateEntity(t, secondary, 1, `{"name":"target","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("seed create: status=%d body=%s", status, body)
	}
	victim := h.seedVictim(t, "cbb-waiters-victim-"+sfx)
	unlock := holdRowLock(t, victim)
	defer unlock()

	holding := make(chan struct{}) // the processor started the callback that will be stuck
	proceed := make(chan struct{}) // the stuck callback is inside PostgreSQL
	refusedCh := make(chan callbackResult, 1)
	released := make(chan struct{}) // the row lock is gone
	outcome := make(chan queueOutcome, 1)
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		var out queueOutcome
		defer func() { outcome <- out }()
		callback := func(do func() (callbackResult, error)) <-chan callbackResult {
			ch := make(chan callbackResult, 1)
			go func() {
				res, err := do()
				if err != nil {
					res = callbackResult{StatusCode: -1, Body: err.Error()}
				}
				ch <- res
			}()
			return ch
		}

		stuck := callback(func() (callbackResult, error) { return rc.UpdateEntity(victim, victimUpdate) })
		close(holding)
		select {
		case <-proceed:
		case <-time.After(15 * time.Second):
			out.err = fmt.Errorf("the stuck callback never reached PostgreSQL")
			return nil, nil
		}

		// Two callbacks arrive while the first is stuck: one queues, the other
		// is refused immediately, so the first answer to come back is the refusal.
		get := func() (callbackResult, error) { return rc.GetEntity(readID) }
		a, b := callback(get), callback(get)
		var queued <-chan callbackResult
		select {
		case out.refused = <-a:
			queued = b
		case out.refused = <-b:
			queued = a
		case <-time.After(15 * time.Second):
			out.err = fmt.Errorf("neither callback was refused")
			return nil, nil
		}
		refusedCh <- out.refused
		<-released
		out.served, out.queued = <-stuck, <-queued
		// The member swallows the refusal; the callout must still succeed.
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, boundsWorkflow("cbb-waiters-wf-"+sfx, proc))

	created := make(chan createEntityResult, 1)
	go func() { created <- h.CreateEntityRaw(primary, 1, `{"name":"parent","amount":100,"status":"new"}`) }()

	select {
	case <-holding:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: the processor never started")
	}
	awaitBlockedStatement(t, "entities", stmtCeilingLockerAppName)
	close(proceed)

	select {
	case res := <-refusedCh:
		assertProblem(t, res.StatusCode, res.Body, http.StatusServiceUnavailable, "TOO_MANY_JOINED_REQUESTS", true)
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: no callback was refused for capacity")
	}
	unlock()
	close(released)

	var out queueOutcome
	select {
	case out = <-outcome:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: the processor never finished")
	}
	if out.err != nil {
		t.Fatalf("%v", out.err)
	}
	if out.served.StatusCode != http.StatusOK {
		t.Errorf("the callback holding the transaction: %d %s; want 200", out.served.StatusCode, out.served.Body)
	}
	if out.queued.StatusCode != http.StatusOK {
		t.Errorf("the queued callback: %d %s; want 200 — the queue must still serve the one it admits", out.queued.StatusCode, out.queued.Body)
	}

	// The transaction is unharmed: the refused callback touched nothing, the
	// ones that were served read and wrote within T, and the transition committed.
	res := awaitCreate(t, created, 30*time.Second)
	if res.status != http.StatusOK {
		t.Fatalf("primary create: %d %s", res.status, res.body)
	}
	if st, code := h.GetEntityState(t, res.entityID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state = %q (http %d); want ACTIVE — a refused callback must not harm T", st, code)
	}
	if st, code := h.GetEntityState(t, readID); code != http.StatusOK || st != "STORED" {
		t.Fatalf("read target state = %q (http %d); want STORED", st, code)
	}
}
