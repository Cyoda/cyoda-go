package e2e_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// callback_txjoin_modes_test.go — feature #287 execution-mode matrix, rows 3-6.
//
// Covers the four remaining rows of the callback E2E matrix beyond the two
// SYNC rows in callback_txjoin_test.go:
//
//   mode                        | failure semantics         | secondary outcome
//   ----------------------------|---------------------------|---------------------
//   ASYNC_NEW_TX (failure)      | savepoint rollback        | discarded; pipeline continues
//   ASYNC_NEW_TX (success)      | savepoint release         | committed with parent T
//   CBD + TX_post (true-branch) | tx rollback               | committed atomically with TX_post
//   CBD + standalone (default)  | independent               | committed independently (no token)
//
// All tests run against the full HTTP+gRPC stack (real Postgres) via the
// callback-capable compute member (callback_harness_test.go). The token
// round-trip — engine mints cyodatxtoken → gRPC calc request → member echoes
// it as X-Tx-Token → TxJoin middleware → JoinFromToken → participate — is
// exercised in every test (or proven absent in the CBD-default case).
//
// The token value is never logged (Gate 3 / spec §8-H10).

// TestCallback_AsyncNewTx_DiscardedOnProcessorFailure proves the ASYNC_NEW_TX
// savepoint semantics for the failure branch:
//
//	(a) The secondary entity the callback created inside the savepoint is
//	    DISCARDED when the processor fails (savepoint rollback).
//	(b) The pipeline CONTINUES: ASYNC_NEW_TX failure is non-fatal.
//	(c) The PRIMARY transition COMMITS: entity reaches ACTIVE without the
//	    secondary write.
//
// This distinguishes ASYNC_NEW_TX from SYNC: a SYNC failure rolls back T
// entirely (primary fails too); ASYNC_NEW_TX only rolls back the savepoint
// and lets the primary succeed.
func TestCallback_AsyncNewTx_DiscardedOnProcessorFailure(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-anytx-fail-primary"
	const secondary = "cb-anytx-fail-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	// created captures the secondary id the callback minted so the test can
	// assert it is gone after the savepoint rollback.
	created := make(chan string, 1)

	h.RegisterProc("cb-anytx-create-then-fail", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"doomed","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create: status=%d body=%s", res.StatusCode, res.Body)
		}
		created <- res.EntityID
		// Deliberately fail AFTER creating the secondary inside the savepoint.
		return nil, fmt.Errorf("processor deliberately fails after callback write")
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "anytx-fail-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-anytx-create-then-fail",
						"executionMode": "ASYNC_NEW_TX",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	// (b) Pipeline continues — POST must succeed despite processor failure.
	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("ASYNC_NEW_TX failure must not kill pipeline: expected 200, got %d: %s", status, body)
	}

	var secondaryID string
	select {
	case secondaryID = <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: callback did not run / did not create secondary entity before failing")
	}

	// (c) Primary transition committed — entity reached ACTIVE.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Errorf("primary must be ACTIVE after pipeline continues; state=%q http=%d", st, code)
	}

	// (a) Savepoint rolled back — the secondary entity the callback wrote inside
	// the savepoint scope must be absent. A 200 here means the write survived
	// the rollback and the savepoint semantics are broken.
	if st, code := h.GetEntityState(t, secondaryID); code == http.StatusOK {
		t.Fatalf("secondary %s still present after savepoint rollback (state=%q, http 200) — savepoint NOT rolled back",
			secondaryID, st)
	} else if code != http.StatusNotFound {
		t.Errorf("secondary GET after savepoint rollback: http %d; want 404", code)
	}
}

// TestCallback_AsyncNewTx_KeptOnSuccess proves the ASYNC_NEW_TX success branch:
// when the processor succeeds the savepoint is released and the callback-created
// secondary entity is committed together with the parent T.
func TestCallback_AsyncNewTx_KeptOnSuccess(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-anytx-ok-primary"
	const secondary = "cb-anytx-ok-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	created := make(chan string, 1)

	h.RegisterProc("cb-anytx-create-ok", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"child","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create: status=%d body=%s", res.StatusCode, res.Body)
		}
		created <- res.EntityID
		// ASYNC_NEW_TX mutations on the returned map are discarded by the engine;
		// the secondary entity (a DB side-effect) persists via savepoint release.
		return nil, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "anytx-ok-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-anytx-create-ok",
						"executionMode": "ASYNC_NEW_TX",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: expected 200, got %d: %s", status, body)
	}

	var secondaryID string
	select {
	case secondaryID = <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: callback did not run / did not create secondary entity")
	}

	// Primary committed in ACTIVE.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Errorf("primary state = %q (http %d); want ACTIVE", st, code)
	}

	// Secondary persists: savepoint was released and committed with T.
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK {
		t.Fatalf("secondary GET: http %d; want 200 (savepoint released, secondary committed with T)", code)
	} else if st != "STORED" {
		t.Errorf("secondary state = %q; want STORED", st)
	}
}

// TestCallback_CBDPost_JoinsTxPost proves the COMMIT_BEFORE_DISPATCH
// startNewTxOnDispatch=true branch (the "true-branch" / TX_post path):
//
//   - TX_pre commits before dispatch (entity durably in pre-callout state).
//   - The dispatcher opens TX_post and mints a token for it.
//   - The callback echoes the TX_post token (X-Tx-Token) → TxJoin middleware
//     → JoinFromToken → the write participates in TX_post.
//   - TX_post commits with both the primary apply-result and the secondary entity.
//
// This is the spec §4.4 / §16 case B (create-time entry point) counterpart
// to TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten (PUT
// entry point, realized in workflow_proc_test.go via the same harness).
func TestCallback_CBDPost_JoinsTxPost(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-cbd-post-primary"
	const secondary = "cb-cbd-post-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	created := make(chan string, 1)

	h.RegisterProc("cb-cbd-post-proc", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"joined-child","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create: status=%d body=%s", res.StatusCode, res.Body)
		}
		created <- res.EntityID
		// Echo secondary id into primary data via apply-result (CBD true-branch
		// processes mutations normally, unlike ASYNC_NEW_TX).
		out := cloneData(rc.entityData) // cloneData is defined in callback_txjoin_test.go
		out["secondaryId"] = res.EntityID
		return out, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cbd-post-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-cbd-post-proc",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": "", "startNewTxOnDispatch": true}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("CBD+TX_post create: expected 200, got %d: %s", status, body)
	}

	var secondaryID string
	select {
	case secondaryID = <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: callback did not run / did not create secondary entity")
	}

	// Primary committed in ACTIVE with secondary id recorded via apply-result.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Errorf("primary state = %q (http %d); want ACTIVE", st, code)
	}
	if got := h.GetEntityData(t, primaryID)["secondaryId"]; got != secondaryID {
		t.Errorf("primary.secondaryId = %v; want %q (TX_post lineage record missing)", got, secondaryID)
	}

	// Secondary committed atomically with TX_post.
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK {
		t.Fatalf("secondary GET: http %d; want 200 (callback write committed atomically with TX_post)", code)
	} else if st != "STORED" {
		t.Errorf("secondary state = %q; want STORED", st)
	}
}

// TestCallback_CBDDefault_RunsStandalone proves the COMMIT_BEFORE_DISPATCH
// default branch (startNewTxOnDispatch=false):
//
//   - TX_pre commits before dispatch.
//   - The dispatcher sends no transaction token (txID="" → no cyodatxtoken
//     attribute on the gRPC CloudEvent).
//   - The member's processor receives an empty token: rc.token == "".
//   - The callback is issued with NO X-Tx-Token header.
//   - The HTTP layer processes the create as a standalone operation (its own
//     Begin/Commit), NOT joining any engine transaction.
//   - The secondary entity is committed independently, before TX_post opens.
//
// This is the regression guard that the CBD-default branch does not leak a
// stale token into the dispatch or accidentally join an unrelated transaction.
func TestCallback_CBDDefault_RunsStandalone(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-cbd-default-primary"
	const secondary = "cb-cbd-default-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	created := make(chan string, 1)
	var (
		receivedToken string
		tokenMu       sync.Mutex
	)

	h.RegisterProc("cb-cbd-default-proc", func(rc *reqCtx) (map[string]any, error) {
		// Capture the token the dispatcher attached (must be empty for CBD default).
		// Gate 3: token value is never logged.
		tokenMu.Lock()
		receivedToken = rc.token
		tokenMu.Unlock()

		res, err := rc.CreateEntity(secondary, 1, `{"name":"standalone-child","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("callback create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("callback create: status=%d body=%s", res.StatusCode, res.Body)
		}
		created <- res.EntityID
		out := cloneData(rc.entityData) // cloneData is defined in callback_txjoin_test.go
		out["secondaryId"] = res.EntityID
		return out, nil
	})

	primaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cbd-default-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": "cb-cbd-default-proc",
						"executionMode": "COMMIT_BEFORE_DISPATCH",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, primary, primaryWF)

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("CBD-default create: expected 200, got %d: %s", status, body)
	}

	var secondaryID string
	select {
	case secondaryID = <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: callback did not run / did not create secondary entity")
	}

	// The processor must have received an empty token (CBD default dispatches
	// with no tx context: txID="" → no cyodatxtoken on the gRPC CloudEvent).
	tokenMu.Lock()
	tok := receivedToken
	tokenMu.Unlock()
	if tok != "" {
		t.Errorf("CBD default: processor received non-empty token; want empty (no tx context during dispatch)")
	}

	// Primary committed in ACTIVE with secondary id via apply-result.
	if st, code := h.GetEntityState(t, primaryID); code != http.StatusOK || st != "ACTIVE" {
		t.Errorf("primary state = %q (http %d); want ACTIVE", st, code)
	}

	// Secondary committed independently (standalone operation, its own Begin/Commit).
	if st, code := h.GetEntityState(t, secondaryID); code != http.StatusOK {
		t.Fatalf("secondary GET: http %d; want 200 (standalone commit must be durable)", code)
	} else if st != "STORED" {
		t.Errorf("secondary state = %q; want STORED", st)
	}
}
