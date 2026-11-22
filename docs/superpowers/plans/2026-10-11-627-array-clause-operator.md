# #627 Array Clause Operator — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The array clause carries a required `operatorType` applied at every tested position, refuses a clause that tests no position, and the spec, SPI, validators, docs and tests all agree on that one shape.

**Architecture:** The array clause stays sugar. `spi.DesugarCondition` is the only place that defines it: it rewrites the clause into an AND of simple clauses using the clause's operator, and returns an error for a clause that tests no position. Every validator and evaluator in cyoda-go reaches the array clause through that rewrite, so the simple-clause rules (operator names, operand shape, BETWEEN arity, patterns, field types) apply unchanged.

**Tech Stack:** Go 1.26.9, cyoda-go-spi (separate module, pseudo-pinned), oapi-codegen (`go generate ./api`), testcontainers e2e, parity suite.

**Spec:** `docs/superpowers/specs/2026-10-10-627-array-clause-operator-design.md`

## Global Constraints

- Wire shape: `{"type":"array","jsonPath":"…[*]","operatorType":"<one of the 26>","values":[…]}`; `operatorType` required; parser also accepts the `operator` and `operation` aliases, as for a simple clause.
- `values` entry: a scalar, or a two-element array for `BETWEEN`/`BETWEEN_INCLUSIVE`; `null` = do not test this position. Object entries are refused.
- A clause that tests no position (missing, empty, all-null `values`) is refused: search surfaces `400 INVALID_CONDITION`, workflow import `400 VALIDATION_FAILED`.
- An array-clause `jsonPath` must end in `[*]` and contain no other `[*]`; otherwise `400 INVALID_FIELD_PATH` (search) / `400 VALIDATION_FAILED` (import).
- `IS_NULL`/`NOT_NULL`: a non-null entry selects the position; its value is not used.
- Workflow import refuses a criterion that does not parse: `400 VALIDATION_FAILED`.
- The operator enum on `ArrayConditionDto`, `SimpleConditionDto`, `LifecycleConditionDto` is exactly `spi.OperatorNames()` (26 names).
- No `#NNN` issue ids in code, comments, errors, help or OpenAPI (PR bodies and commits only).
- `log/slog` only. Wrap errors with `fmt.Errorf("…: %w", err)`.
- Never `git add -A` / `git add .` — `go.work` carries an uncommitted local `use` line during development. Stage files by name.
- Do not run `make test-full` or `make race` locally; CI runs them. Do not add `-count=1`.

## Review Focus

1. **A position past the end of the stored array** with a negated operator (`NOT_EQUAL` at index 5 of a 3-element array) must answer exactly as the equivalent simple clause `$.scores[5] NOT_EQUAL x` does on every backend — owned by Task 10 (parity case).
2. **Large integers in `values`** (beyond float64 precision, e.g. `12345678901234567890`) must survive marshal → parse (async search re-parses the stored job) — owned by Task 1 (round-trip test with `json.Number`).
3. **Cloud's spelling `operation`** instead of `operatorType` must be accepted, so a client written against Cloud's spec works — owned by Task 1 (parse test) and Task 8 (e2e happy path uses `operation` once).
4. **An array clause inside a `NOT` group** must be the exact complement of the clause on every backend (pushed-down and in-memory paths) — owned by Task 10.
5. **A body in the old spec shape** (`operatorType` + `value`, no `values`) must be refused, and on conditional delete must delete nothing — owned by Task 8.

---

## Repositories and setup

- cyoda-go worktree: `/Users/paul/go-projects/cyoda-light/cyoda-go/.worktrees/fix-627-array-condition` (branch `fix-627-array-condition`, base `release/v0.9.0`).
- cyoda-go-spi: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`. Create branch `feat-627-array-operator` from `origin/main` (`50e45dd`).
- Local composition (uncommitted, never staged): in the cyoda-go worktree run `go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi`. Remove it before the pin commit in Task 12.

Streams: Tasks 1–3 (SPI) are sequential. Tasks 4–11 (cyoda-go) need Tasks 1–3 on disk via `go.work`; within them, 4 → 5 → 6 are sequential, then 7, 8, 9, 10 and 11 can run in parallel. Task 12 needs the SPI PR merged. Task 13 needs the cyoda-go PR open.

---

### Task 1: SPI — `ArrayCondition` carries an operator (parse, marshal)

**Files:**
- Modify: `cyoda-go-spi/predicate/condition.go:34-41`
- Modify: `cyoda-go-spi/predicate/parse.go` (`parseSimple`, `parseLifecycle`, `parseArray`)
- Modify: `cyoda-go-spi/predicate/marshal.go:54-65`
- Test: `cyoda-go-spi/predicate/parse_test.go`, `cyoda-go-spi/predicate/marshal_test.go`

**Interfaces:**
- Produces: `predicate.ArrayCondition{JsonPath string; OperatorType string; Values []any}`. JSON field `operatorType`; aliases `operator`, `operation` on input.

- [ ] **Step 1: Write the failing tests** (append to `predicate/parse_test.go`)

```go
func TestParseArrayCondition_ReadsOperator(t *testing.T) {
	for _, field := range []string{"operatorType", "operator", "operation"} {
		body := []byte(`{"type":"array","jsonPath":"$.scores[*]","` + field + `":"GREATER_THAN","values":[50,null,70]}`)
		cond, err := predicate.ParseCondition(body)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", field, err)
		}
		ac := cond.(*predicate.ArrayCondition)
		if ac.OperatorType != "GREATER_THAN" {
			t.Errorf("%s: OperatorType = %q, want GREATER_THAN", field, ac.OperatorType)
		}
	}
}

func TestParseArrayCondition_MissingOperatorIsEmpty(t *testing.T) {
	cond, err := predicate.ParseCondition([]byte(`{"type":"array","jsonPath":"$.t[*]","values":["a"]}`))
	if err != nil {
		t.Fatalf("parse must not reject a missing operator (the validator does): %v", err)
	}
	if op := cond.(*predicate.ArrayCondition).OperatorType; op != "" {
		t.Errorf("OperatorType = %q, want empty", op)
	}
}
```

Append to `predicate/marshal_test.go` (replace the body of `TestRoundTrip_ArrayCondition`, add the precision case):

```go
func TestRoundTrip_ArrayCondition(t *testing.T) {
	original := &ArrayCondition{
		JsonPath:     "$.tags[*]",
		OperatorType: "EQUALS",
		Values:       []any{"red", nil, "blue"},
	}
	roundTripCondition(t, original)
}

func TestRoundTrip_ArrayCondition_LargeIntegerKeepsPrecision(t *testing.T) {
	original := &ArrayCondition{
		JsonPath:     "$.ids[*]",
		OperatorType: "GREATER_THAN",
		Values:       []any{json.Number("12345678901234567890"), nil},
	}
	roundTripCondition(t, original)
}
```

- [ ] **Step 2: Run to verify they fail**

Run (in `cyoda-go-spi`): `go test ./predicate/ -run 'TestParseArrayCondition|TestRoundTrip_ArrayCondition'`
Expected: compile error `unknown field OperatorType in struct literal`.

- [ ] **Step 3: Implement**

`predicate/condition.go`:

```go
// ArrayCondition tests the elements of one array by position. OperatorType
// is applied at every position whose entry in Values is non-nil; a nil entry
// tests nothing at that position. [spi.DesugarCondition] defines the clause's
// meaning: an AND of one SimpleCondition per tested position.
type ArrayCondition struct {
	JsonPath     string
	OperatorType string
	Values       []any // positional operands, nil = skip
}
```

`predicate/parse.go` — one helper for the operator aliases, used by all three leaf parsers:

```go
// operatorFields holds the three spellings of a leaf's operator: the
// canonical operatorType, the "operator" alias, and Cloud's "operation".
type operatorFields struct {
	OperatorType string `json:"operatorType"`
	Operator     string `json:"operator"`
	Operation    string `json:"operation"`
}

func (o operatorFields) name() string {
	return coalesce(o.OperatorType, o.Operator, o.Operation)
}
```

Rewrite the raw structs to embed it (`operatorFields` embedded, anonymous), e.g.:

```go
func parseArray(body []byte) (*ArrayCondition, error) {
	var raw struct {
		operatorFields
		JsonPath string `json:"jsonPath"`
		Values   []any  `json:"values"`
	}
	if err := unmarshalNumberAware(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse array condition: %w", err)
	}
	return &ArrayCondition{
		JsonPath:     raw.JsonPath,
		OperatorType: raw.name(),
		Values:       raw.Values,
	}, nil
}
```

Apply the same embedding to `parseSimple` and `parseLifecycle` (replace their three operator fields and the `coalesce(...)` call with `raw.name()`).

Note: `encoding/json` promotes the exported fields of an embedded *unexported* struct type, so the tags apply. Verify by the tests, not by assumption.

`predicate/marshal.go`:

```go
// MarshalJSON for ArrayCondition emits the discriminator + struct fields.
func (c *ArrayCondition) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type         string `json:"type"`
		JsonPath     string `json:"jsonPath"`
		OperatorType string `json:"operatorType"`
		Values       []any  `json:"values"`
	}{
		Type:         c.Type(),
		JsonPath:     c.JsonPath,
		OperatorType: c.OperatorType,
		Values:       c.Values,
	})
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./predicate/`
Expected: PASS (all, including the existing simple/lifecycle alias tests).

- [ ] **Step 5: Commit**

```bash
git add predicate/condition.go predicate/parse.go predicate/marshal.go predicate/parse_test.go predicate/marshal_test.go
git commit -m "feat(predicate)!: ArrayCondition carries an operator"
```

---

### Task 2: SPI — `DesugarCondition` applies the operator and refuses a clause that tests nothing

**Files:**
- Modify: `cyoda-go-spi/condition_filter.go:148-153` (`ConditionToFilter`), `:185-247` (`DesugarCondition`, `desugarArrayCondition`)
- Modify: `cyoda-go-spi/errors.go` (new sentinel)
- Test: `cyoda-go-spi/condition_filter_test.go` (`TestConditionToFilter_Array*`, `TestDesugarCondition_*`, `TestConditionToFilter_EmptyGroupIdentityEncodings`), every `predicate.ArrayCondition{` literal in the SPI's tests gains `OperatorType: "EQUALS"`

**Interfaces:**
- Consumes: Task 1's `ArrayCondition.OperatorType`.
- Produces: `func DesugarCondition(c predicate.Condition) (predicate.Condition, error)`; `var ErrArrayClauseTestsNothing = errors.New("array clause tests no position")`. `ConditionToFilter` returns an error wrapping `ErrArrayClauseTestsNothing` for such a clause, at any depth.

- [ ] **Step 1: Write the failing tests** (replace `TestDesugarCondition_AllNullIsTautology`, `TestConditionToFilter_ArrayAllNil`, and the `AllNilArrayYieldsNonNilEmptyChildren` subtest; add the operator test)

```go
func TestDesugarCondition_ArrayClauseUsesOperator(t *testing.T) {
	got, err := spi.DesugarCondition(&predicate.ArrayCondition{
		JsonPath: "$.scores[*]", OperatorType: "GREATER_THAN", Values: []any{50, nil, 70},
	})
	if err != nil {
		t.Fatal(err)
	}
	g, ok := got.(*predicate.GroupCondition)
	if !ok || g.Operator != "AND" || len(g.Conditions) != 2 {
		t.Fatalf("want AND of 2 leaves, got %#v", got)
	}
	want := []predicate.SimpleCondition{
		{JsonPath: "$.scores[0]", OperatorType: "GREATER_THAN", Value: 50},
		{JsonPath: "$.scores[2]", OperatorType: "GREATER_THAN", Value: 70},
	}
	for i, c := range g.Conditions {
		if *c.(*predicate.SimpleCondition) != want[i] {
			t.Errorf("leaf %d = %#v, want %#v", i, c, want[i])
		}
	}
}

func TestDesugarCondition_ArrayClauseTestingNothingIsRefused(t *testing.T) {
	for name, vals := range map[string][]any{"nil": nil, "empty": {}, "all-null": {nil, nil}} {
		_, err := spi.DesugarCondition(&predicate.ArrayCondition{
			JsonPath: "$.t[*]", OperatorType: "EQUALS", Values: vals,
		})
		if !errors.Is(err, spi.ErrArrayClauseTestsNothing) {
			t.Errorf("%s: want ErrArrayClauseTestsNothing, got %v", name, err)
		}
	}
}

func TestConditionToFilter_NestedArrayClauseTestingNothingIsRefused(t *testing.T) {
	cond := &predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
		&predicate.ArrayCondition{JsonPath: "$.t[*]", OperatorType: "EQUALS", Values: []any{nil}},
	}}
	if _, err := spi.ConditionToFilter(cond, nil); !errors.Is(err, spi.ErrArrayClauseTestsNothing) {
		t.Fatalf("want ErrArrayClauseTestsNothing, got %v", err)
	}
}
```

In `TestConditionToFilter_EmptyGroupIdentityEncodings`, delete the `AllNilArrayYieldsNonNilEmptyChildren` subtest (the caller-written empty group subtest stays: an empty AND group remains "match everything").

- [ ] **Step 2: Run to verify they fail**

Run: `go test . -run 'TestDesugarCondition|TestConditionToFilter_'`
Expected: compile error (`DesugarCondition` returns one value).

- [ ] **Step 3: Implement**

`errors.go`:

```go
// ErrArrayClauseTestsNothing is returned for an array clause whose Values
// holds no non-nil entry. Such a clause would read as an empty AND and match
// every entity; it is refused instead.
var ErrArrayClauseTestsNothing = errors.New("array clause tests no position")
```

`condition_filter.go`:

```go
func ConditionToFilter(cond predicate.Condition, fields map[string]FieldDescriptor) (Filter, error) {
	if cond == nil {
		return Filter{}, fmt.Errorf("condition is nil")
	}
	d, err := DesugarCondition(cond)
	if err != nil {
		return Filter{}, err
	}
	return desugaredToFilter(d, fields)
}

func DesugarCondition(c predicate.Condition) (predicate.Condition, error) {
	switch v := c.(type) {
	case *predicate.ArrayCondition:
		return desugarArrayCondition(v)
	case *predicate.GroupCondition:
		children := make([]predicate.Condition, len(v.Conditions))
		for i, child := range v.Conditions {
			d, err := DesugarCondition(child)
			if err != nil {
				return nil, err
			}
			children[i] = d
		}
		return &predicate.GroupCondition{Operator: v.Operator, Conditions: children}, nil
	default:
		return c, nil
	}
}

func desugarArrayCondition(c *predicate.ArrayCondition) (predicate.Condition, error) {
	var leaves []predicate.Condition
	for i, val := range c.Values {
		if val == nil {
			continue
		}
		leaves = append(leaves, &predicate.SimpleCondition{
			JsonPath:     desugarArrayElementPath(c.JsonPath, i),
			OperatorType: c.OperatorType,
			Value:        val,
		})
	}
	switch len(leaves) {
	case 0:
		return nil, fmt.Errorf("%w: jsonPath %q", ErrArrayClauseTestsNothing, c.JsonPath)
	case 1:
		return leaves[0], nil
	default:
		return &predicate.GroupCondition{Operator: "AND", Conditions: leaves}, nil
	}
}
```

Rewrite `DesugarCondition`'s doc comment: the leaves carry the clause's operator; a clause with no non-nil value is refused with `ErrArrayClauseTestsNothing` (no longer "empty AND"); the function is total on everything else and still does not check `jsonPath` well-formedness. Remove "EQUALS leaves" wording everywhere in the file. In `TestConditionToFilter_DesugarIsNotReappliedPerLevel` nothing changes.

Update every remaining `predicate.ArrayCondition{` literal in the SPI tests to carry `OperatorType: "EQUALS"` (keeps their current meaning). Find them with `grep -rn 'predicate.ArrayCondition{' --include='*_test.go' .`.

- [ ] **Step 4: Run the tests**

Run: `go test ./...` and `go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add errors.go condition_filter.go condition_filter_test.go <other edited test files by name>
git commit -m "feat(spi)!: DesugarCondition applies the array clause's operator and refuses a clause that tests nothing"
```

---

### Task 3: SPI — operator and pattern validators reach the array clause; changelog

**Files:**
- Modify: `cyoda-go-spi/condition_filter.go:823-826` (`validateOperatorsAtDepth` array arm), `:880-882` (`validatePatternsAtDepth` array arm)
- Modify: `cyoda-go-spi/CHANGELOG.md` (`[Unreleased]` → `### Breaking`)
- Test: `cyoda-go-spi/condition_filter_test.go`

**Interfaces:**
- Consumes: Task 2's `DesugarCondition` signature.
- Produces: `ValidateConditionOperators` / `ValidateConditionPatterns` report an unknown operator, a bad pattern, or `ErrArrayClauseTestsNothing` for an array clause.

- [ ] **Step 1: Write the failing tests**

```go
func TestValidateConditionOperators_ArrayClause(t *testing.T) {
	bad := &predicate.ArrayCondition{JsonPath: "$.t[*]", OperatorType: "FROBNICATE", Values: []any{"a"}}
	if err := spi.ValidateConditionOperators(bad); !errors.Is(err, spi.ErrUnknownOperator) {
		t.Errorf("unknown operator: want ErrUnknownOperator, got %v", err)
	}
	missing := &predicate.ArrayCondition{JsonPath: "$.t[*]", Values: []any{"a"}}
	if err := spi.ValidateConditionOperators(missing); !errors.Is(err, spi.ErrUnknownOperator) {
		t.Errorf("missing operator: want ErrUnknownOperator, got %v", err)
	}
	ok := &predicate.ArrayCondition{JsonPath: "$.t[*]", OperatorType: "GREATER_THAN", Values: []any{1}}
	if err := spi.ValidateConditionOperators(ok); err != nil {
		t.Errorf("valid clause: %v", err)
	}
}

func TestValidateConditionPatterns_ArrayClause(t *testing.T) {
	bad := &predicate.ArrayCondition{JsonPath: "$.t[*]", OperatorType: "MATCHES_PATTERN", Values: []any{"("}}
	if err := spi.ValidateConditionPatterns(bad); err == nil {
		t.Error("an invalid pattern at a position must be reported")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test . -run 'TestValidateCondition(Operators|Patterns)_ArrayClause'`
Expected: FAIL (both arms return nil today).

- [ ] **Step 3: Implement** — both arms become:

```go
	case *predicate.ArrayCondition:
		// The clause means its desugared leaves; check those.
		d, err := DesugarCondition(c)
		if err != nil {
			return err
		}
		return validateOperatorsAtDepth(d, depth+1) // validatePatternsAtDepth in the patterns walker
```

`CHANGELOG.md`, under `## [Unreleased]` → `### Breaking`, add:

```markdown
- **`predicate.ArrayCondition` carries `OperatorType`, applied at every tested
  position.** The parser reads `operatorType` (aliases `operator`,
  `operation`); `MarshalJSON` writes it. `DesugarCondition` now returns
  `(Condition, error)`: its positional leaves use the clause's operator, and a
  clause whose `Values` holds no non-nil entry is refused with the new
  `ErrArrayClauseTestsNothing` instead of becoming an empty AND that matches
  everything. `ConditionToFilter`, `ValidateConditionOperators` and
  `ValidateConditionPatterns` check the array clause through the rewrite.
  Migration: set `OperatorType` (`"EQUALS"` keeps the previous meaning);
  handle the error from `DesugarCondition`; a backend with its own condition
  parser must read the operator.
```

- [ ] **Step 4: Run** `go test ./... && go vet ./...` — Expected: PASS.

- [ ] **Step 5: Commit, push the branch, open the SPI PR**

```bash
git add condition_filter.go condition_filter_test.go CHANGELOG.md
git commit -m "feat(spi)!: operator and pattern validators check the array clause"
git push -u origin feat-627-array-operator
```

Open the PR into `main` with `gh pr create` (title `feat(spi)!: the array clause carries an operator`; body: what changed, the breaking items, link to Cyoda/cyoda-go#627, and the KNOWN_CONSUMERS notification links filled in at Task 13). Do not merge until the cyoda-go side (Tasks 4–11) builds green against it via `go.work`.

---

### Task 4: cyoda-go — callers of `DesugarCondition`; evaluator tests for operators

**Files:**
- Modify: `internal/match/prepared.go:140-142` (`prepare`)
- Modify: `internal/domain/search/condition_type_validate.go:69-77`
- Modify: every `predicate.ArrayCondition{` literal in cyoda-go tests (29; `grep -rn 'predicate.ArrayCondition{' --include='*_test.go' .`) — add `OperatorType: "EQUALS"`
- Test: `internal/match/match_test.go`

**Interfaces:**
- Consumes: `spi.DesugarCondition(c) (predicate.Condition, error)`, `spi.ErrArrayClauseTestsNothing`.
- Produces: `match.Prepare` returns an error wrapping `spi.ErrArrayClauseTestsNothing` for such a clause.

- [ ] **Step 0: Local composition** — `go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi` (do not stage `go.work`).

- [ ] **Step 1: Write the failing tests** (append to `internal/match/match_test.go`; `sampleTypes`, `meta()` exist there; add a local document)

```go
const scoresDoc = `{"scores":[90,40,75]}`

func numericScores(string) []spi.DataType { return []spi.DataType{spi.Integer} }

func TestMatchArrayCondition_OperatorPerPosition(t *testing.T) {
	cases := []struct {
		name string
		cond *predicate.ArrayCondition
		want bool
	}{
		{"GREATER_THAN at 0 and 2", &predicate.ArrayCondition{JsonPath: "$.scores[*]", OperatorType: "GREATER_THAN", Values: []any{50, nil, 70}}, true},
		{"GREATER_THAN fails at 1", &predicate.ArrayCondition{JsonPath: "$.scores[*]", OperatorType: "GREATER_THAN", Values: []any{nil, 50}}, false},
		{"BETWEEN at 1", &predicate.ArrayCondition{JsonPath: "$.scores[*]", OperatorType: "BETWEEN_INCLUSIVE", Values: []any{nil, []any{30, 45}}}, true},
		{"NOT_NULL selects position 1", &predicate.ArrayCondition{JsonPath: "$.scores[*]", OperatorType: "NOT_NULL", Values: []any{nil, true}}, true},
		{"IS_NULL selects position 1", &predicate.ArrayCondition{JsonPath: "$.scores[*]", OperatorType: "IS_NULL", Values: []any{nil, true}}, false},
	}
	for _, tc := range cases {
		p, err := Prepare(tc.cond, numericScores)
		if err != nil {
			t.Fatalf("%s: Prepare: %v", tc.name, err)
		}
		if got := p.Match([]byte(scoresDoc), meta()); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPrepare_ArrayClauseTestingNothingIsRefused(t *testing.T) {
	_, err := Prepare(&predicate.ArrayCondition{JsonPath: "$.scores[*]", OperatorType: "EQUALS", Values: []any{nil}}, numericScores)
	if !errors.Is(err, spi.ErrArrayClauseTestsNothing) {
		t.Fatalf("want ErrArrayClauseTestsNothing, got %v", err)
	}
}
```

(Check `spi.Integer` is the DataType constant name used in this package's other tests; use whatever `sampleTypes` returns for a whole number if the name differs.)

- [ ] **Step 2: Run to verify** — `go test ./internal/match/ ./internal/domain/search/` — Expected: compile errors at the two callers.

- [ ] **Step 3: Implement**

`internal/match/prepared.go`:

```go
func prepare(cond predicate.Condition, fieldTypes FieldTypes) (prepNode, error) {
	d, err := spi.DesugarCondition(cond)
	if err != nil {
		return nil, err
	}
	return prepareDesugared(d, fieldTypes)
}
```

Update its doc comment ("positional EQUALS leaves" → "positional leaves carrying the clause's operator"). Check `prepNode`'s zero value is `nil`-able; if it is a struct, return `prepNode{}`.

`internal/domain/search/condition_type_validate.go` array arm:

```go
	case *predicate.ArrayCondition:
		// The clause means its desugared leaves (spi.DesugarCondition, the
		// same rewrite spi.ConditionToFilter and match.Prepare use), so the
		// leaves are what is type-checked.
		d, err := spi.DesugarCondition(c)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCondition, err)
		}
		return walkConditionTypes(fm, d, depth+1)
```

Add `OperatorType: "EQUALS"` to every `predicate.ArrayCondition{` literal in cyoda-go tests. Existing tests that relied on an all-null clause matching everything (grep `nil, nil}` near `ArrayCondition` in `internal/match/prepared_equivalence_test.go` `genArrayValues`) must stop generating an all-null `Values`: make the generator always emit at least one non-nil entry.

- [ ] **Step 4: Run** — `go test ./internal/match/ ./internal/domain/... && go vet ./...` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/match/prepared.go internal/domain/search/condition_type_validate.go internal/match/match_test.go <each edited test file by name>
git commit -m "feat(match): the array clause's positions use its operator"
```

---

### Task 5: cyoda-go — search validators check the array clause through the rewrite

**Files:**
- Modify: `internal/domain/search/jsonpath_grammar.go:122-133` (`ValidateArrayClauseJSONPath`)
- Modify: `internal/domain/search/operators.go:109-130` (`validateConditionAtDepth` array arm), `:223-235` (`validateCriterionConditionAtDepth` array arm)
- Test: `internal/domain/search/array_condition_validate_test.go`

**Interfaces:**
- Consumes: Task 2/4.
- Produces: `search.ValidateCondition` and `search.ValidateCriterionCondition` return `ErrInvalidCondition` for a missing/unknown operator, an object entry, a bad BETWEEN entry, or a clause testing nothing; `ErrInvalidFieldPath` for a path with more than one `[*]`. `ValidateArrayClauseJSONPath` refuses more than one `[*]` (workflow import gets this for free in Task 6).

- [ ] **Step 1: Write the failing tests** (append; existing tests in this file gain `OperatorType: "EQUALS"`)

```go
func TestValidateCondition_ArrayClauseRefusals(t *testing.T) {
	cases := []struct {
		name string
		cond *predicate.ArrayCondition
		want error
	}{
		{"missing operator", &predicate.ArrayCondition{JsonPath: "$.s[*]", Values: []any{1}}, ErrInvalidCondition},
		{"unknown operator", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "FROBNICATE", Values: []any{1}}, ErrInvalidCondition},
		{"missing values", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS"}, ErrInvalidCondition},
		{"empty values", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS", Values: []any{}}, ErrInvalidCondition},
		{"all-null values", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "EQUALS", Values: []any{nil, nil}}, ErrInvalidCondition},
		{"BETWEEN entry not a pair", &predicate.ArrayCondition{JsonPath: "$.s[*]", OperatorType: "BETWEEN", Values: []any{5}}, ErrInvalidCondition},
		{"earlier wildcard", &predicate.ArrayCondition{JsonPath: "$.orders[*].lines[*]", OperatorType: "EQUALS", Values: []any{"a"}}, ErrInvalidFieldPath},
		{"matrix wildcard", &predicate.ArrayCondition{JsonPath: "$.m[*][*]", OperatorType: "EQUALS", Values: []any{1}}, ErrInvalidFieldPath},
	}
	for _, tc := range cases {
		if err := ValidateCondition(tc.cond); !errors.Is(err, tc.want) {
			t.Errorf("%s: ValidateCondition: want %v, got %v", tc.name, tc.want, err)
		}
		if tc.want == ErrInvalidCondition {
			if err := ValidateCriterionCondition(tc.cond); !errors.Is(err, ErrInvalidCondition) {
				t.Errorf("%s: ValidateCriterionCondition: want ErrInvalidCondition, got %v", tc.name, err)
			}
		}
	}
}

func TestValidateCondition_ArrayClauseAcceptsOperators(t *testing.T) {
	for _, c := range []*predicate.ArrayCondition{
		{JsonPath: "$.s[*]", OperatorType: "GREATER_THAN", Values: []any{50, nil, 70}},
		{JsonPath: "$.s[*]", OperatorType: "BETWEEN_INCLUSIVE", Values: []any{[]any{1, 5}}},
		{JsonPath: "$.s[*]", OperatorType: "IS_NULL", Values: []any{nil, true}},
		{JsonPath: "$.orders[0].lines[*]", OperatorType: "EQUALS", Values: []any{"a"}},
	} {
		if err := ValidateCondition(c); err != nil {
			t.Errorf("%+v: unexpected error: %v", c, err)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/domain/search/ -run 'ArrayClause'` — Expected: FAIL on the operator, values and wildcard cases.

- [ ] **Step 3: Implement**

`jsonpath_grammar.go`, inside `ValidateArrayClauseJSONPath` after the suffix check:

```go
	if strings.Count(path, "[*]") != 1 {
		return fmt.Errorf(
			"%w: jsonPath %q has a wildcard before its trailing \"[*]\"; "+
				"an array clause tests the positions of one array — index the outer array (e.g. \"$.orders[0].lines[*]\")",
			errInvalidFieldPath, path)
	}
```

`operators.go`, `validateConditionAtDepth` array arm:

```go
	case *predicate.ArrayCondition:
		// The clause as a whole: its path names the elements of one array.
		if err := ValidateArrayClauseJSONPath(c.JsonPath); err != nil {
			return err
		}
		// Its meaning is its desugared leaves (spi.DesugarCondition); each is
		// held to the simple-clause rules.
		d, err := spi.DesugarCondition(c)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCondition, err)
		}
		return validateConditionAtDepth(d, depth+1)
```

`validateCriterionConditionAtDepth` array arm — same, without the path check (workflow's `walkCriterion` checks the path first):

```go
	case *predicate.ArrayCondition:
		d, err := spi.DesugarCondition(c)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCondition, err)
		}
		return validateCriterionConditionAtDepth(d, depth+1)
```

Note the desugared leaves have bracket paths (`$.s[0]`), which `ValidateConditionJSONPath` accepts. Delete the two obsolete "ArrayCondition doesn't carry an operator" comments. Add the `spi` import if missing.

- [ ] **Step 4: Run** — `go test ./internal/domain/search/ ./internal/domain/entity/ ./internal/domain/workflow/` — Expected: PASS (fix any existing test whose array fixture lacks an operator by adding `"operatorType":"EQUALS"`).

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/jsonpath_grammar.go internal/domain/search/operators.go internal/domain/search/array_condition_validate_test.go <other edited tests>
git commit -m "fix(search)!: the array clause is validated as its positional clauses"
```

---

### Task 6: cyoda-go — workflow import refuses a criterion that does not parse; array criteria

**Files:**
- Modify: `internal/domain/workflow/validate.go:327-363` (`validateCriterion` and its doc), `:142` comment in `checkCriterionResponseTimeout`
- Modify: `internal/domain/workflow/criterion_regex_test.go:132-140` (invert `TestValidateWorkflowStructure_UnparseableCriterionNotNewlyRejected`)
- Modify: `internal/domain/workflow/criterion_path_test.go:34` (array fixture gains `"operatorType":"EQUALS"`)
- Test: `internal/domain/workflow/criterion_operator_test.go`

**Interfaces:**
- Consumes: Task 5's validators (via `walkCriterion` → `search.ValidateArrayClauseJSONPath`, `search.ValidateCriterionCondition`).
- Produces: `validateImportRequest` returns an error naming the workflow/transition for an unparseable criterion and for every array-clause refusal.

- [ ] **Step 1: Write the failing tests** (append to `criterion_operator_test.go`; `wfWithTransitionCriterion`, `wfWithWorkflowCriterion` exist)

```go
func TestValidateImportRequest_RejectsUnparseableCriterion(t *testing.T) {
	for _, c := range []string{`{"type":"bogus"}`, `[1,2]`, `{"type":"simple","jsonPath":5}`, `{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":"x"}`} {
		err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))})
		if err == nil {
			t.Errorf("%s: an unparseable criterion must be rejected at import", c)
			continue
		}
		if !strings.Contains(err.Error(), "wf-regex") {
			t.Errorf("%s: error must name the workflow, got: %v", c, err)
		}
	}
}

func TestValidateImportRequest_ArrayCriterion(t *testing.T) {
	reject := []string{
		`{"type":"array","jsonPath":"$.t[*]","values":["a"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"FROBNICATE","values":["a"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":[null]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","value":["a"]}`,
		`{"type":"array","jsonPath":"$.o[*].t[*]","operatorType":"EQUALS","values":["a"]}`,
		`{"type":"array","jsonPath":"$.t[*]","operatorType":"MATCHES_PATTERN","values":["("]}`,
	}
	for _, c := range reject {
		if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(c))}); err == nil {
			t.Errorf("%s: must be rejected at import", c)
		}
	}
	ok := `{"type":"array","jsonPath":"$.t[*]","operatorType":"GREATER_THAN","values":[1,null,3]}`
	if err := validateImportRequest([]spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(ok))}); err != nil {
		t.Errorf("valid array criterion rejected: %v", err)
	}
}
```

Invert the old test in `criterion_regex_test.go`:

```go
// (f) a criterion that does not parse as a condition is refused at import:
// it can never be evaluated, so every save that reached it would fail.
func TestValidateWorkflowStructure_UnparseableCriterionRejected(t *testing.T) {
	wf := wfWithTransitionCriterion(json.RawMessage(`{"type":"totally-unknown-shape"}`))
	if err := validateWorkflowStructure(wf); err == nil {
		t.Fatal("an unparseable criterion must be rejected at import")
	}
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/domain/workflow/ -run 'Unparseable|ArrayCriterion'` — Expected: FAIL (`<nil>` for unparseable criteria).

- [ ] **Step 3: Implement** — in `validateCriterion`:

```go
	cond, err := predicate.ParseCondition(trimmed)
	if err != nil {
		return fmt.Errorf("%s: criterion is not a valid condition: %w", location, err)
	}
```

Rewrite the doc paragraph that says an unparseable criterion "is left alone" to say it is refused, because it can never be evaluated. In `checkCriterionResponseTimeout`, change the `// not this rule's to report; see validateCriterion` comment to state that `validateCriterion` has already refused it. Confirm the error surfaces as `400 VALIDATION_FAILED` (the import handler wraps every `validateImportRequest` error that way — read `handler.go:357` to confirm).

- [ ] **Step 4: Run** — `go test ./internal/domain/workflow/` — Expected: PASS. If an existing fixture now fails because its criterion never parsed, fix the fixture (it was a latent broken criterion), never the rule.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/validate.go internal/domain/workflow/criterion_regex_test.go internal/domain/workflow/criterion_path_test.go internal/domain/workflow/criterion_operator_test.go
git commit -m "fix(workflow)!: import refuses a criterion that does not parse"
```

---

### Task 7: cyoda-go — the spec

**Files:**
- Modify: `api/openapi.yaml` — `ArrayConditionDto` (`:9495`), `LifecycleConditionDto` (`:9726`), `SimpleConditionDto` (`:9818`); new component `ConditionOperatorType`
- Regenerate: `api/generated.go` (`go generate ./api`)
- Modify: `.github/oasdiff-err-ignore.txt`
- Test: `api/condition_dto_test.go` (new); `internal/domain/search/array_clause_spec_example_test.go` (new)

**Interfaces:**
- Consumes: `spi.OperatorNames()`; Task 5's `search.ValidateCondition`.
- Produces: `#/components/schemas/ConditionOperatorType` (string enum of the 26). `ArrayConditionDto` = `{type, jsonPath, operatorType, values}`.

- [ ] **Step 1: Write the failing tests**

`api/condition_dto_test.go`:

```go
package api

import (
	"slices"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The operator enum each condition DTO publishes is the set the server
// accepts — no more (a listed name the server refuses misleads every
// generated client) and no less.
func TestConditionDtoOperatorEnumsMatchServer(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	want := spi.OperatorNames()
	for _, dto := range []string{"ArrayConditionDto", "SimpleConditionDto", "LifecycleConditionDto"} {
		s := doc.Components.Schemas[dto]
		if s == nil || s.Value == nil {
			t.Fatalf("%s missing", dto)
		}
		var op *openapi3.SchemaRef
		for _, part := range s.Value.AllOf {
			if p := part.Value.Properties["operatorType"]; p != nil {
				op = p
			}
		}
		if op == nil || op.Value == nil {
			t.Fatalf("%s.operatorType missing", dto)
		}
		var got []string
		for _, v := range op.Value.Enum {
			got = append(got, v.(string))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s.operatorType enum = %v, want %v", dto, got, want)
		}
	}
}

func TestArrayConditionDtoShape(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	s := doc.Components.Schemas["ArrayConditionDto"].Value
	props := map[string]bool{}
	required := map[string]bool{}
	for _, part := range s.AllOf {
		for k := range part.Value.Properties {
			props[k] = true
		}
		for _, r := range part.Value.Required {
			required[r] = true
		}
	}
	for _, r := range s.Required {
		required[r] = true
	}
	for _, want := range []string{"jsonPath", "operatorType", "values"} {
		if !props[want] || !required[want] {
			t.Errorf("ArrayConditionDto.%s must be a required property", want)
		}
	}
	if props["value"] {
		t.Error("ArrayConditionDto must not declare `value`; the server reads `values`")
	}
}
```

(Add `"github.com/getkin/kin-openapi/openapi3"` to the imports. Match whether the required list sits on the `allOf` member or the outer schema by reading how the file currently writes it.)

`internal/domain/search/array_clause_spec_example_test.go` — the spec's own example must parse, validate and mean what it says:

```go
package search_test

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

func TestArrayConditionDtoExampleParsesAndValidates(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	ex := doc.Components.Schemas["ArrayConditionDto"].Value.Example
	if ex == nil {
		t.Fatal("ArrayConditionDto has no whole-clause example")
	}
	body, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	cond, err := predicate.ParseCondition(body)
	if err != nil {
		t.Fatalf("spec example does not parse: %v (%s)", err, body)
	}
	if err := search.ValidateCondition(cond); err != nil {
		t.Fatalf("spec example does not validate: %v (%s)", err, body)
	}
	d, err := spi.DesugarCondition(cond)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := d.(*predicate.GroupCondition)
	if !ok || len(g.Conditions) != 2 {
		t.Fatalf("spec example must test two positions, got %#v", d)
	}
	for i, wantPath := range []string{"$.scores[0]", "$.scores[2]"} {
		leaf := g.Conditions[i].(*predicate.SimpleCondition)
		if leaf.JsonPath != wantPath || leaf.OperatorType != "GREATER_THAN" {
			t.Errorf("leaf %d = %+v, want %s GREATER_THAN", i, leaf, wantPath)
		}
	}
}
```

If `internal/domain/search` (non-test) already imports `api` and an external test package causes no cycle, keep it; if a cycle appears, move the test to `internal/domain/search/searchtest` or `internal/e2e` unchanged in content.

- [ ] **Step 2: Run to verify they fail** — `go test ./api/ -run 'ConditionDto' && go test ./internal/domain/search/ -run SpecExample` — Expected: FAIL (62-name enums; `value` present; no example).

- [ ] **Step 3: Edit the spec**

New component (place it next to `AbstractConditionDto`):

```yaml
    ConditionOperatorType:
      type: string
      description: >-
        A leaf condition's operator. This is the set the server accepts on a
        `simple`, `lifecycle` and `array` clause; any other name is refused
        with 400 `INVALID_CONDITION`. See `cyoda help predicates` for each
        operator's meaning and the field types it applies to.
      enum: [BETWEEN, BETWEEN_INCLUSIVE, CONTAINS, ENDS_WITH, EQUALS,
        GREATER_OR_EQUAL, GREATER_THAN, ICONTAINS, IENDS_WITH, IEQUALS,
        INOT_CONTAINS, INOT_ENDS_WITH, INOT_EQUAL, INOT_STARTS_WITH,
        ISTARTS_WITH, IS_NULL, LESS_OR_EQUAL, LESS_THAN, LIKE,
        MATCHES_PATTERN, NOT_CONTAINS, NOT_ENDS_WITH, NOT_EQUAL, NOT_NULL,
        NOT_STARTS_WITH, STARTS_WITH]
```

In all three DTOs replace the inline `operatorType: {type: string, enum: [...]}` with `operatorType: {$ref: "#/components/schemas/ConditionOperatorType"}`. (If the enum test cannot read through the `$ref`, `op.Value` is the resolved schema — kin-openapi resolves refs on load.)

`ArrayConditionDto` (keep the long `jsonPath` grammar description; change its `example` to `$.scores[*]` and append one sentence: "An `array` clause's path must end in `[*]` and contain no other `[*]`."):

```yaml
            operatorType:
              $ref: "#/components/schemas/ConditionOperatorType"
            values:
              type: array
              minItems: 1
              description: >-
                One entry per array position, from index 0. `operatorType`
                is applied at every position whose entry is not `null`; a
                `null` entry tests nothing at that position. An entry is a
                scalar, or a `[low, high]` pair for `BETWEEN` and
                `BETWEEN_INCLUSIVE`. For `IS_NULL` and `NOT_NULL` a non-null
                entry only selects the position. At least one entry must be
                non-null; a clause that tests no position is refused with
                400 `INVALID_CONDITION`.
              items:
                nullable: true
                oneOf:
                  - type: string
                  - type: number
                  - type: boolean
                  - type: array
                    minItems: 2
                    maxItems: 2
      required:
        - jsonPath
        - operatorType
        - type
        - values
      example:
        type: array
        jsonPath: $.scores[*]
        operatorType: GREATER_THAN
        values: [50, null, 70]
```

Check the file's OpenAPI version (`openapi:` line at the top): under 3.1 write nullability as `type: [string, "null"]`-style or `oneOf` with `{type: "null"}` instead of `nullable: true`, following how the file already spells a nullable property (`grep -n 'nullable\|"null"' api/openapi.yaml | head`).

Then: `go generate ./api`, `go build ./... && go vet ./...`. Fix compile errors where code used the old generated enum types or `ArrayConditionDto.Value` (`grep -rn 'ArrayConditionDtoOperatorType\|SimpleConditionDtoOperatorType\|LifecycleConditionDtoOperatorType' --include='*.go' . | grep -v generated.go`).

- [ ] **Step 4: oasdiff allowances**

Run the gate locally against the base (the pinned version is in `.github/workflows/openapi-breaking-change.yml` — use it):

```bash
git show origin/release/v0.9.0:api/openapi.yaml > /tmp/base-openapi.yaml
go run github.com/oasdiff/oasdiff@<pinned version> breaking /tmp/base-openapi.yaml api/openapi.yaml --fail-on ERR --format singleline 2>&1 | tee /tmp/oasdiff-627.txt
```

Append each reported ERR line verbatim to `.github/oasdiff-err-ignore.txt` under a new comment block:

```
# Condition DTOs (ADR 0003 Decision 7 — spec-to-reality reconciliation; the prior spec never matched the server):
# ArrayConditionDto declared operatorType + value:[string]; the server has always read `values` and ignored
# both declared fields. The operator enums on Array/Simple/LifecycleConditionDto listed 62 names; the server
# has always refused every name outside spi.OperatorNames() (26) with 400 INVALID_CONDITION. No working
# client sends a refused shape or name.
```

Re-run the command with `--err-ignore .github/oasdiff-err-ignore.txt`; Expected: exit 0. Paste only ERR lines that come from these three DTOs; any other ERR line is a mistake in the edit — fix the spec instead.

- [ ] **Step 5: Run** — `go test ./api/ ./internal/domain/search/ && make check-codegen` — Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add api/openapi.yaml api/generated.go api/condition_dto_test.go internal/domain/search/array_clause_spec_example_test.go .github/oasdiff-err-ignore.txt <any compile-fix files>
git commit -m "fix(openapi)!: ArrayConditionDto matches the parser; condition operator enums list what the server accepts"
```

---

### Task 8: cyoda-go — e2e over HTTP

**Files:**
- Create: `internal/e2e/search_array_clause_operator_test.go`
- Modify: `internal/e2e/workflow_criterion_array_clause_test.go` (fixture gains `"operatorType":"EQUALS"`; new criterion tests)

**Interfaces:**
- Consumes: helpers `setupModelSampleWithWorkflow(t, model, sample, workflowJSON)`, `createEntityE2E`, `doAuth`, `readBody`, `directSearch`, `submitAsyncSearch`, `waitForAsyncSearch`, `decodeBuckets`, `findBucket`, `getEntityState`, `queryDB`, `commontest.ExpectErrorCode`, `importModelSampleE2E`, `lockModelE2E` (all in `internal/e2e`).

- [ ] **Step 1: Write the tests** — `internal/e2e/search_array_clause_operator_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
)

// An array clause applies its operatorType at every position whose entry in
// `values` is not null. Entities: A scores [90,40,75], B scores [10,95,60].

const scoresWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "scores-wf", "initialState": "NONE", "active": true,
		"states": {"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]}, "CREATED": {}}
	}]
}`

func setupScoresModel(t *testing.T, model string) (idA, idB string) {
	t.Helper()
	setupModelSampleWithWorkflow(t, model, `{"name":"S","scores":[1,2,3]}`, scoresWorkflow)
	idA = createEntityE2E(t, model, 1, `{"name":"A","scores":[90,40,75]}`)
	idB = createEntityE2E(t, model, 1, `{"name":"B","scores":[10,95,60]}`)
	return idA, idB
}

// arrayClauseRefusals is every 4xx row of the spec's error table that a
// condition body can trigger, with the code each surface answers.
var arrayClauseRefusals = []struct {
	name, cond, code string
}{
	{"missing operator", `{"type":"array","jsonPath":"$.scores[*]","values":[50]}`, "INVALID_CONDITION"},
	{"old spec shape", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","value":["90"]}`, "INVALID_CONDITION"},
	{"unknown operator", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"FROBNICATE","values":[50]}`, "INVALID_CONDITION"},
	{"all null", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null,null]}`, "INVALID_CONDITION"},
	{"empty values", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[]}`, "INVALID_CONDITION"},
	{"object entry", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[{"a":1}]}`, "INVALID_CONDITION"},
	{"BETWEEN not a pair", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN","values":[5]}`, "INVALID_CONDITION"},
	{"values not an array", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":"x"}`, "BAD_REQUEST"},
	{"bare path", `{"type":"array","jsonPath":"$.scores","operatorType":"EQUALS","values":[90]}`, "INVALID_FIELD_PATH"},
	{"text operator on numbers", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"CONTAINS","values":["9"]}`, "CONDITION_TYPE_MISMATCH"},
}

func TestSearch_Sync_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-sync"
	setupScoresModel(t, model)
	for _, tc := range []struct {
		name, cond, want string // want: the one entity name expected
	}{
		{"GREATER_THAN at 0 and 2", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`, "A"},
		{"BETWEEN at 1", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN_INCLUSIVE","values":[null,[30,45]]}`, "A"},
		{"Cloud alias operation", `{"type":"array","jsonPath":"$.scores[*]","operation":"LESS_THAN","values":[50]}`, "B"},
	} {
		status, results := directSearch(t, model, 1, tc.cond)
		if status != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", tc.name, status)
		}
		if len(results) != 1 || extractDataString(t, results[0], "name") != tc.want {
			t.Errorf("%s: want only %s, got %v", tc.name, tc.want, results)
		}
	}
}

func TestSearch_Sync_ArrayClauseRefusals(t *testing.T) {
	const model = "e2e-array-op-sync-refuse"
	setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/direct/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
}

func TestSearch_Async_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-async"
	idA, _ := setupScoresModel(t, model)
	jobID := submitAsyncSearch(t, model, 1, `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`)
	if s := waitForAsyncSearch(t, jobID, 10*time.Second); s != "SUCCESSFUL" {
		t.Fatalf("expected SUCCESSFUL, got %q", s)
	}
	resp := doAuth(t, http.MethodGet, fmt.Sprintf("/api/search/async/%s?pageSize=10&pageNumber=0", jobID), "")
	body := readBody(t, resp)
	var page struct {
		Content []struct {
			Meta struct{ ID string `json:"id"` } `json:"meta"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v; body %s", err, body)
	}
	if len(page.Content) != 1 || page.Content[0].Meta.ID != idA {
		t.Errorf("want only %s, got %s", idA, body)
	}
}

func TestSearch_Async_ArrayClauseRefusals_NoJobIssued(t *testing.T) {
	const model = "e2e-array-op-async-refuse"
	setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/search/async/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
	if n := queryDB(t, "test-tenant", "SELECT count(*) FROM search_jobs WHERE model_name = $1", model); n != 0 {
		t.Errorf("refused conditions issued %d job(s)", n)
	}
}

func TestDeleteConditional_ArrayClauseOperator(t *testing.T) {
	const model = "e2e-array-op-delete"
	idA, idB := setupScoresModel(t, model)
	resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model),
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[null,90]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if r := doAuth(t, http.MethodGet, "/api/entity/"+idB, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("B (scores[1]=95) must be deleted, got %d", r.StatusCode)
	}
	if r := doAuth(t, http.MethodGet, "/api/entity/"+idA, ""); r.StatusCode != http.StatusOK {
		t.Errorf("A (scores[1]=40) must survive, got %d", r.StatusCode)
	}
}

func TestDeleteConditional_ArrayClauseRefusals_NothingDeleted(t *testing.T) {
	const model = "e2e-array-op-delete-refuse"
	idA, idB := setupScoresModel(t, model)
	for _, tc := range arrayClauseRefusals {
		resp := doAuth(t, http.MethodDelete, fmt.Sprintf("/api/entity/%s/1", model), tc.cond)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, tc.code)
		resp.Body.Close()
	}
	for _, id := range []string{idA, idB} {
		if r := doAuth(t, http.MethodGet, "/api/entity/"+id, ""); r.StatusCode != http.StatusOK {
			t.Errorf("entity %s must survive refused deletes, got %d", id, r.StatusCode)
		}
	}
}

func TestGroupedStats_ArrayClauseCondition(t *testing.T) {
	const model = "e2e-array-op-stats"
	setupScoresModel(t, model)
	path := fmt.Sprintf("/api/entity/stats/%s/1/query", model)
	resp := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	buckets := decodeBuckets(t, body)
	if len(buckets) != 1 || findBucket(buckets, "$.name", "A") == nil {
		t.Errorf("want one bucket for A, got %s", body)
	}
	for _, tc := range arrayClauseRefusals {
		r := doAuth(t, http.MethodPost, path, `{"groupBy":["$.name"],"condition":`+tc.cond+`}`)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d; body: %s", tc.name, r.StatusCode, readBody(t, r))
		}
		commontest.ExpectErrorCode(t, r, tc.code)
		r.Body.Close()
	}
}
```

Check `findBucket`'s path argument format against `grouped_stats_test.go` (it may be `"$.name"` or `"name"`); and check the grouped-stats endpoint's code for a body that does not parse — if it is not `BAD_REQUEST`, the stats loop asserts that endpoint's existing parse-failure code and the spec's error table is corrected to match. Read, do not guess.

In `workflow_criterion_array_clause_test.go`: the fixture becomes `{"type":"array","jsonPath":%q,"operatorType":"EQUALS","values":["A"]}`; add:

```go
func TestCriterionArrayClause_OperatorDecidesTheTransition(t *testing.T) {
	const model = "e2e-crit-array-op"
	wf := `{"importMode":"REPLACE","workflows":[{"version":"1.1","name":"crit-array-op-wf","initialState":"NONE","active":true,
		"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
		"CREATED":{"transitions":[{"name":"advance","next":"ADVANCED","manual":false,
			"criterion":{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}}]},
		"ADVANCED":{}}}]}`
	setupModelSampleWithWorkflow(t, model, `{"name":"S","scores":[1,2,3]}`, wf)
	if s := getEntityState(t, createEntityE2E(t, model, 1, `{"name":"A","scores":[90,40,75]}`)); s != "ADVANCED" {
		t.Errorf("A satisfies the criterion: want ADVANCED, got %q", s)
	}
	if s := getEntityState(t, createEntityE2E(t, model, 1, `{"name":"B","scores":[10,95,60]}`)); s != "CREATED" {
		t.Errorf("B does not: want CREATED, got %q", s)
	}
}

func TestCriterionArrayClause_RefusedAtImport(t *testing.T) {
	const model = "e2e-crit-array-refuse"
	importModelSampleE2E(t, model, 1, `{"name":"S","scores":[1]}`)
	lockModelE2E(t, model, 1)
	for _, crit := range []string{
		`{"type":"array","jsonPath":"$.scores[*]","values":[50]}`,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null]}`,
		`{"type":"array","jsonPath":"$.o[*].scores[*]","operatorType":"EQUALS","values":[1]}`,
		`{"type":"bogus"}`,
	} {
		wf := fmt.Sprintf(`{"importMode":"REPLACE","workflows":[{"version":"1.1","name":"w","initialState":"NONE","active":true,
			"states":{"NONE":{"transitions":[{"name":"go","next":"DONE","manual":false,"criterion":%s}]},"DONE":{}}}]}`, crit)
		resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", model), wf)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", crit, resp.StatusCode, readBody(t, resp))
		}
		commontest.ExpectErrorCode(t, resp, "VALIDATION_FAILED")
		resp.Body.Close()
	}
}
```

- [ ] **Step 2: Run** — `go test -timeout 30m ./internal/e2e/ -run 'ArrayClause|ArrayCondition|CriterionArrayClause'` (needs Docker; `make preflight` first). Expected: PASS. A `-run`-isolated green does not prove the conformance report (CI runs the whole package).

- [ ] **Step 3: Commit**

```bash
git add internal/e2e/search_array_clause_operator_test.go internal/e2e/workflow_criterion_array_clause_test.go
git commit -m "test(e2e): array clause operator and refusals on every HTTP surface"
```

---

### Task 9: cyoda-go — gRPC

**Files:**
- Create: `internal/grpc/search_array_clause_test.go`

**Interfaces:**
- Consumes: `newTestEnv`, `importAndLockModel`, `makeCE`, `mockEntityStream`, `validateResponse`, `EntitySearchRequest`, `EntitySnapshotSearchRequest`, `EntityCreateRequest`, `nilUUID`, `events.EntityResponseJson`, `events.EntitySnapshotSearchResponseJson` (all in `internal/grpc` tests).

- [ ] **Step 1: Write the tests**

```go
package grpc

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

func scoresEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context) {
	t.Helper()
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "scored", "1", map[string]any{"name": "S", "scores": []any{1, 2, 3}})
	for i, scores := range [][]any{{90, 40, 75}, {10, 95, 60}} {
		if _, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
			"id": fmt.Sprintf("c-scores-%d", i), "dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "scored", "version": 1},
				"data":  map[string]any{"name": []string{"A", "B"}[i], "scores": scores},
			},
		})); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	return svc, ctx
}

func directSearchCE(cond map[string]any) *cepb.CloudEvent {
	return makeCE(EntitySearchRequest, map[string]any{
		"id": "s-array", "model": map[string]any{"name": "scored", "version": 1}, "condition": cond,
	})
}

func TestRPC_DirectSearch_ArrayClauseOperator(t *testing.T) {
	svc, ctx := scoresEnv(t)
	stream := &mockEntityStream{ctx: ctx}
	err := svc.EntitySearchCollection(directSearchCE(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "operatorType": "GREATER_THAN", "values": []any{50, nil, 70},
	}), stream)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("want exactly A, got %d results", len(stream.sent))
	}
	var typed events.EntityResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if !typed.Success || typed.Payload.Data.(map[string]any)["name"] != "A" {
		t.Errorf("want A, got %+v", typed)
	}
}

func TestRPC_DirectSearch_ArrayClauseRefusals(t *testing.T) {
	svc, ctx := scoresEnv(t)
	for _, tc := range []struct {
		name string
		cond map[string]any
		code string
	}{
		{"missing operator", map[string]any{"type": "array", "jsonPath": "$.scores[*]", "values": []any{50}}, "INVALID_CONDITION"},
		{"all null", map[string]any{"type": "array", "jsonPath": "$.scores[*]", "operatorType": "EQUALS", "values": []any{nil}}, "INVALID_CONDITION"},
		{"earlier wildcard", map[string]any{"type": "array", "jsonPath": "$.o[*].scores[*]", "operatorType": "EQUALS", "values": []any{1}}, "INVALID_FIELD_PATH"},
		{"text operator on numbers", map[string]any{"type": "array", "jsonPath": "$.scores[*]", "operatorType": "CONTAINS", "values": []any{"9"}}, "CONDITION_TYPE_MISMATCH"},
	} {
		stream := &mockEntityStream{ctx: ctx}
		if err := svc.EntitySearchCollection(directSearchCE(tc.cond), stream); err != nil {
			t.Fatalf("%s: transport error: %v", tc.name, err)
		}
		if len(stream.sent) != 1 {
			t.Fatalf("%s: want 1 error response, got %d", tc.name, len(stream.sent))
		}
		var typed events.EntityResponseJson
		validateResponse(t, stream.sent[0], &typed)
		if typed.Success || typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" || !strings.Contains(typed.Error.Message, tc.code) {
			t.Errorf("%s: want CLIENT_ERROR naming %s, got %+v", tc.name, tc.code, typed.Error)
		}
	}
}

func TestRPC_DirectSearch_ArrayClauseValuesNotArray_InvalidArgument(t *testing.T) {
	svc, ctx := scoresEnv(t)
	err := svc.EntitySearchCollection(directSearchCE(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "operatorType": "EQUALS", "values": "x",
	}), &mockEntityStream{ctx: ctx})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestRPC_SnapshotSearch_ArrayClause(t *testing.T) {
	svc, ctx := scoresEnv(t)
	ce := func(cond map[string]any) *cepb.CloudEvent {
		return makeCE(EntitySnapshotSearchRequest, map[string]any{
			"id": "s-array-snap", "model": map[string]any{"name": "scored", "version": 1}, "condition": cond,
		})
	}
	resp, err := svc.EntitySearch(ctx, ce(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "operatorType": "GREATER_THAN", "values": []any{50, nil, 70},
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	var ok events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &ok)
	if !ok.Success || ok.Status.SnapshotID == nilUUID {
		t.Fatalf("want an accepted snapshot job, got %+v", ok)
	}

	resp, err = svc.EntitySearch(ctx, ce(map[string]any{
		"type": "array", "jsonPath": "$.scores[*]", "values": []any{50},
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	var bad events.EntitySnapshotSearchResponseJson
	validateResponse(t, resp, &bad)
	if bad.Success || bad.Error == nil || bad.Error.Code != "CLIENT_ERROR" || !strings.Contains(bad.Error.Message, "INVALID_CONDITION") {
		t.Errorf("missing operator: want CLIENT_ERROR naming INVALID_CONDITION, got %+v", bad.Error)
	}
	if bad.Status.SnapshotID != nilUUID {
		t.Errorf("refused condition must not create a job, got %s", bad.Status.SnapshotID)
	}
}
```

Fix imports to what the package uses (`context`, `fmt`, the `cepb` alias — copy from `search_test.go`'s import block). The snapshot happy path asserts acceptance; the result content of an async job is covered by Task 8 and Task 10.

- [ ] **Step 2: Run** — `go test ./internal/grpc/ -run 'ArrayClause'` — Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/grpc/search_array_clause_test.go
git commit -m "test(grpc): array clause operator and refusals on both search entry points"
```

---

### Task 10: cyoda-go — cross-backend parity

**Files:**
- Create: `e2e/parity/array_clause_operator.go`
- Modify: `e2e/parity/path_addressing.go:49-51,106` (array bodies gain `"operatorType":"EQUALS"`)
- Modify: `e2e/parity/registry.go` (register next to `ArrayClausePositional`), `e2e/parity/registry_count_test.go` (count +1)

**Interfaces:**
- Consumes: `client.NewClient`, `c.SyncSearch`, `c.SyncSearchRaw`, `c.AwaitAsyncSearchResults`, `setupModelWithWorkflow`, `searchWorkflowJSON`, `containsErrorCode`, `fixture.NewTenant`, `fixture.BaseURL`.
- Produces: `func RunArrayClauseOperator(t *testing.T, fixture BackendFixture)`.

- [ ] **Step 1: Write the scenario**

```go
package parity

import (
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunArrayClauseOperator pins that an array clause's operatorType applies at
// every tested position, identically on every backend, on the pushed-down
// path, on the in-memory path (the clause inside NOT), and through async
// search, which stores the condition and re-parses it where the job runs.
func RunArrayClauseOperator(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const modelName = "parity-array-clause-operator"
	const modelVersion = 1
	setupModelWithWorkflow(t, c, modelName, modelVersion, `{"name":"S","scores":[1,2,3]}`, searchWorkflowJSON)
	for _, doc := range []string{`{"name":"A","scores":[90,40,75]}`, `{"name":"B","scores":[10,95,60]}`} {
		if _, err := c.CreateEntity(t, modelName, modelVersion, doc); err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
	}

	const gt = `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`
	// Each case states its expected count, or (want < 0) the simple clause it
	// means: past the end of the array the answer is whatever the positional
	// simple clause answers under the vacuity rules (path-grammar.md §5).
	cases := []struct {
		name, condition string
		want            int
		equiv           string
	}{
		{"GREATER_THAN per position", gt, 1, ""},
		{"inside NOT", `{"type":"group","operator":"NOT","conditions":[` + gt + `]}`, 1, ""},
		{"BETWEEN per position", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"BETWEEN_INCLUSIVE","values":[null,[90,100]]}`, 1, ""},
		{"IS_NULL past the end", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"IS_NULL","values":[null,null,null,true]}`, -1,
			`{"type":"simple","jsonPath":"$.scores[3]","operatorType":"IS_NULL","value":true}`},
		{"NOT_EQUAL past the end", `{"type":"array","jsonPath":"$.scores[*]","operatorType":"NOT_EQUAL","values":[null,null,null,null,null,7]}`, -1,
			`{"type":"simple","jsonPath":"$.scores[5]","operatorType":"NOT_EQUAL","value":7}`},
	}
	for _, tc := range cases {
		results, err := c.SyncSearch(t, modelName, modelVersion, tc.condition)
		if err != nil {
			t.Fatalf("[%s] SyncSearch: %v", tc.name, err)
		}
		want := tc.want
		if want < 0 {
			same, err := c.SyncSearch(t, modelName, modelVersion, tc.equiv)
			if err != nil {
				t.Fatalf("[%s] simple equivalent: %v", tc.name, err)
			}
			want = len(same)
		}
		if len(results) != want {
			t.Errorf("[%s] want %d result(s), got %d", tc.name, want, len(results))
		}
	}

	page, err := c.AwaitAsyncSearchResults(t, modelName, modelVersion, gt, 30*time.Second)
	if err != nil {
		t.Fatalf("async: %v", err)
	}
	if len(page.Content) != 1 {
		t.Errorf("async GREATER_THAN: want 1 result, got %d", len(page.Content))
	}

	status, body, err := c.SyncSearchRaw(t, modelName, modelVersion,
		`{"type":"array","jsonPath":"$.scores[*]","operatorType":"EQUALS","values":[null]}`)
	if err != nil {
		t.Fatalf("SyncSearchRaw: %v", err)
	}
	if status != http.StatusBadRequest || !containsErrorCode(body, "INVALID_CONDITION") {
		t.Errorf("clause testing nothing: want 400 INVALID_CONDITION, got %d %s", status, body)
	}
}
```

Register: in `registry.go` after `{"ArrayClausePositional", RunArrayClausePositional},` add `{"ArrayClauseOperator", RunArrayClauseOperator},` with a two-line comment; bump the expected count in `registry_count_test.go`.

- [ ] **Step 2: Run** — `make test` (runs the parity suites uncached on memory, sqlite, postgres) — Expected: PASS, with `ArrayClauseOperator` executed on each backend.

- [ ] **Step 3: Commit**

```bash
git add e2e/parity/array_clause_operator.go e2e/parity/path_addressing.go e2e/parity/registry.go e2e/parity/registry_count_test.go
git commit -m "test(parity): the array clause's operator answers identically on every backend"
```

---

### Task 11: cyoda-go — documentation

**Files:**
- Modify: `cmd/cyoda/help/content/search.md` (`ArrayCondition` section `:167-181`; error-cause list near `:391`)
- Modify: `cmd/cyoda/help/content/predicates.md:99` (INVALID_CONDITION causes)
- Modify: `cmd/cyoda/help/content/workflows.md:471-473,546` (criteria)
- Modify: `cmd/cyoda/help/content/errors/INVALID_CONDITION.md` (DESCRIPTION)
- Modify: `docs/cloud-parity/path-grammar.md` (§8 "The `array` clause", `:345-395`), `docs/cloud-parity/operator-semantics.md:280-282`
- Create: `docs/cloud-parity/array-clause.md`; Modify: `docs/cloud-parity/README.md` (index row)
- Modify: `docs/workflow-schema-versioning.md` (1.5 entry `:216-288`)
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Breaking`)

All prose: plain, short sentences, present tense, no issue ids, no history narrative.

- [ ] **Step 1: `search.md` ArrayCondition section** — replace the example and bullets with:

```markdown
**ArrayCondition** — test the elements of one array by position:

​```json
{
  "type": "array",
  "jsonPath": "$.scores[*]",
  "operatorType": "GREATER_THAN",
  "values": [50, null, 70]
}
​```

- `jsonPath`: the array's elements. It **must end in `[*]`** and contain no other `[*]`; otherwise `400 errors.INVALID_FIELD_PATH`. To test one inner array of an outer array, index the outer one: `$.orders[0].lines[*]`.
- `operatorType` (required): any operator a `simple` clause accepts (see `predicates`). It is applied at every tested position.
- `values`: one entry per position, from index 0. A `null` entry does not test that position. An entry is a scalar, or a `[low, high]` pair for `BETWEEN`/`BETWEEN_INCLUSIVE`. For `IS_NULL`/`NOT_NULL` a non-null entry only selects the position.

The clause means an `AND` of one `simple` clause per tested position: the example is `$.scores[0] > 50` AND `$.scores[2] > 70`. Every rule of a `simple` clause applies to those positional clauses, including the type check (`CONDITION_TYPE_MISMATCH`). A clause must test at least one position: a missing, empty or all-`null` `values` is `400 errors.INVALID_CONDITION`. To match every entity, send an empty `AND` group.
```

Also delete the old sentence "`values` made entirely of `null` matches every entity" wherever it appears (`grep -rn 'entirely of' cmd/cyoda/help docs/cloud-parity`).

- [ ] **Step 2: `predicates.md:99`, `INVALID_CONDITION.md`** — add to each cause list: "an `array` clause has no `operatorType` or tests no position (`values` missing, empty or all `null`)".

- [ ] **Step 3: `workflows.md`** — in the criteria paragraph (`:473`), add: "An `array` criterion needs `operatorType` and at least one non-null entry in `values`; import refuses it otherwise with `400 VALIDATION_FAILED`." In the import-refusal list (`:546` area) add: "- A criterion that is not a valid condition (unknown `type`, not a JSON object, a malformed clause). It could never be evaluated."

- [ ] **Step 4: `path-grammar.md` §8** — update the example and the desugared form to carry `operatorType` (use the `$.scores` example), replace "An `array` clause whose `values` are all null tests nothing and matches every entity" with the refusal, and add the earlier-wildcard rule with its reason (positions from different inner arrays). `operator-semantics.md:280-282`: say the array clause's operator and operands are checked as its positional `simple` clauses.

- [ ] **Step 5: `docs/cloud-parity/array-clause.md`** (new; one file per behaviour, Gate 7):

```markdown
# The array clause

cyoda-go defines the array clause's wire shape; Cyoda Cloud aligns to it.

## The contract

​```json
{"type":"array", "jsonPath":"$.scores[*]", "operatorType":"GREATER_THAN", "values":[50, null, 70]}
​```

- `operatorType` is required and is applied at every tested position. The
  accepted names are those of a `simple` clause. `operator` and `operation`
  are accepted as aliases.
- `values` holds one entry per position from index 0. `null` does not test
  that position. An entry is a scalar, or a `[low, high]` pair for `BETWEEN`
  and `BETWEEN_INCLUSIVE`. For `IS_NULL`/`NOT_NULL` a non-null entry only
  selects the position.
- The clause means an AND of one `simple` clause per tested position, and
  every `simple`-clause rule applies to those clauses.
- Refused: no operator or an unknown one, a clause that tests no position
  (missing, empty or all-null `values`), an object entry — `400
  INVALID_CONDITION`; a path that does not end in `[*]` or has an earlier
  `[*]` — `400 INVALID_FIELD_PATH`. On workflow import every refusal is `400
  VALIDATION_FAILED`.
- Accepted on every surface that takes a condition: direct and async search,
  conditional delete, grouped-stats `condition`, gRPC search, workflow and
  transition criteria.

## What Cloud changes

Cloud's `ArrayCondition` has `operation` and `value: List<String>` and
applies the operator at every listed position, on REST search only. To align:

- Rename `value` to `values`. Keep `operation` and its `operatorType` alias.
- Accept `null` entries as "do not test this position".
- Accept non-string scalars and `[low, high]` pairs; apply range operators
  per position (Cloud refuses them today).
- Refuse a missing operator, a clause that tests no position, and a path with
  a wildcard before its trailing `[*]`.
- Accept the clause in workflow criteria and in gRPC search.
```

Add the README index row: `| array-clause.md | The array clause's wire shape: a required operator applied at every tested position, null skips a position, at least one position tested, one array per clause; what Cloud changes |`.

- [ ] **Step 6: `workflow-schema-versioning.md`** — in the 1.5 entry: change "Three tightenings" to "Five tightenings" and add:

```markdown
4. **An `array` criterion carries `operatorType`, tests at least one
   position, and names one array.** A criterion without an operator imported
   and worked as `EQUALS`, so this is an optional-to-required change — a MAJOR
   change by the bump rules. It is taken inside 1.5 because no deployment of
   either tier exists and 1.5 is unreleased. An all-`null` clause matched
   every entity; it is refused, as is a path with a wildcard before its
   trailing `[*]` (its positions came from different inner arrays).
5. **A criterion that is not a valid condition is refused at import.** It
   imported and then failed every evaluation, so no working configuration is
   affected.
```

and extend "All three rules apply to an import under any schema version" to "All five rules …".

- [ ] **Step 7: `CHANGELOG.md`** — under `## [Unreleased]` → `### Breaking`:

```markdown
- **An `array` condition carries a required `operatorType`, applied at every
  tested position.** `{"type":"array","jsonPath":"$.scores[*]","operatorType":"GREATER_THAN","values":[50,null,70]}`
  means `scores[0] > 50` and `scores[2] > 70`. A clause without an operator,
  or that tests no position (`values` missing, empty or all `null`), is `400
  INVALID_CONDITION`; before, it matched every entity — a conditional delete
  in the shape the spec documented deleted the whole model. A path with a
  wildcard before its trailing `[*]` is `400 INVALID_FIELD_PATH`. Workflow
  import answers `400 VALIDATION_FAILED` for each, and now also refuses a
  criterion that is not a valid condition. `ArrayConditionDto` in the OpenAPI
  spec is `{type, jsonPath, operatorType, values}`, and the operator enums of
  the condition DTOs list only the 26 operators the server accepts.
  Migration: add `"operatorType":"EQUALS"` to existing array clauses.
```

- [ ] **Step 8: Verify the help tree** — `go test ./cmd/cyoda/...` — Expected: PASS (help-tree and error-code parity tests).

- [ ] **Step 9: Commit**

```bash
git add cmd/cyoda/help/content/search.md cmd/cyoda/help/content/predicates.md cmd/cyoda/help/content/workflows.md cmd/cyoda/help/content/errors/INVALID_CONDITION.md docs/cloud-parity/path-grammar.md docs/cloud-parity/operator-semantics.md docs/cloud-parity/array-clause.md docs/cloud-parity/README.md docs/workflow-schema-versioning.md CHANGELOG.md
git commit -m "docs: the array clause carries an operator"
```

---

### Task 12: Pin the SPI

Precondition: the SPI PR (Task 3) is reviewed and merged into `cyoda-go-spi` `main`, with its CI green on the merge commit.

**Files:** `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`, their `go.sum`; `COMPATIBILITY.md`; `go.work` (drop the local `use` line only — never commit it).

- [ ] **Step 1:** `go work edit -dropuse /Users/paul/go-projects/cyoda-light/cyoda-go-spi` and confirm `git diff go.work` is empty.
- [ ] **Step 2:** Bump to the merge commit's pseudo-version in all four `go.mod` files (`go get github.com/cyoda-platform/cyoda-go-spi@<merge sha>` in root; then `make repin-plugins` for the plugin modules per `MAINTAINING.md` §3), then `make check-spi-pin-sync`. Expected: PASS.
- [ ] **Step 3:** `COMPATIBILITY.md`: record the new SPI pseudo-version in the v0.9.0 in-flight row, with "array clause carries an operator" in the wave narrative.
- [ ] **Step 4:** `make test` and `go vet ./...` — Expected: PASS.
- [ ] **Step 5: Commit** — one commit: `chore(deps): pin cyoda-go-spi <pseudo-version> (array clause operator)`; stage the four `go.mod`, the `go.sum` files and `COMPATIBILITY.md` by name.

---

### Task 13: Outside the repository, and tracking

- [ ] **cassandra issue** in `Cyoda/cyoda-go-cassandra`, titled "Async search ignores the array clause's operator (must land before the v0.9.0 bump)". Body: cyoda-go stores the condition JSON and the plugin re-parses it with its own copy (`search/executor.go:143`, `search/predicate/parse.go:210`, `search/predicate/match.go:221` `matchArray`), which ignores `operatorType` and compares with EQUALS; with cyoda-go v0.9.0 a `GREATER_THAN` array clause returns wrong rows with a 200. Fix: use `predicate.ParseCondition` and `spi.DesugarCondition` from the SPI and delete the copy's array handling. Before stating that `ConvertJSONPath("$.scores[*]")` plus `.%d` is also wrong today, verify it with a unit test in the plugin and quote the result; otherwise leave it out. This issue is the KNOWN_CONSUMERS notification for the SPI PR — link it there.
- [ ] **CaaS ticket** (Jira project CP, `[CaaS]` title prefix, component CaaS) with the "What Cloud changes" list from `docs/cloud-parity/array-clause.md`. The Atlassian connector is disconnected in this session; if it still is, ask Paul to reconnect it (`/mcp`) — do not substitute another channel.
- [ ] **#651:** row 6 status cell → PR, merge commit, closed issue, CaaS ticket, cassandra issue; add the cassandra issue to release-cut step 3; Adjustments entry for anything that changed during implementation.
- [ ] **#627:** closed by hand when the PR merges into `release/v0.9.0`.

---

## After the tasks (gates, not tasks)

1. `superpowers:verification-before-completion`: `make test`, `go vet ./...`, `make check-codegen`.
2. Open the PR into `release/v0.9.0` (check the base), body ending with the attribution line.
3. `superpowers:requesting-code-review` with a fresh-context reviewer over the whole PR range at the PR head.
4. `cyoda-go-security-audit` over the whole PR range at the same head.
5. Any fix after 3–4 means re-running both on the new head.
6. Done = CI green on the exact PR head (read the check list).
