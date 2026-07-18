package e2e_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// scheduled_function_concurrency_test.go — Task 9.3 (issue #419), ISOLATED
// single-backend concurrency coverage. Per .claude/rules/test-coverage.md
// ("Concurrency/race: isolated single-backend e2e, never the shared parity
// suite. Assert consistency ... not a precise interleave.") this does NOT
// live in e2e/parity/scheduledfunction — a goroutine storm on the shared
// parity backend would destabilise unrelated scenarios. It asserts
// CONSISTENCY (exactly one armed scheduled_tasks row, with a scheduledTime
// that provably came from one coherent Function call, never a torn mix of
// two), not which goroutine wins.

// setupModelWithWorkflowAndSample is (*callbackHarness).SetupModelWithWorkflow
// with a caller-supplied sample doc instead of the shared workflowSampleModel
// — needed here because the model schema is strict about unknown fields (see
// scheduledtransition parity package's identical note) and this test's
// entities carry an extra "seq" field the shared sample doesn't declare.
func setupModelWithWorkflowAndSample(t *testing.T, h *callbackHarness, entityName, sampleDoc, workflowJSON string) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", entityName), sampleDoc, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", entityName, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", entityName), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", entityName, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", entityName), workflowJSON, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow %s: %d %s", entityName, resp.StatusCode, body)
	}
}

// TestScheduledFunction_ConcurrentReArm_OneConsistentArmedTask fires N
// parallel data-only loopback updates against the SAME entity, all
// re-triggering reconcileScheduledTasks for its schedule.function
// transition (the SAME upsert path TestScheduledFunction_ReArmOnPlainUpdate_Recomputes
// exercises serially). Each update carries a distinct "seq" that the fake
// Function reads to compute a distinct, identifiable fireAfterMs — so the
// row surviving the race can be checked for a COHERENT value (one call's
// own computed offset, bounded to the set every request could have
// produced), not silently accepted as "some number". The scheduled_tasks
// upsert key is deterministic per (tenant, entity, source state,
// transition) (spi.ScheduledTask's doc comment), so a torn write would
// surface as either more than one row, or a scheduled_time outside every
// individual request's own possible output range.
func TestScheduledFunction_ConcurrentReArm_OneConsistentArmedTask(t *testing.T) {
	h := newCallbackHarness(t)
	const model = "e2e-schedfn-concurrent-rearm"
	const n = 8
	const baseFireAfterMs = int64(600_000) // far future — never fires mid-test, whichever seq wins

	h.RegisterFunction("calcConcurrentRearm", func(rc *reqCtx) (string, map[string]any, error) {
		seq, _ := rc.entityData["seq"].(float64)
		fireAfterMs := baseFireAfterMs + int64(seq)*1000
		return "Schedule", map[string]any{"fireAfterMs": fireAfterMs}, nil
	})

	wf := scheduleFunctionWorkflowJSON("schedfn-concurrent-rearm-wf", validScheduleFunctionJSON("calcConcurrentRearm"))
	setupModelWithWorkflowAndSample(t, h, model, `{"name":"Test Order","amount":100,"status":"draft","seq":0}`, wf)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Test Order","amount":100,"status":"draft","seq":0}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: expected 200, got %d: %s", status, body)
	}
	rows := queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 1 {
		t.Fatalf("expected exactly one scheduled_tasks row after the initial arm; found %d", rows)
	}

	// n concurrent data-only loopback updates (still Open, no state change),
	// each with a distinct seq — every one re-triggers reconcileScheduledTasks
	// and re-invokes the Function, racing to upsert the SAME deterministic row.
	//
	// Workers MUST NOT call t.Fatalf/DoAuth/readBody: Go's testing contract
	// requires FailNow/Fatalf be called only from the goroutine running the
	// test (a t.Fatalf from a worker only Goexits that worker, masking the
	// failure → false green). Each worker instead records its outcome into
	// its OWN preallocated slot (no shared-state race, no mutex) via the
	// goroutine-safe h.callback (no *testing.T; returns result+error), and
	// the TEST goroutine inspects/asserts after wg.Wait() — the same safety
	// pattern callback_concurrency_test.go's fan-out uses.
	type updateOutcome struct {
		status int
		body   string
		err    error
	}
	outcomes := make([]updateOutcome, n)
	beforeUpdatesMs := time.Now().UnixMilli()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			seq := idx + 1
			payload := fmt.Sprintf(`{"name":"Test Order","amount":100,"status":"draft","seq":%d}`, seq)
			res, err := h.callback(http.MethodPut, fmt.Sprintf("/api/entity/JSON/%s", entityID), payload, "")
			outcomes[idx] = updateOutcome{status: res.StatusCode, body: res.Body, err: err}
		}(i)
	}
	wg.Wait()
	afterUpdatesMs := time.Now().UnixMilli()

	// Inspect the collected outcomes on the TEST goroutine (safe to Fatalf here).
	var successCount, conflictCount int
	for idx, o := range outcomes {
		if o.err != nil {
			t.Fatalf("concurrent loopback update %d (seq %d) transport error: %v", idx, idx+1, o.err)
		}
		switch o.status {
		case http.StatusOK:
			successCount++
		case http.StatusConflict, http.StatusPreconditionFailed:
			// 409 (transaction-level conflict) or 412 ENTITY_MODIFIED
			// (optimistic-concurrency version mismatch) — both are the
			// expected "lost the race" outcome for a losing concurrent
			// writer, never a torn write.
			conflictCount++
		default:
			t.Fatalf("concurrent loopback update %d (seq %d): unexpected status %d, body=%s", idx, idx+1, o.status, o.body)
		}
	}

	if successCount < 1 {
		t.Fatalf("expected at least one of the %d concurrent loopback updates to succeed; got %d successes, %d conflicts",
			n, successCount, conflictCount)
	}

	// Exactly one armed row must survive — no torn write (a lost delete, a
	// duplicated upsert, or two half-written rows).
	rows = queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3",
		entityID, "Open", "AutoClose")
	if rows != 1 {
		t.Fatalf("expected exactly ONE consistent armed scheduled_tasks row after %d concurrent re-arms (%d succeeded); found %d — possible torn write",
			n, successCount, rows)
	}

	// That one row's scheduled_time must be a COHERENT value: whichever
	// request actually won computed fireAfterMs = baseFireAfterMs + seq*1000
	// for its own seq in [1,n], relative to its own "now" (armMs) somewhere
	// within this update window. A torn write producing a value outside
	// every individual request's own possible [min,max] output range would
	// fail this bound.
	minScheduledTime := beforeUpdatesMs + baseFireAfterMs + 1*1000
	maxScheduledTime := afterUpdatesMs + baseFireAfterMs + int64(n)*1000
	rows = queryDB(t, "test-tenant",
		"SELECT count(*) FROM scheduled_tasks WHERE entity_id = $1 AND source_state = $2 AND transition = $3 AND scheduled_time BETWEEN $4 AND $5",
		entityID, "Open", "AutoClose", minScheduledTime, maxScheduledTime)
	if rows != 1 {
		t.Fatalf("expected the one surviving armed row's scheduled_time to fall within [%d,%d] (a coherent single-call computation); the bounded count was %d instead of 1 — possible torn write",
			minScheduledTime, maxScheduledTime, rows)
	}

	// The entity itself must remain readable and consistent (no corruption),
	// still resting in Open (the far-future arm never fires mid-test).
	if st, _ := h.GetEntityState(t, entityID); st != "Open" {
		t.Errorf("expected entity to remain in Open after the concurrent re-arm race; got %q", st)
	}
}
