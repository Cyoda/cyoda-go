package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// A criterion's jsonPath is JSON Path nomenclature, exactly like a search
// condition's. Search rejects a path outside the grammar at its API boundary;
// a criterion evaluates through match.Prepare and never through
// spi.ConditionToFilter, so nothing rejected it — a bare "amount" imported
// cleanly and fired transitions. The two spellings of one model syntax
// disagreed on which paths exist.
//
// Import is where a criterion is rejected: it is the only boundary a criterion
// crosses. Failing at evaluation time instead would fail a save, repeatedly,
// long after the workflow was accepted.
//
// The CONDITION variant of the grammar is used, not the scalar one: criteria
// evaluate in memory, so an array subscript ("$.tags[*].name", "$.arr[0]") is
// resolvable and must stay valid.

func simplePathCriterion(path string) json.RawMessage {
	return json.RawMessage(`{"type":"simple","jsonPath":"` + path +
		`","operatorType":"GREATER_THAN","value":50}`)
}

func arrayPathCriterion(path string) json.RawMessage {
	return json.RawMessage(`{"type":"array","jsonPath":"` + path + `","values":["a"]}`)
}

func groupWithNestedPath(path string) json.RawMessage {
	return json.RawMessage(`{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"OPEN"},
			{
				"type": "group",
				"operator": "OR",
				"conditions": [` + string(simplePathCriterion(path)) + `]
			}
		]
	}`)
}

// invalidCriterionPaths are the spellings that are not JSON Path nomenclature.
// Kept in step with internal/domain/search's reject table — the criterion and
// the search condition share one grammar and one implementation of it.
var invalidCriterionPaths = []string{
	"amount",
	"address.city",
	"_meta.state",
	"",
	"$",
	"$.",
	".amount",
	"$.amount.",
	"$..amount",
	"$.foo bar",
	// Malformed BRACKET spellings. Criterion import is grammar-ONLY — no
	// schema check runs behind it — so while the grammar stopped scanning at
	// the first '[', a criterion on "$.a[" imported 200 and then silently
	// never fired, because the in-process evaluator resolves none of these.
	"$.a[",
	"$.a[0",
	"$.a]",
	"$.[0]",
	"$.[*]",
	"$.a[]",
	"$.a[-1]",
	"$.a[0:2]",
	"$.a[0,1]",
	"$.a[?(@.x)]",
	// JSON-escaped: these tables are spliced into a criterion JSON literal
	// verbatim, so the double quotes must survive into the parsed jsonPath
	// rather than terminating it.
	`$.a[\"x\"]`,
	"$.a['x']",
	"$.a[0];DROP",
	"$.a[0]b",
	"$.a[*].",
	"$.a[*]..b",
}

// validCriterionPaths must keep importing. Array subscripts are in the list on
// purpose: they are valid JSON Path that no pushdown filter expresses, and the
// in-process evaluator every criterion uses serves them.
var validCriterionPaths = []string{
	"$.amount",
	"$.a.b",
	"$._meta.state",
	"$.foo-bar",
	"$.tags[*].name",
	"$.arr[0]",
	"$.arr[12].a.b",
	"$.matrix[*][*]",
	"$.orders[*].lines[*].sku",
}

func TestValidateImportRequest_RejectsNonJSONPathCriterionPath(t *testing.T) {
	for _, path := range invalidCriterionPaths {
		t.Run("transition/simple/"+path, func(t *testing.T) {
			err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(simplePathCriterion(path))})
			assertRejectedPath(t, err, path)
		})
		t.Run("transition/array/"+path, func(t *testing.T) {
			err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(arrayPathCriterion(path))})
			assertRejectedPath(t, err, path)
		})
		t.Run("workflow/simple/"+path, func(t *testing.T) {
			err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithWorkflowCriterion(simplePathCriterion(path))})
			assertRejectedPath(t, err, path)
		})
		t.Run("nested-in-group/"+path, func(t *testing.T) {
			err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(groupWithNestedPath(path))})
			assertRejectedPath(t, err, path)
		})
	}
}

func assertRejectedPath(t *testing.T, err error, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("criterion jsonPath %q: expected rejection at import, got nil", path)
	}
	if !errors.Is(err, search.ErrInvalidFieldPath) {
		t.Fatalf("criterion jsonPath %q: error %v does not wrap search.ErrInvalidFieldPath", path, err)
	}
	// The message must locate the offending workflow/state/transition, the
	// same way every other import-time criterion diagnostic does.
	if !strings.Contains(err.Error(), `workflow "wf-regex"`) {
		t.Errorf("criterion jsonPath %q: error %q does not name the offending workflow", path, err)
	}
}

// validCriterionPaths is exercised against the "simple" clause shape only.
// Most of these entries (a bare path, a positional-only subscript, a path
// whose trailing segment follows a wildcard rather than being one) are NOT
// valid for the "array" clause — see invalidArrayCriterionPaths and
// TestValidateImportRequest_RejectsBareArrayCriterionPath, which own that
// clause's acceptance/rejection coverage specifically.
func TestValidateImportRequest_AcceptsJSONPathCriterionPath(t *testing.T) {
	for _, path := range validCriterionPaths {
		t.Run("transition/simple/"+path, func(t *testing.T) {
			if err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(simplePathCriterion(path))}); err != nil {
				t.Errorf("criterion jsonPath %q: expected accepted, got %v", path, err)
			}
		})
		t.Run("workflow/simple/"+path, func(t *testing.T) {
			if err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithWorkflowCriterion(simplePathCriterion(path))}); err != nil {
				t.Errorf("workflow criterion jsonPath %q: expected accepted, got %v", path, err)
			}
		})
	}
}

// A lifecycle criterion names a member of the closed meta vocabulary directly,
// not a path, so the grammar must leave it alone.
func TestValidateImportRequest_LifecycleCriterionFieldIsNotAPath(t *testing.T) {
	for _, field := range []string{"state", "creationDate", "previousTransition"} {
		crit := json.RawMessage(`{"type":"lifecycle","field":"` + field +
			`","operatorType":"EQUALS","value":"2020-01-01T00:00:00Z"}`)
		if err := validateImportRequest([]spi.WorkflowDefinition{
			wfWithTransitionCriterion(crit)}); err != nil {
			t.Errorf("lifecycle criterion field %q: expected accepted, got %v", field, err)
		}
	}
}

// A FUNCTION criterion carries no path and is dispatched to a compute member;
// it must stay exempt.
func TestValidateImportRequest_FunctionCriterionExempt(t *testing.T) {
	if err := validateImportRequest([]spi.WorkflowDefinition{
		wfWithTransitionCriterion(functionCriterion())}); err != nil {
		t.Errorf("function criterion: expected accepted, got %v", err)
	}
}

// The ArrayCondition arm of walkCriterion did not exist at all, so array
// criteria were skipped by every check the walker performs. Paths are the
// check that applies to them — an ArrayCondition carries no OperatorType, so
// the MATCHES_PATTERN compile is inapplicable by construction, and it is not a
// lifecycle clause. This pins that the walker now descends into a group to
// reach an array leaf as well.
func TestValidateImportRequest_ArrayCriterionNestedInGroup(t *testing.T) {
	crit := json.RawMessage(`{
		"type": "group",
		"operator": "AND",
		"conditions": [` + string(arrayPathCriterion("tags")) + `]
	}`)
	err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(crit)})
	assertRejectedPath(t, err, "tags")
}

// invalidArrayCriterionPaths are grammar-valid paths that a "simple" or
// "lifecycle" clause would accept, but that path-grammar.md §8 refuses for
// the "array" clause specifically: a bare path addresses the container, not
// its elements, and a positional-only path names one element rather than
// testing by position across the whole array — neither carries the trailing
// "[*]" an array clause's positional test requires.
//
// This is the same "$.tags" / "$.arr[0]" bare-vs-positional distinction
// section 8's "array" clause table draws on the search-condition surface
// (operators.go's ArrayCondition arm) — until this fix, walkCriterion never
// applied it, so a clause the search API rejects with 400 INVALID_FIELD_PATH
// still imported cleanly as a criterion: two doors, two answers for the
// identical clause.
var invalidArrayCriterionPaths = []string{
	"$.tags",         // bare — addresses the container, not its elements
	"$.arr[0]",       // positional-only — one element, not a positional test
	"$.tags[*].name", // trailing segment after the wildcard, not itself a wildcard
	"$.matrix[*][0]", // trailing subscript is a positional index, not "[*]"
	"$._meta.state",  // bare, non-array-shaped path
}

// validArrayCriterionPaths carry a genuine trailing "[*]" and must keep
// importing as an array clause.
var validArrayCriterionPaths = []string{
	"$.tags[*]",
	"$.matrix[*][*]",
	"$.orders[*].lines[*]",
}

// TestValidateImportRequest_RejectsBareArrayCriterionPath is the RED-phase
// proof for the sixth defect this batch closes: an array clause missing its
// trailing "[*]" imported cleanly as a criterion even though the identical
// clause is 400 INVALID_FIELD_PATH on the search surface.
func TestValidateImportRequest_RejectsBareArrayCriterionPath(t *testing.T) {
	for _, path := range invalidArrayCriterionPaths {
		t.Run("transition/"+path, func(t *testing.T) {
			err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(arrayPathCriterion(path))})
			assertRejectedPath(t, err, path)
		})
		t.Run("nested-in-group/"+path, func(t *testing.T) {
			crit := json.RawMessage(`{
				"type": "group",
				"operator": "AND",
				"conditions": [` + string(arrayPathCriterion(path)) + `]
			}`)
			err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(crit)})
			assertRejectedPath(t, err, path)
		})
		// The identical path is still valid for a "simple" criterion — the
		// trailing-"[*]" requirement is specific to the array clause
		// (path-grammar.md §8: a simple clause's bare path legitimately
		// addresses the container).
		t.Run("simple-clause-still-accepted/"+path, func(t *testing.T) {
			if err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(simplePathCriterion(path))}); err != nil {
				t.Errorf("path %q on a simple clause: expected accepted, got %v", path, err)
			}
		})
	}
}

// TestValidateImportRequest_AcceptsTrailingWildcardArrayCriterionPath is the
// positive control: an array clause whose path genuinely ends in "[*]"
// keeps importing.
func TestValidateImportRequest_AcceptsTrailingWildcardArrayCriterionPath(t *testing.T) {
	for _, path := range validArrayCriterionPaths {
		t.Run(path, func(t *testing.T) {
			if err := validateImportRequest([]spi.WorkflowDefinition{
				wfWithTransitionCriterion(arrayPathCriterion(path))}); err != nil {
				t.Errorf("array criterion jsonPath %q: expected accepted, got %v", path, err)
			}
		})
	}
}
