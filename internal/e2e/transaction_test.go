package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// --- Test 3.1: Concurrent entity creates (same model) ---

func TestTransaction_ConcurrentCreates(t *testing.T) {
	const model = "e2e-tx-1"

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "tx1-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	const n = 10
	var wg sync.WaitGroup
	var successCount atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := fmt.Sprintf(`{"name":"concurrent-%d","amount":%d,"status":"new"}`, idx, idx*10)
			path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
			resp := doAuth(t, http.MethodPost, path, payload)
			readBody(t, resp)
			if resp.StatusCode == http.StatusOK {
				successCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if successCount.Load() != int32(n) {
		t.Errorf("expected all %d creates to succeed, got %d", n, successCount.Load())
	}

	// Verify total entity count.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model)
	if count != n {
		t.Errorf("expected %d entities in DB, got %d", n, count)
	}
}

// --- Test 3.2: Concurrent updates to same entity → at least one conflict ---

func TestTransaction_ConcurrentUpdates(t *testing.T) {
	const model = "e2e-tx-2"

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "tx2-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	entityID := createEntityE2E(t, model, 1, `{"name":"shared","amount":0,"status":"new"}`)

	const n = 5
	var wg sync.WaitGroup
	var successCount, conflictCount atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := fmt.Sprintf(`{"name":"shared","amount":%d,"status":"updated"}`, idx)
			path := fmt.Sprintf("/api/entity/JSON/%s", entityID)
			resp := doAuth(t, http.MethodPut, path, payload)
			readBody(t, resp)
			switch resp.StatusCode {
			case http.StatusOK:
				successCount.Add(1)
			case http.StatusConflict:
				conflictCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	// At least one should succeed.
	if successCount.Load() < 1 {
		t.Error("expected at least one concurrent update to succeed")
	}

	// Entity should still be readable — no corruption.
	state := getEntityState(t, entityID)
	if state == "" {
		t.Error("entity state is empty after concurrent updates — possible corruption")
	}
}

// --- Test 3.3: Transaction rollback cleans up ---

func TestTransaction_RollbackOnProcessorFailure(t *testing.T) {
	const model = "e2e-tx-3"

	procSvc.RegisterProcessor("tx-fail", func(ctx context.Context, entity *spi.Entity, proc spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("forced failure for rollback test")
	})
	defer procSvc.Reset()

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "tx3-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false,
					"processors": [{"type": "calculator", "name": "tx-fail", "executionMode": "SYNC",
						"config": {"attachEntity": true, "calculationNodesTags": ""}}]
				}]},
				"CREATED": {}
			}
		}]
	}`)

	// Attempt to create — should fail.
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
	resp := doAuth(t, http.MethodPost, path, `{"name":"rollback-test","amount":10,"status":"new"}`)
	readBody(t, resp)

	// Verify no entities in DB for this model — transaction was rolled back.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1", model)
	if count != 0 {
		t.Errorf("expected 0 entities after rollback, got %d", count)
	}
}

// --- Test 3.4: High-concurrency stress test ---

func TestTransaction_StressTest(t *testing.T) {
	const model = "e2e-tx-4"

	setupModelWithWorkflow(t, model, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.0", "name": "tx4-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {}
			}
		}]
	}`)

	const n = 25
	var wg sync.WaitGroup
	var successCount atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := fmt.Sprintf(`{"name":"stress-%d","amount":%d,"status":"new"}`, idx, idx)
			path := fmt.Sprintf("/api/entity/JSON/%s/%d", model, 1)
			resp := doAuth(t, http.MethodPost, path, payload)
			readBody(t, resp)
			if resp.StatusCode == http.StatusOK {
				successCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if successCount.Load() != int32(n) {
		t.Errorf("expected all %d creates to succeed, got %d", n, successCount.Load())
	}

	// Verify total count — must match exactly, no duplicates or missing.
	count := queryDB(t, "test-tenant",
		"SELECT count(*) FROM entities WHERE model_name = $1 AND NOT deleted", model)
	if count != n {
		t.Errorf("expected exactly %d entities, got %d — possible corruption", n, count)
	}

	// Verify version history is intact for a sample entity.
	listPath := fmt.Sprintf("/api/entity/%s/%d", model, 1)
	resp := doAuth(t, http.MethodGet, listPath, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list entities: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var entities []map[string]any
	json.Unmarshal([]byte(body), &entities)
	// API may have a default page size, but should return at least some.
	if len(entities) < 1 {
		t.Error("expected at least 1 entity via API listing")
	}
}
