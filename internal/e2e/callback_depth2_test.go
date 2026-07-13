package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// callback_depth2_test.go — ISOLATED single-backend coverage for the depth-2
// nested-join deadlock (a joined callback must NOT hold the per-tx txgate across
// engine.Execute — the H3 invariant, generalised from the owner path to the
// joined path). Like the other txgate-invariant tests (callback_concurrency_test.go)
// this lives in internal/e2e, NOT the shared parity suite: the deadlock is a
// domain-layer lock property identical across every backend, and a gate-hang
// would destabilise unrelated parity scenarios.
//
// primary create (OWNER T)
//   └─ SYNC proc cb-d2-primary  ── owner does NOT hold gate ──▶
//        joined create secondary (owned==false: ACQUIRES gate(T), holds whole body)
//          └─ inner proc cb-d2-secondary ── gate(T) STILL HELD (pre-fix) ──▶
//               joined create tertiary  ──▶ Acquire(gate(T)) BLOCKS until the
//               outer dispatch times out at 30s (pre-fix) / progresses (post-fix)
//
// The inner processor's executionMode is parameterised so the same cascade is
// proven for every dispatch site that keeps the gated txID: SYNC
// (executeSyncProcessor) and ASYNC_NEW_TX (executeAsyncNewTx, which dispatches
// inside a savepoint on the SAME tx). Both must suspend the gate across their
// callout or the third-level joined write deadlocks.

// depth2Chain sets up the primary→secondary→tertiary model chain where the
// secondary's `store` transition runs one processor (execMode) that creates a
// tertiary joined write, and returns the tertiary id the innermost callback
// minted (via tertiaryID) once the cascade commits. It fails the test with a
// labelled DEADLOCK if the cascade does not complete within 20s.
func depth2Chain(t *testing.T, tag, execMode string) {
	t.Helper()
	h := newCallbackHarness(t)

	primary := "cb-d2-" + tag + "-primary-ent"
	secondary := "cb-d2-" + tag + "-secondary-ent"
	tertiary := "cb-d2-" + tag + "-tertiary-ent"

	// tertiary: processor-less innermost joined write (only needs to Acquire the gate).
	h.SetupModelWithWorkflow(t, tertiary, secondaryWorkflow)

	// tertiaryIDs captures the id the innermost callback minted, so the test can
	// assert durability after T commits regardless of whether the inner
	// processor's returned data is applied (ASYNC_NEW_TX discards it).
	tertiaryIDs := make(chan string, 1)

	// secondary: `store` runs a processor (SYNC or ASYNC_NEW_TX) that creates a
	// tertiary in T. Its callout must release the gate or the tertiary write
	// cannot acquire it.
	secProc := "cb-d2-" + tag + "-secondary"
	h.RegisterProc(secProc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(tertiary, 1, `{"name":"t","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("tertiary create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tertiary create status=%d body=%s", res.StatusCode, res.Body)
		}
		select {
		case tertiaryIDs <- res.EntityID:
		default:
		}
		out := cloneData(rc.entityData)
		out["tertiaryId"] = res.EntityID
		return out, nil
	})
	secondaryWF := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "secondary-d2-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": %q,
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"STORED": {}
			}
		}]
	}`, secProc, execMode)
	h.SetupModelWithWorkflow(t, secondary, secondaryWF)

	// primary: `init` runs a SYNC processor that creates a secondary in T.
	primProc := "cb-d2-" + tag + "-primary"
	h.RegisterProc(primProc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"s","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("secondary create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("secondary create status=%d body=%s", res.StatusCode, res.Body)
		}
		out := cloneData(rc.entityData)
		out["secondaryId"] = res.EntityID
		return out, nil
	})
	primaryWF := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-d2-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, primProc)
	h.SetupModelWithWorkflow(t, primary, primaryWF)

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
	case <-time.After(20 * time.Second):
		t.Fatalf("DEADLOCK (%s inner processor): depth-2 nested joined transition did not complete "+
			"within 20s — a joined callback is holding the txgate across engine.Execute (H3 invariant "+
			"not upheld for the joined path); the third-level joined write cannot acquire the gate", execMode)
	}

	if r.status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", r.status, r.body)
	}
	if got := h.GetEntityData(t, r.id)["secondaryId"]; got == nil || got == "" {
		t.Fatal("primary missing secondaryId — depth-2 cascade did not complete")
	}

	// The innermost joined write actually happened and committed atomically with T.
	select {
	case tid := <-tertiaryIDs:
		if st, code := h.GetEntityState(t, tid); code != http.StatusOK {
			t.Errorf("tertiary %s GET after commit: http %d; want 200 (joined write must be durable)", tid, code)
		} else if st != "STORED" {
			t.Errorf("tertiary %s state = %q; want STORED", tid, st)
		}
	default:
		t.Fatal("inner processor never created a tertiary — depth-2 cascade did not reach the third level")
	}
}

// TestCallback_Depth2NestedJoin_Deadlocks proves a depth-2 nested joined cascade
// whose inner (secondary) processor is SYNC completes instead of deadlocking on
// the per-tx gate. This is the issue #410 reproduction: it deadlocks at ~20s
// before the fix (the joined callback held the gate across engine.Execute) and
// passes after.
func TestCallback_Depth2NestedJoin_Deadlocks(t *testing.T) {
	depth2Chain(t, "sync", "SYNC")
}

// TestCallback_Depth2NestedJoin_AsyncNewTx proves the same H3 invariant for an
// ASYNC_NEW_TX inner processor: executeAsyncNewTx dispatches inside a savepoint
// on the SAME (gated) txID, so it too must suspend the gate across its callout —
// otherwise the third-level joined write deadlocks identically.
func TestCallback_Depth2NestedJoin_AsyncNewTx(t *testing.T) {
	depth2Chain(t, "asyncnewtx", "ASYNC_NEW_TX")
}

// TestCallback_Depth2NestedJoin_FunctionCriterion proves the H3 invariant for
// the OTHER external-dispatch site: a FUNCTION criterion. The secondary's `store`
// transition is gated by a FUNCTION criterion whose evaluation (evaluateCriterion
// → DispatchCriteria) issues a joined callback that creates a tertiary in T. The
// criterion dispatch must suspend the gate across its callout or the third-level
// joined write deadlocks exactly like the processor paths.
func TestCallback_Depth2NestedJoin_FunctionCriterion(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-d2-crit-primary-ent"
	const secondary = "cb-d2-crit-secondary-ent"
	const tertiary = "cb-d2-crit-tertiary-ent"

	h.SetupModelWithWorkflow(t, tertiary, secondaryWorkflow)

	tertiaryIDs := make(chan string, 1)

	// secondary's `store` transition is guarded by a FUNCTION criterion that
	// creates a tertiary in T during evaluation, then matches (true).
	h.RegisterCriteria("cb-d2-crit-secondary", func(rc *reqCtx) (bool, error) {
		res, err := rc.CreateEntity(tertiary, 1, `{"name":"t","amount":1,"status":"new"}`)
		if err != nil {
			return false, fmt.Errorf("tertiary create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return false, fmt.Errorf("tertiary create status=%d body=%s", res.StatusCode, res.Body)
		}
		select {
		case tertiaryIDs <- res.EntityID:
		default:
		}
		return true, nil
	})
	secondaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "secondary-crit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false,
					"criterion": {"type": "function", "function": {"name": "cb-d2-crit-secondary",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}}
				}]},
				"STORED": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWF)

	h.RegisterProc("cb-d2-crit-primary", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"s","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("secondary create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("secondary create status=%d body=%s", res.StatusCode, res.Body)
		}
		out := cloneData(rc.entityData)
		out["secondaryId"] = res.EntityID
		return out, nil
	})
	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-crit-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-d2-crit-primary", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

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
	case <-time.After(20 * time.Second):
		t.Fatal("DEADLOCK (FUNCTION criterion): depth-2 nested joined transition did not complete " +
			"within 20s — a joined callback is holding the txgate across the criterion dispatch")
	}

	if r.status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", r.status, r.body)
	}
	if got := h.GetEntityData(t, r.id)["secondaryId"]; got == nil || got == "" {
		t.Fatal("primary missing secondaryId — depth-2 cascade did not complete")
	}
	select {
	case tid := <-tertiaryIDs:
		if st, code := h.GetEntityState(t, tid); code != http.StatusOK {
			t.Errorf("tertiary %s GET after commit: http %d; want 200 (joined write must be durable)", tid, code)
		} else if st != "STORED" {
			t.Errorf("tertiary %s state = %q; want STORED", tid, st)
		}
	default:
		t.Fatal("criterion never created a tertiary — depth-2 cascade did not reach the third level")
	}
}
