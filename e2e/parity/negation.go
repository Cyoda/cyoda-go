package parity

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// negation.go covers the NOT-node plan's Task 13 cross-backend guard: every
// search-condition surface (sync search, conditional delete) answers a
// GroupCondition{Operator:"NOT"} identically on memory/sqlite/postgres (and
// any commercial backend the same registry is run against). Single-backend
// HTTP coverage (arity errors, error codes, job-not-issued guarantees) lives
// in internal/e2e/search_group_operator_not_test.go and
// internal/e2e/delete_not_temporal_typecheck_test.go; this file's job is
// agreement across backends on what NOT actually MATCHES, which no
// single-backend test can guard — see search_type_directed.go's identical
// framing for the type-directed kernel.

// notCond wraps inner (one condition-JSON fragment) in a NOT group. Mirrors
// internal/e2e's notCondition helper; kept package-local here because this
// package builds condition JSON as raw string literals throughout (see
// search.go, search_empty_group.go).
func notCond(inner string) string {
	return `{"type":"group","operator":"NOT","conditions":[` + inner + `]}`
}

// RunSearchNotOverSimpleCondition pins NOT wrapping a single SimpleCondition
// leaf: the textual inversion of a scalar comparison, matching every entity
// the wrapped leaf does NOT match.
func RunSearchNotOverSimpleCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-simple"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	aliceID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	bobID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"inactive"}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}

	cond := notCond(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"inactive"}`)
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch NOT(status==inactive): %v", err)
	}
	assertResultIDSet(t, "NOT(status==inactive)", results, []string{aliceID.String()})

	// The complementary NOT must select the other entity, proving this isn't
	// a group operator silently folded to AND (which would answer the SAME
	// single row for both directions on this 2-entity fixture).
	cond2 := notCond(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"}`)
	results2, err := c.SyncSearch(t, modelName, modelVersion, cond2)
	if err != nil {
		t.Fatalf("SyncSearch NOT(status==active): %v", err)
	}
	assertResultIDSet(t, "NOT(status==active)", results2, []string{bobID.String()})
}

// RunSearchNotOverAndGroup pins NOT wrapping an AND group: De Morgan's answer
// (NOT(A AND B) matches whenever at least one of A, B fails), not a folded
// single-child read.
func RunSearchNotOverAndGroup(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-and"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// Alice: active AND >75 -> AND is true -> excluded by NOT.
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	// Bob: active AND <=75 -> AND false (amount fails) -> included by NOT.
	bobID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}
	// Carol: inactive AND >75 -> AND false (status fails) -> included by NOT.
	carolID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","amount":200,"status":"inactive"}`)
	if err != nil {
		t.Fatalf("CreateEntity Carol: %v", err)
	}

	cond := notCond(`{
		"type": "group",
		"operator": "AND",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"},
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":75}
		]
	}`)
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch NOT(AND(...)): %v", err)
	}
	assertResultIDSet(t, "NOT(status==active AND amount>75)", results, []string{bobID.String(), carolID.String()})
}

// RunSearchNotOverOrGroup pins NOT wrapping an OR group: De Morgan's answer
// (NOT(A OR B) matches only when BOTH A and B fail).
func RunSearchNotOverOrGroup(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-or"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	// Alice: active OR >150 -> OR true (status) -> excluded by NOT.
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":50,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	// Bob: inactive OR >150 (amount=200) -> OR true (amount) -> excluded.
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":200,"status":"inactive"}`); err != nil {
		t.Fatalf("CreateEntity Bob: %v", err)
	}
	// Carol: inactive AND amount<=150 -> OR false on both -> included by NOT.
	carolID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","amount":50,"status":"inactive"}`)
	if err != nil {
		t.Fatalf("CreateEntity Carol: %v", err)
	}

	cond := notCond(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"},
			{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":150}
		]
	}`)
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch NOT(OR(...)): %v", err)
	}
	assertResultIDSet(t, "NOT(status==active OR amount>150)", results, []string{carolID.String()})
}

// RunSearchNotUniversalQuantifierOverWildcard pins the divergence
// docs/cloud-parity and internal/match.TestPrepare_NotOverWildcardIsUniversal
// call out explicitly, now at the HTTP layer across every backend: NOT over a
// wildcard leaf negates "SOME element matches" into "NO element matches" (a
// universal quantifier), which also gives vacuous truth for free — an empty
// array, an explicit null, and an absent field all resolve no elements, so
// the leaf is existentially false and NOT is true for all three, with no
// special-casing. This is the "NOT over empty/null/absent list" row: all
// three vacuity states are asserted (the explicit-null case specifically,
// because an earlier review found the shipped unit test only pinned empty
// array and absent field).
func RunSearchNotUniversalQuantifierOverWildcard(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-wildcard-universal"
	const modelVersion = 1
	// Two samples (one populated, one null) make "tags" nullable so an
	// explicit null can be stored later — see RunPathVacuity's identical
	// setup rationale in path_addressing.go.
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`[{"name":"seed","tags":["red"]},{"name":"seed2","tags":null}]`, searchWorkflowJSON)

	// SOME element is "red" -> the child leaf is true -> NOT is false. Not
	// asserted by ID here: its absence from the result set below IS the
	// assertion (assertResultIDSet fails on any unexpected extra result).
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"someRed","tags":["red","blue"]}`); err != nil {
		t.Fatalf("CreateEntity someRed: %v", err)
	}
	// NO element is "red" -> the child leaf is false -> NOT is true.
	noRedID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"noRed","tags":["blue"]}`)
	if err != nil {
		t.Fatalf("CreateEntity noRed: %v", err)
	}
	emptyID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"empty","tags":[]}`)
	if err != nil {
		t.Fatalf("CreateEntity empty (vacuous): %v", err)
	}
	nullID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"null","tags":null}`)
	if err != nil {
		t.Fatalf("CreateEntity null (vacuous): %v", err)
	}
	absentID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"absent"}`)
	if err != nil {
		t.Fatalf("CreateEntity absent (vacuous): %v", err)
	}

	cond := notCond(`{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"red"}`)
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch NOT($.tags[*] EQUALS red): %v", err)
	}
	assertResultIDSet(t, "NOT($.tags[*] EQUALS \"red\")", results,
		[]string{noRedID.String(), emptyID.String(), nullID.String(), absentID.String()})
}

// RunSearchNotVsNegativeTwinDiffer pins that NOT over a wildcard leaf is NOT
// interchangeable with the element-wise negative-twin operator
// (EQUALS/NOT_EQUAL), on the SAME data: NOT($.tags[*] EQUALS "red") asks "NO
// element is red" (universal), while $.tags[*] NOT_EQUAL "red" asks "SOME
// element differs" (existential) — for ["red","blue"] the first is false (an
// element IS red) and the second is true ("blue" differs). Companion to
// RunSearchNotUniversalQuantifierOverWildcard, which pins the universal
// reading alone; this scenario is the one that would silently pass if a
// future change ever collapsed NOT(EQUALS) into NOT_EQUAL as an
// "optimization".
func RunSearchNotVsNegativeTwinDiffer(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-vs-twin"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"name":"seed","tags":["s"]}`, searchWorkflowJSON)

	mixedID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"mixed","tags":["red","blue"]}`)
	if err != nil {
		t.Fatalf("CreateEntity mixed: %v", err)
	}

	notResults, err := c.SyncSearch(t, modelName, modelVersion,
		notCond(`{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"red"}`))
	if err != nil {
		t.Fatalf("SyncSearch NOT(EQUALS red): %v", err)
	}
	if len(notResults) != 0 {
		t.Errorf("NOT($.tags[*] EQUALS \"red\") over [red,blue]: want 0 results (an element IS red), got %d: %v", len(notResults), notResults)
	}

	neResults, err := c.SyncSearch(t, modelName, modelVersion,
		`{"type":"simple","jsonPath":"$.tags[*]","operatorType":"NOT_EQUAL","value":"red"}`)
	if err != nil {
		t.Fatalf("SyncSearch NOT_EQUAL red: %v", err)
	}
	assertResultIDSet(t, "$.tags[*] NOT_EQUAL \"red\" over [red,blue] (\"blue\" differs)", neResults, []string{mixedID.String()})
}

// RunSearchNotOverAbsentField pins NOT over a SCALAR (non-wildcard) leaf
// addressing a field the entity omits entirely: a bare path's leaf resolves
// no value there, so the leaf is false (existentially) and NOT is true —
// textual inversion, same disposition as internal/match's TestPrepare_Not.
// Distinct from RunSearchNotUniversalQuantifierOverWildcard's vacuity trio,
// which is about a WILDCARD path over an empty/null/absent container; this
// is the plain absent-scalar-field case.
func RunSearchNotOverAbsentField(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-absent-field"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed","tag":"vip"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","tag":"vip"}`); err != nil {
		t.Fatalf("CreateEntity Alice (tag=vip): %v", err)
	}
	// Bob omits $.tag entirely.
	absentID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob"}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob (tag absent): %v", err)
	}

	cond := notCond(`{"type":"simple","jsonPath":"$.tag","operatorType":"EQUALS","value":"vip"}`)
	results, err := c.SyncSearch(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearch NOT(tag==vip): %v", err)
	}
	assertResultIDSet(t, "NOT($.tag EQUALS \"vip\") — absent field inverts to true", results, []string{absentID.String()})
}

// RunSearchNotIsNullDiffersFromNotNullOnWildcard pins that NOT(IS_NULL) is
// NOT the same predicate as NOT_NULL on a wildcard path, even though the two
// look interchangeable. IS_NULL/NOT_NULL are existential ("SOME element is
// null" / "SOME element is non-null"); NOT(IS_NULL) negates the FIRST into a
// universal ("NO element is null"). The two are complements only where the
// container resolves at least one element (see path_addressing.go's
// RunPathVacuity, which pins the un-negated IS_NULL/NOT_NULL table this
// scenario negates one side of):
//
//   - mixed=["red",null]: IS_NULL true (the null) AND NOT_NULL true (the
//     "red") -> NOT(IS_NULL) FALSE, NOT_NULL TRUE. Differ.
//   - allNull=[null]: IS_NULL true, NOT_NULL false -> NOT(IS_NULL) FALSE,
//     NOT_NULL FALSE. Agree (both false).
//   - noNull=["red","blue"]: IS_NULL false, NOT_NULL true -> NOT(IS_NULL)
//     TRUE, NOT_NULL TRUE. Agree (both true) — the case that could mislead
//     someone into treating them as equivalent.
//   - empty=[]: IS_NULL false (no elements), NOT_NULL false (no elements)
//     -> NOT(IS_NULL) TRUE (vacuous), NOT_NULL FALSE. Differ.
//   - absent (no field): same as empty -> NOT(IS_NULL) TRUE, NOT_NULL FALSE.
//     Differ.
func RunSearchNotIsNullDiffersFromNotNullOnWildcard(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-isnull-vs-notnull"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion,
		`[{"name":"seed","a":["s"]},{"name":"seed2","a":null}]`, searchWorkflowJSON)

	mixedID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"mixed","a":["red",null]}`)
	if err != nil {
		t.Fatalf("CreateEntity mixed: %v", err)
	}
	// allNull is not asserted by ID: it is excluded from BOTH result sets
	// below, which IS the "agree" case from the doc comment's table (both
	// NOT(IS_NULL) and NOT_NULL are false for it).
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"allNull","a":[null]}`); err != nil {
		t.Fatalf("CreateEntity allNull: %v", err)
	}
	noNullID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"noNull","a":["red","blue"]}`)
	if err != nil {
		t.Fatalf("CreateEntity noNull: %v", err)
	}
	emptyID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"empty","a":[]}`)
	if err != nil {
		t.Fatalf("CreateEntity empty: %v", err)
	}
	absentID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"absent"}`)
	if err != nil {
		t.Fatalf("CreateEntity absent: %v", err)
	}

	notIsNullResults, err := c.SyncSearch(t, modelName, modelVersion,
		notCond(`{"type":"simple","jsonPath":"$.a[*]","operatorType":"IS_NULL","value":null}`))
	if err != nil {
		t.Fatalf("SyncSearch NOT($.a[*] IS_NULL): %v", err)
	}
	assertResultIDSet(t, "NOT($.a[*] IS_NULL)", notIsNullResults,
		[]string{noNullID.String(), emptyID.String(), absentID.String()})

	notNullResults, err := c.SyncSearch(t, modelName, modelVersion,
		`{"type":"simple","jsonPath":"$.a[*]","operatorType":"NOT_NULL","value":null}`)
	if err != nil {
		t.Fatalf("SyncSearch $.a[*] NOT_NULL: %v", err)
	}
	assertResultIDSet(t, "$.a[*] NOT_NULL", notNullResults, []string{mixedID.String(), noNullID.String()})
}

// RunDeleteConditionalNotOverCondition pins NOT on the delete-by-condition
// surface agrees across backends: DELETE /api/entity/{name}/{version} with a
// NOT condition body removes exactly the entities the NOT condition matches
// and leaves the rest untouched, matching the single-backend HTTP coverage in
// internal/e2e/search_group_operator_not_test.go
// (TestDeleteConditional_GroupOperatorNOT_OneCondition_Accepted) but proving
// it agrees on memory/sqlite/postgres rather than postgres alone.
func RunDeleteConditionalNotOverCondition(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-delete-conditional"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	keepID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice (keep): %v", err)
	}
	dropID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob","amount":50,"status":"inactive"}`)
	if err != nil {
		t.Fatalf("CreateEntity Bob (drop): %v", err)
	}

	// Delete everything that is NOT status==active, i.e. drop the inactive one.
	cond := notCond(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"active"}`)
	result, err := c.DeleteEntitiesConditional(t, modelName, modelVersion, cond, 0)
	if err != nil {
		t.Fatalf("DeleteEntitiesConditional NOT(status==active): %v", err)
	}
	if result.RemovedCount != 1 {
		t.Errorf("NOT(status==active) delete: want 1 removed, got %d (ids=%v, errs=%v)", result.RemovedCount, result.IDs, result.IDToError)
	}

	// GetEntityRaw returns a non-nil error alongside the status code for any
	// non-2xx response (doJSON's contract) — only the status is meaningful
	// here, matching tenant_isolation.go's identical use of this method.
	if status, _ := c.GetEntityRaw(t, keepID); status != http.StatusOK {
		t.Errorf("kept entity (status==active) should survive: status=%d", status)
	}
	if status, _ := c.GetEntityRaw(t, dropID); status != http.StatusNotFound {
		t.Errorf("dropped entity (status==inactive) should be gone: status=%d", status)
	}
}

// RunSearchBadPathInsideNot pins that a malformed field path is still
// rejected with the same 400/errorCode when it appears inside a NOT's single
// child, on every backend — proving path validation recurses into a NOT node
// exactly as it does into AND/OR (validateFilterPaths's "any node with
// children" contract; see the plugins/*/path_validation.go tests this
// mirrors at the unit level). A bare path (no "$." leader) is the same
// deliberately-rejected shape RunSearchPathRequiresJSONPathLeader exercises
// unwrapped.
func RunSearchBadPathInsideNot(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-not-bad-path"
	const modelVersion = 1
	setupSearchModel(t, c, modelName, modelVersion)

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":100,"status":"active"}`); err != nil {
		t.Fatalf("CreateEntity: %v", err)
	}

	cond := notCond(`{"type":"simple","jsonPath":"amount","operatorType":"EQUALS","value":100}`)
	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion, cond)
	if err != nil {
		t.Fatalf("SyncSearchRaw NOT(bare path): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("NOT wrapping a bare (leader-less) path: expected 400, got %d; body=%s", status, body)
	}
	if !containsErrorCode(body, "INVALID_FIELD_PATH") {
		t.Errorf("NOT wrapping a bare path: expected errorCode INVALID_FIELD_PATH, body=%s", body)
	}
}

// RunSearchUnsatisfiableComparisonPolarity pins the Task 1 fix (cyoda-go-spi
// eval_leaf.go EvalLeaf/evalCompare): a comparison for which the stored
// value's own type family has NO surviving sub-condition now answers by
// operator polarity — false for a positive operator, true for a negative
// one — rather than the old blanket "false for everything" bug. $.n
// NOT_EQUAL 12.5 on an INTEGER-only field used to answer false for every
// entity; PostgreSQL agrees the correct answer is true (select 5::int <>
// 12.5 is t), because no integer equals 12.5.
//
// The second half is the polymorphic carve-out the fix's own commit message
// calls out: a field declared [INTEGER, String] is not VOID for the operand
// "12.5" (the String branch accepts it), yet the INTEGER branch still has no
// candidate for a stored INTEGER value, so that value's answer must still
// follow polarity — while a stored STRING value takes the ordinary String
// comparison, unaffected. A=5 (stored int) and B="12.5" (stored string,
// chosen to equal the operand exactly) discriminate the two paths: EQUALS
// must select only B, and NOT_EQUAL must select only A.
func RunSearchUnsatisfiableComparisonPolarity(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	// --- Part 1: single declared type (INTEGER only). ---
	const intModel = "parity-unsat-polarity-int"
	const intModelVersion = 1
	if err := c.ImportModel(t, intModel, intModelVersion, `{"name":"seed","n":1}`); err != nil {
		t.Fatalf("ImportModel (int-only): %v", err)
	}
	if err := c.LockModel(t, intModel, intModelVersion); err != nil {
		t.Fatalf("LockModel (int-only): %v", err)
	}
	if err := c.ImportWorkflow(t, intModel, intModelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow (int-only): %v", err)
	}
	fiveID, err := c.CreateEntity(t, intModel, intModelVersion, `{"name":"Five","n":5}`)
	if err != nil {
		t.Fatalf("CreateEntity Five: %v", err)
	}
	thirtyID, err := c.CreateEntity(t, intModel, intModelVersion, `{"name":"Thirty","n":30}`)
	if err != nil {
		t.Fatalf("CreateEntity Thirty: %v", err)
	}

	eqResults, err := c.SyncSearch(t, intModel, intModelVersion, `{"type":"simple","jsonPath":"$.n","operatorType":"EQUALS","value":12.5}`)
	if err != nil {
		t.Fatalf("SyncSearch EQUALS 12.5 (int-only): %v", err)
	}
	if len(eqResults) != 0 {
		t.Errorf("$.n EQUALS 12.5 on an INTEGER field: want 0 results (no integer equals 12.5), got %d: %v", len(eqResults), eqResults)
	}

	neResults, err := c.SyncSearch(t, intModel, intModelVersion, `{"type":"simple","jsonPath":"$.n","operatorType":"NOT_EQUAL","value":12.5}`)
	if err != nil {
		t.Fatalf("SyncSearch NOT_EQUAL 12.5 (int-only): %v", err)
	}
	assertResultIDSet(t, "$.n NOT_EQUAL 12.5 on an INTEGER field (unsatisfiable comparison, negative polarity -> true for every value)",
		neResults, []string{fiveID.String(), thirtyID.String()})

	// --- Part 2: polymorphic [INTEGER, String]. ---
	const polyModel = "parity-unsat-polarity-poly"
	const polyModelVersion = 1
	if err := c.ImportModel(t, polyModel, polyModelVersion, `{"name":"seed","code":42}`); err != nil {
		t.Fatalf("ImportModel (int sample): %v", err)
	}
	if err := c.ImportModel(t, polyModel, polyModelVersion, `{"name":"seed2","code":"s"}`); err != nil {
		t.Fatalf("ImportModel (string sample): %v", err)
	}
	if err := c.LockModel(t, polyModel, polyModelVersion); err != nil {
		t.Fatalf("LockModel (poly): %v", err)
	}
	if err := c.ImportWorkflow(t, polyModel, polyModelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow (poly): %v", err)
	}
	intID, err := c.CreateEntity(t, polyModel, polyModelVersion, `{"name":"IntFive","code":5}`)
	if err != nil {
		t.Fatalf("CreateEntity IntFive (code=5 int): %v", err)
	}
	stringID, err := c.CreateEntity(t, polyModel, polyModelVersion, `{"name":"StringMatch","code":"12.5"}`)
	if err != nil {
		t.Fatalf("CreateEntity StringMatch (code=\"12.5\" string): %v", err)
	}

	polyEqResults, err := c.SyncSearch(t, polyModel, polyModelVersion, `{"type":"simple","jsonPath":"$.code","operatorType":"EQUALS","value":"12.5"}`)
	if err != nil {
		t.Fatalf("SyncSearch EQUALS \"12.5\" (poly): %v", err)
	}
	assertResultIDSet(t, "$.code EQUALS \"12.5\" (poly [INTEGER,String]): only the exact string match", polyEqResults, []string{stringID.String()})

	polyNeResults, err := c.SyncSearch(t, polyModel, polyModelVersion, `{"type":"simple","jsonPath":"$.code","operatorType":"NOT_EQUAL","value":"12.5"}`)
	if err != nil {
		t.Fatalf("SyncSearch NOT_EQUAL \"12.5\" (poly): %v", err)
	}
	// intID: stored INTEGER 5, operand "12.5" has no valid Integer
	// sub-condition (unsatisfiable in that family) -> polarity fix -> true.
	// stringID: stored STRING "12.5" equals the operand exactly -> NOT_EQUAL
	// is ordinarily false.
	assertResultIDSet(t, "$.code NOT_EQUAL \"12.5\" (poly [INTEGER,String]): the stored-int value follows polarity, the exact string match doesn't",
		polyNeResults, []string{intID.String()})
}
