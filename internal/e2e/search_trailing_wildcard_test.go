package e2e_test

// search_trailing_wildcard_test.go — running-backend (postgres) e2e coverage for
// a condition path whose LAST hop is an array wildcard ("$.tags[*]").
//
// Such a path addresses the array's ELEMENTS. The in-memory evaluator resolved
// it to the array's COUNT instead — the gjson rewrite mapped every "[*]" to "#",
// which projects mid-path but yields the length in final position. So
// `$.tags[*] EQUALS "red"` compared "red" against 2 and answered an empty page
// for an entity that holds the tag, and a workflow criterion on such a path
// silently never fired. A wrong-but-available answer, which the project forbids.
//
// This needs a running backend rather than a unit test because the bug lived
// in the in-memory evaluator's own path rewrite, not in a boundary check a
// unit test on the validator alone would exercise — and the kernel now
// resolves a subscripted path directly (see spi.ResolvePath), so this pins
// the CORRECT resolution end to end, on a real SQL backend, rather than
// merely the absence of the old miscount.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// setupTrailingWildcardModel imports and locks a model carrying one array of
// each shape the trailing-wildcard rule distinguishes:
//
//	tags   — a primitive STRING array; "$.tags[*]" is a comparable leaf
//	items  — an array of PURE objects; "$.items[*]" is a container
//	orders — two nested array hops, so "$.orders[*].lines[*].sku" needs the
//	         projection nesting flattened back to one level
func setupTrailingWildcardModel(t *testing.T, model string) {
	t.Helper()
	importModelSampleE2E(t, model, 1, `{
		"name": "Sample",
		"tags": ["a"],
		"items": [{"sku": "x"}],
		"orders": [{"lines": [{"sku": "x"}]}]
	}`)
	lockModelE2E(t, model, 1)
	status, body := importWorkflowE2E(t, model, 1, `{
		"importMode": "REPLACE",
		"workflows": [{"version": "1.1", "name": "trailing-wildcard-wf", "initialState": "NONE", "active": true,
			"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
			           "CREATED": {}}}]
	}`)
	if status != http.StatusOK {
		t.Fatalf("workflow import for %s: expected 200, got %d: %s", model, status, body)
	}
}

// TestSearch_TrailingWildcard_SelectsByElement is the core proof: a comparison
// on "$.tags[*]" must select the entities whose array CONTAINS the value.
// Before the fix every one of these value comparisons answered zero results.
func TestSearch_TrailingWildcard_SelectsByElement(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-trailing-wildcard"
	setupTrailingWildcardModel(t, model)

	alice := createEntityE2E(t, model, 1, `{
		"name": "Alice", "tags": ["red", "blue"],
		"items": [{"sku": "A1"}],
		"orders": [{"lines": [{"sku": "L1"}, {"sku": "L2"}]}, {"lines": [{"sku": "L3"}]}]
	}`)
	bob := createEntityE2E(t, model, 1, `{
		"name": "Bob", "tags": ["green"],
		"items": [{"sku": "B1"}],
		"orders": [{"lines": [{"sku": "L9"}]}]
	}`)
	// Carol's tags array is EMPTY. Under the count rewrite its length 0 was a
	// present number, so NOT_NULL selected her; existential semantics over the
	// elements does not.
	createEntityE2E(t, model, 1, `{
		"name": "Carol", "tags": [],
		"items": [{"sku": "C1"}],
		"orders": [{"lines": [{"sku": "L8"}]}]
	}`)

	cases := []struct {
		name    string
		cond    string
		wantIDs []string
	}{
		{
			name:    "first element",
			cond:    `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"red"}`,
			wantIDs: []string{alice},
		},
		{
			name:    "second element",
			cond:    `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"blue"}`,
			wantIDs: []string{alice},
		},
		{
			name:    "other entity's element",
			cond:    `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"green"}`,
			wantIDs: []string{bob},
		},
		{
			name:    "no entity holds it",
			cond:    `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"purple"}`,
			wantIDs: nil,
		},
		{
			name:    "string operator applies per element",
			cond:    `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"CONTAINS","value":"ee"}`,
			wantIDs: []string{bob},
		},
		{
			// An EMPTY array has no element to be non-null, so Carol is out.
			name:    "presence test skips the empty array",
			cond:    `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"NOT_NULL","value":null}`,
			wantIDs: []string{alice, bob},
		},
		{
			// Two projection hops: the gjson result nests once per hop and has
			// to be flattened before the per-element comparison sees a scalar.
			name:    "two array hops, scalar leaf",
			cond:    `{"type":"simple","jsonPath":"$.orders[*].lines[*].sku","operatorType":"EQUALS","value":"L3"}`,
			wantIDs: []string{alice},
		},
		{
			name:    "two array hops, no match",
			cond:    `{"type":"simple","jsonPath":"$.orders[*].lines[*].sku","operatorType":"EQUALS","value":"L0"}`,
			wantIDs: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, results := directSearch(t, model, 1, tc.cond)
			if status != http.StatusOK {
				t.Fatalf("expected 200, got %d", status)
			}
			got := make(map[string]bool, len(results))
			for _, r := range results {
				got[resultMetaID(t, r)] = true
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d results, want %d", len(got), len(tc.wantIDs))
			}
			for _, want := range tc.wantIDs {
				if !got[want] {
					t.Errorf("entity %s missing from the result set", want)
				}
			}
		})
	}
}

// A trailing wildcard on an array of PURE objects addresses an element that has
// substructure and no scalar form, so comparing it to a scalar could only ever
// be false. That is refused at the boundary — 400 INVALID_FIELD_PATH — rather
// than answered with an empty page. The rule is the model's, not the grammar's:
// the same spelling on the primitive array above is valid.
func TestSearch_TrailingWildcard_OnObjectArray_Returns400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	const model = "e2e-search-trailing-wildcard-object"
	setupTrailingWildcardModel(t, model)
	createEntityE2E(t, model, 1, `{
		"name": "Alice", "tags": ["red"],
		"items": [{"sku": "A1"}],
		"orders": [{"lines": [{"sku": "L1"}]}]
	}`)

	for _, tc := range []struct {
		name string
		cond string
	}{
		{"equality", `{"type":"simple","jsonPath":"$.items[*]","operatorType":"EQUALS","value":"A1"}`},
		{"string operator", `{"type":"simple","jsonPath":"$.items[*]","operatorType":"CONTAINS","value":"A1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fmt.Sprintf("/api/search/direct/%s/1", model)
			resp := doAuth(t, http.MethodPost, path, tc.cond)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", resp.StatusCode, readBody(t, resp))
			}
			commontest.ExpectErrorCode(t, resp, "INVALID_FIELD_PATH")
		})
	}

	// The positive control that keeps the rejection honest: the leaf BENEATH the
	// container is the path the caller should have written, and it works.
	status, results := directSearch(t, model, 1,
		`{"type":"simple","jsonPath":"$.items[*].sku","operatorType":"EQUALS","value":"A1"}`)
	if status != http.StatusOK || len(results) != 1 {
		t.Fatalf(`"$.items[*].sku" EQUALS "A1": status %d with %d results, want 200 with 1`, status, len(results))
	}
}
