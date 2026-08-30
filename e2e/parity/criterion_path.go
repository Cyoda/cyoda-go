package parity

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// criterion_path.go pins the workflow-criterion twin of the condition jsonPath
// grammar: a criterion addresses entity data with the same model syntax a
// search condition does, and a path outside the grammar is refused when the
// workflow is imported.

// criterionPathWorkflow builds a workflow whose CREATED state has one
// automated transition to ADVANCED, guarded by a simple condition on jsonPath.
// NONE -> CREATED is unconditioned and automated, so creating an entity
// cascades through CREATED and evaluates the guarded transition in the same
// request. jsonPath is interpolated verbatim so a scenario can vary the
// spelling.
func criterionPathWorkflow(wfName, jsonPath string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": {"type":"simple","jsonPath":%q,"operatorType":"GREATER_THAN","value":50}}]},
				"ADVANCED": {}
			}
		}]
	}`, wfName, jsonPath)
}

// RunWorkflowCriterionPathRequiresJSONPathLeader pins that a criterion
// jsonPath obeys the same grammar a search condition's does, rejected at
// workflow import with 400 VALIDATION_FAILED.
//
// It belongs in parity rather than a single-backend test for the same reason
// RunSearchPathRequiresJSONPathLeader does: a criterion is stored per backend
// and read back on every write that touches its transition, so "the workflow
// was never accepted" has to hold identically everywhere — a backend that
// stored it would keep firing transitions off a path the contract says does
// not exist.
//
// The accept control is not optional. A criterion is only ever served by the
// in-process evaluator, which resolves array subscripts, so the CONDITION
// variant of the grammar applies and "$.tags[*]" / "$.arr[0]" must import.
func RunWorkflowCriterionPathRequiresJSONPathLeader(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-criterion-path-leader"
	const modelVersion = 1

	// The model declares every field the spellings below address, so a
	// rejection is about the SPELLING and never about the field's absence.
	if err := c.ImportModel(t, modelName, modelVersion,
		`{"name":"Test","amount":0,"nested":{"inner":""},"tags":[""],"arr":[0]}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}

	// Accepted: JSON Path, including the array-subscripted spellings no
	// pushdown filter can express.
	for _, path := range []string{"$.amount", "$.nested.inner", "$.tags[*]", "$.arr[0]"} {
		status, body, err := c.ImportWorkflowRaw(t, modelName, modelVersion,
			criterionPathWorkflow("criterion-path-wf", path))
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", path, err)
		}
		if status != http.StatusOK {
			t.Fatalf("criterion jsonPath %q: expected 200, got %d; body=%s", path, status, body)
		}
	}

	// Rejected: not JSON Path nomenclature.
	for _, tc := range []struct {
		label string
		path  string
	}{
		{"bare identifier", "amount"},
		{"bare dotted", "nested.inner"},
		{"leader only", "$."},
		{"bare dollar", "$"},
		{"bracket quoted", "$['amount']"},
		{"trailing dot", "$.amount."},
		{"empty segment", "$..amount"},
		// Malformed BRACKET spellings. Criterion import is grammar-only —
		// there is no schema check behind it — so before the grammar scanned
		// past the first '[', a criterion on "$.tags[" imported 200 and then
		// silently never fired.
		{"unclosed subscript", "$.tags["},
		{"unmatched close", "$.tags]"},
		{"subscript without field", "$.[0]"},
		{"empty subscript", "$.tags[]"},
		{"negative index", "$.tags[-1]"},
		{"slice", "$.tags[0:2]"},
		{"double-quoted subscript", `$.tags["x"]`},
		{"sql tail after subscript", "$.tags[0];DROP"},
	} {
		status, body, err := c.ImportWorkflowRaw(t, modelName, modelVersion,
			criterionPathWorkflow("criterion-path-wf", tc.path))
		if err != nil {
			t.Fatalf("[%s] ImportWorkflowRaw: %v", tc.label, err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("[%s] criterion jsonPath %q: expected 400, got %d; body=%s",
				tc.label, tc.path, status, body)
		}
		if !containsErrorCode(body, "VALIDATION_FAILED") {
			t.Errorf("[%s] criterion jsonPath %q: expected errorCode VALIDATION_FAILED, body=%s",
				tc.label, tc.path, body)
		}
	}
}

// RunPositionalSubscriptPathResolves pins that a path addressing one array
// element by position resolves to the value that element holds, on both the
// search surface and the workflow-criterion surface.
//
// No IN-TREE backend can push a subscripted path into its query, so each of
// them falls back to the shared in-process evaluator — and that evaluator answered
// nothing: it handed gjson a path it has no syntax for ("arr[0]", where gjson
// wants "arr.0") and looked the declared type up under a schema key that
// cannot exist ("$.arr[0]" — an array's element type is recorded once, under
// "$.arr[*]"). Either miss alone makes the leaf false for every entity, so the
// answer was an empty page for a field that holds the value.
//
// Backend-agnostic, and here rather than in a single-backend test because
// WHICH plan a query takes is per-backend: the wildcard spelling of the same
// query pushes down on some backends and not others, so "positional and
// wildcard agree" is a claim about all of them at once.
//
// The shared-evaluator argument above does NOT extend to the commercial
// backend, which self-executes search with an evaluator of its own
// (COMPATIBILITY.md, v0.8.4 row). It owes its own implementation of
// positional-subscript resolution — path resolution, declared-type lookup off
// the "$.arr[*]" schema entry, and the field-existence check — and this
// scenario is what surfaces the gap on its next dependency update rather than
// letting it pass silently. See docs/cloud-parity/path-grammar.md.
func RunPositionalSubscriptPathResolves(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-positional-subscript"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`{"name":"Test","amount":0,"status":"new","arr":[0,0]}`,
		criterionPathWorkflow("positional-subscript-wf", "$.arr[1]"))

	// Alice's element 1 is over the threshold, Bob's is under it — and their
	// element 0 values are swapped, so a rewrite that addressed the wrong
	// element inverts every assertion below rather than merely weakening it.
	aID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Alice","amount":100,"status":"active","arr":[1,100]}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion,
		`{"name":"Bob","amount":5,"status":"active","arr":[100,1]}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	// Search: each position selects exactly the entity that holds the value
	// there. The four rows are their own control — the two entities hold the
	// two values in opposite order, so a path that ignored its subscript, or
	// read the wrong element, changes the expected set rather than merely
	// weakening the assertion.
	//
	// The trailing-wildcard spelling of the same path ("$.arr[*]") is a
	// separate question — it addresses ALL the elements rather than one — and
	// has its own scenario, RunSearchTrailingWildcardPathResolves.
	for _, tc := range []struct {
		label string
		cond  string
		want  []string
	}{
		{"element 0 equals", `{"type":"simple","jsonPath":"$.arr[0]","operatorType":"EQUALS","value":1}`, []string{aID.String()}},
		{"element 1 equals", `{"type":"simple","jsonPath":"$.arr[1]","operatorType":"EQUALS","value":1}`, []string{bID.String()}},
		{"element 0 comparison", `{"type":"simple","jsonPath":"$.arr[0]","operatorType":"GREATER_THAN","value":50}`, []string{bID.String()}},
		{"element 1 comparison", `{"type":"simple","jsonPath":"$.arr[1]","operatorType":"GREATER_THAN","value":50}`, []string{aID.String()}},
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
		t.Errorf(`criterion on "$.arr[1]" with arr=[1,100] over threshold 50: Alice is %q, want ADVANCED`, alice.Meta.State)
	}
	bob, err := c.GetEntity(t, bID)
	if err != nil {
		t.Fatalf("GetEntity Bob: %v", err)
	}
	if bob.Meta.State != "CREATED" {
		t.Errorf(`criterion on "$.arr[1]" with arr=[100,1] under threshold 50: Bob is %q, want CREATED`, bob.Meta.State)
	}
}
