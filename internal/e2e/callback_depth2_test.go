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
//          └─ SYNC proc cb-d2-secondary ── gate(T) STILL HELD (pre-fix) ──▶
//               joined create tertiary  ──▶ Acquire(gate(T)) BLOCKS until the
//               outer dispatch times out at 30s (pre-fix) / progresses (post-fix)
func TestCallback_Depth2NestedJoin_Deadlocks(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-d2-primary-ent"
	const secondary = "cb-d2-secondary-ent"
	const tertiary = "cb-d2-tertiary-ent"

	// tertiary: processor-less innermost joined write (only needs to Acquire the gate).
	h.SetupModelWithWorkflow(t, tertiary, secondaryWorkflow)

	// secondary: `store` runs a SYNC processor that creates a tertiary in T.
	h.RegisterProc("cb-d2-secondary", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(tertiary, 1, `{"name":"t","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("tertiary create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tertiary create status=%d body=%s", res.StatusCode, res.Body)
		}
		out := cloneData(rc.entityData)
		out["tertiaryId"] = res.EntityID
		return out, nil
	})
	secondaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "secondary-d2-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-d2-secondary", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"STORED": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWF)

	// primary: `init` runs a SYNC processor that creates a secondary in T.
	h.RegisterProc("cb-d2-primary", func(rc *reqCtx) (map[string]any, error) {
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
			"version": "1.1", "name": "primary-d2-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-d2-primary", "executionMode": "SYNC",
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

	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Fatalf("primary create: status=%d body=%s", r.status, r.body)
		}
		data := h.GetEntityData(t, r.id)
		if data["secondaryId"] == nil || data["secondaryId"] == "" {
			t.Fatal("primary missing secondaryId — depth-2 cascade did not complete")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("DEADLOCK: depth-2 nested joined transition did not complete within 20s — " +
			"a joined callback is holding the txgate across engine.Execute (H3 invariant " +
			"not upheld for the joined path); the third-level joined write cannot acquire the gate")
	}
}
