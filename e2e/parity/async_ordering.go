package parity

import (
	"fmt"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunAsyncOrderingRespected (task E7.2, design §9 row 19: "Async result
// ordering respected end-to-end") submits an async search with an explicit
// user-field sort key over a model seeded with repeated values (forcing
// entity-ID tie-breaks) and asserts the result order is correct on
// whichever backend fixture is passed in.
//
// Deliberately NOT a hardcoded golden ID sequence and NOT a cross-backend
// sequence compare: GetPage/Iterate's canonical entity-ID order is
// per-engine (documented on spi.EntityStore.GetPage), so this asserts the
// same set+pairwise-key properties every backend must independently
// satisfy — ascending "amount", and ascending entity id as the tie-break
// within a tied "amount" — using each backend's own actual created IDs, the
// same technique internal/e2e's TestE2E_AsyncSearch_OrderedAcrossPages uses
// against postgres alone.
func RunAsyncOrderingRespected(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-async-ordering"
	const modelVersion = 1
	setupSortModel(t, c, modelName, modelVersion)

	const total = 9
	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		amount := i % 3 // ties: 0,1,2,0,1,2,0,1,2
		doc := fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"new"}`, i, amount)
		id, err := c.CreateEntity(t, modelName, modelVersion, doc)
		if err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
		created[id.String()] = true
	}

	jobID, err := c.SubmitAsyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"amount:asc"})
	if err != nil {
		t.Fatalf("SubmitAsyncSearchSorted: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var page client.PagedEntityResults
	for {
		status, err := c.GetAsyncSearchStatus(t, jobID)
		if err != nil {
			t.Fatalf("GetAsyncSearchStatus: %v", err)
		}
		if status == "SUCCESSFUL" {
			page, err = c.GetAsyncSearchResults(t, jobID)
			if err != nil {
				t.Fatalf("GetAsyncSearchResults: %v", err)
			}
			break
		}
		if status == "FAILED" || status == "CANCELLED" || status == "NOT_FOUND" {
			t.Fatalf("async search reached terminal status %s (jobId=%s)", status, jobID)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for async search jobId=%s (last status=%s)", jobID, status)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(page.Content) != total {
		t.Fatalf("result count = %d, want %d", len(page.Content), total)
	}
	if int(page.Page.TotalElements) != total {
		t.Errorf("page.totalElements = %d, want %d", page.Page.TotalElements, total)
	}

	seen := make(map[string]bool, total)
	for i, e := range page.Content {
		if !created[e.Meta.ID] {
			t.Errorf("result[%d] id=%s was never created", i, e.Meta.ID)
		}
		if seen[e.Meta.ID] {
			t.Errorf("result[%d] id=%s appeared more than once", i, e.Meta.ID)
		}
		seen[e.Meta.ID] = true

		amount, ok := e.Data["amount"].(float64)
		if !ok {
			t.Fatalf("result[%d] data.amount is not numeric: %v", i, e.Data["amount"])
		}
		if i == 0 {
			continue
		}
		prevAmount, _ := page.Content[i-1].Data["amount"].(float64)
		prevID := page.Content[i-1].Meta.ID
		if amount < prevAmount {
			t.Fatalf("amount not ascending at index %d: %v then %v", i, prevAmount, amount)
		}
		if amount == prevAmount && e.Meta.ID <= prevID {
			t.Fatalf("tie-break not ascending id at index %d: %s then %s (amount=%v)", i, prevID, e.Meta.ID, amount)
		}
	}
	if len(seen) != total {
		t.Errorf("distinct result ids = %d, want %d", len(seen), total)
	}
}
