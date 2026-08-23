package parity

import (
	"fmt"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunListEntitiesPagingConsistency asserts GetAllEntities' cross-backend
// paging contract without asserting a specific cross-engine id sequence —
// GetPage's per-engine canonical order is deterministic per backend but NOT
// guaranteed identical across backends (see spi.EntityStore.GetPage's doc
// comment). Three properties hold on every backend regardless of that
// order:
//
//   - Determinism: the same page request issued twice returns the same id
//     sequence.
//   - Paging self-consistency: page 0 (size N) concatenated with page 1
//     (size N) equals a single page of size 2N starting at offset 0.
//   - Set-equality: the union of every page, walked to exhaustion, is
//     exactly the full model's id set — no duplicates, no omissions across
//     a page boundary.
func RunListEntitiesPagingConsistency(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "list-paging-parity"
	const modelVersion = 1
	const pageSize = 3
	const total = 7 // 2 full pages of 3 + a trailing partial page of 1

	setupSimpleWorkflow(t, c, modelName, modelVersion)

	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id, err := c.CreateEntity(t, modelName, modelVersion, fmt.Sprintf(`{"name":"e%d","amount":%d,"status":"active"}`, i, i))
		if err != nil {
			t.Fatalf("CreateEntity %d: %v", i, err)
		}
		created[id.String()] = true
	}

	idsOf := func(page []client.EntityResult) []string {
		out := make([]string, len(page))
		for i, e := range page {
			out[i] = e.Meta.ID
		}
		return out
	}
	sameSequence := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Determinism: the same request issued twice returns the same sequence.
	page0First, err := c.ListEntitiesByModelPaged(t, modelName, modelVersion, pageSize, 0)
	if err != nil {
		t.Fatalf("ListEntitiesByModelPaged(page 0, first call): %v", err)
	}
	page0Second, err := c.ListEntitiesByModelPaged(t, modelName, modelVersion, pageSize, 0)
	if err != nil {
		t.Fatalf("ListEntitiesByModelPaged(page 0, second call): %v", err)
	}
	if !sameSequence(idsOf(page0First), idsOf(page0Second)) {
		t.Errorf("page 0 not deterministic: first=%v second=%v", idsOf(page0First), idsOf(page0Second))
	}

	page1, err := c.ListEntitiesByModelPaged(t, modelName, modelVersion, pageSize, 1)
	if err != nil {
		t.Fatalf("ListEntitiesByModelPaged(page 1): %v", err)
	}
	page2, err := c.ListEntitiesByModelPaged(t, modelName, modelVersion, pageSize, 2)
	if err != nil {
		t.Fatalf("ListEntitiesByModelPaged(page 2): %v", err)
	}

	if len(page0First) != pageSize {
		t.Fatalf("page 0 size = %d, want %d", len(page0First), pageSize)
	}
	if len(page1) != pageSize {
		t.Fatalf("page 1 size = %d, want %d", len(page1), pageSize)
	}
	if len(page2) != total-2*pageSize {
		t.Fatalf("page 2 (trailing partial) size = %d, want %d", len(page2), total-2*pageSize)
	}

	// Paging self-consistency: page 0 ++ page 1 == a single page of size
	// 2*pageSize starting at offset 0.
	doubleWide, err := c.ListEntitiesByModelPaged(t, modelName, modelVersion, 2*pageSize, 0)
	if err != nil {
		t.Fatalf("ListEntitiesByModelPaged(double-wide): %v", err)
	}
	concatenated := append(append([]string{}, idsOf(page0First)...), idsOf(page1)...)
	if !sameSequence(concatenated, idsOf(doubleWide)) {
		t.Errorf("page0++page1 = %v, want double-wide page = %v", concatenated, idsOf(doubleWide))
	}

	// Set-equality: the union of every page walked to exhaustion equals the
	// full model's id set, with no duplicates and no omissions across a
	// page boundary.
	seen := make(map[string]int, total)
	for _, page := range [][]client.EntityResult{page0First, page1, page2} {
		for _, e := range page {
			seen[e.Meta.ID]++
		}
	}
	if len(seen) != total {
		t.Errorf("paged union has %d distinct ids, want %d: %v", len(seen), total, seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("id %s appeared %d times across pages, want exactly once", id, count)
		}
		if !created[id] {
			t.Errorf("id %s from paged results was never created", id)
		}
	}
	for id := range created {
		if seen[id] == 0 {
			t.Errorf("created id %s never appeared in any page", id)
		}
	}
}
