package externalapi

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/externalapi/driver"
	"github.com/cyoda-platform/cyoda-go/e2e/parity"
)

func init() {
	parity.Register(
		parity.NamedTest{Name: "TransactionControl_BatchedDeleteEntitiesFinalState", Fn: RunTransactionControl_BatchedDeleteEntitiesFinalState},
		parity.NamedTest{Name: "TransactionControl_BatchedDeleteMessages", Fn: RunTransactionControl_BatchedDeleteMessages},
	)
}

// RunTransactionControl_BatchedDeleteEntitiesFinalState — batched
// deleteEntities (?transactionSize=N) final-state consistency (#379).
//
// Seeds 5 entities, deletes them with transactionSize=2 (forcing 3 batches:
// 2, 2, 1) and a condition matching all 5, then asserts only the OBSERVABLE
// final state: response counts (matched=5, removed=5, empty idToError) and
// that every entity is gone. Batch boundaries/timing are never asserted —
// the commercial backend is free to implement batching differently as long
// as the final result matches.
func RunTransactionControl_BatchedDeleteEntitiesFinalState(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	const model = "txctl-batchdel-entities"
	if err := d.CreateModelFromSample(model, 1, `{"n":0}`); err != nil {
		t.Fatalf("CreateModelFromSample: %v", err)
	}
	if err := d.LockModel(model, 1); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	ids := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		id, err := d.CreateEntity(model, 1, fmt.Sprintf(`{"n":%d}`, i))
		if err != nil {
			t.Fatalf("CreateEntity[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	cond := `{"type":"simple","jsonPath":"$.n","operatorType":"GREATER_OR_EQUAL","value":0}`
	result, err := d.DeleteEntitiesConditional(model, 1, cond, 2)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional: %v", err)
	}

	if result.MatchedCount != 5 {
		t.Errorf("MatchedCount = %d, want 5", result.MatchedCount)
	}
	if result.RemovedCount != 5 {
		t.Errorf("RemovedCount = %d, want 5", result.RemovedCount)
	}
	if len(result.IDToError) != 0 {
		t.Errorf("IDToError = %v, want empty", result.IDToError)
	}

	for _, id := range ids {
		if _, err := d.GetEntity(id); err == nil {
			t.Errorf("GetEntity(%s) succeeded after batched delete; expected 404", id)
		}
	}
}

// RunTransactionControl_BatchedDeleteMessages — batched deleteMessages
// (?transactionSize=N) response shape + final-state consistency (#379).
//
// Seeds 5 messages and deletes all 5 ids at once with transactionSize=2,
// forcing the server to page the delete into 3 batches (2, 2, 1). Asserts
// the response has one element per batch, every batch reports success,
// every requested id is accounted for across the batches, and every
// message is gone afterwards.
func RunTransactionControl_BatchedDeleteMessages(t *testing.T, fixture parity.BackendFixture) {
	t.Helper()
	d := driver.NewInProcess(t, fixture)

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		id, err := d.CreateMessage("txctl-batchdel-messages", fmt.Sprintf(`{"n":%d}`, i))
		if err != nil {
			t.Fatalf("CreateMessage[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	results, err := d.DeleteMessagesBatched(ids, 2)
	if err != nil {
		t.Fatalf("DeleteMessagesBatched: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("results = %d elements, want 3 (batches of 2,2,1)", len(results))
	}
	seen := map[string]bool{}
	for i, r := range results {
		if !r.Success {
			t.Errorf("batch[%d].Success = false, want true: %+v", i, r)
		}
		for _, id := range r.EntityIDs {
			seen[id] = true
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("expected id %s to appear across the batched response, got %+v", id, results)
		}
	}

	for _, id := range ids {
		if _, err := d.GetMessage(id); err == nil {
			t.Errorf("GetMessage(%s) succeeded after batched delete; expected 404", id)
		}
	}
}
