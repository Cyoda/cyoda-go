package e2e_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
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

	const primary = "cbb-ceiling-primary"
	const secondary = "cbb-ceiling-secondary"
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
	h.RegisterProc("cbb-read-over-ceiling", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.GetEntity(readID)
		if err != nil {
			return nil, fmt.Errorf("callback read failed: %w", err)
		}
		got <- readResult{res.StatusCode, res.Body}
		// The refusal is the member's to handle: swallowing it here proves the
		// callout and its transaction carry on.
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, boundsWorkflow("cbb-ceiling-wf", "cbb-read-over-ceiling"))

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

// burstOutcome is what the processor reports back to the test goroutine: it
// cannot call t.Fatal from the member's handler goroutine.
type burstOutcome struct {
	served  int
	refused int
	other   []string
	err     error
}

// Callbacks of one transaction are served one at a time, and the queue behind
// the one being served is bounded by CYODA_CALLOUT_JOINED_MAX_WAITERS. Past it
// the member is told 503 TOO_MANY_JOINED_REQUESTS, retryably, and neither the
// callback holding the transaction nor the transaction itself is harmed.
//
// The cap is lowered to one, so a handful of clients released together reach
// it. Nothing here asserts an interleave — only that the bound bit, that the
// queue drained, and that the transition committed.
func TestCallbackBounds_TooManyQueuedCallbacks_503(t *testing.T) {
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		cfg.Callout.JoinedMaxWaiters = 1 // whoever holds the transaction, plus one queued
	})

	const primary = "cbb-waiters-primary"
	const secondary = "cbb-waiters-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	readID, status, body := h.CreateEntity(t, secondary, 1, `{"name":"target","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("seed create: status=%d body=%s", status, body)
	}

	outcome := make(chan burstOutcome, 1)
	h.RegisterProc("cbb-burst", func(rc *reqCtx) (map[string]any, error) {
		const clients, rounds = 8, 3
		start := make(chan struct{})
		results := make(chan callbackResult, clients*rounds)
		failures := make(chan error, clients*rounds)
		var wg sync.WaitGroup
		for range clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range rounds {
					res, err := rc.GetEntity(readID)
					if err != nil {
						failures <- err
						return
					}
					results <- res
				}
			}()
		}
		close(start) // released together, so they contend for the transaction
		wg.Wait()
		close(results)
		close(failures)

		var out burstOutcome
		for err := range failures {
			out.err = err
		}
		for res := range results {
			switch {
			case res.StatusCode == http.StatusOK:
				out.served++
			case res.StatusCode == http.StatusServiceUnavailable && problemErrorCode(res.Body) == "TOO_MANY_JOINED_REQUESTS":
				out.refused++
			default:
				out.other = append(out.other, fmt.Sprintf("status=%d body=%s", res.StatusCode, res.Body))
			}
		}
		outcome <- out
		// The member swallows the refusals; the callout must still succeed.
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, boundsWorkflow("cbb-waiters-wf", "cbb-burst"))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	var out burstOutcome
	select {
	case out = <-outcome:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: the processor's callback burst never finished")
	}
	if out.err != nil {
		t.Fatalf("a callback failed at the transport: %v", out.err)
	}
	if len(out.other) > 0 {
		t.Fatalf("callbacks answered neither 200 nor the cap refusal: %s", strings.Join(out.other, "; "))
	}
	if out.refused == 0 {
		t.Fatalf("no callback was refused for capacity (%d served); the cap did not reach the join layer", out.served)
	}
	if out.served == 0 {
		t.Fatalf("every callback was refused (%d); the queue must still serve the ones it admits", out.refused)
	}

	// The transaction is unharmed: refused callbacks touched nothing, the ones
	// that were served read within T, and the transition committed.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Fatalf("primary state = %q (http %d); want ACTIVE — a refused callback must not harm T", st, code)
	}
	if st, code := h.GetEntityState(t, readID); code != http.StatusOK || st != "STORED" {
		t.Fatalf("read target state = %q (http %d); want STORED", st, code)
	}
}
