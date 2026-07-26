package parity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// search_type_directed.go covers the spec §10 coverage matrix
// (docs/superpowers/specs/2026-07-23-431-cloud-aligned-search-design.md)
// marks these behaviors as needing a cross-backend parity (P) scenario, but
// none had a dedicated named one — each was only exercised at the SPI kernel
// unit level and/or a running-backend e2e test. These scenarios are thin: the
// type-directed kernel (cyoda-go-spi EvalLeaf/ExpandLeaf) is the single
// authority, so agreement across memory/sqlite/postgres is the guard, not the
// interesting logic itself.
//
// The matrix's "temporal subtype + resolution" row for the DATA-field case is
// pinned by RunSearchDataFieldTemporalResolution below. Model discovery now
// content-sniffs ISO-8601 sample strings into their most specific temporal
// subtype (schema.InferDataType via spi.ClassifyTemporalString), and
// classifyType/scalarClass (orderclass.go) buckets those subtypes under
// spi.OrderTemporal — so a data field whose samples are date-shaped compares
// chronologically (with cross-subtype resolution), exactly as META temporal
// fields (creationDate/lastUpdateTime) already do.

// RunSearchPolymorphicIntStringExpansion pins spec §10's polymorphic
// `[INTEGER,STRING]` row: a field observed as an INTEGER in one sample and a
// STRING in another becomes declared [INTEGER, STRING]. A numeric-shaped
// operand ("30") parses against both declared types and matches whichever
// stored branch (int or string) actually carries that value; a non-numeric
// operand ("hello") parses only as STRING and matches just the string
// branch — with no 400 (CONDITION_TYPE_MISMATCH requires the operand to
// parse into NONE of the declared types, and STRING alone is enough).
//
// This scenario originally surfaced a genuine sqlite-only pushdown
// under-selection bug and is now the cross-backend guard for its fix. On
// sqlite, EQUALS "30" against a polymorphic $.code (declared [INTEGER,
// STRING]) once bound the operand as a single SQLite storage class, so
// json_extract's storage-class-preserving extraction (unlike postgres's
// stringifying `doc->>'path'`) made `30 = '30'` false and dropped the int-30
// row from the WHERE candidate set before the kernel re-check could see it —
// an under-select the residual cannot recover. The fix
// (plugins/sqlite/query_planner.go isLeafPushable) routes polymorphic
// comparison leaves to the residual on sqlite, so the kernel evaluates all
// branches and every backend returns the same set.
func RunSearchPolymorphicIntStringExpansion(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-poly-intstr"
	const modelVersion = 1
	// Two samples before lock make $.code polymorphic [INTEGER, STRING]
	// (same mechanism pinned at the unit level in
	// TestSearch_ConditionType_IntegerFieldWithStringValue).
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed","code":42}`); err != nil {
		t.Fatalf("ImportModel (int sample): %v", err)
	}
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed2","code":"s"}`); err != nil {
		t.Fatalf("ImportModel (string sample): %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	aID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","code":30}`)
	if err != nil {
		t.Fatalf("CreateEntity A (int 30): %v", err)
	}
	bID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","code":"30"}`)
	if err != nil {
		t.Fatalf("CreateEntity B (string \"30\"): %v", err)
	}
	cID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C","code":"hello"}`)
	if err != nil {
		t.Fatalf("CreateEntity C (string \"hello\"): %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"D","code":99}`); err != nil {
		t.Fatalf("CreateEntity D (int 99, control): %v", err)
	}

	// operand "30" expands into an int branch and a string branch; each
	// stored value participates only in the branch matching its own JSON
	// kind — int-30 (A) and string-"30" (B) both match, int-99 (D) doesn't.
	eqResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.code","operatorType":"EQUALS","value":"30"}`)
	if err != nil {
		t.Fatalf("SyncSearch EQUALS \"30\": %v", err)
	}
	assertResultIDSet(t, "code EQUALS \"30\"", eqResults, []string{aID.String(), bID.String()})

	// operand "hello" parses only as STRING (not INTEGER) -> a single
	// branch, matching only the string entity carrying that exact value.
	// A 200 with the expected result set (not a 400) proves the polymorphic
	// operand isn't rejected merely because it fails one of the declared
	// branches.
	helloResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.code","operatorType":"EQUALS","value":"hello"}`)
	if err != nil {
		t.Fatalf("SyncSearch EQUALS \"hello\": %v", err)
	}
	assertResultIDSet(t, "code EQUALS \"hello\"", helloResults, []string{cID.String()})
}

// RunSearchNumericBucketRounding pins spec §10's numeric-bucket-rounding row:
// a fractional operand compared against an INTEGER-only field is rounded
// per-direction so the rounded integer still satisfies the original
// comparison — CEILING for GREATER_OR_EQUAL/LESS_THAN, FLOOR for
// LESS_OR_EQUAL/GREATER_THAN (cyoda-go-spi numeric_bucket.go
// roundingModeFor). `>= "12.78"` therefore becomes `>= 13` (matches stored
// 13, not 12); `<= "12.78"` becomes `<= 12` (matches stored 12, not 13).
func RunSearchNumericBucketRounding(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-numeric-bucket"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed","amount":1}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	twelveID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Twelve","amount":12}`)
	if err != nil {
		t.Fatalf("CreateEntity Twelve: %v", err)
	}
	thirteenID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Thirteen","amount":13}`)
	if err != nil {
		t.Fatalf("CreateEntity Thirteen: %v", err)
	}

	// CEILING direction: >= 12.78 rounds up to >= 13 -> only 13 matches.
	gteResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_OR_EQUAL","value":12.78}`)
	if err != nil {
		t.Fatalf("SyncSearch GREATER_OR_EQUAL 12.78: %v", err)
	}
	assertResultIDSet(t, "amount >= 12.78 (ceiling)", gteResults, []string{thirteenID.String()})

	// FLOOR direction: <= 12.78 rounds down to <= 12 -> only 12 matches.
	lteResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.amount","operatorType":"LESS_OR_EQUAL","value":12.78}`)
	if err != nil {
		t.Fatalf("SyncSearch LESS_OR_EQUAL 12.78: %v", err)
	}
	assertResultIDSet(t, "amount <= 12.78 (floor)", lteResults, []string{twelveID.String()})
}

// likeCond builds a {"type":"simple","jsonPath":...,"operatorType":"LIKE",...}
// condition JSON, marshalling value so backslash-escapes survive intact.
func likeCond(t *testing.T, jsonPath, value string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type":         "simple",
		"jsonPath":     jsonPath,
		"operatorType": "LIKE",
		"value":        value,
	})
	if err != nil {
		t.Fatalf("marshal LIKE condition: %v", err)
	}
	return string(b)
}

// RunSearchLikeAnchoredEscapedGlob pins spec §10's LIKE row: Cloud's glob
// grammar (`%` -> any run, `_` -> any single char, `\` escapes a literal `%`
// or `_`), whole-string anchored and case-sensitive
// (cyoda-go-spi eval_leaf.go likeToRegex). Same result on every backend.
func RunSearchLikeAnchoredEscapedGlob(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-like"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed","label":"txt"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	fooID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Foo","label":"foobar"}`)
	if err != nil {
		t.Fatalf("CreateEntity Foo: %v", err)
	}
	abcID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Abc","label":"abc"}`)
	if err != nil {
		t.Fatalf("CreateEntity Abc: %v", err)
	}
	percentID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Percent","label":"50%"}`)
	if err != nil {
		t.Fatalf("CreateEntity Percent: %v", err)
	}
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Xabcx","label":"xabcx"}`); err != nil {
		t.Fatalf("CreateEntity Xabcx (control): %v", err)
	}

	// "foo%" -> anchored prefix match; only "foobar" qualifies.
	prefixResults, err := c.SyncSearch(t, modelName, modelVersion, likeCond(t, "$.label", "foo%"))
	if err != nil {
		t.Fatalf("SyncSearch LIKE foo%%: %v", err)
	}
	assertResultIDSet(t, "label LIKE foo%", prefixResults, []string{fooID.String()})

	// "a_c" -> exactly 3 chars, middle wildcard; "abc" qualifies, the
	// 5-char "xabcx" control does not (whole-string anchored).
	singleCharResults, err := c.SyncSearch(t, modelName, modelVersion, likeCond(t, "$.label", "a_c"))
	if err != nil {
		t.Fatalf("SyncSearch LIKE a_c: %v", err)
	}
	assertResultIDSet(t, "label LIKE a_c", singleCharResults, []string{abcID.String()})

	// `50\%` -> escaped literal "%": matches only the literal "50%" value,
	// not e.g. "50" followed by anything.
	escapedResults, err := c.SyncSearch(t, modelName, modelVersion, likeCond(t, "$.label", `50\%`))
	if err != nil {
		t.Fatalf(`SyncSearch LIKE 50\%%: %v`, err)
	}
	assertResultIDSet(t, `label LIKE 50\%`, escapedResults, []string{percentID.String()})
}

// RunSearchStringOpsCaseSensitivityAndNonTextual pins spec §10's string-ops
// row: case-sensitive ops vs their `I*` case-insensitive twins, and the
// same-type gate — a string op against a non-textual (numeric) stored value
// is a non-match, not a stringify-and-compare or an error.
func RunSearchStringOpsCaseSensitivityAndNonTextual(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-string-ops"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed","amount":1}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	aliceID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Alice","amount":10}`)
	if err != nil {
		t.Fatalf("CreateEntity Alice: %v", err)
	}
	lowerID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"alice","amount":5}`)
	if err != nil {
		t.Fatalf("CreateEntity alice: %v", err)
	}

	// Case-sensitive STARTS_WITH "Alice" matches only the exact-case entity.
	csResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.name","operatorType":"STARTS_WITH","value":"Alice"}`)
	if err != nil {
		t.Fatalf("SyncSearch STARTS_WITH Alice: %v", err)
	}
	assertResultIDSet(t, "name STARTS_WITH Alice (case-sensitive)", csResults, []string{aliceID.String()})

	// Case-insensitive ISTARTS_WITH "alice" matches both, regardless of
	// stored case.
	ciResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.name","operatorType":"ISTARTS_WITH","value":"alice"}`)
	if err != nil {
		t.Fatalf("SyncSearch ISTARTS_WITH alice: %v", err)
	}
	assertResultIDSet(t, "name ISTARTS_WITH alice (case-insensitive)", ciResults, []string{aliceID.String(), lowerID.String()})

	// A string op (CONTAINS) against the non-textual $.amount field: the
	// operand "1" parses fine as INTEGER (so validation accepts it, no
	// 400), but the same-type gate makes a string op against a numeric
	// stored slot a non-match — 200 with zero results, not an error.
	nonTextualResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.amount","operatorType":"CONTAINS","value":"1"}`)
	if err != nil {
		t.Fatalf("SyncSearch CONTAINS \"1\" on numeric field: %v", err)
	}
	if len(nonTextualResults) != 0 {
		t.Errorf("CONTAINS on non-textual field: want 0 results (non-match, not stringify-compare), got %d", len(nonTextualResults))
	}
}

// RunSearchNegativeOpOnAbsentField pins spec §10's negative-op-on-absent-field
// row: NOT_EQUAL/NOT_CONTAINS are null-guarded to non-match on an absent
// field, not `!positive` (which would incorrectly match). Companion to
// RunSearchBoolCondition (which only exercises NOT_EQUAL against a field
// every entity carries) — this is the absent-field case.
func RunSearchNegativeOpOnAbsentField(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-negative-absent"
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
	// Bob omits $.tag entirely — the model describes known structure, not
	// required fields (missing fields are accepted).
	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Bob"}`); err != nil {
		t.Fatalf("CreateEntity Bob (tag absent): %v", err)
	}
	carolID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"Carol","tag":"other"}`)
	if err != nil {
		t.Fatalf("CreateEntity Carol (tag=other): %v", err)
	}

	// NOT_EQUAL "vip": Alice non-matches (equal), Bob non-matches (absent,
	// null-guarded) even though naive `!positive` logic would match it,
	// Carol matches (present and unequal).
	neResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.tag","operatorType":"NOT_EQUAL","value":"vip"}`)
	if err != nil {
		t.Fatalf("SyncSearch NOT_EQUAL vip: %v", err)
	}
	assertResultIDSet(t, "tag NOT_EQUAL vip (absent non-matches)", neResults, []string{carolID.String()})

	// NOT_CONTAINS "vi": same null-guard applies to Bob's absent field;
	// Carol's "other" doesn't contain "vi" so it matches.
	ncResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.tag","operatorType":"NOT_CONTAINS","value":"vi"}`)
	if err != nil {
		t.Fatalf("SyncSearch NOT_CONTAINS vi: %v", err)
	}
	assertResultIDSet(t, "tag NOT_CONTAINS vi (absent non-matches)", ncResults, []string{carolID.String()})
}

// RunSearchIsNullAbsentVsPresentNull pins spec §10's IS_NULL/NOT_NULL row:
// both an absent field and a present-but-JSON-null field satisfy IS_NULL;
// NOT_NULL matches only a present, non-null value.
func RunSearchIsNullAbsentVsPresentNull(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-search-isnull"
	const modelVersion = 1
	if err := c.ImportModel(t, modelName, modelVersion, `{"name":"seed","note":"txt"}`); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if err := c.LockModel(t, modelName, modelVersion); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
	if err := c.ImportWorkflow(t, modelName, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow: %v", err)
	}

	if _, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"A","note":"hi"}`); err != nil {
		t.Fatalf("CreateEntity A (note present): %v", err)
	}
	// Explicit JSON null is compatible with any declared type
	// (schema validate.go validateLeaf: "Null is compatible with any type").
	nullID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"B","note":null}`)
	if err != nil {
		t.Fatalf("CreateEntity B (note=null): %v", err)
	}
	// Carol omits $.note entirely.
	absentID, err := c.CreateEntity(t, modelName, modelVersion, `{"name":"C"}`)
	if err != nil {
		t.Fatalf("CreateEntity C (note absent): %v", err)
	}

	isNullResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.note","operatorType":"IS_NULL","value":null}`)
	if err != nil {
		t.Fatalf("SyncSearch IS_NULL: %v", err)
	}
	assertResultIDSet(t, "note IS_NULL (absent + present-null)", isNullResults, []string{nullID.String(), absentID.String()})

	notNullResults, err := c.SyncSearch(t, modelName, modelVersion, `{"type":"simple","jsonPath":"$.note","operatorType":"NOT_NULL","value":null}`)
	if err != nil {
		t.Fatalf("SyncSearch NOT_NULL: %v", err)
	}
	if len(notNullResults) != 1 || notNullResults[0].Meta.ID == nullID.String() || notNullResults[0].Meta.ID == absentID.String() {
		t.Errorf("note NOT_NULL: want exactly the present-non-null entity, got %d results: %v", len(notNullResults), notNullResults)
	}
}

// RunSearchDataFieldTemporalResolution pins spec §4's data-field temporal row
// (subsumes the earlier standalone temporal-search-on-data-fields work): model
// discovery content-sniffs ISO-8601 sample strings into a temporal subtype, so
// a data field compares chronologically (with cross-subtype resolution) rather
// than lexically. Two independent models exercise the two halves:
//
//  1. A LocalDate field ($.when, discovered from an ISO date sample):
//     GREATER_OR_EQUAL "2024-09-09" is a chronological compare — it matches
//     2024-09-09 and 2024-09-10 but not 2024-09-08. Lexical string order would
//     agree here, so this half is the "temporal, not string-broken" guard.
//
//  2. A Year field ($.yr, discovered from a bare-year sample "2020"): the
//     resolution graph downscales the LocalDate operand "2024-09-09" to Year
//     and, because the floor loses precision, mutates GREATER_OR_EQUAL into a
//     strict GREATER_THAN 2024 — so it matches 2025 but NOT 2024. This is the
//     behavior only chronological subtype resolution can produce; a lexical
//     compare of "2024" >= "2024-09-09" would (wrongly) exclude 2024 by string
//     order but would also mis-handle the boundary, and could never yield the
//     ">2024 matches 2025" mutation. This half is the resolution-graph guard.
func RunSearchDataFieldTemporalResolution(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	// --- Half 1: LocalDate field, chronological >= compare ---------------
	const dateModel = "parity-search-data-localdate"
	const modelVersion = 1
	// Sample "2020-01-01" content-sniffs to LocalDate -> $.when declared
	// [LocalDate]. "seed" stays String (non-temporal).
	if err := c.ImportModel(t, dateModel, modelVersion, `{"name":"seed","when":"2020-01-01"}`); err != nil {
		t.Fatalf("ImportModel (localdate sample): %v", err)
	}
	if err := c.LockModel(t, dateModel, modelVersion); err != nil {
		t.Fatalf("LockModel (localdate): %v", err)
	}
	if err := c.ImportWorkflow(t, dateModel, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow (localdate): %v", err)
	}

	// Eighth (2024-09-08) is the control excluded by the >= boundary.
	if _, err := c.CreateEntity(t, dateModel, modelVersion, `{"name":"Eighth","when":"2024-09-08"}`); err != nil {
		t.Fatalf("CreateEntity Eighth: %v", err)
	}
	ninthID, err := c.CreateEntity(t, dateModel, modelVersion, `{"name":"Ninth","when":"2024-09-09"}`)
	if err != nil {
		t.Fatalf("CreateEntity Ninth: %v", err)
	}
	tenthID, err := c.CreateEntity(t, dateModel, modelVersion, `{"name":"Tenth","when":"2024-09-10"}`)
	if err != nil {
		t.Fatalf("CreateEntity Tenth: %v", err)
	}

	geResults, err := c.SyncSearch(t, dateModel, modelVersion, `{"type":"simple","jsonPath":"$.when","operatorType":"GREATER_OR_EQUAL","value":"2024-09-09"}`)
	if err != nil {
		t.Fatalf("SyncSearch when >= 2024-09-09: %v", err)
	}
	// Chronological: 09-09 (inclusive) and 09-10 match; 09-08 does not.
	assertResultIDSet(t, "when >= 2024-09-09 (chronological)", geResults, []string{ninthID.String(), tenthID.String()})

	// --- Half 2: Year field, resolution-graph op mutation ----------------
	const yearModel = "parity-search-data-year"
	// Sample "2020" content-sniffs to Year -> $.yr declared [Year].
	if err := c.ImportModel(t, yearModel, modelVersion, `{"name":"seed","yr":"2020"}`); err != nil {
		t.Fatalf("ImportModel (year sample): %v", err)
	}
	if err := c.LockModel(t, yearModel, modelVersion); err != nil {
		t.Fatalf("LockModel (year): %v", err)
	}
	if err := c.ImportWorkflow(t, yearModel, modelVersion, searchWorkflowJSON); err != nil {
		t.Fatalf("ImportWorkflow (year): %v", err)
	}

	if _, err := c.CreateEntity(t, yearModel, modelVersion, `{"name":"Y2024","yr":"2024"}`); err != nil {
		t.Fatalf("CreateEntity Y2024: %v", err)
	}
	y2025ID, err := c.CreateEntity(t, yearModel, modelVersion, `{"name":"Y2025","yr":"2025"}`)
	if err != nil {
		t.Fatalf("CreateEntity Y2025: %v", err)
	}

	// The LocalDate operand "2024-09-09" downscales to Year; the imprecise
	// floor mutates >= into > 2024, so only 2025 matches (2024 is excluded).
	yrResults, err := c.SyncSearch(t, yearModel, modelVersion, `{"type":"simple","jsonPath":"$.yr","operatorType":"GREATER_OR_EQUAL","value":"2024-09-09"}`)
	if err != nil {
		t.Fatalf("SyncSearch yr >= 2024-09-09: %v", err)
	}
	assertResultIDSet(t, "yr >= 2024-09-09 resolves to > 2024", yrResults, []string{y2025ID.String()})
}
