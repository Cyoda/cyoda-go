package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// trailing_wildcard_path.go pins what a path whose LAST hop is an array
// wildcard means: it addresses the array's ELEMENTS, and a leaf on it holds
// when SOME element satisfies it.

// RunSearchTrailingWildcardPathResolves pins that "$.arr[*]" resolves to the
// array's elements on both the search surface and the workflow-criterion
// surface.
//
// No in-tree backend can push a subscripted path into its query, so each of
// them falls back to the shared in-process evaluator — and that evaluator
// answered from the array's LENGTH instead of its elements: the gjson rewrite
// mapped every "[*]" to "#", which projects mid-path but yields the count in
// final position. So a comparison on "$.arr[*]" compared against 2, an empty
// page for a field that holds the value, and a criterion on it silently never
// fired.
//
// Backend-agnostic, and here rather than in a single-backend test because WHICH
// plan a query takes is per-backend: a backend that grew a pushdown for
// subscripted paths would answer from its own evaluator, so "the elements
// decide" is a claim about all of them at once.
//
// The shared-evaluator argument does NOT extend to the commercial backend,
// which self-executes search with an evaluator of its own (COMPATIBILITY.md,
// v0.8.4 row). It owes its own element-wise resolution of a trailing wildcard,
// and this scenario is what surfaces the gap on its next dependency update
// rather than letting it pass silently. See
// docs/cloud-parity/path-grammar.md.
func RunSearchTrailingWildcardPathResolves(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-trailing-wildcard"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`{"name":"Test","amount":0,"status":"new","tags":[""],"arr":[0,0]}`,
		criterionPathWorkflow("trailing-wildcard-wf", "$.arr[*]"))

	// Alice holds a value over the criterion threshold in her arr; Bob does
	// not. Both arrays are two elements wide, and 2 is under the threshold, so
	// a length-valued leaf fires for neither — which is what makes Alice's
	// ADVANCED state below evidence rather than a coincidence.
	aID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Alice","amount":100,"status":"active","tags":["red","blue"],"arr":[1,100]}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Bob","amount":5,"status":"active","tags":["green"],"arr":[1,2]}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// Search: each value selects exactly the entity whose array contains it.
	// Alice holds two tags and Bob one, so a leaf answering from the length
	// would separate them on a numeric comparison and never on a tag value —
	// the rows below invert under that reading rather than merely weakening.
	for _, tc := range []struct {
		label string
		cond  string
		want  []string
	}{
		{"first element", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"red"}`, []string{aID.String()}},
		{"second element", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"blue"}`, []string{aID.String()}},
		{"other entity's element", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"green"}`, []string{bID.String()}},
		{"no entity holds it", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"purple"}`, nil},
		{"string operator per element", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"CONTAINS","value":"ee"}`, []string{bID.String()}},
		{"numeric comparison per element", `{"type":"simple","jsonPath":"$.arr[*]","operatorType":"GREATER_THAN","value":50}`, []string{aID.String()}},
	} {
		status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearchRaw: %v", tc.label, err)
		}
		if status != http.StatusOK {
			t.Fatalf("[%s] expected 200, got %d; body=%s", tc.label, status, body)
		}
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.cond)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.label, err)
		}
		assertResultIDSet(t, tc.label, results, tc.want)
	}

	// Criterion: the same path guards a transition, so Alice advances and Bob
	// does not. This is the arm with no fallback at all — a criterion is only
	// ever served by the in-process evaluator.
	alice, err := c.GetEntity(t, aID)
	if err != nil {
		t.Fatalf("GetEntity Alice: %v", err)
	}
	if alice.Meta.State != "ADVANCED" {
		t.Errorf(`criterion on "$.arr[*]" with arr=[1,100] over threshold 50: Alice is %q, want ADVANCED`, alice.Meta.State)
	}
	bob, err := c.GetEntity(t, bID)
	if err != nil {
		t.Fatalf("GetEntity Bob: %v", err)
	}
	if bob.Meta.State != "CREATED" {
		t.Errorf(`criterion on "$.arr[*]" with arr=[1,2] under threshold 50: Bob is %q, want CREATED`, bob.Meta.State)
	}
}
