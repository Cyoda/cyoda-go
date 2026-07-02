package e2e_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// callback_txjoin_test.go — feature #287, spec §7 (SYNC callback) coverage-matrix
// rows "SYNC callback write is atomic with T" and "SYNC callback read sees T's
// uncommitted cascade write", proven end-to-end over the full HTTP+gRPC stack
// against real Postgres via the callback-capable compute member
// (callback_harness_test.go).
//
// These are the FIRST tests that exercise the real token round-trip: engine
// mints cyodatxtoken -> gRPC calc request -> member echoes it as X-Tx-Token ->
// HTTP TxJoin middleware -> JoinFromToken -> participate. The localproc harness
// used by the other workflow tests cannot reach this path (it never goes over
// gRPC, so no token exists).

// secondaryWorkflow: a trivial two-state workflow (NONE -store-> STORED) with no
// processors, so creating a secondary entity from inside a callback runs a
// minimal cascade within the joined transaction T and never dispatches a nested
// processor (which would contend the per-tx gate).
const secondaryWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "secondary-wf", "initialState": "NONE", "active": true,
		"states": {
			"NONE":   {"transitions": [{"name": "store", "next": "STORED", "manual": false}]},
			"STORED": {}
		}
	}]
}`

// TestCallback_SyncWrite_AtomicWithTransition proves the core #287 invariant: a
// SYNC processor whose callback CREATES a secondary entity has that write bound
// to the primary transition's transaction T. On success both are durable; on
// processor failure the secondary is rolled back atomically with T.
//
// Before this feature the callback opened its OWN transaction, so the secondary
// write survived an aborted primary — this test would then fail on the failure
// branch (secondary still present). It therefore genuinely exercises the join.
func TestCallback_SyncWrite_AtomicWithTransition(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-atomic-primary"
	const secondary = "cb-atomic-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	// --- success branch ---
	// created captures the secondary id the callback minted, so the test can
	// look it up afterwards regardless of the transition outcome.
	createdOK := make(chan string, 1)
	h.RegisterProc("cb-create-ok", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"child","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", res.StatusCode, res.Body)
		}
		createdOK <- res.EntityID
		// Echo the secondary id into the primary's data so we can assert lineage.
		out := cloneData(rc.entityData)
		out["secondaryId"] = res.EntityID
		return out, nil
	})

	primaryWF := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-ok-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-create-ok", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`)
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create (success branch): status=%d body=%s", status, body)
	}
	var secondaryID string
	select {
	case secondaryID = <-createdOK:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: callback did not run / did not create secondary entity")
	}

	// Primary committed in ACTIVE with the secondary id recorded.
	if st, code := h.GetEntityState(t, primaryID); st != "ACTIVE" {
		t.Fatalf("primary state = %q (http %d); want ACTIVE", st, code)
	}
	if got := h.GetEntityData(t, primaryID)["secondaryId"]; got != secondaryID {
		t.Errorf("primary.secondaryId = %v; want %q", got, secondaryID)
	}
	// Secondary is durable: the callback write committed atomically with T.
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK {
		t.Fatalf("secondary GET after success: http %d; want 200 (callback write must be durable)", code)
	} else if st != "STORED" {
		t.Errorf("secondary state = %q; want STORED", st)
	}

	// --- failure branch ---
	// The processor creates a secondary, then FAILS. T must roll back, taking
	// the secondary with it.
	createdFail := make(chan string, 1)
	h.RegisterProc("cb-create-then-fail", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"doomed","amount":2,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", res.StatusCode, res.Body)
		}
		createdFail <- res.EntityID
		return nil, fmt.Errorf("processor deliberately fails after issuing callback")
	})

	const primaryFail = "cb-atomic-primary-fail"
	primaryFailWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-create-then-fail", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primaryFail, primaryFailWF)

	failID, failStatus, failBody := h.CreateEntity(t, primaryFail, 1, `{"name":"parent2","amount":100,"status":"new"}`)
	// The transition failed: the create must not report a committed 200 success.
	if failStatus == http.StatusOK {
		t.Fatalf("primary create (failure branch) unexpectedly succeeded: %s", failBody)
	}
	_ = failID // empty on failure — nothing to look up

	var doomedSecondaryID string
	select {
	case doomedSecondaryID = <-createdFail:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: failure-branch callback did not run / did not create secondary entity")
	}

	// THE ATOMICITY PROOF: the secondary the callback created inside T must be
	// gone, because the primary transition aborted and T rolled back. If the
	// callback had run in its own transaction (pre-#287), this GET would be 200.
	if st, code := h.GetEntityState(t, doomedSecondaryID); code == http.StatusOK {
		t.Fatalf("rolled-back secondary %s is still present (state=%q, http 200) — callback write was NOT atomic with T",
			doomedSecondaryID, st)
	} else if code != http.StatusNotFound {
		t.Errorf("secondary GET after rollback: http %d; want 404 (rolled back with T)", code)
	}
}

// TestCallback_SyncRead_SeesUncommittedCascadeWrite proves read-your-own-writes
// across the callback boundary: a SYNC processor creates a secondary entity
// inside T (uncommitted), then READS it back through a second joined callback and
// observes it — a read that would 404 if it ran outside T. The observation is
// echoed into the primary's committed data and asserted after commit.
func TestCallback_SyncRead_SeesUncommittedCascadeWrite(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-read-primary"
	const secondary = "cb-read-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	const marker = "in-flight-marker-42"

	h.RegisterProc("cb-read", func(rc *reqCtx) (map[string]any, error) {
		// 1. Create a secondary entity inside T (uncommitted).
		created, err := rc.CreateEntity(secondary, 1, fmt.Sprintf(`{"name":"child","amount":7,"status":%q}`, marker))
		if err != nil {
			return nil, fmt.Errorf("callback create failed: %w", err)
		}
		if created.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create status=%d body=%s", created.StatusCode, created.Body)
		}

		// 2. Read it back through a joined callback. Outside T this 404s; inside
		//    T it returns the uncommitted row (read-your-own-writes).
		got, err := rc.GetEntity(created.EntityID)
		if err != nil {
			return nil, fmt.Errorf("callback read failed: %w", err)
		}

		out := cloneData(rc.entityData)
		out["readbackStatus"] = float64(got.StatusCode)
		out["readbackFound"] = got.StatusCode == http.StatusOK
		// Surface the marker the joined read observed (the secondary's data.status
		// field, which the create set to `marker`). Empty if the read 404'd.
		out["readbackMarker"] = entityDataField(got.Body, "status")
		out["secondaryId"] = created.EntityID
		return out, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "primary-read-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-read", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}

	data := h.GetEntityData(t, primaryID)
	if found, _ := data["readbackFound"].(bool); !found {
		t.Fatalf("joined callback read did NOT see the uncommitted secondary write: readbackStatus=%v (read-your-writes across the callback boundary is broken)",
			data["readbackStatus"])
	}
	if got := data["readbackMarker"]; got != marker {
		t.Errorf("joined read observed status=%v; want the uncommitted marker %q", got, marker)
	}

	// After commit, the secondary is durable with its marker (belt-and-braces).
	secondaryID, _ := data["secondaryId"].(string)
	if secondaryID == "" {
		t.Fatal("primary data missing secondaryId")
	}
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK {
		t.Fatalf("secondary GET after commit: http %d; want 200", code)
	} else if st != "STORED" {
		t.Errorf("secondary state = %q; want STORED", st)
	}
}
