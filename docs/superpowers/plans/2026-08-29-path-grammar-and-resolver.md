# Path grammar and single resolver — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the engine honour one path grammar and one path resolver, so a
predicate answers the same on every backend and on every execution route.

**Architecture:** The plugin-facing filter path gains the bracket subscript, so
the producer's knowledge that a segment is an array index survives to the
backend. Path resolution becomes syntax-driven — the path says what it
addresses, the stored shape never decides — and moves to one exported SPI
function that both the kernel and the in-process evaluator call. The `array`
condition clause stops being a second addressing mechanism and is read as an
`AND` of positional comparisons.

**Tech Stack:** Go 1.26, `github.com/cyoda-platform/cyoda-go-spi` (local checkout
at `../cyoda-go-spi`, wired through `go.work`), gjson, SQLite `json_extract`,
PostgreSQL JSONB operators, testcontainers-go.

**Spec:** `docs/cloud-parity/path-grammar.md` and
`docs/cloud-parity/operator-semantics.md`. Both are on this branch already. They
state the target contract; this plan makes the code meet it. Read both before
starting — every task argues from a numbered section of one of them.

## Global Constraints

- **The spec on this branch is the contract.** Do not check behaviour against
  `origin/main`'s documents or against the deleted `condition-jsonpath-grammar.md`
  family. They are superseded.
- **Documents and code land in one PR.** No commit is expected to leave the repo
  self-consistent on its own; the branch is.
- **TDD, per `.claude/rules/tdd.md`.** A test that cannot fail is not coverage:
  after each test goes green, revert the production hunk and confirm the test
  fails, then restore. Two rounds of review on this rollout have found tests that
  could not fail.
- **SPI work goes in `../cyoda-go-spi`** on its own branch, consumed locally
  through `go.work`. `go.work` is tracked — never `git add -A`, and never commit
  a `replace` directive. The pin bump is Task 18.
- **No issue numbers** in code, comments, errors, logs, OpenAPI or help text.
- **`make repin-plugins`** must be re-run whenever plugin code changes.
- Run `go test ./e2e/parity/...` explicitly — those suites skip under `-short`,
  and `ok ... 1.9s` means they skipped.
- `make race` once before the PR, not per task.

## Scope

**In:** the path grammar in the SPI filter and both SQL dialects; the single
syntax-driven resolver; the `array` clause; vacuity conformance; four unrelated
defects the design review surfaced (Tasks 14–17).

**Out:** the array-wildcard quantifier and the `NOT` node. A `[*]` leaf becomes
resolvable and validated here, but stays **non-pushable** — a plugin leaves it in
the residual. Making it pushable needs a new filter node, which is a separate
change.

## File structure

| File | Responsibility |
|---|---|
| `../cyoda-go-spi/filter_path.go` *(new)* | The filter-path model: parse into hops, one definition of a well-formed subscript, one validator. The single source both plugins and the resolver consult. |
| `../cyoda-go-spi/filter_path_resolve.go` *(new)* | `ResolvePath` — the one path resolver. Syntax-driven. |
| `../cyoda-go-spi/prepared_filter.go` | `preparedNode` resolves through `ResolvePath` and applies a leaf existentially over the addressed values. |
| `../cyoda-go-spi/condition_filter.go` | `simpleToFilter` keeps well-formed subscripts; `arrayToFilter` is deleted and replaced by desugaring. |
| `../cyoda-go-spi/filter.go` | `Filter.Path` grammar doc. |
| `internal/match/match.go` | `convertJSONPath` and `rewriteSubscripts` deleted; leaf resolution delegates to `spi.ResolvePath`. |
| `internal/match/prepared.go` | `prepArray` deleted. |
| `internal/domain/search/condition_type_validate.go` | The `array` clause gets the checks a `simple` clause has. |
| `internal/domain/search/operators.go` | `array` clause path rule; unknown-operator error class. |
| `internal/domain/search/sortparam.go` | Second sort-path scanner deleted. |
| `plugins/sqlite/query_planner.go`, `plugins/sqlite/path_validation.go` | Bracket rendering; validator delegates to the SPI; `[*]` non-pushable. |
| `plugins/postgres/query_planner.go`, `plugins/postgres/path_validation.go` | The same, in the PostgreSQL dialect. |
| `plugins/memory/path_validation.go` | Validator delegates to the SPI. |
| `e2e/parity/path_addressing.go` *(new)* | Cross-backend scenarios for the addressing rules. |

---

## Task 1: The filter-path model

**Files:**
- Create: `../cyoda-go-spi/filter_path.go`
- Test: `../cyoda-go-spi/filter_path_test.go`

**Interfaces:**
- Produces: `type PathHop struct { Name string; Subs []PathSub }`,
  `type PathSub struct { Wildcard bool; Index int }`,
  `func ParseFilterPath(p string) ([]PathHop, error)`,
  `func ValidateFilterPath(p string) error`.
  `ValidateFilterPath` wraps `ErrInvalidFilterPath` on failure. An empty path
  parses to a nil hop slice and is valid — tree operators carry one.

- [ ] **Step 1: Write the failing test**

```go
package spi

import (
	"errors"
	"testing"
)

func TestParseFilterPath_Accepts(t *testing.T) {
	cases := []struct {
		path string
		want []PathHop
	}{
		{"", nil},
		{"amount", []PathHop{{Name: "amount"}}},
		{"obj.0", []PathHop{{Name: "obj"}, {Name: "0"}}},
		{"tags[0]", []PathHop{{Name: "tags", Subs: []PathSub{{Index: 0}}}}},
		{"tags[*]", []PathHop{{Name: "tags", Subs: []PathSub{{Wildcard: true}}}}},
		{"matrix[*][1]", []PathHop{{Name: "matrix", Subs: []PathSub{{Wildcard: true}, {Index: 1}}}}},
		{"orders[*].lines[*].sku", []PathHop{
			{Name: "orders", Subs: []PathSub{{Wildcard: true}}},
			{Name: "lines", Subs: []PathSub{{Wildcard: true}}},
			{Name: "sku"},
		}},
	}
	for _, tc := range cases {
		got, err := ParseFilterPath(tc.path)
		if err != nil {
			t.Errorf("ParseFilterPath(%q): unexpected error %v", tc.path, err)
			continue
		}
		if !equalHops(got, tc.want) {
			t.Errorf("ParseFilterPath(%q) = %+v, want %+v", tc.path, got, tc.want)
		}
	}
}

func TestParseFilterPath_Rejects(t *testing.T) {
	// Every one of these must wrap ErrInvalidFilterPath: a backend that
	// interpolates a path into SQL relies on this rejection as its injection
	// guard, so a "merely unparseable" classification is not enough.
	bad := []string{
		".a", "a.", "a..b", "$.a", "a b", "a;DROP", "a/etc", "a\\b", "a|b",
		"a[", "a[0", "a]", "a].b", "[0]", "a[]", "a[-1]", "a[+1]", "a[1e2]",
		"a[0:2]", "a[0,1]", "a[?(@.x)]", "a[ 0]", "a[0 ]", "a[0]b", "a[0];DROP",
		"a[x]", "a[*]..b", "a[*].", "a'b", "a\"b", "aé",
	}
	for _, p := range bad {
		if err := ValidateFilterPath(p); err == nil {
			t.Errorf("ValidateFilterPath(%q): want error, got nil", p)
		} else if !errors.Is(err, ErrInvalidFilterPath) {
			t.Errorf("ValidateFilterPath(%q): error does not wrap ErrInvalidFilterPath: %v", p, err)
		}
	}
}

func equalHops(a, b []PathHop) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || len(a[i].Subs) != len(b[i].Subs) {
			return false
		}
		for j := range a[i].Subs {
			if a[i].Subs[j] != b[i].Subs[j] {
				return false
			}
		}
	}
	return true
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../cyoda-go-spi && go test ./ -run TestParseFilterPath -v`
Expected: FAIL — `undefined: ParseFilterPath`.

- [ ] **Step 3: Implement the model**

Write `filter_path.go`. `ParseFilterPath` scans the string once: a hop is a name
of `A-Za-z0-9_-` bytes followed by zero or more `[` … `]` groups whose body is
either `*` or a run of ASCII digits; hops are separated by a single `.`; a
trailing byte after a subscript that is not `.` or `[` is an error. Reject an
empty hop name, an empty path segment, and any byte outside the name set.
`ValidateFilterPath` calls `ParseFilterPath` and discards the hops.

Reuse the existing digit predicate rather than writing a second one — a second
spelling of "is this a well-formed index" is the drift the spec's section 10
forbids.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ../cyoda-go-spi && go test ./ -run TestParseFilterPath -v`
Expected: PASS.

- [ ] **Step 5: Confirm the test can fail**

Delete the `a[0]b` rejection branch, re-run, confirm FAIL, restore.

- [ ] **Step 6: Commit**

```bash
cd ../cyoda-go-spi
git add filter_path.go filter_path_test.go
git commit -m "feat(filter): parse a filter path into hops, one subscript definition"
```

---

## Task 2: `Filter.Path` grammar admits the subscript

**Files:**
- Modify: `../cyoda-go-spi/filter.go:96-132` (the `Path` field's Grammar block)
- Test: `../cyoda-go-spi/filter_test.go`

**Interfaces:**
- Consumes: `ValidateFilterPath` from Task 1.
- Produces: nothing new — this task replaces prose that currently states the
  opposite rule.

- [ ] **Step 1: Write the failing test**

```go
func TestFilterPathGrammarDoc_MatchesValidator(t *testing.T) {
	// The godoc on Filter.Path is what a plugin author implements against.
	// It stated that subscripts are outside the grammar and that an array
	// position is spelled "tags.0". Both are now false, and a stale grammar
	// comment is how a backend ends up with a second, wrong validator.
	if err := ValidateFilterPath("tags[0]"); err != nil {
		t.Fatalf("tags[0] must be a legal filter path: %v", err)
	}
	if err := ValidateFilterPath("tags[*]"); err != nil {
		t.Fatalf("tags[*] must be a legal filter path: %v", err)
	}
	if err := ValidateFilterPath("obj.0"); err != nil {
		t.Fatalf("obj.0 must be a legal filter path (a field named 0): %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../cyoda-go-spi && go test ./ -run TestFilterPathGrammarDoc -v`
Expected: PASS already for the first two if Task 1 is in; the point of this task
is the doc. If it passes, proceed — the doc change is verified by Step 3's
review, not by an assertion.

- [ ] **Step 3: Replace the Grammar block**

In `filter.go`, replace the production and the sentence beginning "Bracketed
array subscripts and wildcards" with:

```
	// # Grammar
	//
	//	path      = segment ( "." segment )*
	//	segment   = name subscript*
	//	name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
	//	subscript = "[" ( "*" / 1*DIGIT ) "]"
	//
	// This is the wire jsonPath grammar with the "$." leader removed. A
	// bracket is an array subscript; a dotted numeric segment is a field
	// whose name is that digit string. The two address different values and
	// a backend MUST NOT collapse them — see docs/cloud-parity/path-grammar.md.
	//
	// An EMPTY Path is legal and is not checked: tree operators (FilterAnd,
	// FilterOr) and any leaf that addresses no field carry one.
	//
	// Parse it with ParseFilterPath and validate it with ValidateFilterPath.
	// A second, independent spelling of the grammar is how a backend admits a
	// form no resolver serves.
```

Keep the "Rejection is mandatory" and injection-guard paragraphs; the guard
argument is unchanged, because digits and brackets cannot terminate a quoted
literal in either dialect.

- [ ] **Step 4: Run the SPI suite**

Run: `cd ../cyoda-go-spi && go test ./... 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ../cyoda-go-spi
git add filter.go filter_test.go
git commit -m "docs(filter): the filter path carries the array subscript"
```

---

## Task 3: The one path resolver

**Files:**
- Create: `../cyoda-go-spi/filter_path_resolve.go`
- Test: `../cyoda-go-spi/filter_path_resolve_test.go`

**Interfaces:**
- Consumes: `ParseFilterPath`, `PathHop`, `PathSub` from Task 1.
- Produces: `func ResolvePath(data []byte, hops []PathHop) []gjson.Result`.
  Returns the values the path addresses, in document order. A bare hop
  contributes exactly one result — the value at that key, whatever its shape,
  **never unwrapped**. A `[N]` hop contributes exactly one result, the element at
  that index, non-existent when the value is not an array or the index is past
  the end. A `[*]` hop contributes one result per element and **none** when the
  value is not an array. A missing key contributes one non-existent result, so
  that a presence test can see it.

This function is the whole of the spec's section 10: it addresses what the path
says and never inspects the stored shape to decide what the path meant.

- [ ] **Step 1: Write the failing test**

```go
package spi

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResolvePath(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		path string
		want []string // gjson .String() of each addressed value; "<absent>" for a non-existent one
	}{
		// A bare path addresses the value, never the elements. This is the
		// rule the previous evaluator broke: it unwrapped an array and
		// compared element-wise, so $.a EQUALS "A" matched ["A","B"].
		{"bare over scalar", `{"a":"A"}`, "a", []string{"A"}},
		{"bare over array", `{"a":["A","B"]}`, "a", []string{`["A","B"]`}},
		{"bare over empty array", `{"a":[]}`, "a", []string{`[]`}},
		{"bare over absent", `{}`, "a", []string{"<absent>"}},

		// A wildcard addresses the elements, never the array, and never
		// wraps a scalar into a one-element sequence.
		{"wildcard over array", `{"a":["A","B"]}`, "a[*]", []string{"A", "B"}},
		{"wildcard over empty array", `{"a":[]}`, "a[*]", nil},
		{"wildcard over scalar", `{"a":"A"}`, "a[*]", nil},
		{"wildcard over null", `{"a":null}`, "a[*]", nil},
		{"wildcard over absent", `{}`, "a[*]", nil},

		// A positional subscript addresses one position, which may be absent.
		{"index present", `{"a":["A","B"]}`, "a[0]", []string{"A"}},
		{"index second", `{"a":["A","B"]}`, "a[1]", []string{"B"}},
		{"index past end", `{"a":["A"]}`, "a[3]", []string{"<absent>"}},
		{"index over empty array", `{"a":[]}`, "a[0]", []string{"<absent>"}},
		{"index over scalar", `{"a":"A"}`, "a[0]", []string{"<absent>"}},

		// A dotted numeric segment is a field name, not an index.
		{"numeric field name", `{"obj":{"0":"Z"}}`, "obj.0", []string{"Z"}},
		{"numeric segment is not an index", `{"tags":["A"]}`, "tags.0", []string{"<absent>"}},

		// Nested hops flatten; an element missing the key contributes an
		// absent value rather than being dropped, so IS_NULL can see it.
		{"nested wildcard", `{"items":[{"sku":"A"},{"sku":"B"}]}`, "items[*].sku", []string{"A", "B"}},
		{"element missing key", `{"items":[{"sku":"A"},{}]}`, "items[*].sku", []string{"A", "<absent>"}},
		{"two hops", `{"o":[{"l":[{"s":"A"},{"s":"B"}]},{"l":[{"s":"C"}]}]}`, "o[*].l[*].s", []string{"A", "B", "C"}},
		{"chained subscripts", `{"m":[["A","B"],["C"]]}`, "m[*][*]", []string{"A", "B", "C"}},
		{"chained index", `{"m":[["A","B"],["C"]]}`, "m[0][1]", []string{"B"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hops, err := ParseFilterPath(tc.path)
			if err != nil {
				t.Fatalf("ParseFilterPath(%q): %v", tc.path, err)
			}
			got := ResolvePath([]byte(tc.doc), hops)
			var gotS []string
			for _, r := range got {
				if !r.Exists() {
					gotS = append(gotS, "<absent>")
					continue
				}
				gotS = append(gotS, resultText(r))
			}
			if len(gotS) != len(tc.want) {
				t.Fatalf("ResolvePath(%s, %q) = %v, want %v", tc.doc, tc.path, gotS, tc.want)
			}
			for i := range gotS {
				if gotS[i] != tc.want[i] {
					t.Fatalf("ResolvePath(%s, %q) = %v, want %v", tc.doc, tc.path, gotS, tc.want)
				}
			}
		})
	}
}

func resultText(r gjson.Result) string {
	if r.IsArray() || r.IsObject() {
		return r.Raw
	}
	return r.String()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../cyoda-go-spi && go test ./ -run TestResolvePath -v`
Expected: FAIL — `undefined: ResolvePath`.

- [ ] **Step 3: Implement**

Walk the hops over a working set of `gjson.Result`, starting from
`gjson.ParseBytes(data)`. For each hop: for every result in the set, take
`r.Get(hop.Name)` — use `gjson.Result.Get`, not a joined path string, so a name
that is all digits cannot be read as an index. Then apply each subscript in
order: `[*]` replaces the value with its elements via `ForEach`, contributing
nothing when `!IsArray()`; `[N]` replaces it with `r.Array()[N]` when
`IsArray()` and `N` is in range, and with a zero `gjson.Result` otherwise.

A hop applied to a non-existent result yields a non-existent result — do not drop
it. Dropping it is what made an element without the key invisible to `IS_NULL`.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ../cyoda-go-spi && go test ./ -run TestResolvePath -v`
Expected: PASS, all 20 subtests.

- [ ] **Step 5: Confirm the test can fail**

Change the `[*]` arm to also emit the scalar when `!IsArray()` (the `lax`
behaviour the spec forbids). Confirm `wildcard over scalar` FAILS. Restore.

- [ ] **Step 6: Commit**

```bash
cd ../cyoda-go-spi
git add filter_path_resolve.go filter_path_resolve_test.go
git commit -m "feat(filter): one syntax-driven path resolver"
```

---

## Task 4: The kernel resolves through `ResolvePath`

**Files:**
- Modify: `../cyoda-go-spi/prepared_filter.go:80` (`prepareNode`), `:121-143`
  (`stored`), and `:110-118` (the leaf arm of `match`)
- Test: `../cyoda-go-spi/prepared_filter_test.go`

**Interfaces:**
- Consumes: `ParseFilterPath`, `ResolvePath`.
- Produces: `preparedNode` gains `hops []PathHop`, parsed once in `prepareNode`.
  A leaf holds when **some** addressed value satisfies it. A leaf addressing no
  values is a non-match for every operator, presence tests included.

- [ ] **Step 1: Write the failing test**

```go
func TestPreparedFilter_ResolvesByPathSyntax(t *testing.T) {
	str := []DataType{String}
	cases := []struct {
		name string
		doc  string
		f    Filter
		want bool
	}{
		// A bare path does not unwrap an array.
		{"bare eq over array", `{"a":["A","B"]}`,
			Filter{Op: FilterEq, Path: "a", Source: SourceData, Value: "A", Declared: str}, false},
		{"bare eq over scalar", `{"a":"A"}`,
			Filter{Op: FilterEq, Path: "a", Source: SourceData, Value: "A", Declared: str}, true},

		// A wildcard is existential over the elements and does not wrap a scalar.
		{"wildcard eq over array", `{"a":["A","B"]}`,
			Filter{Op: FilterEq, Path: "a[*]", Source: SourceData, Value: "B", Declared: str}, true},
		{"wildcard eq over scalar", `{"a":"A"}`,
			Filter{Op: FilterEq, Path: "a[*]", Source: SourceData, Value: "A", Declared: str}, false},

		// A trailing wildcard is not the array's length.
		{"wildcard is not length", `{"tags":["red","blue"]}`,
			Filter{Op: FilterEq, Path: "tags[*]", Source: SourceData, Value: "2", Declared: []DataType{Integer}}, false},

		// Vacuity, per path-grammar.md section 5.
		{"bare NOT_NULL over empty array", `{"a":[]}`,
			Filter{Op: FilterNotNull, Path: "a", Source: SourceData}, true},
		{"wildcard NOT_NULL over empty array", `{"a":[]}`,
			Filter{Op: FilterNotNull, Path: "a[*]", Source: SourceData}, false},
		{"wildcard IS_NULL over empty array", `{"a":[]}`,
			Filter{Op: FilterIsNull, Path: "a[*]", Source: SourceData}, false},
		{"wildcard IS_NULL over null", `{"a":null}`,
			Filter{Op: FilterIsNull, Path: "a[*]", Source: SourceData}, false},
		{"wildcard IS_NULL over absent", `{}`,
			Filter{Op: FilterIsNull, Path: "a[*]", Source: SourceData}, false},
		{"positional IS_NULL over empty array", `{"a":[]}`,
			Filter{Op: FilterIsNull, Path: "a[0]", Source: SourceData}, true},

		// An element missing the key is evaluated, not dropped.
		{"element missing key IS_NULL", `{"items":[{"sku":"A"},{}]}`,
			Filter{Op: FilterIsNull, Path: "items[*].sku", Source: SourceData}, true},

		// A numeric segment is a field name.
		{"numeric field name", `{"obj":{"0":"Z"}}`,
			Filter{Op: FilterEq, Path: "obj.0", Source: SourceData, Value: "Z", Declared: str}, true},
		{"numeric segment is not an index", `{"tags":["A"]}`,
			Filter{Op: FilterEq, Path: "tags.0", Source: SourceData, Value: "A", Declared: str}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Prepare(tc.f).Match([]byte(tc.doc), EntityMeta{}); got != tc.want {
				t.Errorf("Match(%s) on %+v = %v, want %v", tc.doc, tc.f, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../cyoda-go-spi && go test ./ -run TestPreparedFilter_ResolvesByPathSyntax -v`
Expected: FAIL on the wildcard cases (`a[*]` reaches gjson as a literal key) and
on `bare NOT_NULL over empty array`.

- [ ] **Step 3: Implement**

In `prepareNode`, parse the path once for a `SourceData` leaf and store the hops.
A parse failure marks the node unexpanded, which is already a non-match — a
malformed path must never resolve.

Replace `stored` with a `storedAll` returning `[]gjson.Result`: `SourceMeta` keeps
its single bridged result; `SourceData` returns `ResolvePath(data, n.hops)`.

In the leaf arm of `match`, loop the results and return true on the first
`EvalLeaf` that holds. An empty slice returns false.

Leave the `SourceMeta` temporal divergence comment where it is — it documents a
predicate being withdrawn elsewhere and is not in this change's scope.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ../cyoda-go-spi && go test ./ -run TestPreparedFilter -v`
Expected: PASS.

- [ ] **Step 5: Run the whole SPI suite**

Run: `cd ../cyoda-go-spi && go test ./... 2>&1 | tail -30`
Expected: PASS. `prepared_filter_equivalence_test.go` compares this evaluator
against a frozen copy — if it fails, the frozen copy encodes the old
data-driven behaviour and must be updated to the new contract in the same
commit, with the diff explained in the commit message.

- [ ] **Step 6: Commit**

```bash
cd ../cyoda-go-spi
git add prepared_filter.go prepared_filter_test.go prepared_filter_equivalence_test.go
git commit -m "fix(filter)!: the kernel resolves a path by its syntax, not by the stored shape"
```

---

## Task 5: `ConditionToFilter` keeps the subscript

**Files:**
- Modify: `../cyoda-go-spi/condition_filter.go:170-190` (`simpleToFilter`),
  `:425-540` (`stripDollarDot`, `scanWirePathBody`)
- Test: `../cyoda-go-spi/condition_filter_test.go`

**Interfaces:**
- Produces: `simpleToFilter` no longer fails on a well-formed subscript. A
  malformed one still wraps `ErrInvalidFilterPath`. The "well-formed subscript is
  not pushdownable" error class is gone from this function.

- [ ] **Step 1: Write the failing test**

```go
func TestSimpleToFilter_KeepsWellFormedSubscript(t *testing.T) {
	fields := map[string]FieldDescriptor{
		"$.tags[*]": {Types: []DataType{String}},
	}
	for _, tc := range []struct{ in, wantPath string }{
		{"$.tags[0]", "tags[0]"},
		{"$.tags[*]", "tags[*]"},
		{"$.items[*].sku", "items[*].sku"},
	} {
		f, err := ConditionToFilter(&predicate.SimpleCondition{
			JsonPath: tc.in, OperatorType: "EQUALS", Value: "A",
		}, fields)
		if err != nil {
			t.Fatalf("ConditionToFilter(%q): unexpected error %v", tc.in, err)
		}
		if f.Path != tc.wantPath {
			t.Errorf("ConditionToFilter(%q).Path = %q, want %q", tc.in, f.Path, tc.wantPath)
		}
	}
}

func TestSimpleToFilter_StillRejectsMalformedSubscript(t *testing.T) {
	for _, p := range []string{"$.a[-1]", "$.a[0:2]", "$.a[?(@.x)]", "$.a[", "$.a[0]b"} {
		_, err := ConditionToFilter(&predicate.SimpleCondition{
			JsonPath: p, OperatorType: "EQUALS", Value: "A",
		}, nil)
		if !errors.Is(err, ErrInvalidFilterPath) {
			t.Errorf("ConditionToFilter(%q): want ErrInvalidFilterPath, got %v", p, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../cyoda-go-spi && go test ./ -run TestSimpleToFilter -v`
Expected: FAIL — the first test errors with "contains non-pushdownable
array-subscript syntax".

- [ ] **Step 3: Implement**

Delete the arm of `stripDollarDot` that returns the plain non-sentinel error for
a well-formed subscript. Keep every malformed-bracket rejection. The remaining
plain-error class is now only the untranslatable condition **types** (function
conditions), which the engine still reads as "evaluate in process".

Update `ConditionToFilter`'s godoc: the paragraph beginning "A WELL-FORMED
array-subscripted path" is now wrong and must state the new rule.

- [ ] **Step 4: Run tests**

Run: `cd ../cyoda-go-spi && go test ./... 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ../cyoda-go-spi
git add condition_filter.go condition_filter_test.go
git commit -m "feat(filter): a well-formed subscript translates instead of falling back"
```

---

## Task 6: The `array` clause becomes positional comparisons

**Files:**
- Modify: `../cyoda-go-spi/condition_filter.go:295-350` (delete `arrayToFilter`
  and `arrayElementPath`; add `DesugarCondition`)
- Test: `../cyoda-go-spi/condition_filter_test.go`

**Interfaces:**
- Produces: `func DesugarCondition(c predicate.Condition) predicate.Condition`.
  It rewrites every `*predicate.ArrayCondition` in the tree into a
  `*predicate.GroupCondition{Operator: "AND"}` of `*predicate.SimpleCondition`
  leaves with `EQUALS` and a `[N]` path, skipping null positions. All-null
  yields an empty `AND`, which is a tautology. Every other clause is returned
  unchanged. `ConditionToFilter` calls it first, so no evaluator sees an
  `ArrayCondition`.

The clause's `jsonPath` carries a trailing `[*]` per spec section 8, so
`$.tags[*]` with index `i` desugars to `$.tags[i]`.

- [ ] **Step 1: Write the failing test**

```go
func TestDesugarCondition_ArrayClause(t *testing.T) {
	got := DesugarCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]",
		Values:   []any{"A", nil, "C"},
	})
	g, ok := got.(*predicate.GroupCondition)
	if !ok {
		t.Fatalf("want *GroupCondition, got %T", got)
	}
	if g.Operator != "AND" || len(g.Conditions) != 2 {
		t.Fatalf("want AND of 2, got %q of %d", g.Operator, len(g.Conditions))
	}
	want := []struct{ path, value string }{{"$.tags[0]", "A"}, {"$.tags[2]", "C"}}
	for i, w := range want {
		s, ok := g.Conditions[i].(*predicate.SimpleCondition)
		if !ok {
			t.Fatalf("child %d: want *SimpleCondition, got %T", i, g.Conditions[i])
		}
		if s.JsonPath != w.path || s.OperatorType != "EQUALS" || s.Value != w.value {
			t.Errorf("child %d = {%q,%q,%v}, want {%q,EQUALS,%q}",
				i, s.JsonPath, s.OperatorType, s.Value, w.path, w.value)
		}
	}
}

func TestDesugarCondition_AllNullIsTautology(t *testing.T) {
	got := DesugarCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{nil, nil},
	})
	g, ok := got.(*predicate.GroupCondition)
	if !ok || g.Operator != "AND" || len(g.Conditions) != 0 {
		t.Fatalf("want an empty AND, got %#v", got)
	}
}

func TestDesugarCondition_RecursesIntoGroups(t *testing.T) {
	got := DesugarCondition(&predicate.GroupCondition{
		Operator: "OR",
		Conditions: []predicate.Condition{
			&predicate.ArrayCondition{JsonPath: "$.tags[*]", Values: []any{"A"}},
		},
	})
	g := got.(*predicate.GroupCondition)
	if _, isArray := g.Conditions[0].(*predicate.ArrayCondition); isArray {
		t.Fatal("nested ArrayCondition was not desugared")
	}
}

func TestConditionToFilter_ArrayClauseProducesBracketPaths(t *testing.T) {
	// The defect this closes: the positional leaf used to carry the dotted
	// path "tags.0", which both SQL dialects read as a field named "0", so
	// the row was dropped by the WHERE clause and no residual could recover it.
	f, err := ConditionToFilter(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{"A"},
	}, map[string]FieldDescriptor{"$.tags[*]": {Types: []DataType{String}}})
	if err != nil {
		t.Fatalf("ConditionToFilter: %v", err)
	}
	if f.Path != "tags[0]" {
		t.Errorf("Path = %q, want %q", f.Path, "tags[0]")
	}
	if len(f.Declared) != 1 || f.Declared[0] != String {
		t.Errorf("Declared = %v, want [String]", f.Declared)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ../cyoda-go-spi && go test ./ -run 'TestDesugarCondition|TestConditionToFilter_ArrayClause' -v`
Expected: FAIL — `undefined: DesugarCondition`.

- [ ] **Step 3: Implement**

Add `DesugarCondition`. Delete `arrayToFilter` and `arrayElementPath` and the
`*predicate.ArrayCondition` arm of `ConditionToFilter`'s type switch — after
desugaring, that arm is unreachable, and leaving it would be a second definition
of what the clause means.

`ConditionToFilter` calls `DesugarCondition(cond)` as its first statement.

The declared types now resolve through `simpleToFilter`'s ordinary lookup:
`NormalisePath("$.tags[0]")` is `$.tags[0]`, which misses the fields map, so
`simpleToFilter` must canonicalise a positional subscript to the wildcard key
before the lookup. Add that fold — it is the "wire → model key" map of spec
section 10, and it must be the only one in the SPI.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ../cyoda-go-spi && go test ./... 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 5: Confirm the test can fail**

Change the desugared path back to `fmt.Sprintf("%s.%d", …)`. Confirm
`TestConditionToFilter_ArrayClauseProducesBracketPaths` FAILS. Restore.

- [ ] **Step 6: Commit**

```bash
cd ../cyoda-go-spi
git add condition_filter.go condition_filter_test.go
git commit -m "fix(filter)!: an array clause is an AND of positional comparisons"
```

---

## Task 7: Wire the local SPI into the engine build

**Files:**
- Modify: `go.work` (local only — never commit the `use` line)

- [ ] **Step 1: Add the local SPI to the workspace**

```bash
go work edit -use ../cyoda-go-spi
go build ./... && go vet ./...
```

- [ ] **Step 2: Record what breaks**

Run: `go test ./... -short 2>&1 | grep -E '^(FAIL|---)' | head -40`

Expected failures, all of which later tasks fix: `internal/match` (its own
resolver still disagrees), the three plugin path validators (they reject `[`),
and any search test asserting that a subscripted path falls back.

Do not fix them here. This task only establishes the build.

- [ ] **Step 3: Confirm `go.work` is not staged**

```bash
git status --short go.work
```

Expected: `go.work` shows as modified and stays unstaged for every commit in
this plan.

---

## Task 8: `internal/match` delegates to the one resolver

**Files:**
- Modify: `internal/match/match.go:64-160` (delete `convertJSONPath` and
  `rewriteSubscripts`), `internal/match/prepared.go:110-130` (delete the
  `ArrayCondition` arm), `:249-271` (delete `prepareArray`), `:296-330` (the leaf
  and array arms of `match`)
- Delete: `internal/match/jsonpath_subscript_test.go` assertions that pin
  `convertJSONPath`'s output; the behaviour they pin now lives in the SPI
- Test: `internal/match/resolver_parity_test.go` *(new)*

**Interfaces:**
- Consumes: `spi.ParseFilterPath`, `spi.ResolvePath`, `spi.DesugarCondition`.
- Produces: `prepNode` holds `hops []spi.PathHop` instead of `gjsonPath string`.
  `prepArray` no longer exists.

- [ ] **Step 1: Write the failing test**

```go
package match

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// The two evaluators must answer identically. They are reached by different
// routes for the same request — a backend's residual re-check runs the SPI
// kernel, an untranslatable condition and every workflow criterion run this
// one — so a disagreement is a request answering differently depending on its
// query plan.
func TestEvaluatorsAgree(t *testing.T) {
	str := []spi.DataType{spi.String}
	fieldTypes := func(string) []spi.DataType { return str }

	docs := []string{
		`{"a":"A"}`, `{"a":["A","B"]}`, `{"a":[]}`, `{"a":null}`, `{}`,
		`{"obj":{"0":"Z"}}`, `{"items":[{"sku":"A"},{}]}`,
	}
	paths := []string{"$.a", "$.a[*]", "$.a[0]", "$.obj.0", "$.items[*].sku"}
	ops := []string{"EQUALS", "NOT_EQUAL", "CONTAINS", "IS_NULL", "NOT_NULL"}

	for _, doc := range docs {
		for _, p := range paths {
			for _, op := range ops {
				cond := &predicate.SimpleCondition{JsonPath: p, OperatorType: op, Value: "A"}

				prep, err := Prepare(cond, fieldTypes)
				if err != nil {
					t.Fatalf("match.Prepare(%s %s): %v", p, op, err)
				}
				gotMatch := prep.Match([]byte(doc), spi.EntityMeta{})

				f, err := spi.ConditionToFilter(cond, map[string]spi.FieldDescriptor{
					"$.a[*]": {Types: str}, "$.a": {Types: str},
					"$.obj.0": {Types: str}, "$.items[*].sku": {Types: str},
				})
				if err != nil {
					t.Fatalf("spi.ConditionToFilter(%s %s): %v", p, op, err)
				}
				gotKernel := spi.Prepare(f).Match([]byte(doc), spi.EntityMeta{})

				if gotMatch != gotKernel {
					t.Errorf("doc=%s path=%s op=%s: match=%v kernel=%v",
						doc, p, op, gotMatch, gotKernel)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/match/ -run TestEvaluatorsAgree -v`
Expected: FAIL on many combinations — `internal/match` unwraps arrays on a bare
path and the kernel does not.

- [ ] **Step 3: Implement**

In `prepareSimple`, parse the path with `spi.ParseFilterPath` after stripping the
`$.` leader, and store the hops on the node. In `prepLeaf`'s match arm, call
`spi.ResolvePath` and apply the expansion existentially — delete the
`result.IsArray()` branch entirely; that branch is the data-driven routing the
spec forbids.

In `prepare`, call `spi.DesugarCondition` on the incoming condition first, then
delete the `*predicate.ArrayCondition` arm and `prepareArray`.

Delete `convertJSONPath`, `rewriteSubscripts`, `wildcardSubscript` and
`arrayElementFieldPath`. Any caller of `convertJSONPath` outside this file must
move to `spi.ParseFilterPath` — grep for it before deleting.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/match/... -v 2>&1 | tail -40`
Expected: PASS. Tests pinning the old data-driven behaviour will fail; each one
must be read and either updated to the spec's rule or deleted with a one-line
reason in the commit message. Do not "fix" a test by weakening its assertion.

- [ ] **Step 5: Confirm the test can fail**

Restore the `result.IsArray()` branch. Confirm `TestEvaluatorsAgree` FAILS.
Remove it again.

- [ ] **Step 6: Commit**

```bash
git add internal/match/
git commit -m "fix(match)!: one path resolver, shared with the kernel"
```

---

## Task 9: sqlite renders and validates the subscript

**Files:**
- Modify: `plugins/sqlite/path_validation.go:36-81` (`validateJSONPath`),
  `plugins/sqlite/query_planner.go:437-452` (`fieldExpr`), and
  `isLeafPushable` (add the wildcard refusal)
- Test: `plugins/sqlite/query_planner_test.go`, `plugins/sqlite/path_validation_test.go`

**Interfaces:**
- Consumes: `spi.ValidateFilterPath`, `spi.ParseFilterPath`.
- Produces: `fieldExpr` renders a `[N]` hop as SQLite's bracket index. A filter
  whose path contains `[*]` is **not pushable** and goes to the residual.

- [ ] **Step 1: Write the failing test**

```go
func TestFieldExpr_RendersSubscript(t *testing.T) {
	cases := []struct{ path, want string }{
		{"amount", "json_extract(data, '$.amount')"},
		{"obj.0", "json_extract(data, '$.obj.0')"},
		{"tags[0]", "json_extract(data, '$.tags[0]')"},
		{"items[2].sku", "json_extract(data, '$.items[2].sku')"},
	}
	for _, tc := range cases {
		got := fieldExpr(spi.Filter{Source: spi.SourceData, Path: tc.path})
		if got != tc.want {
			t.Errorf("fieldExpr(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestPlanQuery_WildcardIsResidual(t *testing.T) {
	// A wildcard leaf has no SQL form until the quantifier node exists.
	// Pushing it as a scalar comparison would drop every matching row, and a
	// narrowing WHERE cannot be recovered by the residual re-check.
	plan := planFor(spi.Filter{
		Op: spi.FilterEq, Path: "tags[*]", Source: spi.SourceData,
		Value: "A", Declared: []spi.DataType{spi.String},
	})
	if plan.where != "" {
		t.Errorf("wildcard leaf must not narrow; got WHERE %q", plan.where)
	}
	if plan.postFilter == nil {
		t.Error("wildcard leaf must be installed as a residual")
	}
}

func TestPlanQuery_PositionalIsPushed(t *testing.T) {
	plan := planFor(spi.Filter{
		Op: spi.FilterEq, Path: "tags[0]", Source: spi.SourceData,
		Value: "A", Declared: []spi.DataType{spi.String},
	})
	if !strings.Contains(plan.where, "'$.tags[0]'") {
		t.Errorf("positional leaf must push its dialect index; got WHERE %q", plan.where)
	}
}

func TestValidateJSONPath_AcceptsSubscripts(t *testing.T) {
	for _, p := range []string{"tags[0]", "tags[*]", "items[*].sku", "obj.0", "m[0][1]"} {
		if err := validateJSONPath(p); err != nil {
			t.Errorf("validateJSONPath(%q): unexpected error %v", p, err)
		}
	}
	for _, p := range []string{"a'b", "a;DROP", "a[-1]", "a[0:2]", "a[", "a[0]b"} {
		if err := validateJSONPath(p); err == nil {
			t.Errorf("validateJSONPath(%q): want rejection", p)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugins/sqlite/ -run 'TestFieldExpr_RendersSubscript|TestPlanQuery_Wildcard|TestPlanQuery_Positional|TestValidateJSONPath_Accepts' -v`
Expected: FAIL — the validator rejects `[`, and `fieldExpr` emits the path
verbatim, which happens to be right for SQLite once the validator lets it
through; the wildcard and residual assertions fail.

- [ ] **Step 3: Implement**

Replace the body of `validateJSONPath` with a call to `spi.ValidateFilterPath`,
keeping the plugin's own sentinel wrapping so `ErrInvalidFilterPath`
classification is unchanged. Deleting the local scanner is the point: two
scanners drift.

`fieldExpr` needs no change for SQLite once the path is admitted — SQLite's
`$.tags[0]` is the same spelling. Add a test comment saying so, so the next
reader does not "fix" it into a rewrite.

In `isLeafPushable`, return false when the path contains a wildcard hop. Use
`spi.ParseFilterPath` and check `PathSub.Wildcard`; do not string-match `"[*]"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugins/sqlite/... 2>&1 | tail -20`
Expected: PASS.

- [ ] **Step 5: Confirm the test can fail**

Make `isLeafPushable` accept a wildcard. Confirm `TestPlanQuery_WildcardIsResidual`
FAILS. Restore.

- [ ] **Step 6: Commit**

```bash
git add plugins/sqlite/
git commit -m "fix(sqlite): render an array index, and keep a wildcard out of the WHERE"
```

---

## Task 10: postgres renders and validates the subscript

**Files:**
- Modify: `plugins/postgres/path_validation.go:41-86` (`validateJSONPath`),
  `plugins/postgres/query_planner.go:373-412` (`fieldExpr`, `jsonbExtractText`,
  `jsonbExtractJSONB`), and `isLeafPushable`
- Test: `plugins/postgres/query_planner_test.go`, `plugins/postgres/path_validation_test.go`

**Interfaces:** as Task 9, in the PostgreSQL dialect. A `[N]` subscript renders
as an **integer** operand — `-> 0`, never `->> '0'`. A text key against a JSONB
array yields null, which is the defect this closes.

- [ ] **Step 1: Write the failing test**

```go
func TestJsonbExtract_RendersSubscript(t *testing.T) {
	cases := []struct{ path, want string }{
		{"amount", "doc->>'amount'"},
		{"obj.0", "doc->'obj'->>'0'"},
		{"tags[0]", "doc->'tags'->>0"},
		{"items[2].sku", "doc->'items'->2->>'sku'"},
		{"m[0][1]", "doc->'m'->0->>1"},
	}
	for _, tc := range cases {
		got := jsonbExtractText("doc", tc.path)
		if got != tc.want {
			t.Errorf("jsonbExtractText(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
```

Add the wildcard-residual, positional-pushed and validator tests from Task 9,
adapted to this plugin's helpers.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugins/postgres/ -run 'TestJsonbExtract_RendersSubscript' -v`
Expected: FAIL — `jsonbExtractText` splits on `.` only, so `tags[0]` takes the
single-segment branch and emits `doc->>'tags[0]'`, a lookup for a key literally
named `tags[0]`.

- [ ] **Step 3: Implement**

Rewrite `jsonbExtractText` and `jsonbExtractJSONB` over `spi.ParseFilterPath`'s
hops instead of `strings.Split(path, ".")`. Emit `->'name'` for a name hop and
`->N` for a positional subscript, with the final accessor as `->>`. An integer
subscript is written unquoted; a name is written single-quoted, and the grammar
guarantees it holds no quote.

Replace `validateJSONPath`'s body with `spi.ValidateFilterPath` plus the
plugin's sentinel wrapping.

In `isLeafPushable`, refuse a wildcard hop, as in Task 9.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugins/postgres/... 2>&1 | tail -20`
Expected: PASS (needs Docker).

- [ ] **Step 5: Confirm the test can fail**

Change `->N` back to `->>'N'`. Confirm `TestJsonbExtract_RendersSubscript` FAILS.
Restore.

- [ ] **Step 6: Commit**

```bash
git add plugins/postgres/
git commit -m "fix(postgres): an array index renders as an integer accessor, not a text key"
```

---

## Task 11: memory validates against the one grammar

**Files:**
- Modify: `plugins/memory/path_validation.go:55-135`
- Test: `plugins/memory/path_validation_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestValidateJSONPath_MatchesSPIGrammar(t *testing.T) {
	for _, p := range []string{"tags[0]", "tags[*]", "items[*].sku", "obj.0"} {
		if err := validateJSONPath(p); err != nil {
			t.Errorf("validateJSONPath(%q): unexpected error %v", p, err)
		}
	}
	for _, p := range []string{"a'b", "a;DROP", "a[-1]", "a["} {
		if err := validateJSONPath(p); err == nil {
			t.Errorf("validateJSONPath(%q): want rejection", p)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./plugins/memory/ -run TestValidateJSONPath_MatchesSPIGrammar -v`
Expected: FAIL — the local scanner rejects `[`.

- [ ] **Step 3: Implement**

Replace the body with `spi.ValidateFilterPath` plus the plugin's sentinel
wrapping, and delete the local scanner.

- [ ] **Step 4: Run tests, then `make repin-plugins`**

```bash
go test ./plugins/memory/... 2>&1 | tail -10
make repin-plugins
```

- [ ] **Step 5: Commit**

```bash
git add plugins/memory/ plugins/*/go.mod plugins/*/go.sum
git commit -m "fix(memory): validate against the one filter-path grammar"
```

---

## Task 12: The `array` clause is validated like a `simple` clause

**Files:**
- Modify: `internal/domain/search/operators.go:122-127` (the `ArrayCondition`
  arm), `internal/domain/search/condition_type_validate.go:76` (the arm that
  returns nil)
- Test: `internal/domain/search/array_condition_validate_test.go` *(new)*;
  update `internal/domain/search/array_condition_service_test.go:47`

**Interfaces:**
- Produces: an `array` clause whose `jsonPath` has no trailing `[*]` is
  `400 INVALID_FIELD_PATH`. Its `values` go through the same operand-shape check
  a `simple` clause's value does, and the same declared-type check.

- [ ] **Step 1: Write the failing test**

```go
func TestValidateCondition_ArrayClauseRequiresWildcard(t *testing.T) {
	// The clause tests elements by position, so its path must address
	// elements. A bare path addresses the array itself and cannot carry a
	// positional test — the spelling and the meaning disagreed.
	err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags", Values: []any{"A"},
	})
	if err == nil {
		t.Fatal("array clause on a bare path must be rejected")
	}
	if !errors.Is(err, ErrInvalidFieldPath) {
		t.Errorf("want ErrInvalidFieldPath, got %v", err)
	}
}

func TestValidateCondition_ArrayClauseAcceptsWildcard(t *testing.T) {
	if err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{"A", nil, "C"},
	}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateCondition_ArrayClauseRejectsObjectOperand(t *testing.T) {
	// Unchecked, an object operand reaches the kernel and is compared as the
	// literal text "map[a:1]".
	err := ValidateCondition(&predicate.ArrayCondition{
		JsonPath: "$.tags[*]", Values: []any{map[string]any{"a": 1}},
	})
	if err == nil {
		t.Fatal("array clause with an object operand must be rejected")
	}
}
```

For the type check, add a case to the existing table in
`condition_type_validate_test.go`: an `array` clause on an integer-declared
array with a string value must answer `CONDITION_TYPE_MISMATCH`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/domain/search/ -run 'ArrayClause' -v`
Expected: FAIL — the arm accepts everything but a malformed path.

- [ ] **Step 3: Implement**

In `validateConditionAtDepth`'s `ArrayCondition` arm: validate the path, then
require a trailing `[*]`, then run `validateOperandShape` over every non-nil
entry of `Values`.

In `walkConditionTypes`, replace the `ArrayCondition` no-op with
`spi.DesugarCondition` on the clause and a recursive walk of the result, so the
type check is the one a `simple` clause gets rather than a second copy.

Update `TestSearch_ArrayConditionOnContainerPath_IsAccepted` — the bare path is
now rejected for this clause. Rename it to `…_IsRejected` and keep a companion
asserting that `$.tags NOT_NULL` on a `simple` clause is still accepted, so the
container acceptance is not removed by mistake.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/search/... 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/
git commit -m "fix(search)!: an array clause addresses elements and is validated like any other"
```

---

## Task 13: Cross-backend parity for the addressing rules

**Files:**
- Create: `e2e/parity/path_addressing.go`
- Modify: `e2e/parity/registry.go`, `e2e/parity/registry_count_test.go`

**Interfaces:**
- Produces: `RunPathAddressingByDeclaredShape`, `RunArrayClausePositional`,
  `RunPathVacuity`, registered in `allTests`.

- [ ] **Step 1: Write the failing scenarios**

`RunArrayClausePositional` is the scenario that reproduces the original defect.
Import a model whose sample is `{"name":"S","tags":["A","B"],"obj":{"0":"Z"}}`,
create one entity, then assert:

```go
cases := []struct {
	name      string
	condition string
	wantCount int
}{
	{"array clause position 0", `{"type":"array","jsonPath":"$.tags[*]","values":["A"]}`, 1},
	{"array clause position 1", `{"type":"array","jsonPath":"$.tags[*]","values":[null,"B"]}`, 1},
	{"array clause wrong value", `{"type":"array","jsonPath":"$.tags[*]","values":["Z"]}`, 0},
	{"positional simple", `{"type":"simple","jsonPath":"$.tags[0]","operatorType":"EQUALS","value":"A"}`, 1},
	{"wildcard simple", `{"type":"simple","jsonPath":"$.tags[*]","operatorType":"EQUALS","value":"B"}`, 1},
	{"numeric field name", `{"type":"simple","jsonPath":"$.obj.0","operatorType":"EQUALS","value":"Z"}`, 1},
	{"bare path is not unwrapped", `{"type":"simple","jsonPath":"$.tags","operatorType":"EQUALS","value":"A"}`, 0},
}
```

The last row is the addressing rule and the sixth is the case a naive index
rewrite breaks. Assert the exact count on every backend — this scenario is the
one that failed 1/0/0 before the change.

`RunPathAddressingByDeclaredShape` covers the union rule with a field observed as
both a string and an array of strings across two entities, asserting each row of
the spec's section 4 table.

`RunPathVacuity` covers the spec's section 5 table with four entities holding
`["A"]`, `[]`, `null` and the field absent.

Also assert the rejections: an `array` clause on `$.tags` is `400`, and a
`groupBy` of `$.tags[0]` is `400 INVALID_GROUP_BY_PATH`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./e2e/parity/memory/ -run 'TestParity/PathAddressing|TestParity/ArrayClause|TestParity/PathVacuity' -v`
Expected: FAIL before Tasks 4–12 are complete; run this task after them and
expect PASS on memory, then confirm the other two.

- [ ] **Step 3: Register and bump the count**

Add three rows to `allTests` and raise `wantParityScenarioCount` by three.

- [ ] **Step 4: Run all three backends**

```bash
go test ./e2e/parity/memory/ ./e2e/parity/sqlite/ ./e2e/parity/postgres/ -run TestParity -count=1 2>&1 | tail -20
```

Expected: PASS on all three, with identical answers.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/
git commit -m "test(parity): path addressing, array-clause positions and vacuity on every backend"
```

---

## Task 14: One error code for an unknown operator

**Files:**
- Modify: `internal/domain/search/operators.go:169-176` (`validateOperator`)
- Test: `internal/domain/search/operators_test.go`, `internal/e2e/search_condition_test.go`

**Interfaces:**
- Produces: `validateOperator` wraps `ErrInvalidCondition`, so every condition
  surface answers `400 INVALID_CONDITION`. Today search answers `BAD_REQUEST`
  and grouped stats answers `INVALID_CONDITION` for the same input.

- [ ] **Step 1: Write the failing test**

```go
func TestValidateCondition_UnknownOperatorIsInvalidCondition(t *testing.T) {
	err := ValidateCondition(&predicate.SimpleCondition{
		JsonPath: "$.a", OperatorType: "NOT_EQUALS", Value: "x",
	})
	if !errors.Is(err, ErrInvalidCondition) {
		t.Errorf("want ErrInvalidCondition, got %v", err)
	}
}
```

Add an HTTP case to `internal/e2e` asserting `400` with
`properties.errorCode == "INVALID_CONDITION"` on `/search/direct`, and a gRPC
case asserting the envelope carries `INVALID_CONDITION`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/search/ -run TestValidateCondition_UnknownOperator -v`
Expected: FAIL — the error does not wrap the sentinel.

- [ ] **Step 3: Implement**

Wrap both returns in `validateOperator` with `%w: `+`ErrInvalidCondition`. Check
`structuralConditionErrCode` now routes it, and that grouped stats' own re-wrap
is now redundant — remove the duplicate if so.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/search/... ./internal/domain/entity/... -short 2>&1 | tail -20`

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/ internal/domain/entity/ internal/e2e/ internal/grpc/
git commit -m "fix(search): an unknown operator answers one code on every surface"
```

---

## Task 15: Workflow import checks criterion operators

**Files:**
- Modify: `internal/domain/workflow/validate.go:235-259` (`validateCriterion`)
- Test: `internal/domain/workflow/criterion_operator_test.go` *(new)*

**Interfaces:**
- Produces: a criterion carrying an operator outside the canonical set is
  rejected at workflow import with `400 VALIDATION_FAILED`, `detail` naming the
  workflow, state and transition.

- [ ] **Step 1: Write the failing test**

```go
func TestValidateImportRequest_RejectsUnknownCriterionOperator(t *testing.T) {
	// A criterion is never re-validated after import. An unknown operator
	// therefore imports cleanly and the transition it guards silently never
	// fires — the failure mode with no result page to look wrong.
	req := workflowWithTransitionCriterion(&predicate.SimpleCondition{
		JsonPath: "$.amount", OperatorType: "NOT_EQUALS", Value: 1,
	})
	err := validateImportRequest(req)
	if err == nil {
		t.Fatal("unknown criterion operator must be rejected at import")
	}
	if !strings.Contains(err.Error(), "NOT_EQUALS") {
		t.Errorf("detail must name the offending operator: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/workflow/ -run RejectsUnknownCriterionOperator -v`
Expected: FAIL — `validateCriterion` never calls the operator check.

- [ ] **Step 3: Implement**

Call `search.ValidateCondition` from `walkCriterion` instead of only the path
check, keeping the `VALIDATION_FAILED` classification and the workflow / state /
transition naming already in place. `ValidateCondition` also rejects a `function`
clause, which a criterion legitimately carries — so call the operator and
operand checks, not the search-shaped whole. Add a criterion-shaped entry point
in `search` rather than duplicating the operator table in `workflow`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/workflow/... 2>&1 | tail -20`

- [ ] **Step 5: Commit**

```bash
git add internal/domain/workflow/ internal/domain/search/
git commit -m "fix(workflow): reject an unknown criterion operator at import"
```

---

## Task 16: The sort-key surface refreshes the schema once

**Files:**
- Modify: `internal/domain/search/service.go:1735-1740` (`resolveSortKeys`)
- Test: `internal/domain/search/orderresolve_test.go`

**Interfaces:**
- Produces: a sort key naming a field absent from the cached schema triggers one
  bounded `RefreshAndGet` before it is refused, as every condition surface does.

- [ ] **Step 1: Write the failing test**

```go
func TestResolveSortKeys_RefreshesOnceBeforeRefusing(t *testing.T) {
	// A field a peer node has just added is absent from this node's cached
	// schema. Every condition surface refreshes once before refusing; the
	// sort surface did not, so the same field sorted on one node and 400'd
	// on another.
	store := newStaleThenFreshModelStore(t, "$.newField")
	_, err := resolveSortKeys(t.Context(), store, ref, []string{"$.newField"})
	if err != nil {
		t.Fatalf("sort key on a freshly added field must resolve: %v", err)
	}
	if store.refreshCalls != 1 {
		t.Errorf("want exactly one bounded refresh, got %d", store.refreshCalls)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/search/ -run TestResolveSortKeys_Refreshes -v`
Expected: FAIL — no refresh is attempted.

- [ ] **Step 3: Implement**

Route the lookup through the same bounded-refresh helper the condition path uses
rather than a second copy. The bound is required: an unbounded refresh turns a
misconfigured client into a refresh storm.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/search/... 2>&1 | tail -20`

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/
git commit -m "fix(search): a sort key refreshes the schema once before it is refused"
```

---

## Task 17: One sort-path scanner

**Files:**
- Modify: `internal/domain/search/sortparam.go:87-94` (delete `isValidSortPath`)
- Test: `internal/domain/search/sortparam_test.go`

**Interfaces:**
- Produces: the HTTP sort-parameter parser calls `ValidateScalarJSONPath`, the
  same check the service layer applies. The accept sets coincide today; the
  diagnostics do not, and two scanners drift.

- [ ] **Step 1: Write the failing test**

```go
func TestSortParam_UsesTheSharedScanner(t *testing.T) {
	// Both layers must refuse the same spellings with the same reason.
	for _, p := range []string{"amount", "$.a[0]", "$.a[*]", "$.a.", "$.a b"} {
		httpErr := parseSortParam(p)
		svcErr := ValidateScalarJSONPath(p)
		if (httpErr == nil) != (svcErr == nil) {
			t.Errorf("%q: http=%v service=%v — the two scanners disagree", p, httpErr, svcErr)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSortParam_UsesTheSharedScanner -v`
Expected: FAIL on at least one spelling, or PASS with differing messages — in
which case assert the message too.

- [ ] **Step 3: Implement**

Delete `isValidSortPath` and call `ValidateScalarJSONPath`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/domain/search/... 2>&1 | tail -20`

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/
git commit -m "fix(search): one scanner decides a sort path"
```

---

## Task 18: Tag the SPI, bump the pins, finish the documents

**Files:**
- Modify: `go.mod`, `plugins/*/go.mod`, `COMPATIBILITY.md`, `CHANGELOG.md`,
  `docs/cloud-parity/path-grammar.md`, `docs/cloud-parity/operator-semantics.md`,
  `cmd/cyoda/help/content/predicates.md`, `cmd/cyoda/help/content/crud.md`

- [ ] **Step 1: Push the SPI branch and open its PR**

Read `../cyoda-go-spi/MAINTAINING.md` first. Tag only after the PR merges; a Go
module tag can never be moved.

- [ ] **Step 2: Bump the four pins**

```bash
go get github.com/cyoda-platform/cyoda-go-spi@<new>
cd plugins/memory && go get github.com/cyoda-platform/cyoda-go-spi@<new> && go mod tidy
cd ../sqlite && go get github.com/cyoda-platform/cyoda-go-spi@<new> && go mod tidy
cd ../postgres && go get github.com/cyoda-platform/cyoda-go-spi@<new> && go mod tidy
```

Then `go work edit -dropuse ../cyoda-go-spi` and confirm `go.work` is back to its
committed content.

- [ ] **Step 3: Add the test-surface sections**

Both cloud-parity documents end without one. Add a `## Test surface` section to
each, naming the files this plan created: the SPI resolver tests, the parity
scenarios, the plugin planner tests, and the HTTP and gRPC cases. Name files that
exist — check each path before writing it.

- [ ] **Step 4: Update `COMPATIBILITY.md` and `CHANGELOG.md`**

`COMPATIBILITY.md` gains the new SPI row and the commercial-backend obligations
from the spec's section 12: path resolution for every form, the declared-type
lookup with the positional fold, the field-existence check, and the filter-path
grammar including brackets.

`CHANGELOG.md` gets a `### Breaking` section. Declare each caller-visible change:
an `array` clause requires `$.tags[*]` and rejects a bare path; an `array`
clause's values are now type-checked; an unknown operator answers
`INVALID_CONDITION` rather than `BAD_REQUEST`; a criterion with an unknown
operator fails workflow import; a bare path no longer matches array elements and
a wildcard path no longer matches a scalar.

- [ ] **Step 5: Update the help topics**

`cyoda help predicates` and `cyoda help crud` describe path spellings. Check both
against the spec's sections 2, 3 and 8 and correct anything that states the old
rule. Keep them compact — the actionable core, not the spec.

- [ ] **Step 6: Full verification**

```bash
go build ./... && go vet ./...
go test ./... 2>&1 | tail -30
go test ./e2e/parity/... -count=1 2>&1 | tail -20
make test-all 2>&1 | tail -30
make race 2>&1 | tail -20
gofmt -l . | grep -v '^\.claude/'
```

Every one must be clean before the PR.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum COMPATIBILITY.md CHANGELOG.md docs/ cmd/
git commit -m "chore: bump the SPI pin and finish the path-grammar documentation"
```

---

## Coverage matrix

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| bare path does not unwrap an array | T3, T4, T8 | T13 | `PathAddressingByDeclaredShape` | — |
| wildcard path does not wrap a scalar | T3, T4, T8 | T13 | `PathAddressingByDeclaredShape` | — |
| wildcard is not the array's length | T4 | — | `PathAddressingByDeclaredShape` | — |
| positional path resolves and pushes down | T3, T9, T10 | T13 | `ArrayClausePositional` | — |
| dotted numeric segment is a field name | T3, T4, T9, T10 | T13 | `ArrayClausePositional` | — |
| `array` clause matches on every backend | T6 | T13 | `ArrayClausePositional` | — |
| `array` clause on a bare path → `400 INVALID_FIELD_PATH` | T12 | T12 | `ArrayClausePositional` | T12 |
| `array` clause type mismatch → `400 CONDITION_TYPE_MISMATCH` | T12 | T12 | — | T12 |
| vacuity table | T4 | — | `PathVacuity` | — |
| element missing the key is evaluated | T3, T4 | — | `PathVacuity` | — |
| the two evaluators agree | T8 | — | — | — |
| `groupBy` of a subscripted path → `400 INVALID_GROUP_BY_PATH` | existing | existing | `ArrayClausePositional` | — |
| unknown operator → `400 INVALID_CONDITION` | T14 | T14 | — | T14 |
| unknown criterion operator → `400 VALIDATION_FAILED` | T15 | T15 | — | n/a — import is HTTP-only |
| sort key refreshes once | T16 | — | — | — |
| one sort-path scanner | T17 | — | — | — |
| malformed filter path → `400 INVALID_FIELD_PATH` | T1, T9, T10, T11 | existing | existing | existing |

No new error codes are introduced, so no `errors/<CODE>.md` task is needed and
`TestErrCode_Parity` is unaffected.

## Self-review

**Spec coverage.** Section 1 → T1. Section 2 → T1, T2, T9–T11. Section 3 → T3,
T4, T8, T13. Section 4 → T13. Section 5 → T4, T13. Section 6 → T12, T16.
Section 7 → T16, T17, and the existing grouped-stats checks, with T13 pinning the
subscript rejection. Section 8 → T6, T12, T13. Section 9 → T1, T2, T9–T11.
Section 10 → T3, T4, T6, T8. Section 11 → T14, T15, and the existing sentinel
classification. Section 12 → T18's `COMPATIBILITY.md` row.
`operator-semantics.md` sections 4, 5 and 7 → T14, T15, and the pushability rules
in T9 and T10; its other sections are unchanged behaviour already covered.

**Known gap, stated rather than hidden.** The spec's section 11 requires
`ErrInvalidFilterPath` classification on both store routes, and section 11
requires an async job to end `FAILED` when its schema becomes unreadable. Both
are implemented today and no task changes them — they are carried as regression
surface, not new work. If a task breaks either, the existing tests catch it.
