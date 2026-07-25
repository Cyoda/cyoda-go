package parity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// search_type_directed.go closes final-review finding I-1 for #431: the spec
// §10 coverage matrix (docs/superpowers/specs/2026-07-23-431-cloud-aligned-search-design.md)
// marks these behaviors as needing a cross-backend parity (P) scenario, but
// none had a dedicated named one — each was only exercised at the SPI kernel
// unit level and/or a running-backend e2e test. These scenarios are thin: the
// type-directed kernel (cyoda-go-spi EvalLeaf/ExpandLeaf) is the single
// authority, so agreement across memory/sqlite/postgres is the guard, not the
// interesting logic itself.
//
// Note: the matrix's "temporal subtype + resolution" row is deliberately NOT
// added here for the DATA-field case. cyoda-go has no mechanism to ever
// classify a data leaf's declared type as a temporal subtype (LocalDate/
// Year/YearMonth/…) — schema.InferDataType classifies every JSON string as
// spi.String unconditionally (internal/domain/model/schema/validate.go
// inferDataType), and no importer/walker path assigns a temporal DataType to
// a data field either. The resolution graph is fully implemented and unit-
// tested in cyoda-go-spi (temporal_subtype.go) and is live for META fields
// (creationDate/lastUpdateTime — see search_temporal.go), but is unreachable
// for DATA fields today: classifyType/scalarClass (orderclass.go) buckets
// LocalDate/YearMonth/Year under OrderText (plain string compare), not
// OrderTemporal. Writing a parity scenario asserting "a YEAR data field
// resolves >=2024-09-09 to >2024" would describe behavior that cannot
// currently be triggered through any real request — it is a feature gap, not
// a missing test, and is out of scope for a test-only change (no production
// logic changes permitted here). Flagged for a follow-up design decision
// rather than silently added or silently dropped.

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
