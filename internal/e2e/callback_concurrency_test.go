package e2e_test

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

// callback_concurrency_test.go — feature #287, ISOLATED single-backend concurrency
// and gate-invariant coverage. Per .claude/rules/test-coverage.md these MUST NOT
// live in the shared parity suite (a goroutine storm on the shared backend
// destabilises unrelated scenarios); they run as standalone e2e tests asserting
// CONSISTENCY (all writes applied, one coherent final state), not a precise
// interleave. Run under the race detector — that is the point of scenario 4:
//
//	go test ./internal/e2e/... -run 'TestCallback_ConcurrentCallbacks_Serialise|TestDispatch_NeverHoldsGateAcrossDispatch' -race -v
//
// Both exercise the per-transaction gate (internal/txgate): concurrent joined
// callbacks on one transaction serialise their access to the shared pgx.Tx /
// buffer, and the transaction owner never holds that gate across engine.Execute
// (the H3 deadlock invariant).

// TestCallback_ConcurrentCallbacks_Serialise fires N parallel joined callbacks
// from inside a single SYNC processor, all echoing the SAME tx-token (so all
// join the SAME transaction T). The txgate must serialise their access to T's
// shared pgx.Tx; without it, concurrent use of one pgx connection surfaces as a
// "conn busy" pgx fatal or a data race the -race detector flags ("concurrent map
// writes" on the tx buffer). The assertion is on CONSISTENCY: every one of the N
// secondary entities was created within T, has a distinct id, and is durable
// after T commits. A torn write (a lost or duplicated secondary, or a pgx fatal
// aborting T) fails the test.
func TestCallback_ConcurrentCallbacks_Serialise(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-concurrent-primary"
	const secondary = "cb-concurrent-secondary"
	const n = 8
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	// createdIDs captures the secondary ids the fan-out minted so the test can
	// verify durability after commit, regardless of goroutine completion order.
	createdIDs := make(chan []string, 1)

	h.RegisterProc("cb-concurrent-fanout", func(rc *reqCtx) (map[string]any, error) {
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			ids  = make([]string, 0, n)
			errs = make([]error, 0)
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := rc.CreateEntity(secondary, 1,
					fmt.Sprintf(`{"name":"child-%d","amount":%d,"status":"new"}`, i, i))
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, fmt.Errorf("callback %d: %w", i, err))
					return
				}
				if res.StatusCode != http.StatusOK {
					errs = append(errs, fmt.Errorf("callback %d status=%d body=%s", i, res.StatusCode, res.Body))
					return
				}
				ids = append(ids, res.EntityID)
			}(i)
		}
		wg.Wait()
		if len(errs) > 0 {
			// Surface the first failure; a torn write / pgx-busy fatal lands here.
			return nil, fmt.Errorf("concurrent callbacks failed (%d errors); first: %w", len(errs), errs[0])
		}
		createdIDs <- ids
		out := cloneData(rc.entityData)
		out["secondaryCount"] = float64(len(ids))
		return out, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-concurrent-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-concurrent-fanout", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s (concurrent callbacks may have torn T)", status, body)
	}

	var ids []string
	select {
	case ids = <-createdIDs:
	case <-time.After(20 * time.Second):
		t.Fatal("timeout: concurrent-callback processor did not complete")
	}

	// Consistency: every callback produced a distinct, non-empty secondary id.
	if len(ids) != n {
		t.Fatalf("got %d secondary ids, want %d (a callback was lost — torn write)", len(ids), n)
	}
	seen := make(map[string]bool, n)
	for _, id := range ids {
		if id == "" {
			t.Fatal("empty secondary id (callback create did not commit within T)")
		}
		if seen[id] {
			t.Fatalf("duplicate secondary id %s (torn write across the shared tx)", id)
		}
		seen[id] = true
	}

	// The primary committed with the observed count — proves the whole fan-out
	// was atomic with T (all N or none).
	if got := h.GetEntityData(t, primaryID)["secondaryCount"]; got != float64(n) {
		t.Errorf("primary.secondaryCount = %v; want %d", got, n)
	}

	// Every secondary is durable after commit (all applied under the gate).
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		if st, code := h.GetEntityState(t, id); code != http.StatusOK {
			t.Errorf("secondary %s GET after commit: http %d; want 200", id, code)
		} else if st != "STORED" {
			t.Errorf("secondary %s state = %q; want STORED", id, st)
		}
	}
}

// TestDispatch_NeverHoldsGateAcrossDispatch encodes the H3 deadlock invariant:
// the transaction owner must NOT hold the per-transaction txgate while
// engine.Execute (the SYNC processor dispatch) blocks. The proof is a processor
// that, while it is mid-dispatch (the engine is blocked awaiting its response),
// fires a joined callback that Joins T and Saves (creates a secondary within T).
// That callback must ACQUIRE the same txID's gate. Because the owner releases the
// gate across engine.Execute, it acquires it and the cascade completes. If the
// invariant were violated (owner held the gate across dispatch), the callback's
// Acquire would block forever, the processor would never return, dispatch would
// never complete, and this test would hang — the bounded watchdog below converts
// that hang into a fast, clearly-labelled failure.
func TestDispatch_NeverHoldsGateAcrossDispatch(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-gate-primary"
	const secondary = "cb-gate-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	h.RegisterProc("cb-join-during-dispatch", func(rc *reqCtx) (map[string]any, error) {
		// This runs WHILE the engine blocks on the dispatch response. The callback
		// Joins T (via the echoed tx-token) and Saves a secondary — it must acquire
		// the txID gate the owner would be holding if the invariant were broken.
		res, err := rc.CreateEntity(secondary, 1, `{"name":"joined","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("joined callback create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("joined callback create status=%d body=%s", res.StatusCode, res.Body)
		}
		// Read-your-own-writes inside T confirms the Save landed on T's buffer.
		got, err := rc.GetEntity(res.EntityID)
		if err != nil {
			return nil, fmt.Errorf("joined callback read failed: %w", err)
		}
		out := cloneData(rc.entityData)
		out["joinedSecondaryId"] = res.EntityID
		out["joinedReadOK"] = got.StatusCode == http.StatusOK
		return out, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-gate-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-join-during-dispatch", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	// Watchdog: the create blocks until the cascade completes. Run it off-goroutine
	// so a gate-across-dispatch deadlock surfaces as a timeout, not a frozen test.
	type result struct {
		id     string
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		id, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
		done <- result{id, status, body}
	}()

	var r result
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DEADLOCK: primary transition did not complete within 15s — the owner is holding the txgate across engine.Execute (H3 invariant violated); a joined callback cannot acquire the gate")
	}

	if r.status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", r.status, r.body)
	}

	data := h.GetEntityData(t, r.id)
	if ok, _ := data["joinedReadOK"].(bool); !ok {
		t.Fatalf("joined callback read-your-writes failed: joinedReadOK=%v", data["joinedReadOK"])
	}
	secID, _ := data["joinedSecondaryId"].(string)
	if secID == "" {
		t.Fatal("primary data missing joinedSecondaryId — joined Save did not commit with T")
	}
	if st, code := h.GetEntityState(t, secID); code != http.StatusOK {
		t.Errorf("joined secondary GET after commit: http %d; want 200", code)
	} else if st != "STORED" {
		t.Errorf("joined secondary state = %q; want STORED", st)
	}
}
