# Function conditions in criteria, and the function pre-check — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A criterion may hold function conditions at any depth. Each function condition may carry a pre-check of local
conditions, which guards its callout. The engine evaluates in declaration order with the stop rule. It checks
everything before the first callout and records every callout in the audit.

**Architecture:** The SPI parser keeps each function condition's raw JSON and its parsed pre-check. cyoda-go splits a
parsed criterion into a small evaluation tree with three node kinds:
- local units, which are function-free subtrees and pre-checks;
- engine groups, which are groups that hold a function;
- function nodes.

Import and the engine build the same tree with one function. Import checks it. The engine checks and prepares every
local unit, which is the compile phase, and then evaluates the tree. That evaluation dispatches each function node
with its own JSON and records what it did.

**Tech Stack:** Go 1.26, `github.com/cyoda-platform/cyoda-go-spi` (`predicate`), oapi-codegen (`go generate ./api`),
testcontainers e2e (`internal/e2e`), cross-backend parity (`e2e/parity`), the multi-node parity suite
(`e2e/parity/multinode`).

**Spec:** `docs/superpowers/specs/2026-10-11-628-function-conditions-in-criteria-design.md`. Read it before any task.
The section numbers below (§N) refer to it.

## Global Constraints

- Base branch `release/v0.9.0`; the PR targets `release/v0.9.0`. Worktree:
  `/Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/fix-628-function-precheck`, branch
  `worktree-fix-628-function-precheck`.
- SPI checkout: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`. Branch from `origin/main` (`ab06905`). SPI PR into
  `main`, **no tag**. cyoda-go pseudo-pins `main` after the merge, in **one commit** that changes the root `go.mod` and
  `plugins/{memory,sqlite,postgres}/go.mod`, plus the `COMPATIBILITY.md` v0.9.0 row.
- While the SPI change is unmerged, cyoda-go builds against it through a `use ../../../../cyoda-go-spi` line in
  `go.work`. **Never commit that line.** Before each commit, run `git diff --cached go.work` and check that it is
  empty.
- Errors: import rules answer `400 VALIDATION_FAILED`. Evaluation faults answer `400 WORKFLOW_FAILED`. Callout
  infrastructure answers 503 (`NO_COMPUTE_MEMBER_FOR_TAG`, `DISPATCH_TIMEOUT`, `COMPUTE_MEMBER_DISCONNECTED`).
  Model-store failure is a 500 with a ticket (`ErrCriterionTypingInfra`). No new error code.
- gRPC envelope: `success=false`. `Error.Code` is `CLIENT_ERROR` for the 400s and the 503s and `SERVER_ERROR` for the
  500. `Error.Message` carries the domain code.
- Audit reason strings, exactly (§8):
  - `<name> at <path>: pre-check did not match, not called`
  - `<name> at <path>: answered <true|false>`, plus `: <reason>` when the member gave one
  - clauses are joined with `; `; a whole-criterion function leaves out ` at <path>`
  - default `criterion did not match`
- Manual refusal message: `transition "<name>" criterion not matched`, plus `: <reason>` only when the reason is not
  `criterion did not match`.
- Audit `functions` entry keys: `path`, `name`, `preCheck`, `preCheckMatched`, `called`, `matches`, `reason`, with
  the presence rules of §8. `WORKFLOW_FOUND` and `TRANSITION_MAKE` get `data: {"functions": [...]}` only when the
  criterion holds a function condition.
- JSON Pointer positions: group member `/conditions/<i>`; pre-check `/function/criterion`; whole criterion `""`.
- No issue numbers in shipped artefacts (code, comments, errors, help, OpenAPI). Use `log/slog` only.
- `make test` and `go vet ./...` locally. **Do not run `make test-full` or `make race` locally**, because CI runs
  them. Do not add `-count=1`. Never pipe a long run through `tail`; write the full log to a file in the scratchpad.
- TDD: write a failing test first and watch it fail for the right reason. Commit after each green step. Tasks 1–9
  prove RED at the layer where each behaviour lives. Tasks 10–16 add the e2e, gRPC, parity and multi-node coverage
  that `.claude/rules/test-coverage.md` requires for those behaviours. Each of those tasks says how it shows its tests
  are not vacuous.

## Review Focus

These five inputs follow from the spec, but the matrix in §10 does not exercise them. Each one is pinned in the task
named here.

1. **A pre-check written as `null`, or a `function` member written as `null`.** A `null` pre-check means "no
   pre-check", so the callout is made. A `null` `function` member is refused at import. Task 1 covers the parser and
   Task 10 covers import.
2. **The same function name twice in one criterion**, each use with its own pre-check. The audit and the reason must
   tell the two uses apart by `path`. Task 8.
3. **Many callouts with long member reasons under one group.** The joined reason is capped at
   `maxCriterionReasonLen` and never splits a rune. Task 8.
4. **A pre-check operand beyond float64 precision** (`12345678901234567890`). The audit `preCheck` keeps every digit.
   Tasks 3 and 5.
5. **Exporting a workflow that has functions in groups and pre-checks.** The export must equal the import, and the
   e2e response validator, which enforces the OpenAPI schema, must accept it. Task 10.

## File structure

| File | Responsibility |
|---|---|
| SPI `predicate/condition.go`, `parse.go`, `marshal.go` (+ tests, `CHANGELOG.md`) | `FunctionCondition{Raw, PreCheck}` |
| `internal/contract/criterion_function.go` | `ParseCriterionFunction` → `(fn, present, err)`; `CriterionFunction.Criterion` |
| `internal/domain/search/operators.go` | `conditionEntries` counts a pre-check; export `ValidateConditionSize`, `ValidateGroupOperator` |
| `internal/domain/workflow/criterion_tree.go` (new) | split a criterion into local units, engine groups and function nodes |
| `internal/domain/workflow/criterion_eval.go` (new) | compile phase, run phase, outcome, audit and reason |
| `internal/domain/workflow/engine.go` | `evaluateCriterion` rewritten, four call sites use the outcome |
| `internal/domain/workflow/validate.go` | `validateCriterion`, `checkCriterionResponseTimeout` use the tree |
| `internal/contract/processing.go` | `DispatchCriteria` doc: the function condition's own JSON |
| `api/openapi.yaml`, `api/generated.go` | `CriterionGroupConditionDto`, pre-check typing, audit `data` description |
| tests: `internal/domain/workflow/criterion_*_test.go`, `internal/e2e/criterion_function_*_test.go`, `internal/grpc/criterion_function_rpc_test.go`, `e2e/parity/criterion_function.go`, `e2e/parity/externalapi/workflow_externalization.go`, `e2e/parity/multinode/criterion_function.go`, `internal/cluster/dispatch/handover_test.go` | coverage matrix §10 |
| docs: `cmd/cyoda/help/content/{workflows,search,grpc,crud}.md`, `docs/cloud-parity/*`, `docs/workflow-schema-versioning.md`, `CHANGELOG.md`, `COMPATIBILITY.md` | §11 |

## Streams

- **A (SPI):** Tasks 1–2. Everything else depends on A, through `go.work`.
- **B (core, sequential):** Tasks 3 → 4 → 5 → 6 → 7 → 8.
- **C (contract):** Task 9. It only needs A and is independent of B, so it can run beside B.
- **D (tests above unit, parallel after B):** Tasks 10, 11, 12, 13, 14, 15 and 16 touch disjoint files. Only Task 15
  edits `e2e/parity/registry.go`, and only Task 16 edits the multinode registry.
- **E (docs):** Task 17, after Task 8.
- **F (land):** Tasks 18–21, strictly in order.

A parallel stream in its own worktree needs the uncommitted `go.work` `use` line too. Add it in that worktree, with a
relative path that is correct from that worktree.

---

### Task 1: SPI — `FunctionCondition` keeps its JSON and its pre-check

**Files (in `/Users/paul/go-projects/cyoda-light/cyoda-go-spi`):**
- Modify: `predicate/condition.go` (`FunctionCondition`)
- Modify: `predicate/parse.go` (the `"function"` arm and a new `parseFunctionWithDepth`)
- Modify: `predicate/marshal.go` (`FunctionCondition.MarshalJSON`)
- Test: `predicate/parse_test.go`, `predicate/marshal_test.go`
- Modify: `CHANGELOG.md` (`[Unreleased]`)

**Interfaces:**
- Produces: `predicate.FunctionCondition{Raw json.RawMessage; PreCheck Condition}`. `Raw` is a copy of the input.
  `PreCheck` is nil when the pre-check is absent or `null`. The parser parses the pre-check at the function
  condition's depth + 1.

- [ ] **Step 1: Branch**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git fetch origin
git status --short          # must be empty
git switch -c feat-function-precheck origin/main
```

- [ ] **Step 2: Write the failing parser tests** (append to `predicate/parse_test.go`)

```go
func TestParseFunctionCondition_KeepsRawAndPreCheck(t *testing.T) {
	body := []byte(`{"type":"function","function":{"name":"credit-check","criterion":{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}}}`)
	cond, err := predicate.ParseCondition(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fc, ok := cond.(*predicate.FunctionCondition)
	if !ok {
		t.Fatalf("expected *FunctionCondition, got %T", cond)
	}
	if string(fc.Raw) != string(body) {
		t.Errorf("Raw = %s, want the input", fc.Raw)
	}
	body[2] = 'X'
	if fc.Raw[2] == 'X' {
		t.Error("Raw shares the caller's slice; it must be a copy")
	}
	sc, ok := fc.PreCheck.(*predicate.SimpleCondition)
	if !ok {
		t.Fatalf("PreCheck = %T, want *SimpleCondition", fc.PreCheck)
	}
	if sc.JsonPath != "$.amount" || sc.OperatorType != "GREATER_THAN" {
		t.Errorf("PreCheck = %+v", sc)
	}
}

func TestParseFunctionCondition_NoPreCheck(t *testing.T) {
	for name, body := range map[string]string{
		"criterion absent":  `{"type":"function","function":{"name":"f"}}`,
		"criterion null":    `{"type":"function","function":{"name":"f","criterion":null}}`,
		"function null":     `{"type":"function","function":null}`,
		"function absent":   `{"type":"function"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cond, err := predicate.ParseCondition([]byte(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			fc := cond.(*predicate.FunctionCondition)
			if fc.PreCheck != nil {
				t.Errorf("PreCheck = %#v, want nil", fc.PreCheck)
			}
			if string(fc.Raw) != body {
				t.Errorf("Raw = %s, want the input", fc.Raw)
			}
		})
	}
}

// The parser does not enforce what a pre-check may hold; cyoda-go refuses a
// function condition in a pre-check with a message that names its position.
func TestParseFunctionCondition_PreCheckWithFunctionIsParsed(t *testing.T) {
	body := `{"type":"function","function":{"name":"f","criterion":{"type":"group","operator":"AND","conditions":[{"type":"function","function":{"name":"g"}}]}}}`
	cond, err := predicate.ParseCondition([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g, ok := cond.(*predicate.FunctionCondition).PreCheck.(*predicate.GroupCondition)
	if !ok || len(g.Conditions) != 1 {
		t.Fatalf("PreCheck = %#v, want a group of one", cond.(*predicate.FunctionCondition).PreCheck)
	}
	if _, ok := g.Conditions[0].(*predicate.FunctionCondition); !ok {
		t.Errorf("member = %T, want *FunctionCondition", g.Conditions[0])
	}
}

func TestParseFunctionCondition_Refused(t *testing.T) {
	for name, body := range map[string]string{
		"pre-check of unknown type": `{"type":"function","function":{"name":"f","criterion":{"type":"bogus"}}}`,
		"function not an object":    `{"type":"function","function":"f"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := predicate.ParseCondition([]byte(body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseFunctionCondition_DepthCountsThroughPreCheck(t *testing.T) {
	leaf := `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1}`
	nest := func(n int) string {
		s := leaf
		for i := 0; i < n; i++ {
			s = `{"type":"group","operator":"AND","conditions":[` + s + `]}`
		}
		return `{"type":"function","function":{"name":"f","criterion":` + s + `}}`
	}
	// function at depth 0, pre-check root at 1: 49 groups put the leaf at 50.
	if _, err := predicate.ParseCondition([]byte(nest(49))); err != nil {
		t.Fatalf("leaf at the maximum depth: %v", err)
	}
	_, err := predicate.ParseCondition([]byte(nest(50)))
	if err == nil || !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("leaf one level below the maximum: err = %v, want the depth error", err)
	}
}
```

- [ ] **Step 3: Write the failing marshal test** (append to `predicate/marshal_test.go`; this file is in package
  `predicate`)

```go
func TestMarshal_FunctionConditionWritesRaw(t *testing.T) {
	body := `{"type":"function","function":{"name":"f","criterion":{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"A"}}}`
	cond, err := ParseCondition([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := json.Marshal(cond)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != body {
		t.Errorf("marshal = %s, want %s", out, body)
	}
}
```

- [ ] **Step 4: Run them and check that they fail**

Run: `go test ./predicate/ -run 'FunctionCondition' -v 2>&1 | grep -E '^(=== RUN|--- |FAIL|ok)'`
Expected: the new tests FAIL. `Raw` and `PreCheck` are undefined, so this is a compile failure.

- [ ] **Step 5: Implement**

`predicate/condition.go`: replace the placeholder type. Add `"encoding/json"` to the imports.

```go
// FunctionCondition is a condition evaluated by a callout to a compute member.
type FunctionCondition struct {
	// Raw is the function condition's JSON as the parser received it (a
	// copy). The engine dispatches each function condition with its own JSON.
	Raw json.RawMessage
	// PreCheck is the parsed `function.criterion` member: the condition the
	// engine evaluates before the callout. nil when the member is absent or
	// null. The parser does not restrict what it holds.
	PreCheck Condition
}
```

`predicate/parse.go`: change the arm to `case "function": return parseFunctionWithDepth(body, depth)` and add:

```go
// parseFunctionWithDepth keeps the function condition's JSON and parses its
// pre-check one level below it, so the depth limit covers the pre-check.
func parseFunctionWithDepth(body []byte, depth int) (*FunctionCondition, error) {
	var raw struct {
		Function *struct {
			Criterion json.RawMessage `json:"criterion"`
		} `json:"function"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse function condition: %w", err)
	}
	fc := &FunctionCondition{Raw: append(json.RawMessage(nil), body...)}
	if raw.Function == nil {
		return fc, nil
	}
	pc := bytes.TrimSpace(raw.Function.Criterion)
	if len(pc) == 0 || bytes.Equal(pc, []byte("null")) {
		return fc, nil
	}
	pre, err := parseConditionWithDepth(pc, depth+1)
	if err != nil {
		return nil, fmt.Errorf("failed to parse function pre-check: %w", err)
	}
	fc.PreCheck = pre
	return fc, nil
}
```

`predicate/marshal.go`: replace `FunctionCondition.MarshalJSON`:

```go
// MarshalJSON for FunctionCondition writes Raw, the JSON the parser received,
// when it is set, and only the discriminator otherwise.
func (c *FunctionCondition) MarshalJSON() ([]byte, error) {
	if len(c.Raw) > 0 {
		return append([]byte(nil), c.Raw...), nil
	}
	return json.Marshal(struct {
		Type string `json:"type"`
	}{
		Type: c.Type(),
	})
}
```

- [ ] **Step 6: Run the whole SPI module**

Run: `go test ./... 2>&1 | tee /private/tmp/claude-501/-Users-paul-go-projects-cyoda-light-cyoda-go/aa4d5a0d-c919-4575-82f5-0a2cf7df2518/scratchpad/spi-test.log | grep -E '^(FAIL|ok|---)'`
and `go vet ./...`.
Expected: all `ok`. The test files at `condition_filter_test.go:763,1553,1675` build `&predicate.FunctionCondition{}`,
which still compiles, and `ConditionToFilter` still refuses it.

- [ ] **Step 7: CHANGELOG** — under `## [Unreleased]`, add an `### Added` section if the file has none, and add this
  entry to it:

```markdown
- **`predicate.FunctionCondition` keeps its JSON and its pre-check.** `Raw`
  is the function condition's JSON as the parser received it (a copy), and
  `MarshalJSON` writes it. `PreCheck` is the parsed `function.criterion`
  member (nil when absent or null), parsed one level below the function
  condition so the parser's depth limit covers it. The parser does not
  restrict what a pre-check holds. Additive: code that builds
  `&FunctionCondition{}` still compiles, and `ConditionToFilter`,
  `ValidateConditionOperators` and `ValidateConditionPatterns` still treat a
  function condition as before and do not enter its pre-check. A function
  condition whose `function` member is not an object no longer parses.
```

- [ ] **Step 8: Commit**

```bash
git add predicate/ CHANGELOG.md
git commit -m "feat(predicate): FunctionCondition keeps its JSON and its pre-check

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: SPI PR, and build cyoda-go against it

- [ ] **Step 1: Push and open the PR** into `main` of `Cyoda/cyoda-go-spi`. Push with an inline credential, as
  described in the memory note "GitHub mechanics in the sandbox". Write the PR body to a file first, and check that
  the file is not empty before you pass it to `--body-file`. The body states the change and the test list, and ends
  with the attribution line. Do **not** merge yet; Task 18 merges.
- [ ] **Step 2:** In the cyoda-go worktree, add the SPI checkout to `go.work`, and do not commit the change:

```
use (
	.
	../../../../cyoda-go-spi
	./plugins/memory
	./plugins/postgres
	./plugins/sqlite
)
```

- [ ] **Step 3:** `go build ./... && go vet ./...` in the worktree. Expected: success. `api/generated.go` and every
  current use of `FunctionCondition` compile unchanged.
- [ ] **Step 4:** Update the #651 row-7 status cell to "SPI PR open (spi#NN), implementation in progress". Add an
  Adjustments line that links the SPI PR.

---

### Task 3: `ParseCriterionFunction` reports presence and carries the pre-check

**Files:**
- Modify: `internal/contract/criterion_function.go`
- Test: `internal/contract/criterion_function_test.go`
- Modify callers: `internal/grpc/callout.go:202`, `internal/domain/workflow/validate.go:147,362`. Task 6 replaces
  both `validate.go` call sites, so adapt them minimally here.

**Interfaces:**
- Produces: `func ParseCriterionFunction(condition json.RawMessage) (fn CriterionFunction, present bool, err error)`.
  `CriterionFunction.Criterion json.RawMessage` is the pre-check as written: empty when absent, `null` when sent as
  null.

- [ ] **Step 1: Failing test.** In `TestParseCriterionFunction`, add `wantPresent bool` to the case struct. Set it to
  `true` in the first three existing cases, and change the call to `got, present, err := ParseCriterionFunction(...)`.
  Assert `present == tc.wantPresent` for the cases that expect no error. Add these cases:

```go
		{name: "function member absent", in: `{"type":"function"}`, want: CriterionFunction{}, wantPresent: false},
		{name: "function member null", in: `{"type":"function","function":null}`, want: CriterionFunction{}, wantPresent: false},
		{name: "function member not an object", in: `{"type":"function","function":"f"}`, wantErr: true},
```

Add a separate test for the pre-check bytes:

```go
func TestParseCriterionFunction_CarriesThePreCheckAsWritten(t *testing.T) {
	in := `{"type":"function","function":{"name":"f","criterion":{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":12345678901234567890}}}`
	fn, present, err := ParseCriterionFunction(json.RawMessage(in))
	if err != nil || !present {
		t.Fatalf("ParseCriterionFunction: present=%t err=%v", present, err)
	}
	want := `{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":12345678901234567890}`
	if string(fn.Criterion) != want {
		t.Errorf("Criterion = %s, want %s", fn.Criterion, want)
	}
}
```

- [ ] **Step 2:** Run `go test ./internal/contract/ -run ParseCriterionFunction`. Expected: a compile FAIL, because the
  call returns two values and the test expects three.
- [ ] **Step 3: Implement**

```go
// CriterionFunction is the `function` member of a function condition.
type CriterionFunction struct {
	Name   string                  `json:"name"`
	Config CriterionFunctionConfig `json:"config"`
	// Criterion is the pre-check as written: the `function.criterion`
	// member. Empty when absent.
	Criterion json.RawMessage `json:"criterion"`
}

// ParseCriterionFunction decodes the `function` member of a function
// condition. present reports whether that member is there and not null. It
// does not check the condition's `type`: whether a condition is a function
// condition is the caller's knowledge (predicate.ParseCondition). Members it
// does not name are ignored — the criterion is client-owned JSON.
func ParseCriterionFunction(condition json.RawMessage) (fn CriterionFunction, present bool, err error) {
	var envelope struct {
		Function *CriterionFunction `json:"function"`
	}
	if err := json.Unmarshal(condition, &envelope); err != nil {
		return CriterionFunction{}, false, fmt.Errorf("failed to parse criterion function: %w", err)
	}
	if envelope.Function == nil {
		return CriterionFunction{}, false, nil
	}
	return *envelope.Function, true, nil
}
```

Callers: in `internal/grpc/callout.go`, use `fn, _, err := contract.ParseCriterionFunction(criterion)`. The callout
builder keeps its behaviour, because import refuses a function condition without the member (Task 6). In the two
`validate.go` call sites, use `fn, _, err :=` for now.

- [ ] **Step 4:** `go test ./internal/contract/ ./internal/grpc/ ./internal/domain/workflow/` → PASS.
- [ ] **Step 5: Commit** `refactor(contract): ParseCriterionFunction reports presence and carries the pre-check`.

---

### Task 4: search — entries count a pre-check; export the size and group-operator checks

**Files:**
- Modify: `internal/domain/search/operators.go`
- Test: `internal/domain/search/operators_test.go` (or the existing test file for `conditionEntries`. Find it with
  `grep -rln conditionEntries internal/domain/search/*_test.go`.)

**Interfaces:**
- Produces: `search.ValidateConditionSize(cond predicate.Condition) error`, which is the renamed
  `validateConditionSize`. Produces `search.ValidateGroupOperator(operator string, n int) error`, which is the renamed
  `validateGroupOperator`. Both are behaviour-identical. `conditionEntries` counts a function condition as
  `1 + conditionEntries(PreCheck)`.

- [ ] **Step 1: Failing test**

```go
func TestConditionEntries_CountsThePreCheck(t *testing.T) {
	cond, err := predicate.ParseCondition([]byte(`{"type":"group","operator":"AND","conditions":[
		{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":1},
		{"type":"function","function":{"name":"f","criterion":{"type":"array","jsonPath":"$.t[*]","operatorType":"EQUALS","values":[1,null,3]}}}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := conditionEntries(cond); got != 1+1+3 {
		t.Errorf("conditionEntries = %d, want 5 (simple 1, function 1, its array pre-check 3)", got)
	}
}

func TestValidateConditionSize_RefusesAnOversizedPreCheck(t *testing.T) {
	values := make([]any, MaxConditionEntries)
	pre, _ := json.Marshal(map[string]any{"type": "array", "jsonPath": "$.t[*]", "operatorType": "EQUALS", "values": values})
	cond, err := predicate.ParseCondition([]byte(`{"type":"function","function":{"name":"f","criterion":` + string(pre) + `}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = ValidateConditionSize(cond)
	if !errors.Is(err, ErrInvalidCondition) {
		t.Fatalf("err = %v, want ErrInvalidCondition (10,001 entries)", err)
	}
}
```

- [ ] **Step 2:** `go test ./internal/domain/search/ -run 'ConditionEntries_CountsThePreCheck|ValidateConditionSize_'` →
  FAIL. The first test gets 2. The second does not compile.
- [ ] **Step 3: Implement.** In `conditionEntries`, add an arm before `default`:

```go
	case *predicate.FunctionCondition:
		// The function condition is one entry; its pre-check is evaluated
		// with it, so its entries count toward the same limit.
		return 1 + conditionEntries(c.PreCheck)
```

Then update the `MaxConditionEntries` doc. Change "a simple, lifecycle or function clause counts one" to "a simple
or lifecycle clause counts one; a function clause counts one plus the entries of its pre-check". Rename
`validateConditionSize` → `ValidateConditionSize` and `validateGroupOperator` → `ValidateGroupOperator`. Update every
caller and every doc comment that names them; `grep -rn "validateConditionSize\|validateGroupOperator" internal/`
must come back empty. Each exported doc comment states its caller outside the package:
- `ValidateConditionSize`: "workflow import counts the whole criterion, pre-checks included, with it";
- `ValidateGroupOperator`: "the workflow engine checks a group that holds a function condition with it".

- [ ] **Step 4:** `go test ./internal/domain/search/...` → PASS. The search entry points still refuse a function
  condition. A function with a huge pre-check now fails on size first, and both errors are `INVALID_CONDITION`.
- [ ] **Step 5: Commit** `feat(search): a function condition's pre-check counts toward the entry limit`.

---

### Task 5: the criterion tree

**Files:**
- Create: `internal/domain/workflow/criterion_tree.go`
- Test: `internal/domain/workflow/criterion_tree_test.go`

**Interfaces:**
- Consumes: `predicate.FunctionCondition{Raw, PreCheck}` (Task 1), `contract.ParseCriterionFunction` (Task 3),
  `search.ValidateGroupOperator` (Task 4), `match.Prepared`.
- Produces:

```go
type critNode interface {
	collect(units *[]*critLocal, fns *[]*critFunction)
	// Task 7 adds: eval(e *Engine, cc *criterionContext, entity *spi.Entity, rec *[]functionRecord) (bool, error)
}
type critLocal struct{ ptr string; cond predicate.Condition; prepared match.Prepared }
type critGroup struct{ ptr, op string; members []critNode }
type critFunction struct{ ptr string; raw json.RawMessage; fn contract.CriterionFunction; preCheck *critLocal; preCheckValue any }
func buildCriterionTree(cond predicate.Condition, ptr string) (critNode, error)
func collectCriterion(root critNode) (units []*critLocal, fns []*critFunction)
func atPointer(ptr string) string   // "" → "", else " at " + ptr
```

Task 7 adds `eval` to the interface and implements it on the three node types, so this task needs no stub.

- [ ] **Step 1: Failing tests** (`criterion_tree_test.go`)

```go
func mustParse(t *testing.T, s string) predicate.Condition {
	t.Helper()
	c, err := predicate.ParseCondition([]byte(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return c
}

const (
	treeLocal = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	treeFnA   = `{"type":"function","function":{"name":"a"}}`
	treeFnB   = `{"type":"function","function":{"name":"b","criterion":{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"S"}}}`
)

func TestBuildCriterionTree_FunctionFreeIsOneLocalUnit(t *testing.T) {
	root, err := buildCriterionTree(mustParse(t, `{"type":"group","operator":"AND","conditions":[`+treeLocal+`]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	units, fns := collectCriterion(root)
	if l, ok := root.(*critLocal); !ok || l.ptr != "" || len(units) != 1 || len(fns) != 0 {
		t.Fatalf("root = %#v units=%d fns=%d, want one local unit at \"\"", root, len(units), len(fns))
	}
}

func TestBuildCriterionTree_SplitsAtFunctions(t *testing.T) {
	c := `{"type":"group","operator":"OR","conditions":[` + treeLocal + `,{"type":"group","operator":"NOT","conditions":[` + treeFnA + `]},` + treeFnB + `]}`
	root, err := buildCriterionTree(mustParse(t, c), "")
	if err != nil {
		t.Fatal(err)
	}
	g, ok := root.(*critGroup)
	if !ok || g.op != "OR" || len(g.members) != 3 {
		t.Fatalf("root = %#v, want an OR engine group of 3", root)
	}
	units, fns := collectCriterion(root)
	var gotUnits, gotFns []string
	for _, u := range units {
		gotUnits = append(gotUnits, u.ptr)
	}
	for _, f := range fns {
		gotFns = append(gotFns, f.fn.Name+"@"+f.ptr)
	}
	if want := []string{"/conditions/0", "/conditions/2/function/criterion"}; !slices.Equal(gotUnits, want) {
		t.Errorf("units = %v, want %v", gotUnits, want)
	}
	if want := []string{"a@/conditions/1/conditions/0", "b@/conditions/2"}; !slices.Equal(gotFns, want) {
		t.Errorf("functions = %v, want %v", gotFns, want)
	}
	if fns[1].preCheckValue == nil || fns[0].preCheck != nil {
		t.Errorf("pre-check: a=%v b=%v", fns[0].preCheck, fns[1].preCheckValue)
	}
	if string(fns[0].raw) != treeFnA {
		t.Errorf("raw = %s, want the function condition's own JSON %s", fns[0].raw, treeFnA)
	}
}

func TestBuildCriterionTree_Refusals(t *testing.T) {
	cases := []struct{ name, criterion, want string }{
		{"no function member", `{"type":"function"}`, "has no `function` object"},
		{"function member null", `{"type":"function","function":null}`, "has no `function` object"},
		{"no name", `{"type":"group","operator":"AND","conditions":[{"type":"function","function":{"config":{}}}]}`, "function condition at /conditions/0 has no function name"},
		{"bad config", `{"type":"function","function":{"name":"f","config":{"responseTimeoutMs":"5s"}}}`, "failed to parse criterion function"},
		{"function in pre-check", `{"type":"function","function":{"name":"f","criterion":` + treeFnA + `}}`, `function condition "f": its pre-check holds a function condition at /function/criterion`},
		{"function nested in pre-check", `{"type":"group","operator":"AND","conditions":[` + treeLocal + `,{"type":"function","function":{"name":"f","criterion":{"type":"group","operator":"OR","conditions":[` + treeLocal + `,` + treeFnA + `]}}}]}`, "at /conditions/1/function/criterion/conditions/1"},
		{"unknown operator on an engine group", `{"type":"group","operator":"XOR","conditions":[` + treeFnA + `]}`, `unknown group operator "XOR"`},
		{"NOT of two around a function", `{"type":"group","operator":"NOT","conditions":[` + treeFnA + `,` + treeLocal + `]}`, "NOT requires exactly one condition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildCriterionTree(mustParse(t, tc.criterion), "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBuildCriterionTree_PreCheckKeepsBigNumbers(t *testing.T) {
	root, err := buildCriterionTree(mustParse(t, `{"type":"function","function":{"name":"f","criterion":{"type":"simple","jsonPath":"$.a","operatorType":"EQUALS","value":12345678901234567890}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(root.(*critFunction).preCheckValue)
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Errorf("preCheckValue = %s, want every digit kept", out)
	}
}
```

- [ ] **Step 2:** `go test ./internal/domain/workflow/ -run BuildCriterionTree` → FAIL with a compile error, because
  `buildCriterionTree` is undefined.
- [ ] **Step 3: Implement** `criterion_tree.go`:

```go
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// A criterion that holds a function condition is evaluated as a tree of three
// kinds of node: a local unit (a function-free subtree, or a pre-check), which
// the in-memory matcher evaluates; an engine group (a group that holds a
// function condition), whose members the engine evaluates in declaration order
// and stops as soon as the group's value is known; and a function condition,
// which makes a callout unless its pre-check does not match. A criterion with
// no function condition is one local unit. Import and the engine build the
// same tree, so they accept and refuse the same criteria.
type critNode interface {
	// collect appends the node's local units and function conditions, in
	// declaration order.
	collect(units *[]*critLocal, fns *[]*critFunction)
}

// critLocal is a function-free condition: a maximal function-free subtree of
// the criterion, or a pre-check.
type critLocal struct {
	ptr      string // JSON Pointer of the condition in the criterion
	cond     predicate.Condition
	prepared match.Prepared // set by Engine.prepareLocalUnits
}

// critGroup is a group that holds at least one function condition.
type critGroup struct {
	ptr     string
	op      string // AND, OR or NOT; NOT has exactly one member
	members []critNode
}

// critFunction is one function condition.
type critFunction struct {
	ptr           string
	raw           json.RawMessage // the function condition's own JSON, dispatched as is
	fn            contract.CriterionFunction
	preCheck      *critLocal // nil when the function condition has none
	preCheckValue any        // the pre-check as written, decoded for the audit
}

func (l *critLocal) collect(units *[]*critLocal, _ *[]*critFunction) { *units = append(*units, l) }

func (g *critGroup) collect(units *[]*critLocal, fns *[]*critFunction) {
	for _, m := range g.members {
		m.collect(units, fns)
	}
}

func (f *critFunction) collect(units *[]*critLocal, fns *[]*critFunction) {
	if f.preCheck != nil {
		*units = append(*units, f.preCheck)
	}
	*fns = append(*fns, f)
}

// collectCriterion returns root's local units and function conditions in
// declaration order.
func collectCriterion(root critNode) (units []*critLocal, fns []*critFunction) {
	root.collect(&units, &fns)
	return units, fns
}

// buildCriterionTree splits a parsed criterion at its function conditions.
// It refuses a function condition without a `function` object or a name,
// a function condition in a pre-check, and a group around a function
// condition whose operator is not AND, OR or NOT-of-one. ptr is cond's JSON
// Pointer in the criterion ("" for the criterion itself).
func buildCriterionTree(cond predicate.Condition, ptr string) (critNode, error) {
	if _, ok := firstFunction(cond, ptr); !ok {
		return &critLocal{ptr: ptr, cond: cond}, nil
	}
	if g, ok := cond.(*predicate.GroupCondition); ok {
		if err := search.ValidateGroupOperator(g.Operator, len(g.Conditions)); err != nil {
			return nil, fmt.Errorf("group%s: %w", atPointer(ptr), err)
		}
		node := &critGroup{ptr: ptr, op: g.Operator, members: make([]critNode, 0, len(g.Conditions))}
		for i, child := range g.Conditions {
			m, err := buildCriterionTree(child, fmt.Sprintf("%s/conditions/%d", ptr, i))
			if err != nil {
				return nil, err
			}
			node.members = append(node.members, m)
		}
		return node, nil
	}
	// firstFunction finds a function condition only in a group or as cond itself.
	return buildFunctionNode(cond.(*predicate.FunctionCondition), ptr)
}

func buildFunctionNode(c *predicate.FunctionCondition, ptr string) (*critFunction, error) {
	if len(c.Raw) == 0 {
		return nil, fmt.Errorf("function condition%s carries no JSON", atPointer(ptr))
	}
	fn, present, err := contract.ParseCriterionFunction(c.Raw)
	if err != nil {
		return nil, fmt.Errorf("function condition%s: %w", atPointer(ptr), err)
	}
	if !present {
		return nil, fmt.Errorf("function condition%s has no `function` object", atPointer(ptr))
	}
	if fn.Name == "" {
		return nil, fmt.Errorf("function condition%s has no function name", atPointer(ptr))
	}
	node := &critFunction{ptr: ptr, raw: c.Raw, fn: fn}
	if c.PreCheck == nil {
		return node, nil
	}
	pcPtr := ptr + "/function/criterion"
	if at, ok := firstFunction(c.PreCheck, pcPtr); ok {
		return nil, fmt.Errorf("function condition %q%s: its pre-check holds a function condition at %s; a pre-check holds only local conditions",
			fn.Name, atPointer(ptr), at)
	}
	node.preCheck = &critLocal{ptr: pcPtr, cond: c.PreCheck}
	if node.preCheckValue, err = decodePreCheck(fn.Criterion); err != nil {
		return nil, fmt.Errorf("function condition %q%s: %w", fn.Name, atPointer(ptr), err)
	}
	return node, nil
}

// firstFunction returns the JSON Pointer of the first function condition in
// cond, in declaration order, without entering a function condition's
// pre-check.
func firstFunction(cond predicate.Condition, ptr string) (string, bool) {
	switch c := cond.(type) {
	case *predicate.FunctionCondition:
		return ptr, true
	case *predicate.GroupCondition:
		for i, child := range c.Conditions {
			if at, ok := firstFunction(child, fmt.Sprintf("%s/conditions/%d", ptr, i)); ok {
				return at, true
			}
		}
	}
	return "", false
}

// decodePreCheck decodes the pre-check as written into plain JSON values for
// the audit, keeping numbers exact.
func decodePreCheck(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("failed to decode the pre-check: %w", err)
	}
	return v, nil
}

// atPointer renders a position for a message: nothing for the criterion
// itself, " at <pointer>" otherwise.
func atPointer(ptr string) string {
	if ptr == "" {
		return ""
	}
	return " at " + ptr
}
```

Note on the "function in pre-check" case: for a whole-criterion function the message reads
`function condition "f": its pre-check holds a function condition at /function/criterion; ...`. That is the
expectation the test pins.

- [ ] **Step 4:** `go test ./internal/domain/workflow/ -run BuildCriterionTree` → PASS. `go vet ./internal/domain/workflow/`.
- [ ] **Step 5: Commit** `feat(workflow): split a criterion at its function conditions`.

---

### Task 6: import — I1–I8 at every depth

**Files:**
- Modify: `internal/domain/workflow/validate.go` (`validateCriterion`, `checkCriterionResponseTimeout`,
  `validateCalloutLimits` doc, `walkCriterion` doc)
- Test: `internal/domain/workflow/criterion_function_import_test.go` (new)

**Interfaces:**
- Consumes: `buildCriterionTree`, `collectCriterion`, `atPointer` (Task 5); `search.ValidateConditionSize` (Task 4).
- Produces: no new API. `validateImportRequest` and `validateCalloutLimits` enforce §6.

- [ ] **Step 1: Failing tests**

```go
// fnImportWF wraps criterion as the transition criterion of a minimal workflow.
func fnImportWF(criterion string) []spi.WorkflowDefinition {
	return []spi.WorkflowDefinition{wfWithTransitionCriterion(json.RawMessage(criterion))}
}

const (
	impLocal    = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	impFn       = `{"type":"function","function":{"name":"f"}}`
	impLifePre  = `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"S1"}`
	impArrayPre = `{"type":"array","jsonPath":"$.tags[*]","operatorType":"EQUALS","values":["a"]}`
)

func impFnWith(name, preCheck string) string {
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func impGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

func TestImport_FunctionConditionsAccepted(t *testing.T) {
	for name, c := range map[string]string{
		"pre-check simple":     impFnWith("f", impLocal),
		"pre-check lifecycle":  impFnWith("f", impLifePre),
		"pre-check array":      impFnWith("f", impArrayPre),
		"pre-check group":      impFnWith("f", impGroup("AND", impLocal, impLifePre)),
		"pre-check null":       impFnWith("f", "null"),
		"function in AND":      impGroup("AND", impLocal, impFn),
		"function in OR":       impGroup("OR", impFn, impLocal),
		"function in NOT":      impGroup("NOT", impFn),
		"function nested":      impGroup("AND", impGroup("OR", impLocal, impGroup("NOT", impFnWith("g", impLocal)))),
		"same name twice":      impGroup("OR", impFnWith("f", impLocal), impFnWith("f", impLifePre)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateImportRequest(fnImportWF(c)); err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
	}
}

func TestImport_FunctionConditionsRefused(t *testing.T) {
	cases := []struct{ name, criterion, want string }{
		{"function in pre-check", impFnWith("f", impFn), "its pre-check holds a function condition at /function/criterion"},
		{"function deep in pre-check", impGroup("AND", impLocal, impFnWith("f", impGroup("OR", impLocal, impGroup("NOT", impFn)))),
			"at /conditions/1/function/criterion/conditions/1/conditions/0"},
		{"pre-check not a condition", impFnWith("f", `{"type":"bogus"}`), "criterion is not a valid condition"},
		{"pre-check bad path", impGroup("AND", impFnWith("f", `{"type":"simple","jsonPath":"$.a[0]b","operatorType":"EQUALS","value":1}`)),
			"condition at /conditions/0/function/criterion"},
		{"pre-check bad operator", impFnWith("f", `{"type":"simple","jsonPath":"$.a","operatorType":"NOPE","value":1}`),
			"condition at /function/criterion"},
		{"pre-check bad pattern", impFnWith("f", `{"type":"simple","jsonPath":"$.a","operatorType":"MATCHES_PATTERN","value":")|("}`),
			"condition at /function/criterion"},
		{"local member bad operator beside a function", impGroup("OR", impFn, `{"type":"simple","jsonPath":"$.a","operatorType":"NOPE","value":1}`),
			"condition at /conditions/1"},
		{"no function member", impGroup("AND", impLocal, `{"type":"function"}`), "function condition at /conditions/1 has no `function` object"},
		{"no name", impGroup("AND", `{"type":"function","function":{}}`), "function condition at /conditions/0 has no function name"},
		{"bad retryPolicy at depth", impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":{"retryPolicy":"SOMETIMES"}}}`),
			`criterion function "g" at /conditions/1: unknown retryPolicy "SOMETIMES"`},
		{"engine group with unknown operator", impGroup("XOR", impFn), `unknown group operator "XOR"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImportRequest(fnImportWF(tc.criterion))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestImport_LimitsCountThroughPreChecks(t *testing.T) {
	// MaxConditionEntries entries in the pre-check, plus the function
	// condition itself, is one over the limit.
	values := make([]any, search.MaxConditionEntries)
	pre, _ := json.Marshal(map[string]any{"type": "array", "jsonPath": "$.t[*]", "operatorType": "EQUALS", "values": values})
	if err := validateImportRequest(fnImportWF(impFnWith("f", string(pre)))); err == nil || !strings.Contains(err.Error(), "the limit is 10000") {
		t.Fatalf("entries: err = %v, want the entry-limit refusal", err)
	}
	deep := impLocal
	for i := 0; i < 50; i++ {
		deep = impGroup("AND", deep)
	}
	if err := validateImportRequest(fnImportWF(impFnWith("f", deep))); err == nil || !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("depth: err = %v, want the parser's depth refusal", err)
	}
}

func TestCalloutLimits_ResponseTimeoutAtDepth(t *testing.T) {
	c := impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":{"responseTimeoutMs":60001}}}`)
	err := validateCalloutLimits(fnImportWF(c), testMaxResponseTimeout)
	if err == nil || !strings.Contains(err.Error(), `criterion function "g" at /conditions/1: responseTimeoutMs 60001 exceeds`) {
		t.Fatalf("err = %v, want the bound refused with the function's position", err)
	}
	if err := validateCalloutLimits(fnImportWF(impGroup("OR", impLocal, `{"type":"function","function":{"name":"g","config":{"responseTimeoutMs":-1}}}`)), testMaxResponseTimeout); err == nil {
		t.Fatal("a negative responseTimeoutMs at depth must be refused")
	}
}
```

The size message prints the limit with `%d`, so it reads "the limit is 10000" (`operators.go:216`). `testMaxResponseTimeout` already exists in
`validate_limits_test.go`, with the 60000 ms bound.

- [ ] **Step 2:** `go test ./internal/domain/workflow/ -run 'TestImport_|TestCalloutLimits_ResponseTimeoutAtDepth'`
  → FAIL. The cases at depth import cleanly today, and the pre-check cases are accepted unchecked.
- [ ] **Step 3: Implement.** Replace `validateCriterion`'s body after the parse:

```go
	cond, err := predicate.ParseCondition(trimmed)
	if err != nil {
		return fmt.Errorf("%s: criterion is not a valid condition: %w", location, err)
	}
	// The entry limit counts the whole criterion, every pre-check included.
	if err := search.ValidateConditionSize(cond); err != nil {
		return fmt.Errorf("%s: %w", location, err)
	}
	// Function conditions at any depth: each has a `function` object and a
	// name, its config parses, and its pre-check holds only local
	// conditions; a group around a function condition has a valid operator.
	root, err := buildCriterionTree(cond, "")
	if err != nil {
		return fmt.Errorf("%s: %w", location, err)
	}
	units, fns := collectCriterion(root)
	for _, f := range fns {
		if err := checkRetryPolicy(f.fn.Config.RetryPolicy,
			fmt.Sprintf("%s criterion function %q%s", location, f.fn.Name, atPointer(f.ptr))); err != nil {
			return err
		}
	}
	// The local checks run once per local unit — each function-free subtree
	// and each pre-check — naming the unit's position. Paths and lifecycle
	// type-soundness first, then operator/operand shape, then pattern
	// operands: a criterion naming a field that does not exist is reported
	// as the path problem it is, not shadowed by a complaint about its
	// operator or operand.
	for _, u := range units {
		loc := location
		if u.ptr != "" {
			loc += " condition at " + u.ptr
		}
		if err := walkCriterion(u.cond, loc); err != nil {
			return err
		}
		if err := search.ValidateCriterionCondition(u.cond); err != nil {
			return fmt.Errorf("%s: %w", loc, err)
		}
		if err := search.ValidatePatterns(u.cond); err != nil {
			return fmt.Errorf("%s: %w", loc, err)
		}
	}
	return nil
```

Update the doc comments:
- `validateCriterion`: state that function conditions are accepted at any depth and that pre-checks are checked as
  local units.
- `walkCriterion`: replace "FunctionCondition carries neither a path nor an operator and is silently skipped — that
  is how a FUNCTION criterion is exempted from these checks" with "walkCriterion runs on local units only (see
  buildCriterionTree); it never meets a function condition".
- `validateImportRequest` M1: "on every processor, every function condition in a criterion and schedule function".

Then delete walkCriterion's function-skipping wording and check that the switch has no `FunctionCondition` arm: none
exists today, because the default falls through, so nothing more is needed.

Replace `checkCriterionResponseTimeout`'s body after the null check:

```go
	cond, err := predicate.ParseCondition(trimmed)
	if err != nil {
		return nil // validateCriterion has already refused a criterion that does not parse
	}
	root, err := buildCriterionTree(cond, "")
	if err != nil {
		// Not this rule's to report: validateCriterion builds the same tree,
		// with the same location strings, and the import handler runs it
		// first.
		return nil
	}
	_, fns := collectCriterion(root)
	for _, f := range fns {
		if err := checkResponseTimeout(f.fn.Config.ResponseTimeoutMs, maxMs,
			fmt.Sprintf("%s criterion function %q%s", location, f.fn.Name, atPointer(f.ptr))); err != nil {
			return err
		}
	}
	return nil
```

Change its doc to "applies checkResponseTimeout to every function condition in a criterion, at any depth". In the
`validateCalloutLimits` doc, change "each top-level function criterion" to "each function condition in a criterion".
Remove the now-unused `contract` import from `validate.go` if nothing else uses it. Check with `go build`.

- [ ] **Step 4:** `go test ./internal/domain/workflow/...` → PASS. Every existing import test still passes:
  function-free criteria keep their messages, because a function-free criterion is one unit at `""`. Then find every
  fixture and test payload that the two new refusals could hit, a function condition without a `function` object or
  without a `name`:
  `grep -rn '"type": *"function"' --include='*.json' --include='*_test.go' . | grep -v '"name"'`.
  Fix each hit by giving it a name. A hit that is meant to be refused gets an assertion instead.
- [ ] **Step 5: Commit** `feat(workflow): import checks function conditions and pre-checks at every depth`.

---

### Task 7: engine — compile phase and run phase

**Files:**
- Create: `internal/domain/workflow/criterion_eval.go`
- Modify: `internal/domain/workflow/engine.go` (`evaluateCriterion`, `criterionContext` stays; its four callers use
  `out.matched` for now and Task 8 adds the audit)
- Modify: `internal/contract/processing.go` (`DispatchCriteria` doc)
- Modify: the 20 test call sites (see Step 5)
- Test: `internal/domain/workflow/criterion_function_eval_test.go` (new)

**Interfaces:**
- Consumes: Task 5's tree.
- Produces:

```go
type criterionOutcome struct {
	matched   bool
	reason    string           // member's reason for a called whole-criterion function; "" otherwise
	functions []functionRecord // nil when the criterion holds no function condition
	whole     bool             // the criterion is one function condition
}
type functionRecord struct {
	path, name                                          string
	hasPreCheck                                         bool
	preCheck                                            any
	preCheckMatched, called, matches                    bool
	reason                                              string
}
func (e *Engine) evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (criterionOutcome, error)
func (e *Engine) prepareLocalUnits(ctx context.Context, entity *spi.Entity, units []*critLocal) error
func (e *Engine) dispatchCriterionFunction(cc *criterionContext, entity *spi.Entity, raw json.RawMessage) (bool, string, error)
```

- [ ] **Step 1: Failing tests** (`criterion_function_eval_test.go`)

```go
// recordingCriteria answers each criterion function by name and records the
// JSON each callout was dispatched with.
type recordingCriteria struct {
	mu      sync.Mutex
	answers map[string]criteriaAnswer
	calls   []json.RawMessage
}

type criteriaAnswer struct {
	matches bool
	reason  string
	err     error
}

func (r *recordingCriteria) DispatchProcessor(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
	return nil, errors.New("no processor in these tests")
}

func (r *recordingCriteria) DispatchCriteria(_ context.Context, _ *spi.Entity, criterion json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	fn, _, err := contract.ParseCriterionFunction(criterion)
	if err != nil {
		return false, "", err
	}
	r.mu.Lock()
	r.calls = append(r.calls, append(json.RawMessage(nil), criterion...))
	r.mu.Unlock()
	a, ok := r.answers[fn.Name]
	if !ok {
		return false, "", fmt.Errorf("no answer scripted for %q", fn.Name)
	}
	return a.matches, a.reason, a.err
}

func (r *recordingCriteria) DispatchFunction(context.Context, *spi.Entity, spi.ScheduleFunction, string, string, string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, errors.New("no function in these tests")
}

func (r *recordingCriteria) calledNames(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		fn, _, err := contract.ParseCriterionFunction(c)
		if err != nil {
			t.Fatalf("recorded callout JSON does not parse: %v", err)
		}
		out = append(out, fn.Name)
	}
	return out
}

func evalEnv(t *testing.T, answers map[string]criteriaAnswer) (*Engine, *recordingCriteria, *spi.Entity, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	rec := &recordingCriteria{answers: answers}
	engine := NewEngine(factory, uuids, factory.NewTransactionManager(uuids), WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "fn-eval", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})
	entity := makeEntity("e1", ref, map[string]any{"amount": 50})
	entity.Meta.State = "S"
	return engine, rec, entity, ctx
}

const (
	evAmountGT100 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	evAmountGT10  = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10}`
)

func evFn(name, preCheck string) string {
	if preCheck == "" {
		return `{"type":"function","function":{"name":"` + name + `"}}`
	}
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func evGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

func TestEvaluateCriterion_FunctionConditions(t *testing.T) {
	yes, no := criteriaAnswer{matches: true}, criteriaAnswer{matches: false}
	cases := []struct {
		name      string
		criterion string
		answers   map[string]criteriaAnswer
		want      bool
		wantCalls []string
	}{
		{"pre-check false: no callout", evFn("a", evAmountGT100), map[string]criteriaAnswer{"a": yes}, false, nil},
		{"pre-check true: callout decides", evFn("a", evAmountGT10), map[string]criteriaAnswer{"a": no}, false, []string{"a"}},
		{"no pre-check: callout decides", evFn("a", ""), map[string]criteriaAnswer{"a": yes}, true, []string{"a"}},
		{"AND stops at the first false", evGroup("AND", evFn("a", ""), evFn("b", "")), map[string]criteriaAnswer{"a": no, "b": yes}, false, []string{"a"}},
		{"AND calls in order", evGroup("AND", evFn("a", ""), evFn("b", "")), map[string]criteriaAnswer{"a": yes, "b": no}, false, []string{"a", "b"}},
		{"AND local false first: no callout", evGroup("AND", evAmountGT100, evFn("a", "")), map[string]criteriaAnswer{"a": yes}, false, nil},
		{"OR stops at the first true", evGroup("OR", evFn("a", ""), evFn("b", "")), map[string]criteriaAnswer{"a": yes, "b": no}, true, []string{"a"}},
		{"OR local true first: no callout", evGroup("OR", evAmountGT10, evFn("a", "")), map[string]criteriaAnswer{"a": no}, true, nil},
		{"NOT inverts the answer", evGroup("NOT", evFn("a", "")), map[string]criteriaAnswer{"a": yes}, false, []string{"a"}},
		{"NOT of a skipped function is true", evGroup("NOT", evFn("a", evAmountGT100)), map[string]criteriaAnswer{"a": yes}, true, nil},
		{"nested", evGroup("OR", evGroup("AND", evFn("a", ""), evFn("b", "")), evGroup("NOT", evFn("c", ""))),
			map[string]criteriaAnswer{"a": yes, "b": no, "c": no}, true, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, rec, entity, ctx := evalEnv(t, tc.answers)
			out, err := engine.evaluateCriterion([]byte(tc.criterion), entity, &criterionContext{ctx: ctx, target: "TRANSITION"})
			if err != nil {
				t.Fatalf("evaluateCriterion: %v", err)
			}
			if out.matched != tc.want {
				t.Errorf("matched = %t, want %t", out.matched, tc.want)
			}
			if got := rec.calledNames(t); !slices.Equal(got, tc.wantCalls) {
				t.Errorf("callouts = %v, want %v", got, tc.wantCalls)
			}
		})
	}
}

func TestEvaluateCriterion_DispatchesTheFunctionConditionsOwnJSON(t *testing.T) {
	fnB := evFn("b", evAmountGT10)
	engine, rec, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"b": {matches: true}})
	if _, err := engine.evaluateCriterion([]byte(evGroup("AND", evAmountGT10, fnB)), entity, &criterionContext{ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || string(rec.calls[0]) != fnB {
		t.Fatalf("dispatched %s, want the function condition's own JSON %s", rec.calls, fnB)
	}
}

func TestEvaluateCriterion_CompilePhaseFailsBeforeAnyCallout(t *testing.T) {
	undeclared := `{"type":"simple","jsonPath":"$.amonut","operatorType":"GREATER_THAN","value":1}`
	for name, c := range map[string]string{
		"undeclared path in a member the stop rule skips": evGroup("OR", evFn("a", ""), undeclared),
		"undeclared path in a pre-check":                  evGroup("AND", evFn("a", ""), evFn("b", undeclared)),
		"engine group with two NOT members":               evGroup("AND", evFn("a", ""), evGroup("NOT", evFn("b", ""), evAmountGT10)),
		"function without a name":                         evGroup("AND", evFn("a", ""), `{"type":"function","function":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			engine, rec, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"a": {matches: true}, "b": {matches: true}})
			if _, err := engine.evaluateCriterion([]byte(c), entity, &criterionContext{ctx: ctx}); err == nil {
				t.Fatal("want an error")
			}
			if n := len(rec.calledNames(t)); n != 0 {
				t.Errorf("%d callouts made before the fault was found; want 0", n)
			}
		})
	}
}

func TestEvaluateCriterion_CalloutFailureFailsTheEvaluation(t *testing.T) {
	boom := errors.New("member down")
	engine, rec, entity, ctx := evalEnv(t, map[string]criteriaAnswer{"a": {matches: true}, "b": {err: boom}, "c": {matches: true}})
	_, err := engine.evaluateCriterion([]byte(evGroup("AND", evFn("a", ""), evFn("b", ""), evFn("c", ""))), entity, &criterionContext{ctx: ctx})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the callout's error", err)
	}
	if got := rec.calledNames(t); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("callouts = %v, want [a b]: nothing after the failure", got)
	}
}

func TestEvaluateCriterion_NoDispatcherFailsAnywhere(t *testing.T) {
	engine, factory := setupEngine(t)
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "fn-nodisp", ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})
	entity := makeEntity("e1", ref, map[string]any{"amount": 50})
	_, err := engine.evaluateCriterion([]byte(evGroup("OR", evAmountGT10, evFn("a", ""))), entity, &criterionContext{ctx: ctx})
	if err == nil || !strings.Contains(err.Error(), "no external processing service configured") {
		t.Fatalf("err = %v, want the no-dispatcher refusal even though the stop rule would skip the function", err)
	}
}

func TestEvaluateCriterion_ModelStoreFailureBeforeAnyCallout(t *testing.T) {
	base := memory.NewStoreFactory()
	t.Cleanup(func() { base.Close() })
	uuids := common.NewTestUUIDGenerator()
	rec := &recordingCriteria{answers: map[string]criteriaAnswer{"a": {matches: true}}}
	engine := NewEngine(&errModelStoreFactory{StoreFactory: base, err: errors.New("model store down")}, uuids,
		base.NewTransactionManager(uuids), WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	entity := makeEntity("e1", spi.ModelRef{EntityName: "fn-infra", ModelVersion: "1.0"}, map[string]any{"amount": 50})
	_, err := engine.evaluateCriterion([]byte(evGroup("AND", evFn("a", ""), evFn("b", evAmountGT10))), entity, &criterionContext{ctx: ctx})
	if !errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("err = %v, want ErrCriterionTypingInfra", err)
	}
	if n := len(rec.calledNames(t)); n != 0 {
		t.Errorf("%d callouts; want 0", n)
	}
}
```

- [ ] **Step 2:** `go test ./internal/domain/workflow/ -run 'TestEvaluateCriterion_(FunctionConditions|Dispatches|CompilePhase|CalloutFailure|NoDispatcher|ModelStoreFailure)'`
  → FAIL with a compile error, because `out.matched` is undefined.
- [ ] **Step 3: Implement `criterion_eval.go`**

```go
package workflow

// criterionOutcome is the result of evaluating one criterion.
type criterionOutcome struct {
	matched bool
	// reason is the compute member's reason when the criterion is one
	// function condition and it was called; "" otherwise.
	reason string
	// functions holds one record per function condition the evaluation
	// reached, in evaluation order. nil when the criterion holds none.
	functions []functionRecord
	// whole reports that the criterion is a single function condition.
	whole bool
}

// functionRecord is what happened to one function condition.
type functionRecord struct {
	path            string // JSON Pointer in the criterion; "" for a whole-criterion function
	name            string
	hasPreCheck     bool
	preCheck        any // the pre-check as written
	preCheckMatched bool
	called          bool
	matches         bool
	reason          string // the member's reason, capped
}

func (l *critLocal) eval(_ *Engine, _ *criterionContext, entity *spi.Entity, _ *[]functionRecord) (bool, error) {
	return l.prepared.Match(entity.Data, entity.Meta), nil
}

// eval evaluates the members in declaration order and stops as soon as the
// group's value is known: AND at the first false member, OR at the first
// true one. Criteria and functions have no effects, so stopping changes
// which callouts are made, never the value.
func (g *critGroup) eval(e *Engine, cc *criterionContext, entity *spi.Entity, rec *[]functionRecord) (bool, error) {
	if g.op == "NOT" {
		v, err := g.members[0].eval(e, cc, entity, rec)
		if err != nil {
			return false, err
		}
		return !v, nil
	}
	stopOn := g.op == "OR"
	for _, m := range g.members {
		v, err := m.eval(e, cc, entity, rec)
		if err != nil {
			return false, err
		}
		if v == stopOn {
			return stopOn, nil
		}
	}
	return !stopOn, nil
}

// eval makes the callout unless the pre-check does not match, in which case
// the function condition is false.
func (f *critFunction) eval(e *Engine, cc *criterionContext, entity *spi.Entity, rec *[]functionRecord) (bool, error) {
	r := functionRecord{path: f.ptr, name: f.fn.Name}
	if f.preCheck != nil {
		r.hasPreCheck, r.preCheck = true, f.preCheckValue
		r.preCheckMatched = f.preCheck.prepared.Match(entity.Data, entity.Meta)
		if !r.preCheckMatched {
			*rec = append(*rec, r)
			return false, nil
		}
	}
	matches, reason, err := e.dispatchCriterionFunction(cc, entity, f.raw)
	if err != nil {
		return false, err
	}
	r.called, r.matches, r.reason = true, matches, capReason(reason)
	*rec = append(*rec, r)
	return matches, nil
}
```

Add `eval` to `critNode` in `criterion_tree.go`, above `collect`, with the doc comment "eval evaluates the node on
entity and appends one record per function condition it reaches to rec", and add the `spi` import there.

`dispatchCriterionFunction`: move the body of today's top-level-function branch (`engine.go:1026-1046`) here, with
its comment, unchanged except that it dispatches `raw` and returns the uncapped reason:

```go
// dispatchCriterionFunction makes one criterion callout with the function
// condition's own JSON.
func (e *Engine) dispatchCriterionFunction(cc *criterionContext, entity *spi.Entity, raw json.RawMessage) (bool, string, error) {
	// (the existing txgate.Suspend comment, moved verbatim)
	resume := txgate.Suspend(cc.ctx)
	defer resume()
	callCtx, stop := runCallCtx(cc.ctx)
	defer stop()
	matches, reason, err := e.extProc.DispatchCriteria(callCtx, entity, raw, cc.target, cc.workflowName, cc.transitionName, "", cc.txID)
	stop()
	resume()
	if cerr := fence.Check(cc.ctx); cerr != nil {
		return false, "", cerr
	}
	return matches, reason, err
}
```

`prepareLocalUnits`: move the local path of today's `evaluateCriterion` (`engine.go:1048-1201`) here. Keep every
comment block verbatim and in place. Change it in three ways:
- the path set is the union over the units, de-duplicated in order;
- `ValidatePatterns`, `ValidateConditionValueTypes` and `match.Prepare` each run over every unit, in that order and
  as three loops, so precedence stays as it is today;
- each unit's `prepared` is set.

```go
// prepareLocalUnits checks every local unit of a criterion against the
// entity's model and prepares it, before any callout: a fault in any unit —
// also one evaluation would not reach — fails the criterion.
func (e *Engine) prepareLocalUnits(ctx context.Context, entity *spi.Entity, units []*critLocal) error {
	if len(units) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, u := range units {
		for _, p := range search.ConditionFieldPaths(u.cond) {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
		}
	}
	// ... the existing model load, ValidateKnownPaths, loadErr and pathErr
	// handling, unchanged, with cc.ctx → ctx and returning plain errors ...
	for _, u := range units {
		if err := search.ValidatePatterns(u.cond); err != nil {
			return err
		}
	}
	for _, u := range units {
		if err := search.ValidateConditionValueTypes(fields, u.cond); err != nil {
			return err
		}
	}
	for _, u := range units {
		p, err := match.Prepare(u.cond, fieldTypes)
		if err != nil {
			return err
		}
		u.prepared = p
	}
	return nil
}
```

`evaluateCriterion` in `engine.go` becomes:

```go
// evaluateCriterion evaluates a criterion against the entity in two phases.
// The compile phase splits it at its function conditions
// (buildCriterionTree) and checks and prepares every local unit — each
// function-free subtree and each pre-check — against the model; no callout
// is made until all of it has passed. The run phase evaluates the tree:
// groups in declaration order with the stop rule, and each function
// condition reached by its pre-check and, when that matches, one callout
// with its own JSON. A failed callout fails the evaluation.
func (e *Engine) evaluateCriterion(criterion []byte, entity *spi.Entity, cc *criterionContext) (criterionOutcome, error) {
	cond, err := predicate.ParseCondition(criterion)
	if err != nil {
		return criterionOutcome{}, fmt.Errorf("failed to parse criterion: %w", err)
	}
	root, err := buildCriterionTree(cond, "")
	if err != nil {
		return criterionOutcome{}, err
	}
	units, fns := collectCriterion(root)
	if len(fns) > 0 && e.extProc == nil {
		return criterionOutcome{}, fmt.Errorf("no external processing service configured for FUNCTION criteria")
	}
	if err := e.prepareLocalUnits(cc.ctx, entity, units); err != nil {
		return criterionOutcome{}, err
	}
	var out criterionOutcome
	if len(fns) > 0 {
		out.functions = []functionRecord{}
		_, out.whole = root.(*critFunction)
	}
	if out.matched, err = root.eval(e, cc, entity, &out.functions); err != nil {
		return criterionOutcome{}, err
	}
	if out.whole && out.functions[0].called {
		out.reason = out.functions[0].reason
	}
	return out, nil
}
```

In the four callers, use `out, err := e.evaluateCriterion(...)`, then `matched := out.matched` and
`reason := out.reason`. The existing logic stays until Task 8.

`internal/contract/processing.go`: change the `DispatchCriteria` doc to "evaluates one function condition on an
external node. criterion is that function condition's own JSON (`{"type":"function","function":{…}}`), whatever its
position in the criterion. reason …".

- [ ] **Step 4: Update the test call sites.** Run
  `grep -rn "evaluateCriterion(" internal/domain/workflow/*_test.go`:
  - `_, _, err :=` becomes `_, err :=`;
  - `got, _, err :=` and `got, _, err =` become `out, err :=` / `out, err =`, and that test's later `got` becomes
    `out.matched`;
  - the helper in `criterion_precedence_test.go`, `_, _, err := e.evaluateCriterion(criterion, entity, cc)`, becomes
    `_, err := ...`.
- [ ] **Step 5:** `go test ./internal/domain/workflow/...` → PASS, new tests and old tests alike.
  `TestEngine_AutomatedCriterionNoMatch_RecordsReason` still passes: a whole function without a pre-check loads no
  model. Then `go vet ./...`.
- [ ] **Step 6: Commit** `feat(workflow): evaluate function conditions at any depth, with the pre-check as guard`.

---

### Task 8: audit `functions`, the reason, and the manual message

**Files:**
- Modify: `internal/domain/workflow/criterion_eval.go` (audit and reason helpers)
- Modify: `internal/domain/workflow/engine.go` (`selectWorkflow`, `fireTransition`, `cascadeAutomated`; the
  `defaultCriterionReason` doc)
- Test: `internal/domain/workflow/criterion_function_audit_test.go` (new)

**Interfaces:**
- Produces:

```go
func (r functionRecord) auditValue() map[string]any
func (o criterionOutcome) auditFunctions() []any
func (o criterionOutcome) functionsData() map[string]any   // nil, or {"functions": [...]}
func (o criterionOutcome) addFunctions(data map[string]any)
func (o criterionOutcome) notMatchedReason() string
```

- [ ] **Step 1: Failing unit tests for the helpers**

```go
func TestNotMatchedReason(t *testing.T) {
	cases := []struct {
		name string
		out  criterionOutcome
		want string
	}{
		{"no function", criterionOutcome{}, "criterion did not match"},
		{"whole function, member reason", criterionOutcome{whole: true, reason: "too small",
			functions: []functionRecord{{name: "f", called: true, reason: "too small"}}}, "too small"},
		{"whole function, no reason", criterionOutcome{whole: true,
			functions: []functionRecord{{name: "f", called: true}}}, "criterion did not match"},
		{"whole function, pre-check false", criterionOutcome{whole: true,
			functions: []functionRecord{{name: "f", hasPreCheck: true}}}, "f: pre-check did not match, not called"},
		{"group, none reached", criterionOutcome{functions: []functionRecord{}}, "criterion did not match"},
		{"group", criterionOutcome{functions: []functionRecord{
			{path: "/conditions/0", name: "f", called: true, matches: true},
			{path: "/conditions/1", name: "f", hasPreCheck: true},
			{path: "/conditions/2", name: "g", called: true, reason: "no credit"},
		}}, "f at /conditions/0: answered true; f at /conditions/1: pre-check did not match, not called; g at /conditions/2: answered false: no credit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.out.notMatchedReason(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNotMatchedReason_IsCappedOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", maxCriterionReasonLen/2)
	out := criterionOutcome{functions: []functionRecord{
		{path: "/conditions/0", name: "a", called: true, reason: long},
		{path: "/conditions/1", name: "b", called: true, reason: long},
	}}
	got := out.notMatchedReason()
	if len(got) > maxCriterionReasonLen || !utf8.ValidString(got) {
		t.Errorf("len %d, valid UTF-8 %t; want ≤ %d and valid", len(got), utf8.ValidString(got), maxCriterionReasonLen)
	}
}

func TestFunctionRecordAuditValue(t *testing.T) {
	notCalled := functionRecord{path: "/conditions/1", name: "f", hasPreCheck: true, preCheck: map[string]any{"type": "simple"}}
	if got := notCalled.auditValue(); !maps.EqualFunc(got, map[string]any{
		"path": "/conditions/1", "name": "f", "called": false, "preCheckMatched": false, "preCheck": map[string]any{"type": "simple"},
	}, func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }) {
		t.Errorf("not called: %v", got)
	}
	called := functionRecord{name: "g", called: true, matches: true}
	got := called.auditValue()
	if _, ok := got["preCheck"]; ok {
		t.Error("preCheck present without a pre-check")
	}
	if _, ok := got["reason"]; ok {
		t.Error("reason present though the member gave none")
	}
	if got["matches"] != true || got["path"] != "" {
		t.Errorf("called: %v", got)
	}
}
```

- [ ] **Step 2: Failing engine-level audit tests.** These go through `engine.Execute` and `engine.ManualTransition`
  with `recordingCriteria` and `saveWorkflow`, and read the events with
  `factory.StateMachineAuditStore(txCtx).GetEventsByTransaction` in the same way as
  `TestEngine_AutomatedCriterionNoMatch_RecordsReason`. Write one test per event, each with its own model:

```go
func auditEnv(t *testing.T, answers map[string]criteriaAnswer, wfs ...spi.WorkflowDefinition) (*Engine, spi.StoreFactory, spi.TransactionManager, *recordingCriteria, spi.ModelRef) {
	t.Helper()
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	rec := &recordingCriteria{answers: answers}
	engine := NewEngine(factory, uuids, txMgr, WithExternalProcessing(rec))
	ctx := ctxWithTenant(testTenant)
	ref := spi.ModelRef{EntityName: "fn-audit-" + strings.ToLower(t.Name()), ModelVersion: "1.0"}
	registerModelFields(t, ctx, factory, ref, map[string]schema.DataType{"amount": schema.Integer})
	saveWorkflow(t, factory, ctx, ref, wfs)
	return engine, factory, txMgr, rec, ref
}

func eventsOf(t *testing.T, factory spi.StoreFactory, txCtx context.Context, entityID, txID string, typ spi.StateMachineEventType) []spi.StateMachineEvent {
	t.Helper()
	store, err := factory.StateMachineAuditStore(txCtx)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.GetEventsByTransaction(txCtx, entityID, txID)
	if err != nil {
		t.Fatal(err)
	}
	var out []spi.StateMachineEvent
	for _, ev := range all {
		if ev.EventType == typ {
			out = append(out, ev)
		}
	}
	return out
}
```

The tests:
- `TestAudit_CascadeNoMatch_FunctionsAndReason`: the transition criterion is
  `evGroup("AND", evFn("a", ""), evFn("a", evAmountGT100))` and `a` answers true, so the pre-check of the second use is
  false. Assert:
  - `TRANSITION_NOT_MATCH_CRITERION` data `reason` =
    `a at /conditions/0: answered true; a at /conditions/1: pre-check did not match, not called`;
  - `functions` has 2 entries with paths `/conditions/0` and `/conditions/1`;
  - `criterion` = `group`;
  - the entity stays in its state.
- `TestAudit_CascadeMatch_TransitionMakeCarriesFunctions`: the criterion is `evFn("a", evAmountGT10)` and `a` answers
  true. `TRANSITION_MAKE` data = `{"functions": [{path:"", name:"a", preCheck:…, preCheckMatched:true, called:true,
  matches:true}]}`. A second transition with a local-only criterion records `TRANSITION_MAKE` with nil `Data`.
- `TestAudit_Selection_FoundAndSkipped`: workflow `wf-a` has the criterion `evFn("sel", evAmountGT100)`, and workflow
  `wf-b` has none. Assert:
  - `WORKFLOW_SKIP` data has `workflowName` `wf-a`, `reason` `sel: pre-check did not match, not called`, and
    `functions` with one entry where `called` is false;
  - the second workflow's `WORKFLOW_FOUND` has nil `Data`;
  - with `amount` 150 instead, `WORKFLOW_FOUND` for `wf-a` has `data.functions[0].called == true`.
- `TestAudit_ManualRefused_Message`: the manual transition `approve` has the criterion
  `evGroup("AND", evFn("a", ""), evFn("b", ""))`, `a` answers true, and `b` answers `{matches:false, reason:"too small"}`.
  `engine.ManualTransition` returns an error with `errors.Is(err, ErrCriterionNotMatched)` and `err.Error()` =
  `transition "approve" criterion not matched: a at /conditions/0: answered true; b at /conditions/1: answered false: too small`.
- `TestAudit_ManualRefused_WholeFunctionNoReasonStaysBare`: the criterion is `evFn("a", "")` and `a` answers false
  with no reason. `err.Error()` = `transition "approve" criterion not matched`.
- `TestAudit_ManualAllowed_TransitionMakeCarriesFunctions`: the criterion is `evGroup("OR", evFn("a", ""))` and `a`
  answers true. `TRANSITION_MAKE` data has `functions` with one entry.
- `TestFireTransition_ClassifiesForTheScheduler`: `FireScheduledTransition` reads `ErrCriterionNotMatched` as
  Declined and any other error as a failure to retry (`fire_scheduled.go:269`). Through `engine.ManualTransition`,
  which shares `fireTransition` with scheduled firing:
  - a pre-check that does not match gives an error with `errors.Is(err, ErrCriterionNotMatched)`, and 0 callouts;
  - `evGroup("AND", evAmountGT10, evFn("b", ""))`, where `b` answers with an error, gives an error where
    `errors.Is(err, ErrCriterionNotMatched)` is false.

- [ ] **Step 3:** `go test ./internal/domain/workflow/ -run 'NotMatchedReason|FunctionRecordAuditValue|TestAudit_'`
  → FAIL.
- [ ] **Step 4: Implement the helpers** in `criterion_eval.go`:

```go
// auditValue is the record's entry in an audit event's `functions` list.
func (r functionRecord) auditValue() map[string]any {
	v := map[string]any{"path": r.path, "name": r.name, "called": r.called}
	if r.hasPreCheck {
		v["preCheck"] = r.preCheck
		v["preCheckMatched"] = r.preCheckMatched
	}
	if r.called {
		v["matches"] = r.matches
		if r.reason != "" {
			v["reason"] = r.reason
		}
	}
	return v
}

func (o criterionOutcome) auditFunctions() []any {
	list := make([]any, len(o.functions))
	for i, r := range o.functions {
		list[i] = r.auditValue()
	}
	return list
}

// functionsData is the `data` of WORKFLOW_FOUND and TRANSITION_MAKE: nil
// unless the criterion holds a function condition.
func (o criterionOutcome) functionsData() map[string]any {
	if o.functions == nil {
		return nil
	}
	return map[string]any{"functions": o.auditFunctions()}
}

// addFunctions adds `functions` to an event's data when the criterion holds
// a function condition.
func (o criterionOutcome) addFunctions(data map[string]any) {
	if o.functions != nil {
		data["functions"] = o.auditFunctions()
	}
}

// notMatchedReason explains a criterion that did not match. A criterion
// without a function condition, and a called whole-criterion function, keep
// the member's reason or the default. Otherwise every function condition
// reached is listed in order with what happened to it; no answer is singled
// out as the cause, because under NOT a true answer can refuse.
func (o criterionOutcome) notMatchedReason() string {
	if o.functions == nil || (o.whole && o.functions[0].called) {
		if o.reason != "" {
			return o.reason
		}
		return defaultCriterionReason
	}
	if len(o.functions) == 0 {
		return defaultCriterionReason
	}
	clauses := make([]string, 0, len(o.functions))
	for _, r := range o.functions {
		var b strings.Builder
		b.WriteString(r.name)
		b.WriteString(atPointer(r.path))
		if !r.called {
			b.WriteString(": pre-check did not match, not called")
		} else {
			fmt.Fprintf(&b, ": answered %t", r.matches)
			if r.reason != "" {
				b.WriteString(": ")
				b.WriteString(r.reason)
			}
		}
		clauses = append(clauses, b.String())
	}
	return capReason(strings.Join(clauses, "; "))
}
```

- [ ] **Step 5: Wire the four sites in `engine.go`.**

`selectWorkflow`:

```go
		out, err := e.evaluateCriterion(wf.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, target: "WORKFLOW",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to evaluate workflow criterion for %q: %w", wf.Name, err)
		}
		if out.matched {
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventWorkflowFound, fmt.Sprintf("Workflow %q matched criterion", wf.Name), out.functionsData())
			return wf, nil
		}
		data := map[string]any{"workflowName": wf.Name, "reason": out.notMatchedReason()}
		out.addFunctions(data)
		e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
			spi.SMEventWorkflowSkipped, fmt.Sprintf("Workflow %q criterion not matched", wf.Name), data)
```

`fireTransition`:

```go
	var criterionData map[string]any // TRANSITION_MAKE data
	if len(transition.Criterion) > 0 && string(transition.Criterion) != "null" {
		// (existing comment)
		out, err := e.evaluateCriterion(transition.Criterion, entity, &criterionContext{
			ctx: ctx, txID: txID, workflowName: wf.Name, transitionName: transitionName, target: "TRANSITION",
		})
		if err != nil {
			return ctx, txID, fmt.Errorf("failed to evaluate transition criterion: %w", err)
		}
		if !out.matched {
			reason := out.notMatchedReason()
			data := map[string]any{
				"workflowName": wf.Name,
				"transition":   transitionName,
				"criterion":    criterionName(transition.Criterion),
				"reason":       reason,
			}
			out.addFunctions(data)
			e.recordEvent(auditStore, ctx, entity.Meta.ID, txID, entity.Meta.State,
				spi.SMEventTransitionCriterionNoMatch,
				fmt.Sprintf("Transition %q criterion not matched", transitionName), data)
			// The manual path rolls back, so this message — not the audit
			// event — is where the caller learns why.
			if reason != defaultCriterionReason {
				return ctx, txID, &criterionNotMatchedError{msg: fmt.Sprintf("transition %q criterion not matched: %s", transitionName, reason)}
			}
			return ctx, txID, &criterionNotMatchedError{msg: fmt.Sprintf("transition %q criterion not matched", transitionName)}
		}
		criterionData = out.functionsData()
	}
```

Then pass `criterionData` as the last argument of the `SMEventTransitionMade` `recordEvent` in `fireTransition`.

`cascadeAutomated`: make the same change. The not-matched `data` gains `addFunctions`, its `reason` is
`out.notMatchedReason()`, and the `TRANSITION_MAKE` `recordEvent` takes `out.functionsData()`. Declare `var
criterionData map[string]any` per iteration, before the criterion block, so that a transition without a criterion
passes nil.

Update the `defaultCriterionReason` doc to read "a criterion without a function condition, or a whole-criterion
function whose member gave no reason". Update the `ErrCriterionNotMatched` doc to read "the reason is appended when it
is not the default".

- [ ] **Step 6:** `go test ./internal/domain/workflow/...` → PASS. The existing reason tests still pass.
- [ ] **Step 7: Commit** `feat(workflow): the audit records every function condition a criterion reached`.

---

### Task 9: OpenAPI contract

**Files:**
- Modify: `api/openapi.yaml`
- Regenerate: `api/generated.go` (`go generate ./api`)
- Test: `api/` contract tests, if any exist (`grep -rln "GroupConditionDto\|FunctionConditionDto" api/*_test.go internal/**/openapi*_test.go`)

- [ ] **Step 1:** Add `CriterionGroupConditionDto` next to `GroupConditionDto`. Copy `GroupConditionDto` exactly,
  then make these changes:
  - its description gets one more paragraph: "A criterion's group may also hold `function` conditions, at any depth;
    the engine evaluates the members in declaration order and stops as soon as the group's value is known (`AND` at
    the first false member, `OR` at the first true one). A search condition's group (`GroupConditionDto`) holds local
    conditions only.";
  - its `conditions.items.oneOf` is `ArrayConditionDto`, `CriterionGroupConditionDto`, `FunctionConditionDto`,
    `LifecycleConditionDto`, `SimpleConditionDto`.
- [ ] **Step 2:** The transition `criterion` (≈`:10329`) and the workflow `criterion` (≈`:10522`) `oneOf`: replace
  `GroupConditionDto` with `CriterionGroupConditionDto`.
- [ ] **Step 3:** `ExternalizedFunctionDto.criterion` becomes:

```yaml
        criterion:
          description: |
            Optional pre-check, evaluated before the function is called. It holds
            only local conditions (`simple`, `lifecycle`, `array`, and `group` of
            those); a `function` condition anywhere in it is refused at import
            with 400 VALIDATION_FAILED. When the pre-check does not match, the
            function condition is false and the function is not called; when it
            matches, or is absent, the function is called and its answer is the
            function condition's value.
          oneOf:
            - $ref: "#/components/schemas/ArrayConditionDto"
            - $ref: "#/components/schemas/GroupConditionDto"
            - $ref: "#/components/schemas/LifecycleConditionDto"
            - $ref: "#/components/schemas/SimpleConditionDto"
```

- [ ] **Step 4:** Add this to the `StateMachineAuditEvent.data` description (≈`:11545`): "When the criterion holds a
  function condition, WORKFLOW_FOUND, WORKFLOW_SKIP, TRANSITION_NOT_MATCH_CRITERION and TRANSITION_MAKE carry
  `functions`: one entry per function condition the evaluation reached, in order, with `path` (JSON Pointer in the
  criterion, empty for a whole-criterion function), `name`, `called`, and when present `preCheck`, `preCheckMatched`,
  `matches` and `reason`."
- [ ] **Step 5:** Run `go generate ./api`, then `make check-codegen` and `go build ./...`. Expected: the generated
  file gains `CriterionGroupConditionDto` and the union helpers; no Go code outside `api/` changes. Then check that
  the search schemas are unchanged: `git diff api/openapi.yaml | grep -n "7162\|7588\|11074"` gives no hunk there.
- [ ] **Step 6: Commit** `feat(api): criteria groups hold function conditions; the pre-check is local-only`.

---

### Task 10: e2e — import and export

**Files:** Create `internal/e2e/criterion_function_import_test.go`

Shared helpers go in this file. Tasks 11 and 14 use them, and Task 11 adds its own helpers in its own file:

```go
func cfWorkflow(name, criterion string) string {
	return `{"importMode":"REPLACE","workflows":[{"version":"1.5","name":"` + name + `","initialState":"NONE","active":true,
	"states":{"NONE":{"transitions":[{"name":"init","next":"CREATED","manual":false}]},
	"CREATED":{"transitions":[{"name":"promote","next":"PROMOTED","manual":false,"criterion":` + criterion + `}]},
	"PROMOTED":{}}}]}`
}

func cfFn(name, preCheck string) string {
	if preCheck == "" {
		return `{"type":"function","function":{"name":"` + name + `"}}`
	}
	return `{"type":"function","function":{"name":"` + name + `","criterion":` + preCheck + `}}`
}

func cfGroup(op string, members ...string) string {
	return `{"type":"group","operator":"` + op + `","conditions":[` + strings.Join(members, ",") + `]}`
}

const (
	cfAmountGT100 = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}`
	cfAmountGT10  = `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":10}`
	cfStateIsCreated = `{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"CREATED"}`
)
```

- [ ] **Step 1: Tests**
  - `TestCriterionFunctionImport_Accepted` is table-driven. Each row imports `cfWorkflow` into its own model, created
    with `importModelE2E` + `lockModelE2E`, and expects 200. The rows: pre-check simple, lifecycle, array
    (`{"type":"array","jsonPath":"$.tags[*]","operatorType":"EQUALS","values":["a"]}`), group, and `null`; function in
    AND, OR, NOT and nested; the same name twice.
  - `TestCriterionFunctionImport_Refused` is table-driven. Each row expects 400 with `assertErrorCode(t, body,
    "VALIDATION_FAILED")`, and the body contains the position. The rows are:
    - the Task 6 refusal cases;
    - one whose `function` member is `null`;
    - the two limit cases of `TestImport_LimitsCountThroughPreChecks`, built the same way: 10,000 entries in an
      `array` pre-check, and 50 nested groups in a pre-check.
  - `TestCriterionFunctionImport_ExportRoundTrip`: import a criterion that holds
    `cfGroup("AND", cfAmountGT10, cfGroup("NOT", cfFn("cf-exp-a", cfStateIsCreated)), cfFn("cf-exp-b", ""))`. Then use
    `exportWorkflowE2E` and assert:
    - 200 (the e2e response validator checks the body against `CriterionGroupConditionDto`);
    - the exported transition criterion, compared as decoded JSON, equals the imported one.
- [ ] **Step 2:** `go test -timeout 30m ./internal/e2e/ -run 'TestCriterionFunctionImport_' 2>&1 | tee <scratchpad>/e2e-import.log | grep -E '^(--- |FAIL|ok)'`.
  Expected: PASS once Tasks 6 and 9 are in. Then check that the tests are not vacuous:
  - `git worktree add <scratchpad>/red628 <Task 5 commit>`;
  - add the same `go.work` `use` line there;
  - run `-run 'TestCriterionFunctionImport_Refused/function_in_pre-check'` and expect it to FAIL (200);
  - `git worktree remove <scratchpad>/red628`.

  Record the result in the commit body.
- [ ] **Step 3: Commit** `test(e2e): function conditions and pre-checks at import and export`.

---

### Task 11: e2e — evaluation through HTTP (localproc)

**Files:** Create `internal/e2e/criterion_function_eval_test.go`

The shared e2e server dispatches criteria to `procSvc`, which is `localproc`. Use unique function names per test, in
the form `cf-<test>-<x>`, so that `procSvc.CriteriaCallCount(name)` is exact under parallel tests. Register each name
with `procSvc.RegisterCriteriaReason`. Payloads use `workflowSampleModel` fields (`name`, `amount`, `status`).

Helpers:

```go
// cfEventData returns the data of the first state-machine event of type typ.
func cfEventData(t *testing.T, entityID, typ string) map[string]any {
	t.Helper()
	for _, ev := range getSMAuditEvents(t, entityID) {
		if ev["eventType"] == typ {
			d, _ := ev["data"].(map[string]any)
			return d
		}
	}
	t.Fatalf("no %s event for %s", typ, entityID)
	return nil
}

func cfFunctions(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	raw, ok := data["functions"].([]any)
	if !ok {
		t.Fatalf("data has no functions list: %v", data)
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

func cfAnswer(m bool, reason string) func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) {
	return func(context.Context, *spi.Entity, json.RawMessage) (bool, string, error) { return m, reason, nil }
}
```

- [ ] **Step 1: Tests** (each with its own model name; every assertion is listed):
  1. `TestCriterionFunction_Cascade_PreCheckFalse_NoCallout`: the criterion is `cfFn("cf-pcf-a", cfAmountGT100)`,
     `a` answers true, and amount is 50. Assert:
     - the state is `CREATED`;
     - `CriteriaCallCount("cf-pcf-a") == 0`;
     - `TRANSITION_NOT_MATCH_CRITERION` reason = `cf-pcf-a: pre-check did not match, not called`;
     - `functions[0]`: `path` `""`, `called` false, `preCheckMatched` false, and `preCheck.jsonPath` = `$.amount`.
  2. `TestCriterionFunction_Cascade_PreCheckTrue_Called`: the same with amount 150. Assert:
     - the state is `PROMOTED`;
     - the call count is 1;
     - `TRANSITION_MAKE` `functions[0]` has `called` true and `matches` true.
  3. `TestCriterionFunction_Cascade_Groups` is table-driven, with amount 50:

     | criterion | answers | state | call counts |
     |---|---|---|---|
     | `AND(a, b)` | a true, b false | CREATED | a 1, b 1 |
     | `AND(amount>100, a)` | a true | CREATED | a 0 |
     | `OR(a, b)` | a true | PROMOTED | a 1, b 0 |
     | `OR(a, b)` | a false, b true | PROMOTED | a 1, b 1 |
     | `NOT(a)` | a true | CREATED | a 1 |
     | `NOT(AND(a, b))` | a true, b false | PROMOTED | a 1, b 1 |

     For the first row, also assert the reason
     `cf-grp1-a at /conditions/0: answered true; cf-grp1-b at /conditions/1: answered false`. For the `NOT(a)` row,
     assert the reason `cf-grp5-a at /conditions/0: answered true`.
  4. `TestCriterionFunction_Cascade_FalseGroupThenNextTransitionFires`: `CREATED` has `promote`
     (`cfGroup("AND", cfFn(a,""))`, where `a` answers false) and then `fallback` (no criterion → `FALLBACK`). The
     state must be `FALLBACK`. Write the workflow JSON inline.
  5. `TestCriterionFunction_Manual_Refused_ReasonListsCallouts`: the manual transition `approve` has
     `cfGroup("AND", cfFn(a,""), cfFn(b,""))`; `a` answers true and `b` answers `(false, "too small")`. Call
     `PUT /api/entity/JSON/{id}/approve` and assert:
     - 400 and `WORKFLOW_FAILED`;
     - the body contains `criterion not matched: cf-man-a at /conditions/0: answered true; cf-man-b at /conditions/1: answered false: too small`.
  6. `TestCriterionFunction_Manual_Allowed_TransitionMakeFunctions`: the manual criterion is `cfGroup("OR", cfFn(a,""))`
     and `a` answers true. Assert 200, and that `TRANSITION_MAKE` `functions` has length 1.
  7. `TestCriterionFunction_Selection_PreCheck`: workflow `wf-fn` has the criterion `cfFn(sel, cfAmountGT100)` and
     goes `NONE → A`; workflow `wf-plain` has none and goes `NONE → B`; `sel` answers true. Assert:
     - with amount 50: state `B`, call count 0, and `WORKFLOW_SKIP` `functions[0].called` false;
     - with amount 150: state `A`, and `WORKFLOW_FOUND` `functions[0].called` true.
  8. `TestCriterionFunction_Selection_FunctionInGroup`: the workflow criterion is `cfGroup("OR", cfFn(a,""), cfFn(b,""))`;
     `a` answers false and `b` answers true. The workflow is selected, and `WORKFLOW_FOUND` `functions` has paths
     `/conditions/0` and `/conditions/1`.
  9. `TestCriterionFunction_TransitionsRead`: the workflow criterion is `cfGroup("AND", cfAmountGT10, cfFn(tr,""))`;
     `tr` answers true; `CREATED` has the manual transition `approve`. After the create, read `CriteriaCallCount = n`.
     Then:
     - `GET /api/entity/{id}/transitions` gives 200, the list contains `approve`, and the count is `n+1`;
     - re-register `tr` to return an error, and the GET gives 400 `WORKFLOW_FAILED`.
  10. `TestCriterionFunction_Unevaluable_NoCallout`: the model is the sample. Three cases:
      - the criterion is `cfGroup("OR", cfFn(a,""), <simple on $.amonut>)`. The import passes, because the grammar is
        fine and the model is not checked at import. The create gives 400 `WORKFLOW_FAILED`, and `a`'s call count is 0;
      - the same with the undeclared path inside `a`'s pre-check;
      - a manual transition with the same criterion, fired on an entity created under a criterion-free first step,
        gives 400 and 0 calls.
- [ ] **Step 2:** Run `go test -timeout 30m ./internal/e2e/ -run 'TestCriterionFunction_' 2>&1 | tee <scratchpad>/e2e-eval.log | grep -E '^(--- |FAIL|ok)'`.
  Expected: PASS.
- [ ] **Step 3: Commit** `test(e2e): function conditions and pre-checks through every HTTP entry point`.

---

### Task 12: e2e — callout failures and scheduled firing (scripted cnodes)

**Files:** Create `internal/e2e/criterion_function_callout_test.go`

These tests use `newCallbackHarness(t)` and `newSchedulerHarness(t, nil)`, `h.AttachCnode`, `scriptAlways`,
`answerMatches`, `answerFail`, `uniq`. Workflows put `calculationNodesTags: tag` in each function's `config`. A
scripted cnode can answer per name, using a script that switches on `call.Name`:

```go
func scriptByName(replies map[string]cnodeReply) cnodeScript {
	return func(_ context.Context, call receivedCallout, _ *reqCtx) cnodeReply { return replies[call.Name] }
}
```

- [ ] **Step 1: Tests**
  1. `TestCriterionFunctionCallout_NoMemberInGroup_503`: the criterion is `AND(amount>10, fn x)`, where `x`'s tag has
     no cnode. The create gives 503 `NO_COMPUTE_MEMBER_FOR_TAG`.
  2. `TestCriterionFunctionCallout_MemberFailsInGroup_400`: the criterion is `OR(fn a, fn b)` with a cnode on the tag.
     The script answers `a` with `answerMatches(false)` and `b` with `answerFail("b down")`. Assert:
     - the create gives 400 `WORKFLOW_FAILED`;
     - `cn.Received()` names `a`, then `b`.
  3. `TestCriterionFunctionCallout_Scheduled_PreCheckFalseDeclined`: a copy of `TestSchedRun_Declined`, whose criterion
     is the function `c` with the pre-check `{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":1000}`
     (the sample amount is 100). Assert:
     - `TRANSITION_NOT_MATCH_CRITERION`, then task removal;
     - the state stays `Open`;
     - `len(cn.Received()) == 0`.
  4. `TestCriterionFunctionCallout_Scheduled_FailureInGroupRetried`: a copy of `TestSchedRun_CriterionErrorRetried`,
     whose criterion is `AND(amount>10, fn c)` and whose cnode answers `answerFail("crit boom")`. Assert that
     `firstAttempt` shows `WAITING`, attempts 1, and lastError `crit boom`.
- [ ] **Step 2:** Run with `-run TestCriterionFunctionCallout_` and a log file in the scratchpad → PASS.
- [ ] **Step 3: Commit** `test(e2e): callout failures and scheduled firing with function conditions in groups`.

---

### Task 13: gRPC

**Files:** Create `internal/grpc/criterion_function_rpc_test.go`

- [ ] **Step 1: Harness**

```go
// newTestEnvWithLocalCriteria is newTestEnvWithWorkflow with a localproc
// external-processing service, so criteria callouts run in-process and each
// name's calls are counted.
func newTestEnvWithLocalCriteria(t *testing.T) (*CloudEventsServiceImpl, *workflow.Handler, context.Context, *localproc.LocalProcessingService) {
	// body of newTestEnvWithWorkflow (scheduled_transition_test.go:30), with
	// lp := localproc.New() and workflow.NewEngine(..., workflow.WithExternalProcessing(lp))
}
```

Write the body out in full by copying `newTestEnvWithWorkflow`. Factoring it out is a bigger change, so do not do it.
Import the workflow through `wfHandler.ImportEntityModelWorkflow`, as `workflow_selection_test.go` does, and create
through `svc.EntityManage(ctx, makeCE(EntityCreateRequest, …))`.

- [ ] **Step 2: Tests**
  - Selection, pre-check false → `kind-b-wf` is chosen and `lp.CriteriaCallCount("sel") == 0`. This is a variant of
    the two-workflow fixture in `workflow_selection_test.go`. Its `kind-a-wf` criterion, written inline, is
    `{"type":"function","function":{"name":"sel","criterion":{"type":"simple","jsonPath":"$.kind","operatorType":"EQUALS","value":"a"}}}`,
    and the entity data is `kind: b`.

  Every criterion in this file is written as inline JSON. The e2e helpers (`cfFn`, `cfGroup`) are in another package.
  - Cascade, pre-check false → the state stays at the source and the count is 0. Pre-check true → the next state and
    count 1.
  - Cascade groups: `AND(a,b)` with a false → count `a` 1, `b` 0; `OR(a,b)` with a true → `b` 0; `NOT(a)` with a true
    → no transition.
  - Manual refused: `EntityTransitionRequest`, with `a` true and `b` false with the reason "too small". Assert:
    - `success=false` and `Error.Code == "CLIENT_ERROR"`;
    - the message contains `WORKFLOW_FAILED` and `b at /conditions/1: answered false: too small`.
  - Unevaluable in a skipped member → `CLIENT_ERROR` with `WORKFLOW_FAILED`, and count 0.
  - Callout failure: localproc returns an error for `b` → `CLIENT_ERROR` with `WORKFLOW_FAILED`. No member, using
    `newTestEnvWithDispatch(t)` with an empty registry, function in a group → `assertClientErrorEnvelope(t, typed,
    "NO_COMPUTE_MEMBER_FOR_TAG")`.
- [ ] **Step 3:** `go test ./internal/grpc/ -run 'CriterionFunction'` → PASS.
- [ ] **Step 4: Commit** `test(grpc): function conditions and pre-checks through the gRPC entry points`.

---

### Task 14: search — a malformed pre-check in a search condition

**Files:** Modify `internal/e2e/search_function_condition_test.go`, `internal/grpc/search_function_condition_test.go`

- [ ] **Step 1:** e2e `TestSearch_Sync_FunctionWithMalformedPreCheck_Returns400`. Search on
  `{"type":"function","function":{"name":"x","criterion":{"type":"bogus"}}}` and assert 400 `INVALID_CONDITION`. Copy
  the sync-search call of `TestSearch_Sync_FunctionCondition_Returns400` in the same file.
- [ ] **Step 2:** gRPC `TestRPC_DirectSearch_FunctionWithMalformedPreCheck_InvalidArgument`. `EntitySearchCollection`
  returns a transport error with `status.Code(err) == codes.InvalidArgument`, and its message contains
  `invalid condition`.
- [ ] **Step 3:** Run both; expected PASS. These pin the boundary that Task 1 moved. Prove RED by running them
  against the SPI before Task 1: comment out the `go.work` `use` line for one run, and expect the function refusal
  (`CLIENT_ERROR` envelope) instead of `InvalidArgument`. Restore the line.
- [ ] **Step 4: Commit** `test(search): a function with a malformed pre-check is refused at parse`.

---

### Task 15: parity scenarios

**Files:**
- Modify: `e2e/parity/externalapi/workflow_externalization.go` (09/12 rewritten)
- Create: `e2e/parity/criterion_function.go`
- Modify: `e2e/parity/registry.go`

Each scenario starts a further compute client with the catalog behaviour on a tag of its own:
`parity.StartComputeClientOrSkip(t, fixture, ComputeClientSpec{TenantID: tenant.ID, Tags: []string{tag}})` with
`tenant := fixture.NewTenant(t)`. The catalog criteria are `always-true`, `always-false`, `amount-gt-100`. Count
callouts from `cc.Received(t)` with `Kind == "criterion"`. Read audit events with `c.GetAuditEvents(t, id)`, then
`AsStateMachine()`, then decode `Data` into `map[string]any`.

- [ ] **Step 1: Rewrite 09/12** to be Cloud's `testExternalizedCriteriaConditionSkipsExternalCallWhenNotMatched`. The
  criterion is
  `{"type":"function","function":{"name":"always-true","config":{"calculationNodesTags":"<tag>"},"criterion":{"type":"simple","jsonPath":"$.flag","operatorType":"EQUALS","value":true}}}`,
  and the sample is `{"k":1,"flag":false}`. Assert:
  - an entity with `flag:false` stays `CREATED` and the client has received 0 criterion requests;
  - an entity with `flag:true` reaches `PROCESSED` and the client has received exactly 1 criterion request, named
    `always-true`;
  - `TRANSITION_MAKE` `data.functions[0].called == true`.

  Replace the doc comment's "not observable through HTTP" paragraph with the Cloud test it mirrors.
- [ ] **Step 2: New scenarios** in `criterion_function.go`:
  - `RunCriterionFunction_GroupStopRule`: three subtests, each with its own model and tag, with amount 50:
    - `AND(always-false, always-true)` → stays, received [always-false];
    - `OR(always-true, always-false)` → moves, received [always-true];
    - `NOT(always-true)` → stays, received [always-true].
  - `RunCriterionFunction_SelectionPreCheck`: the two-workflow selection of Task 11 #7, with the parity client, and
    `WORKFLOW_SKIP` `functions[0].called == false`.
  - `RunCriterionFunction_SelectionGroup`: Task 11 #8 with catalog names.
  - `RunCriterionFunction_ManualRefusedReason`: manual `approve` with `AND(always-true, always-false)`. Use
    `c.UpdateEntityRaw(t, id, "approve", body)`, then assert 400 `WORKFLOW_FAILED` and that the detail contains
    `always-true at /conditions/0: answered true; always-false at /conditions/1: answered false`.

  Register them in `registry.go` as `{"CriterionFunction_GroupStopRule", RunCriterionFunction_GroupStopRule}` and so
  on, next to the other compute-client scenarios (≈`:244`).
- [ ] **Step 3:** Run `make test` (it runs parity on memory, sqlite and postgres) with a log in the scratchpad.
  Expected: the new scenarios PASS on every backend; `TestComputeClientScenariosRegistered` passes.
- [ ] **Step 4: Commit** `test(parity): function conditions in groups and the pre-check across backends`.

---

### Task 16: hand-over — a peer builds from the function condition's own JSON

**Files:**
- Modify: `internal/cluster/dispatch/handover_test.go`
- Create: `e2e/parity/multinode/criterion_function.go`

- [ ] **Step 1: Unit**

```go
// A criterion callout handed to a peer carries the function condition's own
// JSON — a member of a group, with its pre-check — and the peer builds the
// callout from it alone.
func TestToCallout_FunctionConditionFromAGroup(t *testing.T) {
	raw := json.RawMessage(`{"type":"function","function":{"name":"credit-check","config":{"calculationNodesTags":"python"},"criterion":{"type":"simple","jsonPath":"$.amount","operatorType":"GREATER_THAN","value":100}}}`)
	req := validRequest(t, "criteria")
	req.Criterion = raw
	call, failure := req.toCallout()
	if failure != nil {
		t.Fatalf("toCallout: %v", failure)
	}
	if call.Name != "credit-check" || call.Tags != "python" || string(call.Source.Criterion) != string(raw) {
		t.Errorf("callout name=%q tags=%q criterion=%s", call.Name, call.Tags, call.Source.Criterion)
	}
}
```

  Run `go test ./internal/cluster/dispatch/ -run FunctionConditionFromAGroup`. It passes against the current code,
  because the peer already parses `function` only. That is the point: it pins the contract the engine relies on.
  Record in the commit body that it is a characterisation test.
- [ ] **Step 2: Multi-node scenario** `RunCriterionFunction_HandOverFromAGroup`. Use `mnRequire(t, fixture, 2)` and a
  fresh tenant. The owner (pnode 0) has no client for the tag, and
  `remote := mnWarmUp(t, fixture, owner, tenant, 1, "mn-cf-w", tag)`. The workflow `NONE → ACTIVE` has the criterion
  `AND(amount>5, {"type":"function","function":{"name":"always-true","config":{"calculationNodesTags":tag},"criterion":{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"new"}}})`.
  Create through the owner with `mnSample`, then assert:
  - the entity is `ACTIVE`;
  - `remote.Received` holds one record with `Kind == "criterion"` and `Name == "always-true"`.

  Register it with `Register(NamedTest{Name: "CriterionFunction_HandOverFromAGroup", Fn: ...})` in an `init()`. Build
  the workflow with `json.Marshal`, as `mnWorkflow` does.
- [ ] **Step 3:** The multi-node suite runs in CI (`TestMultiNode`, postgres parity). Locally,
  `go vet ./e2e/parity/...` and `go test ./e2e/parity/multinode/` only compile it; do not run `TestMultiNode` locally.
- [ ] **Step 4: Commit** `test(cluster): a peer builds a criterion callout from the function condition's own JSON`.

---

### Task 17: documentation

**Files:** `cmd/cyoda/help/content/workflows.md`, `search.md`, `grpc.md`, `crud.md`;
`docs/cloud-parity/function-conditions-in-criteria.md` (new) and the cloud-parity index (`docs/cloud-parity/README.md`);
`docs/cloud-parity/function-condition-search-rejection.md`, `criterion-stoppage-reason.md`,
`unevaluable-criterion-fails-save.md`; `docs/workflow-schema-versioning.md`; `CHANGELOG.md`.

- [ ] **Step 1: `workflows.md` CRITERIA** (`:471`). Replace the sentence "a `function` criterion … must be the whole
  criterion — one nested inside a `group` fails the evaluation". Add a `### Function conditions` subsection that
  states:
  - Rules 1–3, in plain words and without issue numbers;
  - the evaluation order and the stop rule;
  - that a pre-check holds only local conditions;
  - that the whole criterion and every pre-check are checked before the first callout;
  - that a failed callout fails the operation;
  - the audit `functions` field, with an example entry;
  - the reason format.

  In IMPORT REQUEST (`:542`, `:543`, `:551`), replace "`function`-type criterion" with "function condition in a
  criterion, at any depth". Add these bullets:
  - a function condition without a `function` object or a `name`;
  - a function condition in a pre-check (with its position);
  - pre-check local checks and the entry and depth limits counted through pre-checks.
- [ ] **Step 2: `search.md:207`** — function conditions are valid in workflow and transition criteria at any depth,
  and never in a search condition. **`grpc.md:343`** — one `EntityCriteriaCalculationRequest` per function condition
  reached, from workflow selection and transitions, never for one whose pre-check did not match. **`crud.md:404,751`**
  — the transitions read makes the callouts that workflow criteria need. Read each line first and change only what is
  now false.
- [ ] **Step 3: cloud-parity.**
  - The new `function-conditions-in-criteria.md`, in the format of its siblings: Rules 1–4, the wire contract, the
    audit, and what Cloud must change (§12).
  - Add a line to the index.
  - Update the three sibling docs where they state the "whole criterion only" rule or the reason and message rules.
- [ ] **Step 4: `workflow-schema-versioning.md` 1.5 entry.**
  - Change "Nine tightenings" to "Eleven tightenings" in the heading sentence and in the paragraph that names the
    count.
  - Add rule 10: a pre-check is evaluated, and one that holds a function condition or breaks a local check or limit
    is refused. Rubric point 1: it was silently ignored.
  - Add rule 11: a function condition without a `function` object or `name` is refused. It failed every dispatch.
  - Add this to the "Additive" part: function conditions inside groups are accepted. They imported before and failed
    every evaluation.
  - Extend the "Why dual-shape" paragraph with one sentence on rules 10–11.
- [ ] **Step 5: `CHANGELOG.md` `[Unreleased]`.**
  - `### Breaking`: pre-checks are evaluated, so a function whose pre-check does not match is not called and is
    false; a pre-check with a function condition, and a function condition without `function`/`name`, are refused at
    import; audit `data` gains `functions`.
  - `### Added`: function conditions in `AND`/`OR`/`NOT` groups at any depth, evaluated in declaration order with the
    stop rule.

  No issue numbers in the text.
- [ ] **Step 6:** `go test ./cmd/cyoda/...` (help-topic tests) → PASS. Then
  `grep -rn "must be the whole criterion\|nested inside a .group. fails" cmd/ docs/ README.md` → empty.
- [ ] **Step 7: Commit** `docs: function conditions in criteria and the pre-check`.

---

### Task 18: SPI merge and the pin

- [ ] **Step 1:** Wait until the SPI PR's CI is green. Read the check list, not the exit code. Then merge it into
  `main`, as was done for spi#66 (`gh api` merge, squash). Record the merge commit SHA.
- [ ] **Step 2:** In the cyoda-go worktree, remove the `use ../../../../cyoda-go-spi` line from `go.work`, so that
  `git diff go.work` is empty. Then:

```bash
GOFLAGS=-mod=mod go get github.com/cyoda-platform/cyoda-go-spi@<merge-sha>
(cd plugins/memory && go get github.com/cyoda-platform/cyoda-go-spi@<merge-sha> && go mod tidy)
(cd plugins/sqlite && go get github.com/cyoda-platform/cyoda-go-spi@<merge-sha> && go mod tidy)
(cd plugins/postgres && go get github.com/cyoda-platform/cyoda-go-spi@<merge-sha> && go mod tidy)
go mod tidy
make check-spi-pin-sync
```

  If the plugin modules require the root module at a pseudo-version, `make repin-plugins` applies only after the push;
  follow MAINTAINING.md §3.
- [ ] **Step 3:** In the `COMPATIBILITY.md` v0.9.0 row:
  - replace the pseudo-version in both pin columns;
  - change "resolves the SPI's `main` at the merge of spi#66 (`ab06905`)" to name this merge;
  - prepend the additive surface: "Added — `predicate.FunctionCondition` keeps `Raw` and `PreCheck`."
- [ ] **Step 4:** `go build ./... && go vet ./...`, then `make test` with a log in the scratchpad. Expected: green.
- [ ] **Step 5: Commit**, one commit: `build(deps): pin cyoda-go-spi to <pseudo-version>` (all four `go.mod`/`go.sum`
  and `COMPATIBILITY.md`).

---

### Task 19: verify, review, audit

- [ ] **Step 1:** `make preflight`, `make test` (full log to the scratchpad), `go vet ./...`, `make check-codegen`,
  `make todos` (nothing new). Expected: all green. If anything fails, fix it with TDD and run again.
- [ ] **Step 2:** `grep -rn "#628\|#651" --include='*.go' --include='*.md' --include='*.yaml' cmd/ internal/ api/ docs/cloud-parity/ README.md CHANGELOG.md`
  → empty. Specs and plans may carry issue numbers.
- [ ] **Step 3:** Push the branch. Push with an inline credential.
- [ ] **Step 4: Code review gate.** Dispatch a fresh-context reviewer subagent on the whole branch range
  `release/v0.9.0..HEAD`, with `superpowers:requesting-code-review`, on the Opus model. Give it the spec and plan
  paths. Act on its findings with `superpowers:receiving-code-review`.
- [ ] **Step 5: Security gate.** Run the project skill `cyoda-go-security-audit` over the whole PR range at the final
  head. Pay particular attention to these points:
  - audit `data` carries client-authored pre-check JSON and member reasons, which are capped;
  - the reason is reflected in a 400 body;
  - tenant scoping of the callouts is unchanged;
  - the hand-over payload.

  If either gate leads to a fix commit, run **both** gates again on the new head.

---

### Task 20: PR, CI, Cloud ticket

- [ ] **Step 1:** `gh pr create --base release/v0.9.0`. The body goes in a non-empty file that you check first. It
  covers the rules, the contract change, the SPI pin, the coverage matrix, and the two gates' results, and it ends
  with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. Do not put `Closes #628` in it, because #628
  is closed by hand on merge.
- [ ] **Step 2:** Wait for CI and read every check (`gh pr checks <n>`). Done means everything is green on the exact
  head. For a CI failure, fix it with TDD, push, and run both gates again if the fix touches code.
- [ ] **Step 3: Cloud's pre-check error behaviour.** In `~/dev/cyoda`, read `ExternalizedCriteriaChecker.kt` around
  `:63-70` and whatever evaluates `externalizedCriteriaCondition`. Establish, with `file:line`, what happens when the
  pre-check throws: read as false, or the error propagates. Do not file until this is settled.
- [ ] **Step 4:** File the CaaS ticket in Jira project CP, with a `[CaaS]` title prefix and component CaaS. Its
  content is §12, with the verified error behaviour and the cyoda-go PR link. Ask Cloud to adopt §2, §3 and §8.

---

### Task 21: close out

- [ ] **Step 1:** Update #651: the row-7 status cell (PR link, "in review"), and an Adjustments line with the rulings
  made in this item:
  - Rules 1–4;
  - the audit `functions`;
  - the manual message rule;
  - the CaaS ticket key.
- [ ] **Step 2:** After merge: close #628 by hand, set the #651 row to done, and remove the worktree and the branch.
  Update the memory note "v0.9.0 rollout tracker" and add a project memory for this item, marked DONE/MERGED with the
  merge SHA and the CaaS key.
- [ ] **Step 3:** Stop. Paul clears context before the next row.
