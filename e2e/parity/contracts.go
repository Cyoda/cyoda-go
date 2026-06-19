package parity

import (
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// --- Phase 4b: Distributed-safety contract tests (Tasks 4b.9-10) ---

// RunConcurrentConflictingUpdate fires two goroutines that concurrently
// attempt the same manual transition on the same entity with conflicting
// payloads. The minimum contract: the final entity state is consistent
// (one of the two written values, not the initial value or a corrupted
// merge). Whether exactly one fails or both succeed depends on the
// backend's concurrency semantics — the memory backend may allow both
// (last-write-wins) while postgres may serialize.
func RunConcurrentConflictingUpdate(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "concurrent-conflict-test"
	const modelVersion = 1

	// Setup: model with NONE→CREATED(auto) and CREATED→CREATED(manual "update").
	if err := c.ImportModel(t, modelName, modelVersion, `{"amount":0}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	wf := `{"importMode":"REPLACE","workflows":[{"version":"1.0","name":"conflict-wf","initialState":"NONE","active":true,"states":{"NONE":{"transitions":[{"name":"create","next":"CREATED","manual":false}]},"CREATED":{"transitions":[{"name":"update","next":"CREATED","manual":true}]}}}]}`
	if err := c.ImportWorkflow(t, modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	entityID, err := c.CreateEntity(t, modelName, modelVersion, `{"amount":0}`)
	if err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	// Two concurrent updates with conflicting values.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = c.UpdateEntity(t, entityID, "update", `{"amount":1}`)
	}()
	go func() {
		defer wg.Done()
		errs[1] = c.UpdateEntity(t, entityID, "update", `{"amount":2}`)
	}()
	wg.Wait()

	// At least one must succeed.
	successCount := 0
	for _, e := range errs {
		if e == nil {
			successCount++
		}
	}
	if successCount < 1 {
		t.Fatalf("both concurrent updates failed: err0=%v, err1=%v", errs[0], errs[1])
	}

	// The final entity state must be consistent — amount is one of the two
	// values, not the initial zero or a corrupted merge.
	got, err := c.GetEntity(t, entityID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	amount := got.Data["amount"]
	if amount != float64(1) && amount != float64(2) {
		t.Errorf("final amount = %v, expected 1 or 2 (not the initial 0)", amount)
	}
}

// RunConcurrentTransitionsDifferentEntities fires N concurrent entity
// creates, each triggering a workflow with the tag-with-foo processor.
// All N must succeed and each entity must have tag="foo" after creation.
// This catches partition-level contention in the dispatch and
// transaction layer.
func RunConcurrentTransitionsDifferentEntities(t *testing.T, fixture BackendFixture) {
	tenant := fixture.ComputeTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "concurrent-different-test"
	const modelVersion = 1
	const N = 10 // CI-friendly concurrency level

	// Setup with tag-with-foo processor on create transition.
	if err := c.ImportModel(t, modelName, modelVersion, `{"index":0}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	wf := `{"importMode":"REPLACE","workflows":[{"version":"1.0","name":"concurrent-wf","initialState":"NONE","active":true,"states":{"NONE":{"transitions":[{"name":"create","next":"CREATED","manual":false,"processors":[{"type":"calculator","name":"tag-with-foo","executionMode":"SYNC","config":{"attachEntity":true,"calculationNodesTags":""}}]}]},"CREATED":{}}}]}`
	if err := c.ImportWorkflow(t, modelName, modelVersion, wf); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	var wg sync.WaitGroup
	entityIDs := make([]uuid.UUID, N)
	createErrs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id, createErr := c.CreateEntity(t, modelName, modelVersion, fmt.Sprintf(`{"index":%d}`, idx))
			entityIDs[idx] = id
			createErrs[idx] = createErr
		}(i)
	}
	wg.Wait()

	for i, e := range createErrs {
		if e != nil {
			t.Errorf("CreateEntity[%d]: %v", i, e)
		}
	}

	successCount := 0
	for i, id := range entityIDs {
		if id == uuid.Nil {
			continue
		}
		got, getErr := c.GetEntity(t, id)
		if getErr != nil {
			t.Errorf("GetEntity[%d]: %v", i, getErr)
			continue
		}
		if got.Data["tag"] == "foo" {
			successCount++
		} else {
			t.Errorf("entity[%d] tag=%v, expected \"foo\"", i, got.Data["tag"])
		}
	}

	if successCount != N {
		t.Errorf("expected %d entities with tag=foo, got %d", N, successCount)
	}
}

// --- Deferred tests (Tasks 4b.11-13) ---

// TODO(#172): RunProcessorDisconnectMidFlight — requires crash-on-receive
// catalog entry that kills the compute client process, plus fixture support
// for RestartComputeClient. Deferred to v2.

// TODO(#172): RunProcessorAsyncNewTxRollback — requires update-and-fail-async
// catalog entry AND ASYNC_NEW_TX workflow semantics (savepoint). The
// ASYNC_NEW_TX semantics are not fully tested yet. Deferred to v2.

// TODO(#172): RunProcessorTimeoutBoundary — requires slow-configurable with
// duration close to the dispatch timeout, and per-workflow timeout
// configuration. Timeout behavior may differ across backends. Deferred to v2.
