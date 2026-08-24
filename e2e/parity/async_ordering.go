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
// Scope — what is and is NOT a cross-engine property:
//
//   - ASSERTED, because every backend must independently satisfy it: the
//     requested user-field key ("amount" ascending) governs the result
//     order; the result set equals the created set with no duplicates and
//     no strangers; and the order is REPEATABLE — an identical second
//     submission yields the identical id sequence, which is what the
//     entity-id tiebreaker exists to guarantee ("deterministic across
//     repeated calls on a given backend", docs/cloud-parity/search-sort.md
//     §5).
//   - NOT asserted: the concrete tie-break sequence. Canonical entity-ID
//     order is PER-ENGINE (documented on spi.EntityStore.GetPage and in
//     docs/cloud-parity/2026-08-22-async-ordering-and-list-order.md §2):
//     the in-house backends order byte-wise, the commercial backend orders
//     by its native timeuuid clustering key. A conforming backend may
//     therefore return tied "amount" rows in an order that is not
//     byte-wise ascending by id, so asserting that here would fail a
//     correct implementation. The byte-wise tie-break is pinned where it
//     is actually a contract — per engine — by internal/e2e's
//     TestE2E_AsyncSearch_OrderedAcrossPages against postgres alone.
//     RunListEntitiesPagingConsistency and
//     RunHistoryReadsChangesMetadataAndTransactionLookup draw the same
//     line.
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

	page := runSortedAsyncSearch(t, c, modelName, modelVersion, "first")

	if len(page.Content) != total {
		t.Fatalf("result count = %d, want %d", len(page.Content), total)
	}
	if int(page.Page.TotalElements) != total {
		t.Errorf("page.totalElements = %d, want %d", page.Page.TotalElements, total)
	}

	seen := make(map[string]bool, total)
	order := make([]string, 0, total)
	for i, e := range page.Content {
		if !created[e.Meta.ID] {
			t.Errorf("result[%d] id=%s was never created", i, e.Meta.ID)
		}
		if seen[e.Meta.ID] {
			t.Errorf("result[%d] id=%s appeared more than once", i, e.Meta.ID)
		}
		seen[e.Meta.ID] = true
		order = append(order, e.Meta.ID)

		amount, ok := e.Data["amount"].(float64)
		if !ok {
			t.Fatalf("result[%d] data.amount is not numeric: %v", i, e.Data["amount"])
		}
		if i == 0 {
			continue
		}
		prevAmount, _ := page.Content[i-1].Data["amount"].(float64)
		if amount < prevAmount {
			t.Fatalf("amount not ascending at index %d: %v then %v", i, prevAmount, amount)
		}
	}
	if len(seen) != total {
		t.Errorf("distinct result ids = %d, want %d", len(seen), total)
	}

	// Repeatability: the tiebreaker's cross-engine guarantee is that a
	// repeated identical request yields the identical order on the same
	// backend — a second, independent job must reproduce the first job's id
	// sequence exactly. (WITHOUT a tiebreaker the tied "amount" rows are
	// free to come back in any order, so this catches a backend that omits
	// it, without demanding a specific canonical order.)
	second := runSortedAsyncSearch(t, c, modelName, modelVersion, "second")
	if len(second.Content) != total {
		t.Fatalf("second run: result count = %d, want %d", len(second.Content), total)
	}
	for i, e := range second.Content {
		if e.Meta.ID != order[i] {
			t.Fatalf("repeated identical search returned a different order at index %d: %s then %s (full first order %v)",
				i, order[i], e.Meta.ID, order)
		}
	}
}

// runSortedAsyncSearch submits an "amount:asc" async search over the whole
// model, waits for it to succeed, and returns its result page. label
// distinguishes the two runs in failure messages.
func runSortedAsyncSearch(t *testing.T, c *client.Client, modelName string, modelVersion int, label string) client.PagedEntityResults {
	t.Helper()

	jobID, err := c.SubmitAsyncSearchSorted(t, modelName, modelVersion, sortMatchAll, []string{"amount:asc"})
	if err != nil {
		t.Fatalf("%s run: SubmitAsyncSearchSorted: %v", label, err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		status, err := c.GetAsyncSearchStatus(t, jobID)
		if err != nil {
			t.Fatalf("%s run: GetAsyncSearchStatus: %v", label, err)
		}
		if status == "SUCCESSFUL" {
			page, err := c.GetAsyncSearchResults(t, jobID)
			if err != nil {
				t.Fatalf("%s run: GetAsyncSearchResults: %v", label, err)
			}
			return page
		}
		if status == "FAILED" || status == "CANCELLED" || status == "NOT_FOUND" {
			t.Fatalf("%s run: async search reached terminal status %s (jobId=%s)", label, status, jobID)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s run: timeout waiting for async search jobId=%s (last status=%s)", label, jobID, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
