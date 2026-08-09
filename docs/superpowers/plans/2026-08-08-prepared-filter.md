# Prepared Filter (prepare/execute split) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split the search leaf evaluator into a once-per-query `Prepare` and a per-row `Match`, so operand parsing, type bucketing and `regexp.Compile` stop running once per candidate entity.

**Architecture:** Two tree walkers over one shared leaf kernel. `spi.Prepare(Filter) PreparedFilter` and `match.Prepare(predicate.Condition, FieldTypes) (Prepared, error)` each build an immutable prepared tree holding an `spi.Expansion` per leaf plus that evaluator's own addressing. `spi.MatchFilter`, `spi.EvalLeafString`, `spi.evalLeafFast`, `Expansion.Void`, `match.Match` and `match.MatchFilter` are deleted — removal is what forces every caller to re-site `Prepare` above its row loop. Answers do not change anywhere except the one declared behaviour change in §5 of the spec.

**Tech Stack:** Go 1.26, `github.com/tidwall/gjson`, two Go modules (`cyoda-go-spi`, `cyoda-go`) plus three plugin submodules, composed locally via `go.work`.

**Spec:** `docs/superpowers/specs/2026-08-07-prepared-filter-design.md`
**Research:** `docs/superpowers/research/2026-08-07-search-condition-pipeline.md`

---

## Global Constraints

- **Two repos, two PRs.** SPI work commits to `cyoda-go-spi` on branch `feat/30-prepared-filter`; everything else commits to `cyoda-go` on branch `feat/30-prepared-filter` (worktree `.claude/worktrees/feat-30-prepared-filter`). The SPI PR merges to `main` FIRST; the cyoda-go PR targets `release/v0.8.4` and carries the pin bump.
- **Local composition is via `go.work` only.** Never add a `replace` directive to any `go.mod`. `go.work` is tracked — stage files explicitly, never `git add -A` (the local SPI `use` line must stay uncommitted).
- **Answers do not change** except the two declared items in spec §5 (workflow-criterion structural error now raised eagerly; infra-failure precedence inverted). Every other test that changes expectation is a bug in the change.
- **No status code changes.** Not 500→400 for unknown lifecycle field, not for unknown group operator. Error wrap text at `search/service.go` (`"predicate match failed: %w"`) is preserved verbatim so its mapping is unchanged.
- **No issue numbers in shipped artefacts** — no `#NNN` in code comments, error messages, help topics, or OpenAPI content. Issue IDs belong in commits, PR bodies and `docs/`.
- **The zero-value asymmetry is load-bearing and must survive** (spec §3): root `Filter{}` → true, root `Filter{Op: And}` no children → true, root `Filter{Op: Or}` no children → false, `AND[leaf, Filter{}]` → **false**, `OR[non-matching leaf, Filter{}]` → false. A recursive `Prepare` that hoists the `Op == ""` check into the recursion silently flips the last two. This is the single most likely slip in the change.
- **`postFilter` nil-ness is load-bearing in three ways** (spec §7): `LIMIT` pushdown, native `GROUP BY`, and sqlite's scan budget all key off `plan.postFilter == nil`. `sqlPlan.postFilter *spi.Filter` therefore stays exactly as it is; a parallel `preparedPostFilter *spi.PreparedFilter` field is added, non-nil exactly when `postFilter` is non-nil.
- **Prepared values must be immutable after construction** — one is shared across N errgroup workers by the commercial Cassandra direct-search fan-out. No lazy field, no memo, no captured mutable closure.
- **`Prepare` consumes the `fieldTypes` closure eagerly and never retains it.** The engine's closure mutates three captured variables with no synchronisation; retaining it in a value shared across goroutines would be a data race.
- **A plugin's exported surface has root-module callers that plugin-scoped commands cannot see.** `plugins/sqlite.EvaluateFilter` is called from `internal/match/match_filter_sqlite_parity_test.go` in the ROOT module. After changing any exported plugin symbol, run `go build ./...` and `go vet ./internal/...` from the repo root as well as the plugin's own suite — `cd plugins/sqlite && go test ./...` passes while the root module fails to compile. (`plugins/memory` and `plugins/postgres` expose no filter-evaluation symbol, so only sqlite carries this hazard.)
- **Test discipline:** `go test -short ./...` or scoped package tests during iteration. `make race` once, before PR creation. Full `make test-all` once, at end of deliverable. Do not put `-race` or the full suite in intermediate steps.
- **Go conventions:** `log/slog` only; wrap errors `fmt.Errorf("failed to X: %w", err)`; every `Lock()`/`RLock()` immediately followed by `defer Unlock()`/`defer RUnlock()`.

---

## File Structure

### cyoda-go-spi (module `github.com/cyoda-platform/cyoda-go-spi`)

| file | responsibility |
|---|---|
| `prepared_filter.go` **(new)** | `PreparedFilter`, `preparedNode`, `Prepare`, `Match` — the prepared tree walker. Owns the zero-value asymmetry. |
| `prepared_filter_test.go` **(new)** | zero-value table, compile-once counter, cross-goroutine agreement. |
| `prepared_filter_equivalence_test.go` **(new)** | the merge gate: frozen `MatchFilter`/`evalFilter`/`evalLeafFilter`/`EvalLeafString`/`evalLeafFast` reference vs `Prepare().Match()` over a randomised corpus. |
| `eval_leaf.go` | gains the `compileRegex` package-var indirection; loses `EvalLeafString`, `evalLeafFast`, `Expansion.Void`; doc comment corrected. |
| `filter_match.go` | loses `MatchFilter`, `evalFilter`, `evalLeafFilter`; keeps `filterStoredResult` (re-shaped for the node), `metaGjsonResult`, `OperandString`, `valuesToStrings`, `extractFilterMetaValue`, `timeToMicro`. |
| `eval_leaf_test.go` | the fast-path-vs-general differential is retired with `evalLeafFast`. |
| `filter_match_test.go`, `filter_match_internal_test.go`, `condition_filter_test.go` | migrate `spi.MatchFilter(f, d, m)` → `spi.Prepare(f).Match(d, m)`. |
| `CHANGELOG.md` | `### Breaking` section with migration notes. |

### cyoda-go (root module)

| file | responsibility |
|---|---|
| `internal/match/prepared.go` **(new)** | `Prepared`, `prepNode`, `Prepare`, `Match`, `expandNamed`, `errUnsupportedOperator`, the meta bridges. |
| `internal/match/prepared_test.go` **(new)** | structural-error table, never-match table, array positions, zero-value fail-closed. |
| `internal/match/prepared_equivalence_test.go` **(new)** | the merge gate against a frozen `match.Match` reference. |
| `internal/match/match.go` | loses `Match`, `matchSimple`, `matchArrayWildcard`, `matchLifecycle`, `applyStringLifecycle`, `matchTemporalMeta`, `matchGroup`, `matchArray`, `MatchFilter`; keeps `FieldTypes`, `convertJSONPath`, `fieldMapKey`, `arrayElementFieldPath`, `isTemporalOperator`. Gains the §9 divergence comment. |
| `internal/match/operators.go` | loses `applyOperator`; keeps `opNameToFilterOp`, `operandsToStrings`, `betweenBounds`. |
| `internal/domain/search/service.go` | hoists `match.Prepare` above the fallback row loop. |
| `internal/domain/entity/grouped_stats_service.go` | hoists `match.Prepare` above the streaming-tally loop. |
| `internal/domain/workflow/engine.go` | `evaluateCriterion` re-sites `Prepare`; **infra-failure precedence inverted**. |
| `internal/domain/search/regex_validate.go` | stale `opMatchesPattern` comments at `:25-34` and `:71` corrected. |
| `internal/domain/workflow/validate.go` | stale `opMatchesPattern` comments at `:215`, `:269` corrected. |
| `internal/match/match_filter_sqlite_parity_test.go` | migrates to the prepared form on both sides. |
| `e2e/parity/txsearchryw/tx_search_ryw_test.go` | oracle migrates to `spi.Prepare(...).Match(...)`. |
| `internal/e2e/criterion_prepare_test.go` **(new)** | the §5 criterion behaviour change through the full HTTP stack. |
| `docs/cloud-parity/`, `COMPATIBILITY.md`, `CHANGELOG.md`, `docs/workflow-schema-versioning.md` | Gate 4 / Gate 7 documentation. |

### cyoda-go plugin submodules

| file | responsibility |
|---|---|
| `plugins/memory/searcher.go` | `matchSortBounded` takes a `spi.PreparedFilter`; the in-tx RYW branch prepares once above both loops. |
| `plugins/memory/grouped_stats.go` | `memoryIter` carries a prepared filter; `GroupedAggregate` prepares once above its loop; `msMatchFilter` deleted. |
| `plugins/sqlite/query_planner.go` | `sqlPlan` gains `preparedPostFilter *spi.PreparedFilter`, populated at the single `return plan`. |
| `plugins/sqlite/post_filter.go` | `evaluateFilter`/`EvaluateFilter` become prepared-taking and error-free. |
| `plugins/sqlite/searcher.go` | two raw row loops read the prepared field; the in-tx buffered-adds loop prepares once. |
| `plugins/sqlite/grouped_stats.go` | `sqliteSliceIter` and `sqliteIter` carry prepared filters. |
| `plugins/postgres/query_planner.go` | same `preparedPostFilter` addition. |
| `plugins/postgres/grouped_stats.go` | `postgresIter` (built from **two** sites) carries a prepared filter; `evalPostFilter` becomes prepared-taking. |
| `plugins/postgres/searcher.go` | the second `postgresIter` construction site. |
| each plugin's `*_test.go` | migrate `spi.MatchFilter` → `spi.Prepare(f).Match(...)`. |

---

## Task Order and Why

The tree stays green at every commit. That forces this order:

1. **SPI adds `PreparedFilter` additively** (Tasks 1–2) — old API still present, nothing breaks.
2. **cyoda-go migrates every call site onto the new API** (Tasks 3–10) — both APIs coexist.
3. **cyoda-go deletes its own `Match`/`MatchFilter`** (Task 11) — removes the last `spi.EvalLeafString` and `spi.MatchFilter` consumers.
4. **SPI deletes the old API** (Task 12) — safe only now.
5. **Behaviour test, comments, docs, pins** (Tasks 13–16).

Deleting in the SPI before step 3 breaks the cyoda-go build under `go.work`.

---

## Task 1: SPI — `PreparedFilter`, `Prepare`, `Match`

**Files:**
- Create: `cyoda-go-spi/prepared_filter.go`
- Create: `cyoda-go-spi/prepared_filter_test.go`
- Modify: `cyoda-go-spi/eval_leaf.go` (add the `compileRegex` indirection; use it in `ExpandLeaf`)

**Interfaces:**
- Consumes: existing `Filter`, `FieldSource`, `SourceMeta`, `Expansion`, `ExpandLeaf`, `EvalLeaf`, `OperandString`, `valuesToStrings`, `metaGjsonResult`.
- Produces:
  - `type PreparedFilter struct{ root *preparedNode }`
  - `func Prepare(f Filter) PreparedFilter`
  - `func (p PreparedFilter) Match(data []byte, meta EntityMeta) bool`
  - `var compileRegex = regexp.Compile` (package-private, test-swappable)

- [ ] **Step 1: Reset the stale SPI branch onto main**

The branch `feat/30-prepared-filter` sits at `150ecfa`, two commits behind `main` (`d475ae1`), and carries no unique work — it is a strict ancestor. Reset it, do not merge.

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
git log --oneline main..HEAD          # expect: EMPTY output — no unique commits
git reset --hard main
git log --oneline -1                  # expect: d475ae1 feat(search): restore the condition translator...
```

If `main..HEAD` is NOT empty, STOP and surface it — the reset would discard work.

- [ ] **Step 2: Write the failing zero-value table test**

Create `cyoda-go-spi/prepared_filter_test.go`. This is spec §3's asymmetry table, verbatim — the five rows that a naive recursive `Prepare` gets wrong.

```go
package spi_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPrepare_ZeroValueAsymmetry pins spec §3's table: a zero-Op filter is
// match-all at the ROOT only. A zero-Op CHILD is a leaf that never matches —
// evalFilter routed it to the leaf evaluator, ExpandLeaf hit its default arm,
// and the leaf was false for every row. Hoisting the Op == "" check into the
// recursion silently flips the AND/OR child rows, so they are pinned here.
//
// sqlite depends on the root behaviour in plugins/sqlite/grouped_stats.go,
// which special-cases an empty Op before reaching the evaluator.
func TestPrepare_ZeroValueAsymmetry(t *testing.T) {
	leaf := spi.Filter{
		Op:       spi.FilterEq,
		Source:   spi.SourceData,
		Path:     "name",
		Value:    "Alice",
		Declared: []spi.DataType{spi.String},
	}
	data := []byte(`{"name":"Alice"}`)

	tests := []struct {
		name string
		f    spi.Filter
		want bool
	}{
		{"root zero filter matches all", spi.Filter{}, true},
		{"root empty AND is the AND identity", spi.Filter{Op: spi.FilterAnd}, true},
		{"root empty OR is the OR identity", spi.Filter{Op: spi.FilterOr}, false},
		{
			"zero-Op child annihilates an AND",
			spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{leaf, {}}},
			false,
		},
		{
			"zero-Op child does not rescue an OR",
			spi.Filter{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Source: spi.SourceData, Path: "name",
						Value: "Bob", Declared: []spi.DataType{spi.String}},
					{},
				},
			},
			false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := spi.Prepare(tc.f).Match(data, spi.EntityMeta{}); got != tc.want {
				t.Errorf("Prepare(%+v).Match() = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// TestPreparedFilter_ZeroValueMatchesAll pins that the zero PreparedFilter —
// the value a caller gets from a nil-able field or an unassigned variable —
// matches everything, mirroring Prepare(Filter{}). Both spellings of "no
// filter" must agree.
func TestPreparedFilter_ZeroValueMatchesAll(t *testing.T) {
	var p spi.PreparedFilter
	if !p.Match([]byte(`{"a":1}`), spi.EntityMeta{}) {
		t.Error("zero PreparedFilter.Match() = false, want true (match-all)")
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
go test ./... -run 'TestPrepare_ZeroValueAsymmetry|TestPreparedFilter_ZeroValueMatchesAll' -v
```
Expected: FAIL — `undefined: spi.Prepare`, `undefined: spi.PreparedFilter`.

- [ ] **Step 4: Write `prepared_filter.go`**

Create `cyoda-go-spi/prepared_filter.go`:

```go
package spi

import "github.com/tidwall/gjson"

// prepared_filter.go is the prepare/execute split of the Filter evaluator.
// Prepare resolves everything that depends only on the query — operand
// normalisation, type bucketing, and regex compilation — into an immutable
// tree. Match then walks that tree per row and does no query-invariant work at
// all.
//
// The prepared value is safe for concurrent use by any number of goroutines
// after Prepare returns: nothing in it is written again, and nothing is
// resolved lazily. The commercial Cassandra direct-search fan-out depends on
// this, handing one prepared filter to N errgroup workers.

// PreparedFilter is a Filter compiled for repeated evaluation. Build it once
// per query with Prepare, then call Match once per candidate row.
//
// The zero PreparedFilter matches everything, mirroring Prepare(Filter{}) and
// the "no filter" convention every backend already relies on.
type PreparedFilter struct {
	// root is nil exactly for the match-all filter. A nil root is what makes
	// the zero value match-all without a separate flag.
	root *preparedNode
}

// preparedNode is one node of the prepared tree: a group with children, or a
// leaf carrying its addressing plus the expansion its operand produced.
type preparedNode struct {
	op       FilterOp
	children []preparedNode

	// Leaf addressing, mirroring Filter.Source / Filter.Path.
	source FieldSource
	path   string

	// exp is meaningful only when expanded is true. A leaf whose ExpandLeaf
	// failed is a leaf that never matches — the same answer evalLeafFilter
	// produced by absorbing the error into `matched && err == nil`, but stated
	// explicitly rather than relying on the zero Expansion happening to fall
	// through EvalLeaf's switch.
	exp      Expansion
	expanded bool
}

// Prepare compiles f for repeated evaluation. It returns no error: a leaf whose
// operand cannot be expanded becomes a leaf that never matches, which is
// exactly what the per-row evaluator did before. Promoting that to a hard
// rejection is a cross-backend contract change and is deliberately not done
// here.
//
// Prepare consumes f. It does not retain a reference to it, and it is not a
// defence against a caller mutating f afterwards — no such defence is owed.
func Prepare(f Filter) PreparedFilter {
	// Root-only match-all. This check must NOT move into prepareNode: a
	// zero-Op CHILD is a leaf that never matches, and hoisting the check into
	// the recursion would silently turn it into an identity element.
	if f.Op == "" {
		return PreparedFilter{}
	}
	n := prepareNode(f)
	return PreparedFilter{root: &n}
}

func prepareNode(f Filter) preparedNode {
	switch f.Op {
	case FilterAnd, FilterOr:
		n := preparedNode{op: f.Op}
		if len(f.Children) > 0 {
			n.children = make([]preparedNode, len(f.Children))
			for i, c := range f.Children {
				n.children[i] = prepareNode(c)
			}
		}
		return n
	}

	// Leaf — including a zero-Op child, which ExpandLeaf's default arm rejects.
	n := preparedNode{op: f.Op, source: f.Source, path: f.Path}
	exp, err := ExpandLeaf(f.Op, OperandString(f.Value), valuesToStrings(f.Values), f.Declared)
	if err == nil {
		n.exp = exp
		n.expanded = true
	}
	return n
}

// Match reports whether the entity satisfies the prepared filter. It performs
// no parsing, bucketing or regex compilation — all of that happened in Prepare.
func (p PreparedFilter) Match(data []byte, meta EntityMeta) bool {
	if p.root == nil {
		return true
	}
	return p.root.match(data, meta)
}

func (n *preparedNode) match(data []byte, meta EntityMeta) bool {
	switch n.op {
	case FilterAnd:
		for i := range n.children {
			if !n.children[i].match(data, meta) {
				return false
			}
		}
		return true
	case FilterOr:
		for i := range n.children {
			if n.children[i].match(data, meta) {
				return true
			}
		}
		return false
	}
	if !n.expanded {
		return false
	}
	return EvalLeaf(n.exp, n.stored(data, meta))
}

// stored resolves the value this leaf addresses, keeping gjson's .Raw so the
// kernel can classify numerics and temporals precisely. Same contract as the
// pre-split filterStoredResult: a missing data path yields a non-existent
// Result, and SourceMeta values are bridged through metaGjsonResult.
func (n *preparedNode) stored(data []byte, meta EntityMeta) gjson.Result {
	if n.source == SourceMeta {
		r, _ := metaGjsonResult(n.path, meta)
		return r
	}
	return gjson.GetBytes(data, n.path)
}
```

- [ ] **Step 5: Run the zero-value tests to verify they pass**

```bash
go test ./... -run 'TestPrepare_ZeroValueAsymmetry|TestPreparedFilter_ZeroValueMatchesAll' -v
```
Expected: PASS, all seven subtests.

- [ ] **Step 6: Write the failing compile-once test**

Append to `cyoda-go-spi/prepared_filter_test.go`? No — it needs the unexported `compileRegex`, so it must be in package `spi`. Create `cyoda-go-spi/prepared_filter_internal_test.go`:

```go
package spi

import (
	"regexp"
	"testing"

	"github.com/tidwall/gjson"
)

// TestPrepare_CompilesRegexExactlyOncePerQuery is the test the whole change
// exists for. regexp.Compile is query-invariant work; before the split it ran
// once per candidate entity because MATCHES_PATTERN and LIKE never reached a
// fast path.
//
// Must NOT be t.Parallel() and must not overlap any other test that touches
// compileRegex — the indirection swap is itself a data race otherwise.
func TestPrepare_CompilesRegexExactlyOncePerQuery(t *testing.T) {
	for _, op := range []FilterOp{FilterMatchesRegex, FilterLike} {
		t.Run(string(op), func(t *testing.T) {
			calls := 0
			orig := compileRegex
			compileRegex = func(expr string) (*regexp.Regexp, error) {
				calls++
				return orig(expr)
			}
			defer func() { compileRegex = orig }()

			operand := "A.*"
			if op == FilterLike {
				operand = "A%"
			}
			p := Prepare(Filter{
				Op:       op,
				Source:   SourceData,
				Path:     "name",
				Value:    operand,
				Declared: []DataType{String},
			})

			if calls != 1 {
				t.Fatalf("Prepare compiled %d times, want exactly 1", calls)
			}

			data := []byte(`{"name":"Alice"}`)
			for i := 0; i < 1000; i++ {
				if !p.Match(data, EntityMeta{}) {
					t.Fatalf("Match = false on row %d, want true", i)
				}
			}

			if calls != 1 {
				t.Errorf("compiled %d times across Prepare + 1000 Match calls, want exactly 1", calls)
			}
		})
	}
}

// TestEvalLeaf_UsesStoredRawForRegex guards the indirection itself: swapping
// compileRegex must not change what a pattern leaf answers.
func TestEvalLeaf_UsesStoredRawForRegex(t *testing.T) {
	exp, err := ExpandLeaf(FilterMatchesRegex, "A.*e", nil, []DataType{String})
	if err != nil {
		t.Fatalf("ExpandLeaf: %v", err)
	}
	if !EvalLeaf(exp, gjson.Parse(`"Alice"`)) {
		t.Error("EvalLeaf = false for a matching anchored pattern, want true")
	}
	if EvalLeaf(exp, gjson.Parse(`"Alicia"`)) {
		t.Error("EvalLeaf = true for a non-matching value, want false")
	}
}
```

- [ ] **Step 7: Run it to verify it fails**

```bash
go test ./... -run 'TestPrepare_CompilesRegexExactlyOncePerQuery' -v
```
Expected: FAIL — `undefined: compileRegex`.

- [ ] **Step 8: Add the `compileRegex` indirection in `eval_leaf.go`**

In `cyoda-go-spi/eval_leaf.go`, add the package var immediately above `ExpandLeaf`:

```go
// compileRegex is regexp.Compile behind a package var so an internal test can
// count compilations and prove they happen once per query rather than once per
// row. Production code never reassigns it.
var compileRegex = regexp.Compile
```

Then change the two compile sites inside `ExpandLeaf` (currently `eval_leaf.go:142` and `:146`):

```go
		case FilterLike:
			if re, err := compileRegex(anchor(likeToRegex(operand))); err == nil {
				e.strRegex = re
			}
		case FilterMatchesRegex:
			if re, err := compileRegex(anchor(operand)); err == nil {
				e.strRegex = re
			}
```

- [ ] **Step 9: Run the compile-once tests to verify they pass**

```bash
go test ./... -run 'TestPrepare_CompilesRegexExactlyOncePerQuery|TestEvalLeaf_UsesStoredRawForRegex' -v
```
Expected: PASS — `calls == 1` for both `matches_regex` and `like`.

- [ ] **Step 10: Write the failing concurrency test**

Append to `cyoda-go-spi/prepared_filter_test.go` (external package — no `compileRegex` access, so it can never overlap the counter test's swap):

```go
// TestPreparedFilter_ConcurrentMatch pins that one prepared filter is safe to
// share across goroutines and that they all agree. Asserting agreement, not
// merely absence of a race report, is what catches a lazily-resolved field:
// under -race a torn read shows up as a wrong answer even when the detector
// misses the write.
//
// The commercial Cassandra direct-search fan-out hands one prepared filter to
// N errgroup workers, so this is a real usage shape, not a synthetic one.
func TestPreparedFilter_ConcurrentMatch(t *testing.T) {
	p := spi.Prepare(spi.Filter{
		Op: spi.FilterOr,
		Children: []spi.Filter{
			{Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "name",
				Value: "A.*", Declared: []spi.DataType{spi.String}},
			{Op: spi.FilterGt, Source: spi.SourceData, Path: "qty",
				Value: "10", Declared: []spi.DataType{spi.Integer}},
			{Op: spi.FilterEq, Source: spi.SourceMeta, Path: "state",
				Value: "active", Declared: []spi.DataType{spi.String}},
		},
	})

	rows := []struct {
		data []byte
		meta spi.EntityMeta
		want bool
	}{
		{[]byte(`{"name":"Alice","qty":1}`), spi.EntityMeta{State: "idle"}, true},
		{[]byte(`{"name":"Bob","qty":50}`), spi.EntityMeta{State: "idle"}, true},
		{[]byte(`{"name":"Bob","qty":1}`), spi.EntityMeta{State: "active"}, true},
		{[]byte(`{"name":"Bob","qty":1}`), spi.EntityMeta{State: "idle"}, false},
	}

	const workers = 16
	const iterations = 200

	results := make([][]bool, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			got := make([]bool, 0, len(rows)*iterations)
			for i := 0; i < iterations; i++ {
				for _, r := range rows {
					got = append(got, p.Match(r.data, r.meta))
				}
			}
			results[w] = got
		}(w)
	}
	wg.Wait()

	for w := 0; w < workers; w++ {
		for i, got := range results[w] {
			want := rows[i%len(rows)].want
			if got != want {
				t.Fatalf("worker %d observation %d = %v, want %v", w, i, got, want)
			}
		}
	}
}
```

Add `"sync"` to that file's imports.

- [ ] **Step 11: Run the concurrency test**

```bash
go test ./... -run 'TestPreparedFilter_ConcurrentMatch' -race -v
```
Expected: PASS, no race report. (This is the one place `-race` is warranted mid-plan — the test's entire purpose is the race.)

- [ ] **Step 12: Run the whole SPI suite and commit**

```bash
go test ./... 2>&1 | tail -20
go vet ./...
```
Expected: all PASS (the old API is untouched and still green).

```bash
git add prepared_filter.go prepared_filter_test.go prepared_filter_internal_test.go eval_leaf.go
git commit -m "feat(search): add PreparedFilter — the prepare/execute split of the Filter evaluator

Prepare resolves operand normalisation, type bucketing and regex compilation
once per query; Match walks the prepared tree per row and does none of it. The
prepared value is immutable and safe to share across goroutines.

Additive: MatchFilter and EvalLeafString are untouched and still the path every
caller uses. They are removed once their callers have re-sited Prepare.

Refs #30"
```

---

## Task 2: SPI — the merge gate (randomised equivalence against a frozen reference)

This is the gate that lets every later task delete code with confidence. `Prepare(f).Match(d, m)` must equal `MatchFilter(f, d, m)` for every generated `(filter, entity)` pair, with **no exceptions and no carve-outs** — the change alters no answers, so the net can demand exact agreement.

The reference is a **frozen copy** of `MatchFilter`, `evalFilter`, `evalLeafFilter`, `EvalLeafString` and `evalLeafFast`, living in the test file. Freezing only `evalFilter` would leave the reference calling live code through the others, and `MatchFilter` itself carries the root `f.Op == ""` check that the whole asymmetry table turns on. Because it is a copy, it survives Task 12's deletion of the originals and keeps guarding afterwards.

**Files:**
- Create: `cyoda-go-spi/prepared_filter_equivalence_test.go` (package `spi` — the reference needs `valuesToStrings`, `metaGjsonResult`, `evalLeafFast`)

**Interfaces:**
- Consumes: `Prepare`, `Match` (Task 1); `ExpandLeaf`, `EvalLeaf`, `OperandString`, `valuesToStrings`, `metaGjsonResult`, `evalLeafFast`.
- Produces: `func frozenMatchFilter(f Filter, data []byte, meta EntityMeta) bool` — the reference every later task's equivalence assertion reuses.

- [ ] **Step 1: Write the frozen reference**

Create `cyoda-go-spi/prepared_filter_equivalence_test.go`:

```go
package spi

// The merge gate for the prepare/execute split.
//
// frozenMatchFilter below is a verbatim copy of the pre-split evaluator —
// MatchFilter, evalFilter, evalLeafFilter, EvalLeafString and evalLeafFast —
// taken before any of them were deleted. It is a COPY on purpose: it must keep
// answering the old way after the originals are gone, or the gate stops
// guarding anything the moment the deletion lands.
//
// Do not "simplify" this by calling live code. Do not update it when the live
// evaluator changes. If it and Prepare disagree, the live code is wrong.

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// --- frozen reference ------------------------------------------------------

func frozenMatchFilter(f Filter, data []byte, meta EntityMeta) bool {
	if f.Op == "" {
		return true
	}
	return frozenEvalFilter(f, data, meta)
}

func frozenEvalFilter(f Filter, data []byte, meta EntityMeta) bool {
	switch f.Op {
	case FilterAnd:
		for _, c := range f.Children {
			if !frozenEvalFilter(c, data, meta) {
				return false
			}
		}
		return true
	case FilterOr:
		for _, c := range f.Children {
			if frozenEvalFilter(c, data, meta) {
				return true
			}
		}
		return false
	}
	return frozenEvalLeafFilter(f, data, meta)
}

func frozenEvalLeafFilter(f Filter, data []byte, meta EntityMeta) bool {
	stored := frozenStoredResult(f, data, meta)
	matched, err := frozenEvalLeafString(f.Op, OperandString(f.Value), valuesToStrings(f.Values), f.Declared, stored)
	return matched && err == nil
}

func frozenStoredResult(f Filter, data []byte, meta EntityMeta) gjson.Result {
	if f.Source == SourceMeta {
		r, _ := metaGjsonResult(f.Path, meta)
		return r
	}
	return gjson.GetBytes(data, f.Path)
}

func frozenEvalLeafString(op FilterOp, operand string, values []string, declared []DataType, stored gjson.Result) (bool, error) {
	if matched, handled := frozenEvalLeafFast(op, operand, declared, stored); handled {
		return matched, nil
	}
	exp, err := ExpandLeaf(op, operand, values, declared)
	if err != nil {
		return false, err
	}
	return EvalLeaf(exp, stored), nil
}

func frozenEvalLeafFast(op FilterOp, operand string, declared []DataType, stored gjson.Result) (matched, handled bool) {
	if len(declared) != 1 {
		return false, false
	}
	switch op {
	case FilterEq, FilterNe, FilterGt, FilterGte, FilterLt, FilterLte:
	default:
		return false, false
	}
	nullish := !stored.Exists() || stored.Type == gjson.Null

	switch declared[0] {
	case String:
		if nullish {
			return false, true
		}
		if stored.Type != gjson.String {
			return false, true
		}
		return cmpResult(strings.Compare(stored.String(), operand), op), true

	case UnboundDecimal:
		opDec, err := ParseDecimal(operand)
		if err != nil {
			return false, false
		}
		if nullish {
			return false, true
		}
		if stored.Type != gjson.Number {
			return false, true
		}
		storedDec, err := ParseDecimal(stored.Raw)
		if err != nil {
			return false, true
		}
		return cmpResult(storedDec.Cmp(opDec), op), true
	}
	return false, false
}

var _ = regexp.MustCompile // keeps the import honest if the reference is trimmed
```

- [ ] **Step 2: Write the generator and the equivalence test**

Append to the same file:

```go
// --- generated corpus ------------------------------------------------------

// The generator emits only WELL-FORMED filters — the shapes ConditionToFilter
// actually produces. Spec §5's changed cases are malformed by construction and
// are covered by hand-written tables elsewhere (Tasks 9 and 14), not here.

var genOps = []FilterOp{
	FilterEq, FilterNe, FilterGt, FilterGte, FilterLt, FilterLte,
	FilterContains, FilterStartsWith, FilterEndsWith, FilterLike, FilterMatchesRegex,
	FilterNotContains, FilterNotStartsWith, FilterNotEndsWith,
	FilterIEq, FilterINe, FilterIContains, FilterINotContains,
	FilterIStartsWith, FilterINotStartsWith, FilterIEndsWith, FilterINotEndsWith,
	FilterIsNull, FilterNotNull,
	FilterBetween, FilterBetweenInclusive,
}

var genDeclared = [][]DataType{
	{String},
	{Integer},
	{Long},
	{UnboundDecimal},
	{Double},
	{Boolean},
	{UUIDType},
	{ZonedDateTime},
	{LocalDate},
	{Integer, String},
	{Double, Integer},
	{ZonedDateTime, String},
	nil,
}

var genDataPaths = []string{"name", "qty", "price", "flag", "uid", "when", "missing", "nested.inner"}

var genMetaPaths = []string{"state", "id", "creationDate", "lastUpdateTime", "transactionId", "version", "change_type"}

var genOperands = []string{
	"Alice", "alice", "ALICE", "Bob", "", "A%", "A.*", "a.*e", "%ice",
	"30", "30.0", "12.78", "-5", "0", "9223372036854775807", "1e3",
	"true", "false",
	"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	"2024-01-01T00:00:00Z", "2024-06-01", "2024", "2024-01-01T00:00:00+02:00",
	"não", "日本", "\\%literal",
}

var genDocs = []string{
	`{"name":"Alice","qty":30,"price":12.78,"flag":true,"uid":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","when":"2024-01-01T00:00:00Z","nested":{"inner":"deep"}}`,
	`{"name":"alice","qty":31,"price":-5,"flag":false,"when":"2024-06-01"}`,
	`{"name":"","qty":0,"price":0.0,"when":"2024"}`,
	`{"name":"não","qty":9223372036854775807,"when":"2024-01-01T00:00:00+02:00"}`,
	`{"name":null,"qty":null}`,
	`{}`,
	`{"name":30,"qty":"30"}`,
	`{"name":["Alice","Bob"],"qty":[1,2]}`,
}

func genLeaf(r *rand.Rand) Filter {
	f := Filter{Op: genOps[r.Intn(len(genOps))]}
	if r.Intn(4) == 0 {
		f.Source = SourceMeta
		f.Path = genMetaPaths[r.Intn(len(genMetaPaths))]
	} else {
		f.Source = SourceData
		f.Path = genDataPaths[r.Intn(len(genDataPaths))]
	}
	f.Declared = genDeclared[r.Intn(len(genDeclared))]
	if f.Op == FilterBetween || f.Op == FilterBetweenInclusive {
		// Deliberately also emit the wrong arity sometimes: ExpandLeaf's arity
		// error is a per-row non-match in both implementations and must stay so.
		switch r.Intn(4) {
		case 0:
			f.Values = []any{genOperands[r.Intn(len(genOperands))]}
		default:
			f.Values = []any{
				genOperands[r.Intn(len(genOperands))],
				genOperands[r.Intn(len(genOperands))],
			}
		}
		return f
	}
	f.Value = genOperands[r.Intn(len(genOperands))]
	return f
}

func genFilter(r *rand.Rand, depth int) Filter {
	if depth <= 0 || r.Intn(3) == 0 {
		return genLeaf(r)
	}
	op := FilterAnd
	if r.Intn(2) == 0 {
		op = FilterOr
	}
	n := r.Intn(4) // 0..3 children — zero children exercises the group identities
	f := Filter{Op: op}
	for i := 0; i < n; i++ {
		f.Children = append(f.Children, genFilter(r, depth-1))
	}
	return f
}

func genMeta(r *rand.Rand) EntityMeta {
	metas := []EntityMeta{
		{ID: "ent-1", State: "active", Version: 7,
			CreationDate: mustTime("2024-01-01T00:00:00Z"), LastModifiedDate: mustTime("2024-06-01T00:00:00Z"),
			ChangeType: "UPDATED", TransactionID: "tx-1", TransitionForLatestSave: "approve"},
		{ID: "ent-2", State: "", Version: 0},
		{},
	}
	return metas[r.Intn(len(metas))]
}

// TestPrepare_EquivalentToFrozenMatchFilter is the merge gate. Exact agreement,
// no carve-outs: the prepare/execute split changes no answers.
func TestPrepare_EquivalentToFrozenMatchFilter(t *testing.T) {
	const cases = 200000
	r := rand.New(rand.NewSource(0x30C0DE))

	for i := 0; i < cases; i++ {
		f := genFilter(r, 3)
		data := []byte(genDocs[r.Intn(len(genDocs))])
		meta := genMeta(r)

		want := frozenMatchFilter(f, data, meta)
		got := Prepare(f).Match(data, meta)

		if got != want {
			t.Fatalf("DIVERGENCE at case %d\n  prepared=%v frozen=%v\n  filter=%#v\n  data=%s\n  meta=%+v",
				i, got, want, f, data, meta)
		}
	}
}

// TestPrepare_MatchIsRepeatable pins that a prepared filter gives the same
// answer on every call — no state is consumed by evaluation.
func TestPrepare_MatchIsRepeatable(t *testing.T) {
	r := rand.New(rand.NewSource(0xBEEFED))
	for i := 0; i < 2000; i++ {
		f := genFilter(r, 3)
		data := []byte(genDocs[r.Intn(len(genDocs))])
		meta := genMeta(r)
		p := Prepare(f)
		first := p.Match(data, meta)
		for k := 0; k < 5; k++ {
			if p.Match(data, meta) != first {
				t.Fatalf("non-repeatable answer at case %d: filter=%#v", i, f)
			}
		}
	}
}
```

Add a `mustTime` helper if the file does not already have one:

```go
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
```
(add `"time"` to the imports; if `mustTime` already exists in another `spi` package test file, reuse it and do not redeclare).

- [ ] **Step 3: Run the gate**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
go test ./... -run 'TestPrepare_EquivalentToFrozenMatchFilter|TestPrepare_MatchIsRepeatable' -v
```
Expected: PASS. 200,000 cases run in a few seconds.

**If it FAILS:** the failure message prints the exact filter, document and meta. Fix `prepared_filter.go` — the reference is right by definition. The most likely causes, in order: the zero-Op child check leaked into `prepareNode`; the leaf `expanded` flag inverted; `valuesToStrings` not applied to `Values`.

- [ ] **Step 4: Widen the corpus once, as a one-off confidence run**

The seed is fixed, so `-count=10` re-runs the **identical** corpus and explores
nothing new. Make the corpus size and seed overridable so a widened run is real
and reproducible, defaulting to the committed 200,000-case gate:

```go
// Corpus size and seed are overridable so a one-off widened exploration is
// reproducible. The committed defaults ARE the standing gate; -count alone
// widens nothing, because a fixed seed regenerates the same corpus.
func equivCases() int  { return envInt("SPI_EQUIV_CASES", 200000) }
func equivSeed() int64 { return int64(envInt("SPI_EQUIV_SEED", 0x30C0DE)) }

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
```

Then run genuinely distinct corpora:

```bash
for s in 1 2 3 4 5; do
  SPI_EQUIV_SEED=$s SPI_EQUIV_CASES=400000 go test . -run 'TestPrepare_EquivalentToFrozenMatchFilter' || break
done
```
Expected: PASS, 2,000,000 distinct cases across five seeds. Do not commit a raised default.

- [ ] **Step 5: Commit**

```bash
go vet ./...
git add prepared_filter_equivalence_test.go
git commit -m "test(search): merge gate — Prepare/Match must equal the frozen pre-split evaluator

200k generated (filter, entity) pairs, exact agreement, no carve-outs. The
reference is a frozen copy of MatchFilter/evalFilter/evalLeafFilter/
EvalLeafString/evalLeafFast so it keeps guarding after those are deleted.

Refs #30"
```

---

## Task 3: cyoda-go — `match.Prepared`, `Prepare`, `Match`

Additive: the existing `match.Match` stays until Task 11. Everything here lands in a new file so the old evaluator is untouched and stays available as the equivalence reference.

**Files:**
- Create: `internal/match/prepared.go`
- Create: `internal/match/prepared_test.go`

**Interfaces:**
- Consumes: `spi.Prepare`/`PreparedFilter` are NOT used here — this is the `predicate.Condition` walker. It consumes `spi.ExpandLeaf`, `spi.EvalLeaf`, `spi.Expansion`, `spi.OperandString`, and this package's existing `FieldTypes`, `convertJSONPath`, `fieldMapKey`, `arrayElementFieldPath`, `isTemporalOperator`, `opNameToFilterOp`, `betweenBounds`.
- Produces:
  - `type Prepared struct{ root prepNode }`
  - `func Prepare(cond predicate.Condition, fieldTypes FieldTypes) (Prepared, error)`
  - `func (p Prepared) Match(data []byte, meta spi.EntityMeta) bool`
  - `var errUnsupportedOperator = errors.New("unsupported operator")`

**The five structural errors that move from per-row to `Prepare`** (spec §4) — all keep their exact message text so no status mapping moves:

| error text | old site | new site |
|---|---|---|
| `function conditions not implemented` | `match.go:42` | `prepare` default switch |
| `unknown condition type: %T` | `match.go:44` | `prepare` default switch |
| `unknown lifecycle field: %s` | `match.go:182` | `prepareLifecycle` default arm |
| `unknown group operator: %s` | `match.go:266` | `prepareGroup` default arm |
| `unsupported operator: %s` | `operators.go:33` | `expandNamed` |

**The two things that must NOT become errors** (spec §4), both deliberate never-match behaviour sitting in FRONT of the error path:

1. A non-temporal operator on `creationDate`/`lastUpdateTime` — `matchTemporalMeta` returned `(false, nil)` before `applyOperator` could raise. The guard is **field-dependent, not operator-dependent**: `creationDate IS_CHANGED` is a never-match leaf, while `state IS_CHANGED` is an error.
2. A leaf whose `ExpandLeaf` fails (operand parsing into no declared type; an untyped field under a comparison operator). `search/service.go` deliberately supplies nil declared types for unknown paths so comparison leaves degrade to non-match.

- [ ] **Step 1: Write the failing structural-error and never-match tests**

Create `internal/match/prepared_test.go`:

```go
package match_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/internal/match"
)

// typed is a FieldTypes that declares every path it is asked about as t.
func typed(t ...spi.DataType) match.FieldTypes {
	return func(string) []spi.DataType { return t }
}

// TestPrepare_StructuralErrors pins the five faults that move from per-row
// evaluation into Prepare. Each keeps its exact message so no error mapping
// moves — these are reported from the condition's own shape now, not from
// which rows happen to be present.
func TestPrepare_StructuralErrors(t *testing.T) {
	tests := []struct {
		name string
		cond predicate.Condition
		want string
	}{
		{
			"function condition",
			&predicate.FunctionCondition{},
			"function conditions not implemented",
		},
		{
			"function condition nested in a group",
			&predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{
				&predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"},
				&predicate.FunctionCondition{},
			}},
			"function conditions not implemented",
		},
		{
			"unknown lifecycle field",
			&predicate.LifecycleCondition{Field: "nosuchfield", OperatorType: "EQUALS", Value: "x"},
			"unknown lifecycle field: nosuchfield",
		},
		{
			"unknown group operator",
			&predicate.GroupCondition{Operator: "NOT", Conditions: []predicate.Condition{
				&predicate.LifecycleCondition{Field: "state", OperatorType: "EQUALS", Value: "active"},
			}},
			"unknown group operator: NOT",
		},
		{
			"lowercase group operator",
			&predicate.GroupCondition{Operator: "or", Conditions: nil},
			"unknown group operator: or",
		},
		{
			"unsupported operator name on a data leaf",
			&predicate.SimpleCondition{JsonPath: "$.amount", OperatorType: "FROBNICATE", Value: 1},
			"unsupported operator: FROBNICATE",
		},
		{
			"IS_CHANGED on a non-temporal meta field",
			&predicate.LifecycleCondition{Field: "state", OperatorType: "IS_CHANGED"},
			"unsupported operator: IS_CHANGED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := match.Prepare(tc.cond, typed(spi.Integer))
			if err == nil {
				t.Fatalf("Prepare() = nil error, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Prepare() error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPrepare_NeverMatchIsNotAnError pins the two cases that sit in FRONT of
// the error path: they are deliberate never-match behaviour and turning either
// into a Prepare error would reject conditions that evaluate cleanly today.
func TestPrepare_NeverMatchIsNotAnError(t *testing.T) {
	meta := spi.EntityMeta{
		State:        "active",
		CreationDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name       string
		cond       predicate.Condition
		fieldTypes match.FieldTypes
		data       []byte
	}{
		{
			// Field-dependent, not operator-dependent: the same operator on
			// `state` IS an error (covered above).
			"IS_CHANGED on a temporal meta field",
			&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "IS_CHANGED"},
			nil,
			[]byte(`{}`),
		},
		{
			"CONTAINS on a temporal meta field",
			&predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2026"},
			nil,
			[]byte(`{}`),
		},
		{
			"comparison leaf on an untyped path",
			&predicate.SimpleCondition{JsonPath: "$.unknown", OperatorType: "GREATER_THAN", Value: 5},
			func(string) []spi.DataType { return nil },
			[]byte(`{"unknown":10}`),
		},
		{
			"operand parses into no declared type",
			&predicate.SimpleCondition{JsonPath: "$.qty", OperatorType: "GREATER_THAN", Value: "not-a-number"},
			typed(spi.Integer),
			[]byte(`{"qty":10}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := match.Prepare(tc.cond, tc.fieldTypes)
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil (never-match, not an error)", err)
			}
			if p.Match(tc.data, meta) {
				t.Error("Match() = true, want false (never-match leaf)")
			}
		})
	}
}

// TestPrepare_ZeroValueNeverMatches pins that the zero Prepared fails closed.
// Prepare returns it alongside an error, and a caller that ignored the error
// must not get a match-all.
func TestPrepare_ZeroValueNeverMatches(t *testing.T) {
	var p match.Prepared
	if p.Match([]byte(`{"a":1}`), spi.EntityMeta{}) {
		t.Error("zero Prepared.Match() = true, want false (fail closed)")
	}
}

// TestPrepare_PreviousTransitionCanonicalisedBeforeFieldCheck pins the
// ordering trap: previousTransition must be rewritten to
// transitionForLatestSave BEFORE the unknown-field check, or a working field
// name starts erroring.
func TestPrepare_PreviousTransitionCanonicalisedBeforeFieldCheck(t *testing.T) {
	cond := &predicate.LifecycleCondition{
		Field: "previousTransition", OperatorType: "EQUALS", Value: "approve",
	}
	p, err := match.Prepare(cond, nil)
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if !p.Match([]byte(`{}`), spi.EntityMeta{TransitionForLatestSave: "approve"}) {
		t.Error("Match() = false, want true")
	}
	if p.Match([]byte(`{}`), spi.EntityMeta{TransitionForLatestSave: "reject"}) {
		t.Error("Match() = true for a non-equal transition, want false")
	}
}

// TestPrepare_ArrayConditionOneExpansionPerPosition pins that an
// ArrayCondition resolves one expansion per NON-NIL position — each position
// is an EQUALS with its own operand, so a single leaf-level expansion cannot
// serve them. Nil positions are skipped, and an all-nil condition matches.
func TestPrepare_ArrayConditionOneExpansionPerPosition(t *testing.T) {
	cond := &predicate.ArrayCondition{
		JsonPath: "$.tags",
		Values:   []any{"red", nil, "blue"},
	}
	p, err := match.Prepare(cond, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !p.Match([]byte(`{"tags":["red","anything","blue"]}`), spi.EntityMeta{}) {
		t.Error("Match() = false, want true: positions 0 and 2 match, 1 is skipped")
	}
	if p.Match([]byte(`{"tags":["red","anything","green"]}`), spi.EntityMeta{}) {
		t.Error("Match() = true, want false: position 2 does not match")
	}

	allNil := &predicate.ArrayCondition{JsonPath: "$.tags", Values: []any{nil, nil}}
	pn, err := match.Prepare(allNil, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !pn.Match([]byte(`{"tags":[]}`), spi.EntityMeta{}) {
		t.Error("Match() = false for an all-nil array condition, want true")
	}
}

// TestPrepare_ArrayWildcardRoutesPerRow pins that array-vs-scalar routing stays
// PER ROW: matchSimple routed on the DATA's shape, not the condition's, so the
// same prepared leaf must handle both an array and a scalar stored value.
func TestPrepare_ArrayWildcardRoutesPerRow(t *testing.T) {
	cond := &predicate.SimpleCondition{
		JsonPath: "$.laureates[*].motivation", OperatorType: "CONTAINS", Value: "peace",
	}
	p, err := match.Prepare(cond, typed(spi.String))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !p.Match([]byte(`{"laureates":[{"motivation":"for war"},{"motivation":"for peace"}]}`), spi.EntityMeta{}) {
		t.Error("Match() = false, want true: one element matches")
	}
	if p.Match([]byte(`{"laureates":[{"motivation":"for war"}]}`), spi.EntityMeta{}) {
		t.Error("Match() = true, want false: no element matches")
	}
	if p.Match([]byte(`{"laureates":[]}`), spi.EntityMeta{}) {
		t.Error("Match() = true for an empty array, want false")
	}
}

var _ = errors.Is // retained for the error-identity assertions added in Task 9
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
go test ./internal/match/... -run 'TestPrepare_' -v
```
Expected: FAIL — `undefined: match.Prepare`, `undefined: match.Prepared`.

- [ ] **Step 3: Write `internal/match/prepared.go`**

```go
package match

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tidwall/gjson"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
)

// prepared.go is the prepare/execute split of the predicate-tree evaluator.
// Prepare resolves everything that depends only on the query — declared types,
// operand parsing, type bucketing, regex compilation, and the gjson path
// conversion — into an immutable tree. Match then walks that tree per row.
//
// Prepare returns an error where the Filter-side spi.Prepare does not, because
// the two consume different input types: spi.FilterOp is a closed enum, while
// predicate.Condition carries free-text operator and field names that can name
// nothing.
//
// Errors are structural properties of the CONDITION, never of the row. Every
// row-dependent failure stays a non-match, exactly as before.

// errUnsupportedOperator marks an operator NAME with no kernel op — a
// structural fault that fails Prepare. It is deliberately distinct from an
// expansion failure (an operand that parses into no declared type), which is a
// leaf that never matches. Collapsing the two would reject conditions that
// evaluate cleanly today.
var errUnsupportedOperator = errors.New("unsupported operator")

// prepKind discriminates the prepared node shapes.
type prepKind int

const (
	// prepNever is the ZERO VALUE on purpose: an unpopulated node, and the
	// zero Prepared that Prepare returns alongside an error, must fail closed.
	prepNever prepKind = iota
	prepGroup
	prepLeaf         // data leaf, addressed by a gjson path
	prepMetaString   // lifecycle leaf on a string-valued meta field
	prepMetaTemporal // lifecycle leaf on creationDate / lastUpdateTime
	prepArray
)

// Prepared is a predicate.Condition compiled for repeated evaluation. Build it
// once per query with Prepare, then call Match once per candidate row. It is
// immutable after Prepare returns and safe to share across goroutines.
//
// The zero Prepared never matches.
type Prepared struct {
	root prepNode
}

type prepNode struct {
	kind prepKind

	// prepGroup
	or       bool
	children []prepNode

	// prepLeaf
	gjsonPath string

	// prepMetaString / prepMetaTemporal
	metaField string

	// prepLeaf / prepMetaString / prepMetaTemporal
	exp spi.Expansion

	// prepArray
	arrayBase string
	positions []arrayPos
}

// arrayPos is one positional EQUALS of an ArrayCondition. Each position has
// its own operand and therefore its own expansion — one expansion per leaf
// would be wrong here.
type arrayPos struct {
	idx int
	exp spi.Expansion
}

// Prepare compiles cond against the declared types fieldTypes supplies.
//
// fieldTypes is consumed during preparation and never retained. Callers whose
// closure mutates captured state (the workflow engine's does) rely on this:
// calling it once, on one goroutine, is what makes the result safe to share.
//
// Declared types are resolved for exactly the leaf set the pre-split evaluator
// resolved — every SimpleCondition and every ArrayCondition, whatever the
// operator, not only the comparison and range leaves that consume them.
// Narrowing the set would stop resolving types for leaves that resolve them
// today, and those are precisely the leaves whose lookup failure currently
// fails a criterion closed. Resolving fewer would be a fail-open movement on
// the write path.
func Prepare(cond predicate.Condition, fieldTypes FieldTypes) (Prepared, error) {
	n, err := prepare(cond, fieldTypes)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{root: n}, nil
}

func prepare(cond predicate.Condition, fieldTypes FieldTypes) (prepNode, error) {
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return prepareSimple(c, fieldTypes)
	case *predicate.LifecycleCondition:
		return prepareLifecycle(c)
	case *predicate.GroupCondition:
		return prepareGroup(c, fieldTypes)
	case *predicate.ArrayCondition:
		return prepareArray(c, fieldTypes)
	case *predicate.FunctionCondition:
		return prepNode{}, fmt.Errorf("function conditions not implemented")
	default:
		return prepNode{}, fmt.Errorf("unknown condition type: %T", cond)
	}
}

// expandNamed maps an operator NAME to its kernel op and expands the operand.
// A name with no kernel op is a structural fault; anything else the kernel
// rejects is an expansion failure the caller turns into a never-match leaf.
func expandNamed(operatorType string, value any, declared []spi.DataType) (spi.Expansion, error) {
	op, ok := opNameToFilterOp(operatorType)
	if !ok {
		return spi.Expansion{}, fmt.Errorf("%w: %s", errUnsupportedOperator, operatorType)
	}
	var values []string
	if op == spi.FilterBetween || op == spi.FilterBetweenInclusive {
		values = betweenBounds(value)
	}
	return spi.ExpandLeaf(op, spi.OperandString(value), values, declared)
}

// leafNode builds a prepared leaf of the given kind, or a never-match node when
// expansion fails. It propagates only the structural fault.
func leafNode(kind prepKind, operatorType string, value any, declared []spi.DataType) (prepNode, error) {
	exp, err := expandNamed(operatorType, value, declared)
	if err != nil {
		if errors.Is(err, errUnsupportedOperator) {
			return prepNode{}, err
		}
		// An operand that parses into no declared type, or a malformed range
		// arity, is a leaf that never matches — the swallowed expansion error
		// the per-row evaluator produced, made explicit.
		return prepNode{kind: prepNever}, nil
	}
	return prepNode{kind: kind, exp: exp}, nil
}

func prepareSimple(c *predicate.SimpleCondition, fieldTypes FieldTypes) (prepNode, error) {
	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(fieldMapKey(c.JsonPath))
	}
	n, err := leafNode(prepLeaf, c.OperatorType, c.Value, declared)
	if err != nil {
		return prepNode{}, err
	}
	if n.kind == prepLeaf {
		n.gjsonPath = convertJSONPath(c.JsonPath)
	}
	return n, nil
}

func prepareLifecycle(c *predicate.LifecycleCondition) (prepNode, error) {
	// Canonicalise BEFORE the field check, or previousTransition — a working
	// field name — starts erroring.
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}

	switch field {
	case "creationDate", "lastUpdateTime":
		// Field-identity guard, sitting in FRONT of the operator check: a
		// temporal field admits only comparison, range and null operators, and
		// anything else is a never-match leaf rather than an error. It must
		// never lexically substring-match the formatted RFC3339 rendering.
		if !isTemporalOperator(c.OperatorType) {
			return prepNode{kind: prepNever}, nil
		}
		n, err := leafNode(prepMetaTemporal, c.OperatorType, c.Value, []spi.DataType{spi.ZonedDateTime})
		if err != nil {
			return prepNode{}, err
		}
		if n.kind == prepMetaTemporal {
			n.metaField = field
		}
		return n, nil

	case "state", "transitionForLatestSave", "transactionId", "id":
		n, err := leafNode(prepMetaString, c.OperatorType, c.Value, []spi.DataType{spi.String})
		if err != nil {
			return prepNode{}, err
		}
		if n.kind == prepMetaString {
			n.metaField = field
		}
		return n, nil

	default:
		// Reported with the ORIGINAL field name, not the canonicalised one.
		return prepNode{}, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}
}

func prepareGroup(c *predicate.GroupCondition, fieldTypes FieldTypes) (prepNode, error) {
	var or bool
	switch c.Operator {
	case "AND":
	case "OR":
		or = true
	default:
		return prepNode{}, fmt.Errorf("unknown group operator: %s", c.Operator)
	}

	n := prepNode{kind: prepGroup, or: or}
	if len(c.Conditions) > 0 {
		n.children = make([]prepNode, 0, len(c.Conditions))
		for _, child := range c.Conditions {
			cn, err := prepare(child, fieldTypes)
			if err != nil {
				return prepNode{}, err
			}
			n.children = append(n.children, cn)
		}
	}
	return n, nil
}

func prepareArray(c *predicate.ArrayCondition, fieldTypes FieldTypes) (prepNode, error) {
	var declared []spi.DataType
	if fieldTypes != nil {
		declared = fieldTypes(arrayElementFieldPath(c.JsonPath))
	}

	n := prepNode{kind: prepArray, arrayBase: convertJSONPath(c.JsonPath)}
	for i, expected := range c.Values {
		if expected == nil {
			continue // nil positions are skipped
		}
		// EQUALS always maps to a kernel op, so expandNamed can only fail here
		// with an expansion error — which made the whole array condition false
		// for every row, since matchArray returned on the first failing
		// position regardless of data.
		exp, err := expandNamed("EQUALS", expected, declared)
		if err != nil {
			return prepNode{kind: prepNever}, nil
		}
		n.positions = append(n.positions, arrayPos{idx: i, exp: exp})
	}
	return n, nil
}

// Match reports whether the entity satisfies the prepared condition. It cannot
// fail: every way this evaluation could error is a structural property of the
// condition and was already reported by Prepare.
func (p Prepared) Match(data []byte, meta spi.EntityMeta) bool {
	return p.root.match(data, meta)
}

func (n *prepNode) match(data []byte, meta spi.EntityMeta) bool {
	switch n.kind {
	case prepGroup:
		if n.or {
			for i := range n.children {
				if n.children[i].match(data, meta) {
					return true
				}
			}
			return false
		}
		for i := range n.children {
			if !n.children[i].match(data, meta) {
				return false
			}
		}
		return true

	case prepLeaf:
		result := gjson.GetBytes(data, n.gjsonPath)
		// Routing on the DATA's shape, not the condition's, stays per row: an
		// array-wildcard path yields an array for one entity and nothing for
		// the next. Both branches consume the same expansion, which is why
		// hoisting the expansion is safe while the routing is not.
		if result.IsArray() {
			matched := false
			result.ForEach(func(_, v gjson.Result) bool {
				if spi.EvalLeaf(n.exp, v) {
					matched = true
					return false // short-circuit
				}
				return true
			})
			return matched
		}
		return spi.EvalLeaf(n.exp, result)

	case prepMetaString:
		return spi.EvalLeaf(n.exp, metaStringResult(n.metaField, meta))

	case prepMetaTemporal:
		return spi.EvalLeaf(n.exp, metaTemporalResult(n.metaField, meta))

	case prepArray:
		for _, pos := range n.positions {
			r := gjson.GetBytes(data, fmt.Sprintf("%s.%d", n.arrayBase, pos.idx))
			if !spi.EvalLeaf(pos.exp, r) {
				return false
			}
		}
		return true
	}

	// prepNever, and any unpopulated node.
	return false
}

// metaStringResult wraps a string-valued meta field in a one-key document and
// reads it back, so meta string comparison goes through the same kernel as
// data leaves with a declared String type.
func metaStringResult(field string, meta spi.EntityMeta) gjson.Result {
	var v string
	switch field {
	case "state":
		v = meta.State
	case "transitionForLatestSave":
		v = meta.TransitionForLatestSave
	case "transactionId":
		v = meta.TransactionID
	case "id":
		v = meta.ID
	}
	return gjson.Get(fmt.Sprintf(`{"v":%q}`, v), "v")
}

// metaTemporalResult bridges a stored meta instant to a gjson.Result. A zero
// time (unset) bridges to an ABSENT result, not to a comparable ~year-1
// instant: IS_NULL matches it and every binary op, negatives included,
// non-matches under the kernel's null uniformity.
func metaTemporalResult(field string, meta spi.EntityMeta) gjson.Result {
	var t time.Time
	switch field {
	case "creationDate":
		t = meta.CreationDate
	case "lastUpdateTime":
		t = meta.LastModifiedDate
	}
	if t.IsZero() {
		return gjson.Result{}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return gjson.Result{}
	}
	return gjson.ParseBytes(b)
}
```

- [ ] **Step 4: Run to verify the tests pass**

```bash
go test ./internal/match/... -run 'TestPrepare_' -v
```
Expected: PASS, every subtest.

- [ ] **Step 5: Run the whole match package and commit**

```bash
go test ./internal/match/... && go vet ./internal/match/...
```
Expected: PASS — the old `match.Match` is untouched.

```bash
git add internal/match/prepared.go internal/match/prepared_test.go
git commit -m "feat(search): add match.Prepared — the prepare/execute split of the predicate evaluator

The five structural errors move from per-row evaluation into Prepare, so a
malformed condition is reported from its own shape rather than from which rows
happen to be present. Never-match behaviour that sat in front of the error path
stays never-match: a non-temporal operator on a temporal meta field, and a leaf
whose operand parses into no declared type.

Additive: match.Match is untouched and remains the equivalence reference.

Refs #30"
```

---

## Task 4: cyoda-go — the merge gate for `match.Prepare`

Same shape as Task 2: a frozen copy of the old evaluator versus the prepared one, over a randomised corpus.

**The generator is mode-driven and feeds two separate tests.** `genValid` emits only
conditions both evaluators accept; `genInvalid` emits conditions carrying exactly one
structural fault. Do not mix the two in one corpus — a randomly-seeded fault makes
coverage a lottery and makes a failure ambiguous about which property broke.

The gate has two jobs, and one corpus cannot do both:

- **Answer equivalence** needs well-formed conditions. It asserts the two evaluators
  return the same boolean, and neither can error — so it `t.Fatalf`s on any error
  rather than carrying a `continue` that quietly swallows one.
- **Fault reporting** needs malformed conditions. It asserts `Prepare` reports exactly
  the fault the condition carries, wherever that fault sits: standalone, first in an
  AND, second in an AND whose first child is false (the short-circuit-hides-it case),
  and nested two groups deep. Position must not change the outcome, which is the point
  of varying it.

Its reference is the **bare faulty leaf**, not the wrapped condition. A fault behind an
always-false branch is legitimately unreachable for every document, so asserting the
row-walk reaches it would fail on correct code; comparing against the bare leaf
decouples the assertion from reachability.

An earlier draft of this task asked for one corpus that was well-formed *and* exercised
the fault path. Those cannot both hold — structural errors only come from malformed
input — so the fault check was unreachable dead code that looked rigorous and never ran.

**Files:**
- Create: `internal/match/prepared_equivalence_test.go` (package `match` — the reference needs `applyOperator`, `matchSimple` and friends while they still exist, and must keep working after Task 11 deletes them)

**Interfaces:**
- Consumes: `Prepare`, `Match` (Task 3); `convertJSONPath`, `fieldMapKey`, `arrayElementFieldPath`, `isTemporalOperator`, `opNameToFilterOp`, `betweenBounds`, `spi.EvalLeafString`, `spi.OperandString`.
- Produces: `func frozenMatch(cond predicate.Condition, data []byte, meta spi.EntityMeta, fieldTypes FieldTypes) (bool, error)` — the frozen reference.

- [ ] **Step 1: Write the frozen reference**

Create `internal/match/prepared_equivalence_test.go`, package `match`. Copy the bodies of `Match`, `matchSimple`, `matchArrayWildcard`, `matchLifecycle`, `applyStringLifecycle`, `matchTemporalMeta`, `matchGroup`, `matchArray` and `applyOperator` **verbatim** from `match.go` and `operators.go`, renaming each with a `frozen` prefix and rewiring the internal calls to the frozen names. It must depend only on things that survive Task 11: `convertJSONPath`, `fieldMapKey`, `arrayElementFieldPath`, `isTemporalOperator`, `opNameToFilterOp`, `betweenBounds`, and the SPI kernel.

**One deliberate exception:** `frozenApplyOperator` must call `spi.ExpandLeaf` + `spi.EvalLeaf` rather than `spi.EvalLeafString`, because Task 12 deletes `EvalLeafString`. Inline the fast path too, so the reference keeps exercising it:

```go
func frozenApplyOperator(operatorType string, actual gjson.Result, expected any, declared []spi.DataType) (bool, error) {
	op, ok := opNameToFilterOp(operatorType)
	if !ok {
		return false, fmt.Errorf("unsupported operator: %s", operatorType)
	}
	var values []string
	if op == spi.FilterBetween || op == spi.FilterBetweenInclusive {
		values = betweenBounds(expected)
	}
	exp, err := spi.ExpandLeaf(op, spi.OperandString(expected), values, declared)
	if err != nil {
		return false, nil // swallowed to a per-entity non-match, as before
	}
	return spi.EvalLeaf(exp, actual), nil
}
```

Header comment for the file:

```go
// The merge gate for the predicate-evaluator prepare/execute split.
//
// The frozen* functions below are a verbatim copy of the pre-split evaluator,
// taken before it was deleted. They are a COPY on purpose: they must keep
// answering the old way after the originals are gone.
//
// Do not "simplify" them by calling live code, and do not update them when the
// live evaluator changes. If they and Prepare disagree, the live code is wrong.
```

- [ ] **Step 2: Write the generator and the equivalence test**

Append to the same file:

```go
var genOperators = []string{
	"EQUALS", "NOT_EQUAL", "GREATER_THAN", "LESS_THAN", "GREATER_OR_EQUAL", "LESS_OR_EQUAL",
	"CONTAINS", "STARTS_WITH", "ENDS_WITH", "LIKE", "MATCHES_PATTERN",
	"NOT_CONTAINS", "NOT_STARTS_WITH", "NOT_ENDS_WITH",
	"IEQUALS", "INOT_EQUAL", "ICONTAINS", "INOT_CONTAINS",
	"ISTARTS_WITH", "INOT_STARTS_WITH", "IENDS_WITH", "INOT_ENDS_WITH",
	"IS_NULL", "NOT_NULL", "BETWEEN", "BETWEEN_INCLUSIVE",
}

// genMetaFields is the canonical meta vocabulary matchLifecycle handles. The
// generator stays inside it: an unknown field is a structural error, covered
// by the hand-written table in Task 3, and the generator emits only
// well-formed conditions.
var genMetaFields = []string{
	"state", "id", "transactionId", "transitionForLatestSave",
	"previousTransition", "creationDate", "lastUpdateTime",
}

var genJSONPaths = []string{
	"$.name", "$.qty", "$.price", "$.flag", "$.uid", "$.when",
	"$.missing", "$.nested.inner", "name", "$.laureates[*].motivation", "$.tags",
}

var genValues = []any{
	"Alice", "alice", "", "A%", "A.*", "%ice",
	"30", 30, 30.5, -5, 0, true, false,
	"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	"2024-01-01T00:00:00Z", "2024-06-01", "2024",
	[]any{"1", "100"}, []any{"a", "z"}, []any{"1"},
	"não",
}

var genFieldTypeSets = [][]spi.DataType{
	{spi.String}, {spi.Integer}, {spi.Long}, {spi.UnboundDecimal}, {spi.Double},
	{spi.Boolean}, {spi.UUIDType}, {spi.ZonedDateTime}, {spi.LocalDate},
	{spi.Integer, spi.String}, {spi.Double, spi.Integer}, nil,
}

var genEqDocs = []string{
	`{"name":"Alice","qty":30,"price":12.78,"flag":true,"uid":"6ba7b810-9dad-11d1-80b4-00c04fd430c8","when":"2024-01-01T00:00:00Z","nested":{"inner":"deep"},"tags":["red","blue"],"laureates":[{"motivation":"for peace"}]}`,
	`{"name":"alice","qty":31,"when":"2024-06-01","tags":["red"],"laureates":[]}`,
	`{"name":null,"qty":null,"tags":[]}`,
	`{}`,
	`{"name":30,"qty":"30"}`,
	`{"laureates":[{"motivation":"for war"},{"motivation":"for peace"}]}`,
}

func genCondition(r *rand.Rand, depth int) predicate.Condition {
	if depth <= 0 || r.Intn(3) == 0 {
		switch r.Intn(3) {
		case 0:
			return &predicate.LifecycleCondition{
				Field:        genMetaFields[r.Intn(len(genMetaFields))],
				OperatorType: genOperators[r.Intn(len(genOperators))],
				Value:        genValues[r.Intn(len(genValues))],
			}
		case 1:
			return &predicate.ArrayCondition{
				JsonPath: "$.tags",
				Values:   []any{genValues[r.Intn(len(genValues))], nil},
			}
		default:
			return &predicate.SimpleCondition{
				JsonPath:     genJSONPaths[r.Intn(len(genJSONPaths))],
				OperatorType: genOperators[r.Intn(len(genOperators))],
				Value:        genValues[r.Intn(len(genValues))],
			}
		}
	}
	op := "AND"
	if r.Intn(2) == 0 {
		op = "OR"
	}
	g := &predicate.GroupCondition{Operator: op}
	for i := 0; i < r.Intn(4); i++ {
		g.Conditions = append(g.Conditions, genCondition(r, depth-1))
	}
	return g
}

// TestPrepare_EquivalentToFrozenMatch is the merge gate for the predicate
// evaluator. Exact agreement on every well-formed condition.
//
// Where the frozen reference ERRORS, Prepare must error too — that is the
// declared change made testable: the error moves from evaluation time to
// preparation time, but a condition that could error on SOME row must not
// become silently clean, and one that never errored must not start.
func TestPrepare_EquivalentToFrozenMatch(t *testing.T) {
	const cases = 200000
	r := rand.New(rand.NewSource(0x30C0DE))

	metas := []spi.EntityMeta{
		{ID: "ent-1", State: "active", TransactionID: "tx-1", TransitionForLatestSave: "approve",
			CreationDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			LastModifiedDate: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "ent-2", State: ""},
		{},
	}

	for i := 0; i < cases; i++ {
		cond := genCondition(r, 3)
		data := []byte(genEqDocs[r.Intn(len(genEqDocs))])
		meta := metas[r.Intn(len(metas))]
		types := genFieldTypeSets[r.Intn(len(genFieldTypeSets))]
		fieldTypes := func(string) []spi.DataType { return types }

		wantMatch, wantErr := frozenMatch(cond, data, meta, fieldTypes)
		prepared, prepErr := Prepare(cond, fieldTypes)

		if wantErr != nil {
			// The frozen evaluator reached the fault on THIS row. Prepare must
			// have reported it from the condition's shape.
			if prepErr == nil {
				t.Fatalf("case %d: frozen errored %q but Prepare succeeded\n  cond=%#v\n  data=%s",
					i, wantErr, cond, data)
			}
			if prepErr.Error() != wantErr.Error() {
				t.Fatalf("case %d: error text moved: frozen=%q prepared=%q", i, wantErr, prepErr)
			}
			continue
		}

		if prepErr != nil {
			// Prepare found a fault the frozen walk did not REACH on this row.
			// That is the declared change — but only for a condition that
			// genuinely carries the fault, so re-run the frozen walk over every
			// document and require at least one to surface the same error.
			if !frozenErrorsOnSomeDoc(cond, meta, fieldTypes, prepErr) {
				t.Fatalf("case %d: Prepare errored %q but no document makes the frozen evaluator raise it\n  cond=%#v",
					i, prepErr, cond)
			}
			continue
		}

		if got := prepared.Match(data, meta); got != wantMatch {
			t.Fatalf("DIVERGENCE at case %d\n  prepared=%v frozen=%v\n  cond=%#v\n  data=%s\n  meta=%+v\n  types=%v",
				i, got, wantMatch, cond, data, meta, types)
		}
	}
}

// frozenErrorsOnSomeDoc reports whether the frozen evaluator raises want for at
// least one document in the corpus — i.e. whether the fault Prepare reported is
// genuinely carried by the condition rather than invented.
func frozenErrorsOnSomeDoc(cond predicate.Condition, meta spi.EntityMeta, fieldTypes FieldTypes, want error) bool {
	for _, d := range genEqDocs {
		if _, err := frozenMatch(cond, []byte(d), meta, fieldTypes); err != nil &&
			err.Error() == want.Error() {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run the gate**

```bash
go test ./internal/match/... -run 'TestPrepare_EquivalentToFrozenMatch' -v
```
Expected: PASS.

**If it FAILS with "Prepare errored but no document makes the frozen evaluator raise it":** that is a real defect — `Prepare` is rejecting something the old evaluator never would, on any data. Most likely `leafNode` is propagating an expansion error instead of returning a never-match node.

- [ ] **Step 4: One-off widened run, then commit**

Use the same env-var override Task 2 introduced (`MATCH_EQUIV_CASES` /
`MATCH_EQUIV_SEED`, defaults 200000 / 0x30C0DE), so the widened run explores
genuinely distinct corpora rather than re-running one:

```bash
for s in 1 2 3 4 5; do
  MATCH_EQUIV_SEED=$s MATCH_EQUIV_CASES=400000 go test ./internal/match/ -run 'TestPrepare_EquivalentToFrozenMatch' || break
done
go vet ./internal/match/...
git add internal/match/prepared_equivalence_test.go
git commit -m "test(search): merge gate — match.Prepare/Match must equal the frozen pre-split evaluator

200k generated (condition, entity) pairs. Where the frozen walk reaches a
structural fault, Prepare must report the same text; where Prepare reports one
the walk did not reach, some document must make the walk reach it — which is
exactly the declared change, made testable rather than assumed.

Refs #30"
```

---

## Task 5: memory plugin — hoist `Prepare` above every row loop

**Files:**
- Modify: `plugins/memory/searcher.go` (`Search` in-tx RYW branch; `matchSortBounded` at `:176-190`)
- Modify: `plugins/memory/grouped_stats.go` (`Iterate` at `:63`; `memoryIter` at `:205-222`; `GroupedAggregate` at `:246-270`; delete `msMatchFilter` at `:494-503`)
- Modify: `plugins/memory/searcher_test.go`, `plugins/memory/grouped_stats_test.go`

**Interfaces:**
- Consumes: `spi.Prepare`, `spi.PreparedFilter`, `(spi.PreparedFilter).Match` (Task 1).
- Produces: `func matchSortBounded(pf spi.PreparedFilter, rows []*spi.Entity, order []spi.OrderSpec, limit int) ([]*spi.Entity, error)` — signature change, three callers in this file.

- [ ] **Step 1: Change `matchSortBounded` to take a prepared filter**

In `plugins/memory/searcher.go`, replace the function and its doc comment:

```go
// matchSortBounded filters rows with a prepared filter, orders with
// spi.LessByOrder, and enforces the bounded-or-fail cap: limit > 0 means the
// whole matched set must fit, and a larger match set is an error rather than a
// truncated prefix. limit <= 0 is unbounded. Used by the non-tx and in-tx PIT
// branches; the RYW overlay branch gets the same bound from spi.MergeBounded.
//
// It takes an already-prepared filter so the caller pays the operand parse,
// type bucketing and regex compilation once per query rather than once per row.
func matchSortBounded(pf spi.PreparedFilter, rows []*spi.Entity, order []spi.OrderSpec, limit int) ([]*spi.Entity, error) {
	filtered := make([]*spi.Entity, 0, len(rows))
	for _, e := range rows {
		if pf.Match(e.Data, e.Meta) {
			filtered = append(filtered, e)
			// Short-circuit before sorting: the result is an error either way.
			if limit > 0 && len(filtered) > limit {
				return nil, fmt.Errorf("search: more than %d matches: %w", limit, spi.ErrSearchResultLimitExceeded)
			}
		}
	}
	sortByOrder(filtered, order)
	return filtered, nil
}
```

- [ ] **Step 2: Prepare once at the top of `Search`**

In `plugins/memory/searcher.go`, immediately after the `modelRef` / `tx` lines at the top of `Search`, add:

```go
	// Prepare once per query. Every branch below evaluates the same filter, so
	// a single prepared value serves the non-tx scan, the PIT scan, and both
	// loops of the read-your-own-writes overlay.
	pf := spi.Prepare(filter)
```

Then replace the three per-row sites:

- `return matchSortBounded(filter, committed, opts.OrderBy, opts.Limit)` (non-tx branch) → `return matchSortBounded(pf, committed, opts.OrderBy, opts.Limit)`
- the same line in the in-tx PIT branch → `matchSortBounded(pf, committed, opts.OrderBy, opts.Limit)`
- `if spi.MatchFilter(filter, e.Data, e.Meta) {` in the `filteredCommitted` loop → `if pf.Match(e.Data, e.Meta) {`
- `if spi.MatchFilter(filter, e.Data, e.Meta) {` in the `tx.Buffer` adds loop → `if pf.Match(e.Data, e.Meta) {`

- [ ] **Step 3: Move the prepared filter into `memoryIter`**

In `plugins/memory/grouped_stats.go`, change the iterator struct field and its construction:

```go
	return &memoryIter{
		snapshot: snapshot,
		prepared: spi.Prepare(filter),
		ctx:      ctx,
	}, nil
```

Change the struct field `filter spi.Filter` to `prepared spi.PreparedFilter`, and in `Next()`:

```go
		if !it.prepared.Match(e.Data, e.Meta) {
			continue
		}
```

- [ ] **Step 4: Prepare above `GroupedAggregate`'s loop and delete `msMatchFilter`**

In `GroupedAggregate`, immediately after the `buildSnapshot` call:

```go
	// Prepared once for the whole aggregation — the filter does not vary by row.
	pf := spi.Prepare(filter)

	buckets := make(map[string]*memBucket)
	for _, e := range snapshot {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !pf.Match(e.Data, e.Meta) {
			continue
		}
```

Delete `msMatchFilter` (`:494-503`) entirely — both call sites are gone, and keeping a per-row shim is exactly what the change removes. Move the part of its doc comment that still says something true onto `Iterate`:

```go
// Iterate implements spi.Iterable. The filter is prepared once here and
// evaluated per row by the shared kernel — the same evaluator the sqlite
// (plugins/sqlite/post_filter.go) and postgres (plugins/postgres/
// grouped_stats.go) backends use, so all three backends agree bit-for-bit on
// filter semantics, including CoerceTemporal and the canonical client-name meta
// vocabulary. A zero-value filter prepares to match-all, matching the
// historical "no filter" contract.
```

- [ ] **Step 5: Migrate this plugin's tests**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter/plugins/memory
grep -n 'spi\.MatchFilter\|matchSortBounded(' *_test.go
```

Rewrite each `spi.MatchFilter(f, data, meta)` as `spi.Prepare(f).Match(data, meta)` and each `matchSortBounded(f, ...)` as `matchSortBounded(spi.Prepare(f), ...)`. **Expectations do not change** — if a test needs a different expected value, that is a defect in the migration, not a test that needed updating.

- [ ] **Step 6: Run the memory plugin suite**

```bash
go test ./... -v 2>&1 | tail -30
go vet ./...
```
Expected: PASS, no expectation edits.

- [ ] **Step 7: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
git add plugins/memory/
git commit -m "perf(memory): prepare the search filter once per query, not once per row

matchSortBounded, memoryIter and GroupedAggregate take an already-prepared
filter; Search prepares once and serves all four of its row loops from it. The
msMatchFilter per-row shim is deleted rather than re-pointed — swapping its body
would have left the compile inside the loop.

Refs #30"
```

---

## Task 6: sqlite plugin — `preparedPostFilter` and the row loops

The delicate part: `plan.postFilter`'s **nil-ness** drives `LIMIT` pushdown, native `GROUP BY`, and the scan budget. Collapsing it to a zero `PreparedFilter` (which means match-all, not absent) breaks all three silently, with every result still correct.

**Files:**
- Modify: `plugins/sqlite/query_planner.go` (`sqlPlan` at `:25-29`; `planQuery`'s single return at `:83`)
- Modify: `plugins/sqlite/post_filter.go` (both functions)
- Modify: `plugins/sqlite/searcher.go` (`:130-131`, `:265-266`, `:292`)
- Modify: `plugins/sqlite/grouped_stats.go` (`sqliteSliceIter` at `:90`/`:140-152`; `sqliteIter` at `:129-133`/`:234-241`)
- Modify: `plugins/sqlite/query_planner_test.go`, `searcher_tx_test.go`, `grouped_stats_test.go`, `soundness_property_test.go`
- Create: `plugins/sqlite/prepared_postfilter_test.go`

**Interfaces:**
- Consumes: `spi.Prepare`, `spi.PreparedFilter`.
- Produces:
  - `sqlPlan.preparedPostFilter *spi.PreparedFilter` — non-nil **exactly when** `postFilter` is non-nil
  - `func EvaluateFilter(p spi.PreparedFilter, entity *spi.Entity) bool` (error return dropped — it was always nil)
  - `func evaluateFilter(p spi.PreparedFilter, entity *spi.Entity) bool`

- [ ] **Step 1: Write the failing `postFilter`-absence test**

Create `plugins/sqlite/prepared_postfilter_test.go`:

```go
package sqlite

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestPlanQuery_PreparedPostFilterMatchesNilness pins the invariant the whole
// representation choice rests on: preparedPostFilter is non-nil EXACTLY when
// postFilter is non-nil.
//
// Absence must stay a nil pointer. A zero spi.PreparedFilter means match-all,
// not "no filter", so collapsing the two would silently cost LIMIT pushdown,
// disable native GROUP BY, and arm the scan budget on every query — with every
// returned result still correct, so nothing would fail loudly.
func TestPlanQuery_PreparedPostFilterMatchesNilness(t *testing.T) {
	cases := []struct {
		name   string
		filter spi.Filter
	}{
		{"exact leaf, no residual", spi.Filter{
			Op: spi.FilterIsNull, Source: spi.SourceData, Path: "name"}},
		{"inexact leaf forces a full re-check", spi.Filter{
			Op: spi.FilterEq, Source: spi.SourceData, Path: "name",
			Value: "Alice", Declared: []spi.DataType{spi.String}}},
		{"unpushable leaf becomes a residual", spi.Filter{
			Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "name",
			Value: "A.*", Declared: []spi.DataType{spi.String}}},
		{"mixed AND", spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
			{Op: spi.FilterIsNull, Source: spi.SourceData, Path: "a"},
			{Op: spi.FilterMatchesRegex, Source: spi.SourceData, Path: "b",
				Value: "x", Declared: []spi.DataType{spi.String}},
		}}},
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
		{"explicit empty OR", spi.Filter{Op: spi.FilterOr}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planQuery(tc.filter)
			if (plan.postFilter == nil) != (plan.preparedPostFilter == nil) {
				t.Fatalf("nil-ness diverged: postFilter==nil is %v, preparedPostFilter==nil is %v",
					plan.postFilter == nil, plan.preparedPostFilter == nil)
			}
			if plan.postFilter == nil {
				return
			}
			// And the prepared one must agree with the residual it stands for.
			data := []byte(`{"name":"Alice","a":null,"b":"x"}`)
			want := spi.Prepare(*plan.postFilter).Match(data, spi.EntityMeta{})
			if got := plan.preparedPostFilter.Match(data, spi.EntityMeta{}); got != want {
				t.Errorf("preparedPostFilter.Match = %v, want %v (the residual it stands for)", got, want)
			}
		})
	}
}

// TestSearch_MatchAllLeavesNoResidual pins the three
// consequences of postFilter absence, for BOTH spellings of match-all: the
// zero Filter{} and the explicit empty AND that ConditionToFilter emits for a
// nil condition. The two took different branches historically and must not
// drift apart again.
func TestSearch_MatchAllLeavesNoResidual(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter spi.Filter
	}{
		{"zero filter", spi.Filter{}},
		{"explicit empty AND", spi.Filter{Op: spi.FilterAnd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var plan sqlPlan
			if tc.filter.Op != "" {
				plan = planQuery(tc.filter)
			}
			if plan.postFilter != nil {
				t.Fatalf("postFilter = %+v, want nil: a match-all query has nothing to post-filter, "+
					"and a non-nil residual costs LIMIT pushdown, disables native GROUP BY, "+
					"and arms the scan budget", plan.postFilter)
			}
			if plan.preparedPostFilter != nil {
				t.Fatalf("preparedPostFilter = %+v, want nil", plan.preparedPostFilter)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter/plugins/sqlite
go test ./... -run 'TestPlanQuery_PreparedPostFilterMatchesNilness|TestSearch_MatchAllLeavesNoResidual' -v
```
Expected: FAIL — `plan.preparedPostFilter undefined`.

- [ ] **Step 3: Add the field and populate it at the single return**

In `plugins/sqlite/query_planner.go`, extend the struct and its doc:

```go
type sqlPlan struct {
	where      string
	args       []any
	postFilter *spi.Filter
	// preparedPostFilter is postFilter compiled for per-row evaluation. It is
	// non-nil EXACTLY when postFilter is non-nil.
	//
	// postFilter itself stays a *spi.Filter and stays the field the planner's
	// own predicates read, because its NIL-NESS is what gates LIMIT pushdown,
	// native GROUP BY and the scan budget. A zero spi.PreparedFilter means
	// match-all, not absent, so replacing the field outright — or pairing a
	// value with a bool — would put that invariant back in play at every
	// consumer. Row loops read this field; planner decisions read postFilter.
	preparedPostFilter *spi.PreparedFilter
}
```

At the end of `planQuery`, replace `return plan` with:

```go
	// Single population point, so the nil-ness invariant cannot drift between
	// the branches above.
	if plan.postFilter != nil {
		p := spi.Prepare(*plan.postFilter)
		plan.preparedPostFilter = &p
	}
	return plan
}
```

- [ ] **Step 4: Run the new tests to verify they pass**

```bash
go test ./... -run 'TestPlanQuery_PreparedPostFilterMatchesNilness|TestSearch_MatchAllLeavesNoResidual' -v
```
Expected: PASS.

- [ ] **Step 5: Convert `post_filter.go` to the prepared form**

Replace the whole file body:

```go
package sqlite

import (
	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// EvaluateFilter is a public wrapper around evaluateFilter exposed so that
// cross-module parity tests (against internal/match.Prepare) can pin the
// contract that grouped-stats / streaming-tally must produce the same boolean
// as the sqlite post-filter step for any (filter, entity) tuple. NOT intended
// for hot-path use by other code — call sites within this plugin should keep
// using evaluateFilter directly.
func EvaluateFilter(p spi.PreparedFilter, entity *spi.Entity) bool {
	return evaluateFilter(p, entity)
}

// evaluateFilter evaluates an already-prepared filter against an entity's data
// in Go, for residual (non-pushable) predicates. It takes a prepared filter
// rather than a spi.Filter so the operand parse, type bucketing and regex
// compilation happen once per query at the plan site, not once per row here.
//
// Delegates to the canonical cross-backend kernel — see spi.Prepare for why
// this plugin must never grow an evaluator of its own.
func evaluateFilter(p spi.PreparedFilter, entity *spi.Entity) bool {
	return p.Match(entity.Data, entity.Meta)
}
```

The error return is dropped: it was unconditionally nil, and every caller had dead error-handling around it.

- [ ] **Step 6: Re-point sqlite's four row loops**

`plugins/sqlite/searcher.go`, the `searchCommitted` loop (`:130-137`):

```go
		if plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e) {
			continue
		}
```

`plugins/sqlite/searcher.go`, the `next` closure inside the in-tx branch (`:265-272`):

```go
			if plan.preparedPostFilter != nil && !evaluateFilter(*plan.preparedPostFilter, e) {
				continue
			}
```

`plugins/sqlite/searcher.go`, the in-tx buffered-adds loop (`:292`) — prepare **above** the loop, next to where `plan` is built:

```go
	// The buffered own-writes are matched against the FULL original filter (not
	// the residual), so they need their own prepared value. Prepared once,
	// above the loop.
	pf := spi.Prepare(filter)
	...
		if pf.Match(e.Data, e.Meta) {
			adds = append(adds, copyEntity(e))
		}
```

Place the `pf := spi.Prepare(filter)` line immediately after the `var plan sqlPlan` / `planQuery` block at the top of that function, **outside** the `func() error { ... }()` closure and outside the `for id, e := range tx.Buffer` loop.

The scan-budget checks (`plan.postFilter != nil && scanned >= …`) keep reading `plan.postFilter`. Do not switch them to the prepared field — they are planner decisions, not row evaluation.

- [ ] **Step 7: Re-point sqlite's two iterators**

`plugins/sqlite/grouped_stats.go`, `sqliteSliceIter` — replace `filter spi.Filter` with `prepared spi.PreparedFilter`, construct with `prepared: spi.Prepare(filter)`, and in `Next()` replace the whole `if it.filter.Op != "" { … }` block with:

```go
		// A zero-value filter prepares to match-all, so no Op guard is needed
		// here any more: spi.Prepare handles the root asymmetry.
		if !it.prepared.Match(e.Data, e.Meta) {
			continue
		}
```

`sqliteIter` — replace `postFilter *spi.Filter` with `preparedPostFilter *spi.PreparedFilter`, construct with `preparedPostFilter: plan.preparedPostFilter`, and in `Next()`:

```go
		if it.preparedPostFilter != nil && !evaluateFilter(*it.preparedPostFilter, e) {
			continue
		}
```

- [ ] **Step 8: Migrate sqlite's tests**

```bash
grep -n 'spi\.MatchFilter\|EvaluateFilter(\|evaluateFilter(' *_test.go
```

`soundness_property_test.go` has ten sites and is the most valuable of them — it is the property test asserting the pushed SQL is a sound superset of the kernel. Rewrite each `spi.MatchFilter(f, e.Data, e.Meta)` as `spi.Prepare(f).Match(e.Data, e.Meta)`; where it appears inside a row loop, hoist the `spi.Prepare(f)` above the loop so the test exercises the shape production does. Drop the now-absent error return at every `evaluateFilter` / `EvaluateFilter` call.

Expectations do not change.

- [ ] **Step 9: Run the sqlite suite and commit**

```bash
go test ./... 2>&1 | tail -30
go vet ./...
```
Expected: PASS.

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
git add plugins/sqlite/
git commit -m "perf(sqlite): prepare the residual post-filter once per plan, not once per row

sqlPlan gains preparedPostFilter, non-nil exactly when postFilter is non-nil and
populated at planQuery's single return. postFilter itself is untouched: its
nil-ness gates LIMIT pushdown, native GROUP BY and the scan budget, and a zero
PreparedFilter means match-all rather than absent. Pinned by a test for both
spellings of match-all.

evaluateFilter drops an error return that was unconditionally nil.

Refs #30"
```

---

## Task 7: postgres plugin — `preparedPostFilter` and `postgresIter`'s two construction sites

Structurally the twin of Task 6, with two differences: postgres has **no scan budget**, and `postgresIter` is built from **two** sites — `grouped_stats.go:89-92` and `searcher.go:220`. Missing the second leaves `Search`'s residual path compiling per row.

**Files:**
- Modify: `plugins/postgres/query_planner.go` (`sqlPlan` at `:28-32`; `planQuery`'s single return)
- Modify: `plugins/postgres/grouped_stats.go` (`postgresIter` struct; construction at `:89-92`; `Next()` at `:171-180`; `evalPostFilter` at `:444-447`)
- Modify: `plugins/postgres/searcher.go` (`postgresIter` construction at `:220`)
- Modify: `plugins/postgres/query_planner_test.go`, `grouped_stats_test.go`, `searcher_tx_test.go`, `soundness_property_test.go`
- Create: `plugins/postgres/prepared_postfilter_test.go`

**Interfaces:**
- Consumes: `spi.Prepare`, `spi.PreparedFilter`.
- Produces: `sqlPlan.preparedPostFilter *spi.PreparedFilter`; `func evalPostFilter(p spi.PreparedFilter, entity *spi.Entity) bool`.

- [ ] **Step 1: Write the failing nil-ness test**

Create `plugins/postgres/prepared_postfilter_test.go` with the same two tests as Task 6 Step 1, package `postgres`, with the scan-budget wording dropped from the second test's failure message (postgres has none — `searcher.go:30-35`) and `LIMIT` pushdown plus native `GROUP BY` kept:

```go
			if plan.postFilter != nil {
				t.Fatalf("postFilter = %+v, want nil: a match-all query has nothing to post-filter, "+
					"and a non-nil residual costs LIMIT pushdown and disables native GROUP BY", plan.postFilter)
			}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter/plugins/postgres
go test ./... -run 'TestPlanQuery_PreparedPostFilterMatchesNilness|TestSearch_MatchAll' -v
```
Expected: FAIL — `plan.preparedPostFilter undefined`.

- [ ] **Step 3: Add the field, populate it, convert `evalPostFilter`**

Add `preparedPostFilter *spi.PreparedFilter` to `sqlPlan`, mirroring sqlite's **field** — name, type, shape and population point — since that is what keeps the two planners legible as twins and what parity depends on. Populate at `planQuery`'s single return with the identical block.

**The prose is not mirrored where the backends differ.** sqlite's comment names the scan budget among the things `postFilter`'s nil-ness gates; postgres has none (`searcher.go:30-35`). Postgres's copy states what it actually gates: `LIMIT` pushdown (`searcher.go:211`), native `GROUP BY` (`grouped_stats.go:223`), and the collection-loop shape (`searcher.go:226`).

Convert `evalPostFilter`, preserving its existing doc paragraph about being given `entity.Data` rather than the raw JSONB document — that reasoning is unrelated to this change and still load-bearing:

```go
// evalPostFilter evaluates an already-prepared residual filter against a
// decoded entity in Go. It takes a prepared filter so the operand parse, type
// bucketing and regex compilation happen once per query at the plan site rather
// than once per row here.
//
// It is given entity.Data, not the raw JSONB document. The stored document
// carries a "_meta" block this plugin merges in (marshalEntityDoc); passing it
// would make storage-layer meta a matchable SourceData path here and nowhere
// else, since memory and sqlite hold domain data and meta apart. Meta stays
// reachable through Source: SourceMeta, which reads entity.Meta. The domain
// bytes are unaffected: unmarshalEntityDoc decodes into json.RawMessage values
// and re-emits them verbatim, so numbers survive byte-for-byte and strings stay
// semantically identical. Only document-level framing differs — key order,
// whitespace, and HTML escaping of < > & U+2028 U+2029 inside string literals —
// none of which a path lookup or the kernel's stored.String() observes.
func evalPostFilter(p spi.PreparedFilter, entity *spi.Entity) bool {
	return p.Match(entity.Data, entity.Meta)
}
```

- [ ] **Step 4: Re-point `postgresIter` at BOTH construction sites**

Change the struct field `postFilter *spi.Filter` to `preparedPostFilter *spi.PreparedFilter`, and `Next()`:

```go
		if it.preparedPostFilter != nil && !evalPostFilter(*it.preparedPostFilter, e) {
			continue
		}
```

Site 1 — `plugins/postgres/grouped_stats.go:89-92`:

```go
	return &postgresIter{
		ctx:                ctx,
		rows:               rows,
		preparedPostFilter: plan.preparedPostFilter,
	}, nil
```

Site 2 — `plugins/postgres/searcher.go:220`:

```go
	it := &postgresIter{ctx: ctx, rows: rows, preparedPostFilter: plan.preparedPostFilter}
```

Verify both were changed:

```bash
grep -n 'postgresIter{' *.go
```
Expected: exactly two hits, both carrying `preparedPostFilter`.

The `plan.postFilter == nil` predicates in `runSearch` — the `LIMIT` push at `:211` and the collection-loop-shape choice at `:226` — keep reading `plan.postFilter`. They are planner decisions.

- [ ] **Step 5: Migrate postgres tests, run, commit**

```bash
grep -n 'spi\.MatchFilter\|evalPostFilter(' *_test.go
```
Same mechanical rewrite as Task 6 Step 8; hoist `spi.Prepare` above row loops in `soundness_property_test.go`; drop the absent error return. Expectations do not change.

```bash
go test ./... 2>&1 | tail -30    # requires Docker (testcontainers)
go vet ./...
```
Expected: PASS.

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
git add plugins/postgres/
git commit -m "perf(postgres): prepare the residual post-filter once per plan, not once per row

Mirrors the sqlite change exactly, including the preparedPostFilter nil-ness
invariant. postgresIter is built from two sites — grouped_stats and searcher —
and both are migrated; missing the second would have left Search's residual path
compiling per row.

Refs #30"
```

---

## Task 8: search service and grouped-stats service — hoist `Prepare` above the fallback loops

These are the two per-row `match.Match` callers outside the engine. Both are the fallback path — a full `GetAll` scan with a compile on every entity, which spec §1 identifies as the worst-affected case.

**Files:**
- Modify: `internal/domain/search/service.go:376-390`
- Modify: `internal/domain/entity/grouped_stats_service.go:228-280`

**Interfaces:**
- Consumes: `match.Prepare`, `match.Prepared`, `(match.Prepared).Match` (Task 3).
- Produces: nothing new.

- [ ] **Step 1: Hoist in `search/service.go`**

Replace the row loop (currently `:382-390`) with:

```go
	// Prepared once for the whole scan. Everything the leaf evaluator can fault
	// on is a structural property of the condition, so it surfaces here rather
	// than on whichever row happens to reach it first.
	prepared, prepErr := match.Prepare(cond, fieldTypes)
	if prepErr != nil {
		return nil, fmt.Errorf("predicate match failed: %w", prepErr)
	}

	var matches []*spi.Entity
	for _, e := range entities {
		if prepared.Match(e.Data, e.Meta) {
			matches = append(matches, e)
		}
	}
```

The wrap text `"predicate match failed: %w"` is preserved **verbatim** — it is what the existing mapping at `service.go:386` keys on, and no status code moves in this change.

- [ ] **Step 2: Hoist in `grouped_stats_service.go`**

In `tallyStreaming`, immediately after the `fieldTypes` closure is defined and **before** `it.Iterate`, add:

```go
	// Prepared once, only when there is actually a residual to apply. The
	// guard mirrors the one in the loop below exactly: preparing an unused
	// condition would resolve declared types for a query that never evaluates
	// it.
	var residual match.Prepared
	if !pushable && parsedCond != nil {
		p, err := match.Prepare(parsedCond, fieldTypes)
		if err != nil {
			return nil, err
		}
		residual = p
	}
```

Then replace the per-row block:

```go
		// Residual predicate evaluation: only when the original condition
		// was not pushable and we therefore need to filter per entity.
		if !pushable && parsedCond != nil && !residual.Match(e.Data, e.Meta) {
			continue
		}
```

Keep the `!pushable && parsedCond != nil` guard in the loop rather than relying on the zero `match.Prepared`: the zero value never matches, so dropping the guard would silently filter out every row on the pushdown path.

- [ ] **Step 3: Run the two packages**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
go test ./internal/domain/search/... ./internal/domain/entity/... 2>&1 | tail -20
```
Expected: PASS with no expectation edits.

- [ ] **Step 4: Commit**

```bash
git add internal/domain/search/service.go internal/domain/entity/grouped_stats_service.go
git commit -m "perf(search): prepare the fallback predicate once per query, not once per row

Both in-memory fallback loops — the GetAll scan in SearchService and the
streaming tally in GroupedStatsService — prepared the operand, bucketed types
and compiled the regex for every candidate entity. The wildcard-path fallback
is the worst-affected path in the system, since it scans the whole model.

Error text is unchanged, so no status mapping moves.

Refs #30"
```

---

## Task 9: workflow engine — re-site `Prepare` and invert the infra-failure precedence

Two changes in one function, both declared in spec §5. The precedence inversion is the reason they are one task: hoisting the structural checks into `Prepare` is what makes the ordering observable, so the fix and the change that exposes it must land together.

**Today** (`engine.go:1022-1028`): the match error is checked **first** and `loadErr` second, so a structural error masks a model-store outage — and the `loadErr` is then discarded entirely, never even logged. Reachable: with the store down, `OR[$.age > 5, $.x IS_CHANGED]` latches `loadErr` on the first leaf (`matchSimple` resolved types for **every** simple leaf regardless of operator), degrades it to non-match, raises "unsupported operator" on the sibling, and the caller sees a 400 for a server-side outage.

**After:** `loadErr` wins. An unavailable dependency a correct result requires fails the operation; it is never reported as a client error.

**Files:**
- Modify: `internal/domain/workflow/engine.go:1022-1028`
- Create: `internal/domain/workflow/criterion_precedence_test.go`

**Interfaces:**
- Consumes: `match.Prepare`, `match.Prepared`; existing `ErrCriterionTypingInfra`, the `fieldTypes` closure at `engine.go:1000-1020`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing precedence test**

Create `internal/domain/workflow/criterion_precedence_test.go`. The condition **must** be one whose first leaf `Prepare` resolves a type for, or `loadErr` never latches and the test asserts nothing:

```go
package workflow

import (
	"errors"
	"testing"
)

// TestEvaluateCriterion_InfraFailureBeatsStructuralError pins spec §5's
// precedence: with the model store down, a criterion that ALSO carries a
// structural fault must report the infrastructure failure, not the client
// error. Reporting a 400 for a server-side outage is the wrong way round, and
// the design philosophy says the operation fails closed on the unavailable
// dependency.
//
// This INVERTS the pre-split order, where the match error was checked first
// and loadErr was discarded without even being logged.
//
// The condition is chosen so its FIRST leaf resolves a declared type: without
// that, loadErr never latches and the test would assert nothing. A leaf like
// `state == "X"` would not do — a purely lifecycle criterion loads no schema.
func TestEvaluateCriterion_InfraFailureBeatsStructuralError(t *testing.T) {
	criterion := []byte(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5},
			{"type":"simple","jsonPath":"$.x","operatorType":"IS_CHANGED"}
		]
	}`)

	e, entity, cc := newEngineWithFailingModelStore(t)

	_, _, err := e.evaluateCriterion(criterion, entity, cc)
	if err == nil {
		t.Fatal("evaluateCriterion() = nil error, want the infra failure")
	}
	if !errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("evaluateCriterion() error = %v, want one wrapping ErrCriterionTypingInfra "+
			"(the structural fault on the sibling leaf must not mask a model-store outage)", err)
	}
}

// TestEvaluateCriterion_StructuralErrorReportedWhenStoreIsHealthy is the
// control: with the store up, the same criterion reports the structural fault.
// Without this row the test above would pass on an engine that always returned
// the infra error.
func TestEvaluateCriterion_StructuralErrorReportedWhenStoreIsHealthy(t *testing.T) {
	criterion := []byte(`{
		"type": "group",
		"operator": "OR",
		"conditions": [
			{"type":"simple","jsonPath":"$.age","operatorType":"GREATER_THAN","value":5},
			{"type":"simple","jsonPath":"$.x","operatorType":"IS_CHANGED"}
		]
	}`)

	e, entity, cc := newEngineWithHealthyModelStore(t)

	_, _, err := e.evaluateCriterion(criterion, entity, cc)
	if err == nil {
		t.Fatal("evaluateCriterion() = nil error, want the structural fault")
	}
	if errors.Is(err, ErrCriterionTypingInfra) {
		t.Fatalf("evaluateCriterion() error = %v, want the structural fault, not an infra error", err)
	}
	if err.Error() != "unsupported operator: IS_CHANGED" {
		t.Errorf("evaluateCriterion() error = %q, want %q", err.Error(), "unsupported operator: IS_CHANGED")
	}
}
```

**Write the two helpers against the fixtures this package already has.** Before writing them, run:

```bash
grep -rn 'func newTestEngine\|ModelStore(\|fakeFactory\|stubFactory' internal/domain/workflow/*_test.go | head -20
```

Build `newEngineWithFailingModelStore` on the existing fixture, overriding the factory so `ModelStore(ctx)` returns an error (that is the branch at `engine.go:1004-1007` which sets `loadErr`). `newEngineWithHealthyModelStore` returns the same fixture with a model store that resolves `$.age` to `[Integer]`. Both return `(*Engine, *spi.Entity, *criterionContext)`. Do not invent a new fixture family if one exists — reuse it.

- [ ] **Step 2: Run to verify the first test fails**

```bash
go test ./internal/domain/workflow/ -run 'TestEvaluateCriterion_InfraFailureBeatsStructuralError' -v
```
Expected: FAIL — the current code returns the "unsupported operator" client error, masking the outage.

- [ ] **Step 3: Re-site `Prepare` and invert the precedence**

In `engine.go`, replace the tail of `evaluateCriterion` (currently `:1022-1028`):

```go
	// Prepared once. Every structural fault the criterion carries surfaces
	// here, from the condition's own shape, rather than from whichever entity
	// happens to reach it — so a criterion that cannot be evaluated fails the
	// transition instead of being silently read as "not satisfied".
	prepared, prepErr := match.Prepare(cond, fieldTypes)

	// Infra failure wins. A model-store outage is a server-side condition and
	// must not surface as a client error just because the same criterion also
	// carries a malformed operator. This ordering is deliberate and is the
	// reverse of the pre-split one.
	if loadErr != nil {
		return false, "", loadErr
	}
	if prepErr != nil {
		return false, "", prepErr
	}

	return prepared.Match(entity.Data, entity.Meta), "", nil
```

Also update the `fieldTypes` doc comment above it — the "loaded lazily — only when the criterion actually evaluates a data leaf" claim is now wrong in a specific way:

```go
	// Type-directed evaluation: the predicate kernel compares data leaves by
	// their declared model types (a temporal data field compares temporally,
	// consistent with the search path). The FieldsMap is loaded on the first
	// data leaf the criterion carries — preparation resolves types for every
	// simple and array leaf, whatever its operator, so a criterion that
	// references any data leaf loads the schema even when a lifecycle conjunct
	// would previously have short-circuited past it. A purely lifecycle
	// criterion still touches nothing. A load failure on a criterion that DOES
	// reference data leaves is surfaced (fail closed): the model schema is a
	// required input for correct typing, so we reject rather than silently
	// mis-evaluate.
```

- [ ] **Step 4: Run both tests**

```bash
go test ./internal/domain/workflow/ -run 'TestEvaluateCriterion_' -v
```
Expected: PASS, both.

- [ ] **Step 5: Run the whole workflow package**

```bash
go test ./internal/domain/workflow/... 2>&1 | tail -30
```

Expected: PASS. **If an existing test now fails**, read it before touching it. There are exactly two legitimate reasons a workflow test's expectation moves in this change, both declared in spec §5:
1. A stored criterion carrying a structural fault that a short-circuit previously skipped now fails the transition.
2. A criterion mixing a short-circuiting lifecycle conjunct with a data leaf now resolves the data leaf's type, so a stubbed-out model store that previously went unnoticed now surfaces.

Anything else is a defect in the change. Do not "fix" a test by relaxing it.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/workflow/engine.go internal/domain/workflow/criterion_precedence_test.go
git commit -m "fix(workflow): an infra failure must beat a structural criterion error

evaluateCriterion checked the match error first and loadErr second, so a
malformed operator on one conjunct masked a model-store outage on another — and
the loadErr was then discarded without being logged. With the store down,
OR[\$.age > 5, \$.x IS_CHANGED] reported a 400 for a server-side outage.

The criterion is now prepared once, and loadErr is checked before any structural
error preparation reports. Failing closed on the unavailable dependency is the
stated design philosophy; reporting a client error for it was the wrong way
round.

Refs #30"
```

---

## Task 10: migrate the cross-module and cross-backend evaluator tests

Three tests hold implementations to each other across module boundaries. They are the last consumers of the old API outside the code being deleted, and they must **migrate**, not be deleted — they are the only things pinning these agreements.

**Files:**
- Modify: `internal/match/match_filter_sqlite_parity_test.go` (`:45`, `:325`)
- Modify: `e2e/parity/txsearchryw/tx_search_ryw_test.go` (`:225`)
- Modify: `internal/match/match_filter_test.go` (21 sites)
- Modify: `internal/domain/entity/grouped_stats_service_test.go`, `internal/domain/search/regex_validate_test.go`, `internal/grpc/search_invalid_regex_test.go`, `internal/e2e/search_invalid_regex_test.go`, `internal/e2e/grouped_stats_invalid_regex_test.go`

**Interfaces:**
- Consumes: `spi.Prepare`, `match.Prepare`, `sqlite.EvaluateFilter` (new prepared signature from Task 6).
- Produces: nothing new.

- [ ] **Step 1: Migrate the cross-module sqlite parity test**

This is the only thing holding `internal/match` and `plugins/sqlite` to the same answer across the module boundary. Both sides change, so it migrates to the prepared form on both.

Rename `TestMatchFilter_SqliteParity_Smoke` → `TestPrepared_SqliteParity_Smoke` and replace the assertion body (`:325-329`):

```go
			ent := &spi.Entity{Meta: tc.meta, Data: data}

			// Both sides prepared, mirroring production: the plugin prepares
			// at its plan site, the domain evaluator at its query site.
			prepared := spi.Prepare(tc.f)
			sqliteRes := sqlite.EvaluateFilter(prepared, ent)
			kernelRes := prepared.Match(data, tc.meta)

			if sqliteRes != kernelRes {
				t.Fatalf("PARITY DRIFT: sqlite=%v kernel=%v\n  filter=%+v", sqliteRes, kernelRes, tc.f)
			}
```

Update the file's header comment: the two things being pinned are now `plugins/sqlite.EvaluateFilter` and `spi.PreparedFilter.Match`, and the sentence about `internal/match.MatchFilter` is stale.

- [ ] **Step 2: Migrate the txsearchryw parity oracle**

`e2e/parity/txsearchryw/tx_search_ryw_test.go:225` uses `GetAll + spi.MatchFilter` as its oracle across all three backends. Hoist the prepare above the loop — the oracle should exercise the same shape production does:

```bash
sed -n '215,235p' e2e/parity/txsearchryw/tx_search_ryw_test.go
```

Rewrite so `spi.Prepare(filter)` is called once, above the `GetAll` result loop, and the loop body calls `.Match(e.Data, e.Meta)`.

- [ ] **Step 3: Mechanically migrate the remaining test files**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
grep -rn 'spi\.MatchFilter\|match\.MatchFilter' --include='*_test.go' .
```

Every hit becomes `spi.Prepare(f).Match(data, meta)`. `internal/match/match_filter_test.go` has 21 of them and is otherwise unchanged — its whole point is the `Filter`-side semantics table, which still holds.

Rename the file to `internal/match/prepared_filter_delegation_test.go` **only if** it still tests something after Task 11 deletes `match.MatchFilter`. Check first: if every case is really an `spi` semantics assertion, it belongs in the SPI's own `filter_match_test.go` and this file is redundant duplication. Decide by reading it, and state the decision in the commit message either way.

- [ ] **Step 4: Verify no production or test code references the old API**

```bash
grep -rn 'spi\.MatchFilter\|match\.MatchFilter\|spi\.EvalLeafString\|\.Void()' --include='*.go' . | grep -v 'frozen'
```
Expected: hits only in `internal/match/match.go` (the function being deleted in Task 11) and `internal/match/operators.go` (`applyOperator`, same). Anything else is a missed site.

- [ ] **Step 5: Run the root module and commit**

```bash
go test -short ./... 2>&1 | tail -30
go vet ./...
```
Expected: PASS.

```bash
git add -u internal/match internal/domain internal/grpc internal/e2e e2e/parity
git commit -m "test(search): migrate the cross-module evaluator parity tests to the prepared form

The sqlite cross-module smoke test and the txsearchryw parity oracle both
migrate rather than being deleted — they are the only things holding these
implementations to the same answer across a module boundary, and both sides of
each are changing.

Refs #30"
```

---

## Task 11: cyoda-go — delete the old evaluator and fix the stale comments

Removal is the mechanism that forces every caller to re-site `Prepare`. This must land **before** Task 12, because `match.MatchFilter` delegates to `spi.MatchFilter` and `applyOperator` calls `spi.EvalLeafString` — deleting the SPI side first breaks the build.

**Files:**
- Modify: `internal/match/match.go` (delete `Match`, `matchSimple`, `matchArrayWildcard`, `matchLifecycle`, `applyStringLifecycle`, `matchTemporalMeta`, `matchGroup`, `matchArray`, `MatchFilter`)
- Modify: `internal/match/operators.go` (delete `applyOperator`)
- Modify: `internal/domain/search/regex_validate.go:25-34`, `:71`
- Modify: `internal/domain/workflow/validate.go:215`, `:269`

**Interfaces:**
- Consumes: nothing new.
- Produces: `internal/match`'s exported surface is now exactly `FieldTypes`, `Prepared`, `Prepare`, `(Prepared).Match`.

- [ ] **Step 1: Delete the dead functions**

From `internal/match/match.go`, delete: `Match`, `matchSimple`, `matchArrayWildcard`, `matchLifecycle`, `applyStringLifecycle`, `matchTemporalMeta`, `matchGroup`, `matchArray`, `MatchFilter`, and the `// --- spi.Filter-based evaluation ---` comment block at `:299-313`.

**Keep**: `FieldTypes` and its doc, `convertJSONPath`, `fieldMapKey`, `arrayElementFieldPath`, `isTemporalOperator`. All five are consumed by `prepared.go` and by the frozen equivalence reference.

From `internal/match/operators.go`, delete `applyOperator` and its doc comment. **Keep** `opNameToFilterOp`, `operandsToStrings`, `betweenBounds`.

- [ ] **Step 2: Compile and let the compiler find the stragglers**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
go build ./... && go vet ./...
```
Expected: clean. Any `undefined: match.Match` is a call site Tasks 8–10 missed — fix it by hoisting `Prepare`, never by resurrecting the function.

- [ ] **Step 3: Fix the four stale `opMatchesPattern` comments**

`opMatchesPattern` has not existed since the evaluator convergence work. Four comments still mandate mirroring it, which sends a reader to a function that is not there.

`internal/domain/search/regex_validate.go:25-34` — the comment mandating that the compile call mirror `opMatchesPattern`. Rewrite to name what actually derives the pattern now:

```go
// The compile call MUST mirror the kernel's own pattern derivation, so
// validation accepts exactly the patterns evaluation can run (no accept/reject
// skew). The kernel anchors the pattern and compiles it inside ExpandLeaf
// (cyoda-go-spi eval_leaf.go); this validator must apply the same anchoring to
// the same input.
```

`internal/domain/search/regex_validate.go:71` — same substitution on `compileRegexPattern`'s doc.

`internal/domain/workflow/validate.go:215` and `:269` — both cite `internal/match/operators.go`'s `opMatchesPattern`. Re-point at `spi.ExpandLeaf`, which is where the compile actually happens. While in `validate.go:200-204`, check the claim recorded as defect D10 in the research doc — that it "claims a temporal rejection that does not happen". If the comment is still wrong, delete the false sentence; do not soften it.

`internal/match/match.go:291` and the two SPI-side citations of `MatchFilterSqliteEvaluateFilterParity` — that parity scenario does not exist and never did. The real test is `TestPrepared_SqliteParity_Smoke` (renamed in Task 10). Fix the cyoda-go one here; the SPI ones are Task 12.

- [ ] **Step 4: Sweep every comment naming the deleted API**

The four `opMatchesPattern` comments are not the whole set. `spi.MatchFilter` is named in
roughly 35 comments across the three plugin modules and two domain files — all of which
point at a function that stops existing one task later. They are load-bearing prose:
several explain the pushdown soundness contract by naming the kernel that re-checks
candidates, and a reader who greps for the name will find nothing.

```bash
grep -rn 'spi\.MatchFilter\|match\.MatchFilter\|EvalLeafString\|opMatchesPattern\|MatchFilterSqliteEvaluateFilterParity' \
  --include='*.go' . | grep -v frozen
```

Rewrite each hit to name what actually evaluates now — `spi.Prepare` / `PreparedFilter.Match`
for the kernel, `ExpandLeaf` + `EvalLeaf` for the leaf primitives. Do not mechanically
substitute the identifier: several of these sentences describe *when* evaluation happens,
and the prepare/execute split changed that. Read each one.

Expected afterwards: hits only in `internal/match/match.go` and `internal/match/operators.go`
(the functions being deleted in this task), plus the two frozen equivalence references,
which are deliberate copies and must keep naming the old shapes.

- [ ] **Step 5: Run the root module and commit**

```bash
go test -short ./... 2>&1 | tail -30
```
Expected: PASS — including both merge gates, which now guard the deleted code from their frozen copies.

```bash
git add internal/match internal/domain/search/regex_validate.go internal/domain/workflow/validate.go
git commit -m "refactor(search): delete the per-row predicate evaluator

match.Match, its eight tree-walk helpers, applyOperator and the MatchFilter
forwarder are gone. Every caller now prepares above its row loop. The frozen
copies in the two equivalence gates keep holding the new evaluator to the old
answers.

Also corrects four comments mandating that validation mirror opMatchesPattern,
a function that has not existed since the evaluator convergence work, and three
citing a parity scenario that never existed.

Refs #30"
```

---

## Task 12: SPI — delete `MatchFilter`, `EvalLeafString`, `evalLeafFast`, `Void`

Safe only now: Task 11 removed the last consumers.

**Files:**
- Modify: `cyoda-go-spi/filter_match.go` (delete `MatchFilter`, `evalFilter`, `evalLeafFilter`, `filterStoredResult`)
- Modify: `cyoda-go-spi/eval_leaf.go` (delete `EvalLeafString`, `evalLeafFast`, `Expansion.Void`; correct the header doc)
- Modify: `cyoda-go-spi/eval_leaf_test.go` (retire the fast-path differential)
- Modify: `cyoda-go-spi/filter_match_test.go`, `filter_match_internal_test.go`, `condition_filter_test.go`
- Modify: `cyoda-go-spi/CHANGELOG.md`

**Interfaces:**
- Consumes: nothing new.
- Produces: the SPI's leaf-evaluation surface is now `ExpandLeaf`, `EvalLeaf`, `Prepare`, `PreparedFilter.Match`, `OperandString`.

- [ ] **Step 1: Migrate the SPI's own tests first**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
grep -rn 'MatchFilter(\|EvalLeafString(\|\.Void()' --include='*_test.go' . | grep -v frozen
```

Rewrite each `MatchFilter(f, d, m)` / `spi.MatchFilter(f, d, m)` as `Prepare(f).Match(d, m)` / `spi.Prepare(f).Match(d, m)`. Test names keep their `TestMatchFilter_` prefix only where the name still describes the subject — prefer renaming to `TestPrepare_` where it does not. Expectations do not change.

`eval_leaf_test.go:45,265-270` is the fast-path-vs-general differential. Deleting `evalLeafFast` makes it **vacuous** — it would compare the general path with itself. Retire it: delete the "Path B" block and the `gotA != gotB` assertion, keep the "Path A" `ExpandLeaf` + `EvalLeaf` oracle rows, and update the doc comment at `:45` which says each row is evaluated twice.

The deletion is covered without a separate differential: the merge gate's frozen reference includes `frozenEvalLeafFast`, which routes monomorphic `String`/`UnboundDecimal` comparables through the fast path first, so any divergence the deletion could cause already surfaces in the 200k-case corpus. Two prior runs (46,000 and 3,000,000 cases) found none.

- [ ] **Step 2: Delete from `filter_match.go`**

Delete `MatchFilter`, `evalFilter`, `evalLeafFilter` and `filterStoredResult` (the prepared node has its own `stored` method). **Keep** `metaGjsonResult`, `OperandString`, `valuesToStrings`, `extractFilterMetaValue`, `timeToMicro`.

Replace the file header block (`:11-18`) — it currently cites a parity scenario that does not exist:

```go
// --- Filter operand and meta-value plumbing ---
//
// The helpers below are what the prepared evaluator (prepared_filter.go) and
// the sqlite/postgres post-filter steps share, so an in-process evaluation
// produces bit-identical results to a SQL backend's post-filter step. Drift
// between them would silently change grouped-stats results across backends;
// TestPrepared_SqliteParity_Smoke in cyoda-go pins the contract across the
// module boundary.
```

- [ ] **Step 3: Delete from `eval_leaf.go`**

Delete `EvalLeafString` (`:519-532`), `evalLeafFast` (`:534-586`), and `Expansion.Void` (`:105-110` — zero call sites anywhere in either repo, confirmed by grep).

Correct the header doc (`:22-31`), which is where the defect hid. Its "This is the once-per-query work" claim was false for as long as the fused call existed:

```go
// Two entry points:
//
//   - ExpandLeaf parses the operand once against the field's declared type set
//     and produces an Expansion — the typed sub-conditions (numeric / temporal /
//     other branches) plus a void flag or an error. This is the once-per-query
//     work, and prepared_filter.go is what makes that true: Prepare calls it
//     once per leaf and Match never calls it at all.
//   - EvalLeaf classifies a single stored gjson.Result and decides match/no-match
//     against a pre-built Expansion. This is the per-row work.
//
// There is deliberately no fused expand-and-evaluate entry point. One existed,
// and it is why operand parsing, type bucketing and regex compilation ran once
// per candidate entity instead of once per query.
```

- [ ] **Step 4: Build, test, and confirm the gate still holds**

```bash
go build ./... && go vet ./...
go test ./... 2>&1 | tail -30
```
Expected: PASS, including `TestPrepare_EquivalentToFrozenMatchFilter` — which is the point of freezing a copy rather than referencing the originals.

- [ ] **Step 5: Confirm the whole workspace still builds**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
go build ./... && (cd plugins/memory && go build ./...) && (cd plugins/sqlite && go build ./...) && (cd plugins/postgres && go build ./...)
```
Expected: clean. This is the moment the breaking change is real — if anything here fails, a call site was missed.

- [ ] **Step 6: Write the CHANGELOG entry**

In `cyoda-go-spi/CHANGELOG.md`, under the unreleased heading:

```markdown
### Breaking

- **`MatchFilter`, `EvalLeafString`, `evalLeafFast` and `Expansion.Void` are removed.**
  Filter evaluation is now a prepare/execute split: build a `PreparedFilter` once
  per query with `Prepare(Filter)`, then call `Match(data, meta)` once per row.

  Migration:

  ```go
  // before — parsed the operand, bucketed types and compiled the regex per row
  for _, e := range rows {
      if spi.MatchFilter(f, e.Data, e.Meta) { … }
  }

  // after — all of that happens once, above the loop
  p := spi.Prepare(f)
  for _, e := range rows {
      if p.Match(e.Data, e.Meta) { … }
  }
  ```

  `Prepare` returns no error: a leaf whose operand cannot be expanded becomes a
  leaf that never matches, exactly as the per-row evaluator did. A `PreparedFilter`
  is immutable and safe to share across goroutines. The zero `PreparedFilter`, and
  `Prepare(Filter{})`, both match everything.

  A leaf-level `EvalLeafString` replacement is deliberately not provided: leaving
  one would let a caller keep compiling per row while only the tree walk was forced
  open. Use `ExpandLeaf` once and `EvalLeaf` per row if you need leaf-level control.

  No one-release `// Deprecated:` grace period: removal is the mechanism that
  forces each caller to re-site the preparation, and a shim would silently preserve
  the defect.
```

- [ ] **Step 7: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
git add -u
git commit -m "feat(search)!: remove MatchFilter, EvalLeafString, evalLeafFast and Expansion.Void

BREAKING CHANGE: Filter evaluation is a prepare/execute split. Build a
PreparedFilter once per query with Prepare(Filter), then Match(data, meta) per
row.

EvalLeafString is removed for the same reason as MatchFilter: it is the
leaf-level fused call, and leaving it would let a caller keep compiling per row
while only the tree walk was forced open. evalLeafFast existed solely to avoid
per-row expansion and has nothing left to optimise. Expansion.Void had zero call
sites in any known consumer.

The ExpandLeaf doc claimed to be 'the once-per-query work'. That claim was false
while the fused call existed, and is what concealed the defect; it is now true
and says why.

Refs #30"
```

---

## Task 13: record the temporal divergence at both evaluators

Spec §9. `CONTAINS "2021"` on `creationDate` matches through the `Filter` evaluator and does not through the predicate evaluator — the same API request, two answers depending on whether the query pushes down. Measured, not inferred; a v0.8.3 regression.

**It is deliberately not fixed here.** Text and pattern operators on a temporal field are a predicate that a separate acceptance-policy change removes; making the two evaluators agree on what such a query evaluates to would specify semantics for a feature being withdrawn, in code that change then makes unreachable. There is one fix — refuse the condition at the shared validation boundary — and it is owned elsewhere. Leaving it also keeps the strongest property this refactor has: no answer changes, so the equivalence gates need no carve-out.

What ships here is a comment at each evaluator, so the next person to find the divergence does not "helpfully" align one side.

**Files:**
- Modify: `internal/match/prepared.go` (the `prepareLifecycle` temporal arm)
- Modify: `cyoda-go-spi/prepared_filter.go` (the leaf `stored`/`match` path)

**Note on placement:** spec §9 names `internal/match.matchTemporalMeta` as the site. That function no longer exists — Task 3 moved its field-identity guard into `prepareLifecycle`'s temporal arm, and Task 11 deleted the original. The guard is the site; put the comment there.

**Interfaces:** none — comments only. No behaviour change, no test.

- [ ] **Step 1: Comment the predicate-side guard**

In `internal/match/prepared.go`, extend the comment on `prepareLifecycle`'s `case "creationDate", "lastUpdateTime":` arm:

```go
	case "creationDate", "lastUpdateTime":
		// Field-identity guard, sitting in FRONT of the operator check: a
		// temporal field admits only comparison, range and null operators, and
		// anything else is a never-match leaf rather than an error. It must
		// never lexically substring-match the formatted RFC3339 rendering.
		//
		// KNOWN DIVERGENCE, deliberately not resolved here. The Filter-side
		// evaluator has no such guard: a text or pattern operator on a temporal
		// meta field reaches its string branch there and matches against the
		// RFC3339 rendering, so the same request answers differently depending
		// on whether the query pushes down.
		//
		// Do NOT resolve this by aligning either evaluator. A text or pattern
		// operator on a temporal field is not a supported predicate, and the
		// resolution is to refuse it at the shared validation boundary, which
		// makes both evaluators' behaviour unreachable. Aligning here would
		// specify semantics for a predicate that is being withdrawn.
		if !isTemporalOperator(c.OperatorType) {
			return prepNode{kind: prepNever}, nil
		}
```

- [ ] **Step 2: Comment the Filter-side leaf**

In `cyoda-go-spi/prepared_filter.go`, on `preparedNode.stored`:

```go
// stored resolves the value this leaf addresses, keeping gjson's .Raw so the
// kernel can classify numerics and temporals precisely. A missing data path
// yields a non-existent Result, and SourceMeta values are bridged through
// metaGjsonResult.
//
// KNOWN DIVERGENCE, deliberately not resolved here. A temporal meta field
// (creationDate / lastUpdateTime) bridges to an RFC3339 string, and this
// evaluator applies a text or pattern operator to it lexically — where the
// predicate-tree evaluator in the consuming service guards the same case to a
// non-match on field identity. The same request therefore answers differently
// depending on whether the query pushes down.
//
// Do NOT resolve this by aligning either evaluator. A text or pattern operator
// on a temporal field is not a supported predicate, and the resolution is to
// refuse it at the shared validation boundary, which makes both evaluators'
// behaviour unreachable. Aligning here would specify semantics for a predicate
// that is being withdrawn.
```

No issue number in either comment — the convention on shipped artefacts is that a comment has to stand on its own.

- [ ] **Step 3: Confirm the shipped docs were NOT changed**

`cmd/cyoda/help/content/search.md` and `predicates.md` already describe these queries as rejected with `400 CONDITION_TYPE_MISMATCH`. They were rewritten ahead of the code on purpose, so that refusing them later is a bug fix rather than the removal of an advertised feature. **Do not "correct" them back to describe current behaviour.**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
git diff --name-only origin/release/v0.8.4 -- cmd/cyoda/help/content/
```
Expected: no output for `search.md` or `predicates.md`.

- [ ] **Step 4: Build both repos and commit**

```bash
go build ./... && (cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter && go build ./...)
```

Two commits, one per repo:

```bash
git add internal/match/prepared.go
git commit -m "docs(search): record the temporal text-operator divergence at the predicate evaluator

A text or pattern operator on creationDate/lastUpdateTime is a non-match here
and a lexical match on the RFC3339 rendering in the Filter evaluator, so the
same request answers differently depending on whether it pushes down.

Not resolved here on purpose: the predicate is being withdrawn, and specifying
what it evaluates to in code that a refusal at the validation boundary then makes
unreachable would be work in the wrong direction. The comment says so, so the
next reader does not align one side.

Refs #30"

cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
git add prepared_filter.go
git commit -m "docs(search): record the temporal text-operator divergence at the Filter evaluator

Counterpart to the note at the consuming service's predicate evaluator. Neither
side is to be aligned; the resolution is refusal at the shared validation
boundary.

Refs #30"
```

---

## Task 14: E2E — the declared criterion behaviour change

Spec §5's one Gate-7 item, and the only place the change is visible to a client. `evaluateCriterion` runs `ParseCondition` only — workflow import checks patterns but **not** operator names (`validate.go:248-249`), so a criterion carrying an unsupported operator imports clean and then fails on every evaluation.

A stored criterion `AND[state == "X", $.amount FROBNICATE 1]` today evaluates false for entities outside state X — the AND short-circuits before reaching the bad operator — and the transition simply does not fire. Afterwards it fails for every entity.

**Accepted deliberately:** a criterion that cannot be evaluated must not be silently treated as "not satisfied".

An unknown *meta field* cannot be used to build this test — import already rejects it (`validate.go:250-256` → `search.ValidateLifecycleCondition`). An unsupported *operator name* is the reachable case.

**Files:**
- Create: `internal/e2e/criterion_prepare_test.go`

**Interfaces:**
- Consumes existing E2E helpers: `setupModelWithWorkflow(t, entityName, workflowJSON)`, `createEntityRaw(t, entityName, modelVersion, payload) (int, string)`, `createEntityE2E`, `getEntityState`.
- Produces: nothing.

- [ ] **Step 1: Write the test**

Create `internal/e2e/criterion_prepare_test.go`:

```go
package e2e_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The one client-visible behaviour change of the prepare/execute split.
//
// Workflow import validates regex patterns but NOT operator names, so a
// criterion carrying an unsupported operator stores cleanly. Before the split,
// the tree walk was lazy: AND[state == "X", $.amount FROBNICATE 1] short-
// circuited on the first conjunct for any entity outside state X and never
// reached the bad operator, so the transition silently did not fire and the
// save returned 2xx.
//
// Preparation walks the whole condition, so the fault is now reported from the
// criterion's own shape. A criterion that cannot be evaluated must not be
// silently read as "not satisfied".
// ---------------------------------------------------------------------------

// unevaluableCriterionWorkflow builds a workflow whose CREATED state has one
// automated transition guarded by AND[state == "NEVER_REACHED", $.amount
// FROBNICATE 1]. NONE -> CREATED is unconditioned and automated, so creating an
// entity cascades into CREATED and evaluates the guarded criterion in the same
// request — with the entity in state CREATED, i.e. outside the state the first
// conjunct names.
func unevaluableCriterionWorkflow(t *testing.T, wfName string) string {
	t.Helper()
	criterion, err := json.Marshal(map[string]any{
		"type":     "group",
		"operator": "AND",
		"conditions": []any{
			map[string]any{
				"type": "lifecycle", "field": "state",
				"operatorType": "EQUALS", "value": "NEVER_REACHED",
			},
			map[string]any{
				"type": "simple", "jsonPath": "$.amount",
				"operatorType": "FROBNICATE", "value": 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal criterion: %v", err)
	}
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, wfName, string(criterion))
}

// TestCriterion_UnevaluableOperator_FailsTheSave pins the declared change: a
// criterion carrying an unsupported operator name fails the save with 400
// WORKFLOW_FAILED and rolls the transaction back, even for an entity whose
// state makes the sibling conjunct false.
//
// Before the split this returned 2xx and left the entity at CREATED.
func TestCriterion_UnevaluableOperator_FailsTheSave(t *testing.T) {
	const model = "e2e-criterion-unevaluable"

	// Import must SUCCEED — this is the premise. Workflow import does not check
	// operator names, which is why the criterion is storable at all.
	setupModelWithWorkflow(t, model, unevaluableCriterionWorkflow(t, "criterion-unevaluable-wf"))

	status, body := createEntityRaw(t, model, 1, `{"name":"X","amount":1}`)

	if status != 400 {
		t.Fatalf("create with an unevaluable criterion: status = %d, want 400\n  body: %s\n"+
			"a criterion that cannot be evaluated must fail the save, not be read as 'not satisfied'",
			status, body)
	}
	if !strings.Contains(body, "WORKFLOW_FAILED") {
		t.Errorf("create response body = %s, want it to carry error code WORKFLOW_FAILED", body)
	}
	if !strings.Contains(body, "FROBNICATE") {
		t.Errorf("create response body = %s, want it to name the unsupported operator "+
			"(4xx responses carry full domain detail)", body)
	}
}

// TestCriterion_UnevaluableOperator_RollsBackTheWrite pins that the failed save
// leaves nothing behind: a criterion evaluation failure rolls the whole
// transaction back, so the entity write is discarded rather than committed with
// the transition skipped.
func TestCriterion_UnevaluableOperator_RollsBackTheWrite(t *testing.T) {
	const model = "e2e-criterion-unevaluable-rollback"

	setupModelWithWorkflow(t, model, unevaluableCriterionWorkflow(t, "criterion-unevaluable-rollback-wf"))

	status, _ := createEntityRaw(t, model, 1, `{"name":"X","amount":1}`)
	if status != 400 {
		t.Fatalf("precondition: create status = %d, want 400", status)
	}

	// Nothing was persisted. Search the model and require an empty result set.
	found := countEntitiesInModel(t, model, 1)
	if found != 0 {
		t.Errorf("model holds %d entities after a rolled-back save, want 0", found)
	}
}

// TestCriterion_EvaluableCriterionStillShortCircuits is the control: the same
// workflow shape with a SUPPORTED operator on the second conjunct still saves
// cleanly and still leaves the entity at CREATED, because the criterion is
// genuinely false. Without this row, the two tests above would pass on an
// engine that failed every save.
func TestCriterion_EvaluableCriterionStillShortCircuits(t *testing.T) {
	const model = "e2e-criterion-evaluable-control"

	criterion, err := json.Marshal(map[string]any{
		"type":     "group",
		"operator": "AND",
		"conditions": []any{
			map[string]any{"type": "lifecycle", "field": "state",
				"operatorType": "EQUALS", "value": "NEVER_REACHED"},
			map[string]any{"type": "simple", "jsonPath": "$.amount",
				"operatorType": "GREATER_THAN", "value": 1},
		},
	})
	if err != nil {
		t.Fatalf("marshal criterion: %v", err)
	}
	wf := fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "criterion-evaluable-control-wf",
			"initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "CREATED", "manual": false}]},
				"CREATED": {"transitions": [{"name": "advance", "next": "ADVANCED", "manual": false,
					"criterion": %s}]},
				"ADVANCED": {}
			}
		}]
	}`, string(criterion))

	setupModelWithWorkflow(t, model, wf)
	entityID := createEntityE2E(t, model, 1, `{"name":"X","amount":1}`)

	if state := getEntityState(t, entityID); state != "CREATED" {
		t.Errorf("state = %q, want CREATED: the criterion is false, so the transition must not fire", state)
	}
}
```

- [ ] **Step 2: Add the `countEntitiesInModel` helper if the suite lacks one**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter/internal/e2e
grep -rn 'func countEntitiesInModel\|func searchEntities\|func getAllEntities' *.go
```

Reuse an existing one if present. If not, write it next to the other helpers in `criterion_prepare_test.go`, issuing a search with a match-all condition against the model and returning the result count. Follow the shape of whichever search helper the suite already has — do not invent a new HTTP-calling idiom.

- [ ] **Step 3: Run the three tests**

```bash
go test ./internal/e2e/ -run 'TestCriterion_' -v      # requires Docker
```
Expected: all three PASS.

**If the first test returns 2xx**, the engine is still short-circuiting — check that Task 9's `prepared, prepErr := match.Prepare(...)` really replaced the lazy walk and that `prepErr` is returned.

**If the import step fails**, the premise is broken: something now rejects the criterion at import. That would be a genuine finding — surface it rather than weakening the test, because it would mean the change is unreachable and the whole §5 declaration is moot.

- [ ] **Step 4: Run the full E2E suite**

```bash
go test ./internal/e2e/ 2>&1 | tail -30
```
Expected: PASS. Watch specifically for existing criterion tests that stored a malformed criterion and relied on the short-circuit — those would now fail, and if any exists it is a real instance of the declared change, to be updated with a comment saying so.

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
git add internal/e2e/criterion_prepare_test.go
git commit -m "test(e2e): a criterion that cannot be evaluated now fails the save

Workflow import validates regex patterns but not operator names, so
AND[state == 'X', \$.amount FROBNICATE 1] stores cleanly. The lazy tree walk
short-circuited past the bad operator for any entity outside state X, so the
save returned 2xx and the transition silently did not fire.

Preparation walks the whole condition, so it is now 400 WORKFLOW_FAILED with the
transaction rolled back. A criterion that cannot be evaluated must not be read as
'not satisfied'. Covered with a control row so the assertion cannot pass on an
engine that fails every save.

Refs #30"
```

---

## Task 15: documentation and the SPI pin bump

**Do not start this task until the SPI PR has merged to `cyoda-go-spi` `main`** — the pseudo-version cannot be computed before then.

**Files:**
- Modify: `go.mod`, `plugins/memory/go.mod`, `plugins/sqlite/go.mod`, `plugins/postgres/go.mod`
- Modify: `COMPATIBILITY.md` (the v0.8.4 row)
- Modify: `CHANGELOG.md`
- Modify: `docs/workflow-schema-versioning.md` (Changelog section)
- Create: `docs/cloud-parity/<name>.md`

**Interfaces:** none.

- [ ] **Step 1: Open and merge the SPI PR**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
make race 2>&1 | tail -20      # or: go test -race ./...
go test ./... 2>&1 | tail -10
git push -u origin feat/30-prepared-filter
```

PR body must link both `KNOWN_CONSUMERS.md` entries as notified before merge, and carry the `### Breaking` migration block verbatim from the CHANGELOG. State that the `// Deprecated:` one-release grace is waived deliberately: removal is the mechanism that forces each caller to re-site `Prepare`.

Merge via the API — `gh pr merge` collides with a worktree on the base branch:

```bash
gh api -X PUT repos/cyoda-platform/cyoda-go-spi/pulls/<N>/merge -f merge_method=squash
gh api -X DELETE repos/cyoda-platform/cyoda-go-spi/git/refs/heads/feat/30-prepared-filter
```

- [ ] **Step 2: Bump the SPI pin in all four `go.mod` files**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
NEW=$(GOFLAGS=-mod=mod GOWORK=off go list -m -f '{{.Version}}' github.com/cyoda-platform/cyoda-go-spi@main)
echo "new pseudo-version: $NEW"
for d in . plugins/memory plugins/sqlite plugins/postgres; do
  (cd "$d" && GOWORK=off go mod edit -require=github.com/cyoda-platform/cyoda-go-spi@"$NEW")
done
grep -h 'cyoda-go-spi' go.mod plugins/*/go.mod
```
Expected: four identical lines. The `pin-sync` CI job (`scripts/check-spi-pin-sync.sh`) enforces that they agree.

They currently sit at `v0.8.4-0.20260808050403-d475ae118741`; this moves them to a newer pseudo-version, and the real tag lands at milestone end.

- [ ] **Step 3: Verify the pinned build resolves without `go.work`**

```bash
for d in . plugins/memory plugins/sqlite plugins/postgres; do
  (cd "$d" && GOWORK=off go build ./... && echo "OK $d")
done
```
Expected: four `OK` lines. This is what the `GOWORK=off` CI jobs check — a pass here means consumers can resolve the module without the local composition.

- [ ] **Step 4: Rewrite `COMPATIBILITY.md`'s v0.8.4 row**

It currently reads that v0.8.4 involves "no SPI change, deliberately … no SPI tag and no coordinated cross-repo release". That is no longer true — the pin already moved for the translator relocation, and this change moves it again. Rewrite the row to state that v0.8.4 **does** require a coordinated release, that the SPI carries a breaking removal of `MatchFilter`/`EvalLeafString`, and that out-of-tree plugins must re-site `Prepare` above their row loops.

- [ ] **Step 5: Write the cloud-parity record**

Gate 7 applies to exactly one thing: §5's workflow-criterion change. Cloud mirrors criterion semantics. Create `docs/cloud-parity/criterion-unevaluable-fails-the-transition.md` recording:

- **What changed:** a stored criterion carrying a structural fault a lazy walk previously skipped now fails the transition rather than silently not firing it.
- **Why:** a criterion that cannot be evaluated is not the same as a criterion that evaluates false.
- **Wire impact:** `400 WORKFLOW_FAILED`, transaction rolled back. No new error code, no status code moved on any other path.
- **What did NOT change:** no API shape, no other status code. The unknown-group-operator and unknown-meta-field cross-path divergences are explicitly out of scope and owned by the search acceptance-policy work.

- [ ] **Step 6: Add the workflow-schema-versioning changelog entry**

No schema bump is needed — the `WorkflowConfigurationDto` import surface is unchanged. But `docs/workflow-schema-versioning.md` needs a Changelog entry for the same reason its v0.8.3 malformed-regex entry exists: this newly rejects a stored criterion that previously ran. Record that import acceptance is unchanged while evaluation is stricter, and that a workflow stored before this release carrying an unsupported operator name will now fail its transition.

- [ ] **Step 7: Add the cyoda-go CHANGELOG entry**

Under the unreleased v0.8.4 heading, covering: the prepare/execute split and what it fixes; the criterion behaviour change (under `### Breaking`, since backward compatibility is not a constraint at this stage and the correct default is the one to ship); the infra-precedence fix; and the SPI pin move.

- [ ] **Step 8: Commit**

```bash
git add go.mod plugins/memory/go.mod plugins/sqlite/go.mod plugins/postgres/go.mod \
        COMPATIBILITY.md CHANGELOG.md docs/workflow-schema-versioning.md docs/cloud-parity/
git status --short          # confirm go.work is NOT staged
git commit -m "chore(deps): bump the cyoda-go-spi pin for the prepare/execute split

All four go.mod files move together, as pin-sync requires. Documents the
coordinated release COMPATIBILITY.md previously said v0.8.4 would not need, the
criterion behaviour change for cloud parity, and the stricter evaluation in the
workflow schema changelog.

Refs #30"
```

**Do not stage `go.work`** — it is tracked in its CI-safe form and the local SPI `use` line must stay uncommitted. `git add -A` here commits an absolute path and breaks CI.

---

## Task 16: end-of-deliverable verification and the cyoda-go PR

This is the Gate 5 checkpoint. Everything below runs **once**, here, not per task.

- [ ] **Step 1: Full suite, root module and every plugin**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/feat-30-prepared-filter
make test-all 2>&1 | tail -40
```
Expected: PASS across root + `plugins/memory|sqlite|postgres`. Requires Docker.

- [ ] **Step 2: Vet everything**

```bash
go vet ./...
for d in plugins/memory plugins/sqlite plugins/postgres; do (cd "$d" && go vet ./...); done
```

- [ ] **Step 3: Race detector, once**

```bash
make race 2>&1 | tail -20
```
Expected: PASS. This is the only full `-race` run in the plan.

- [ ] **Step 4: Confirm the old API is gone everywhere**

```bash
grep -rn 'spi\.MatchFilter\|match\.MatchFilter\|EvalLeafString\|evalLeafFast\|\.Void()' \
  --include='*.go' . /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter \
  | grep -v 'frozen'
```
Expected: no output. Hits containing `frozen` are the two equivalence references and are correct.

- [ ] **Step 5: Confirm both merge gates still pass at width**

```bash
for s in 11 12 13 14 15; do
  MATCH_EQUIV_SEED=$s MATCH_EQUIV_CASES=400000 go test ./internal/match/ -run 'TestPrepare_EquivalentToFrozenMatch' || break
done
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi/.worktrees/feat-30-prepared-filter
for s in 11 12 13 14 15; do
  SPI_EQUIV_SEED=$s SPI_EQUIV_CASES=400000 go test . -run 'TestPrepare_EquivalentToFrozenMatchFilter' || break
done
```
Expected: PASS, 2,000,000 DISTINCT cases each side — distinct seeds, not `-count`,
which regenerates the same corpus.

- [ ] **Step 6: Dispatch the fresh-context code review**

Both review gates need a reviewer that has not seen the working context. This is a standing request — dispatch a subagent, do not run the review inline.

Use `superpowers:requesting-code-review`, then `antigravity-bundle-security-developer:cc-skill-security-review`. Give the reviewer the spec and this plan, and specifically ask it to check the three things the plan itself flags as the likeliest failures:

1. The zero-Op child asymmetry (spec §3's table) — did the `Op == ""` check leak into the recursion anywhere?
2. `preparedPostFilter` nil-ness on both SQL backends — is it non-nil exactly when `postFilter` is, at every construction site including `postgres/searcher.go:220`?
3. `Prepare` placement — is it above every row loop, or did any site leave it inside? Spec §8 records this as the one gap the test suite does not close (a second compile counter would need the `compileRegex` indirection exported from a public SPI, which costs more than it buys), so it is an explicit review obligation.

Also ask it to confirm cluster/multi-node correctness was not descoped, and that no availability-motivated fallback was introduced.

- [ ] **Step 7: Open the cyoda-go PR**

Target `release/v0.8.4`, not `main`.

```bash
git push -u origin feat/30-prepared-filter
gh pr create --base release/v0.8.4 --title "perf(search): prepare/execute split for the search leaf evaluator"
```

PR body must state: the SPI PR it depends on and that it is merged; the one Gate-7 behaviour change and its cloud-parity record; the infra-precedence inversion; the coverage waivers from spec §8 with their one-line reasons; and the known temporal divergence deliberately left alone.

Milestone the PR to v0.8.4 and put `Closes` lines for every issue it closes in the PR body — a release-merge PR does not re-scan commits, and an un-milestoned closed-by issue is invisible to the release notes.

- [ ] **Step 8: Notify the commercial Cassandra backend**

It is the third step of the landing sequence and cannot build against the new SPI until this PR merges and produces consumable plugin versions. Open a courtesy issue or PR there describing the migration — `spi.MatchFilter(f, e.Data, e.Meta)` → `p := spi.Prepare(f)` hoisted above the errgroup fan-out, and the same for its own `search/predicate/` evaluator and `shard_executor.go`.

**Keep it strictly in scope.** A courtesy cross-repo change must not bundle drive-by fixes; if the work there surfaces a bug, report it, do not commit it.

---

## Coverage Matrix

Carried forward from spec §8, gap-free.

| scenario | unit | running-backend e2e | cross-backend parity | gRPC |
|---|---|---|---|---|
| tree-walk equivalence, `Filter` side | Task 2 (200k randomised, frozen reference) | — | — | — |
| tree-walk equivalence, predicate side | Task 4 (200k randomised, frozen reference) | — | — | — |
| zero-value asymmetry (all five rows) | Task 1 | — | — | — |
| compiles exactly once per query | Task 1 (`compileRegex` counter, 1000-row loop) | — | — | — |
| concurrent `Match` agreement | Task 1 (16 goroutines, `-race`) | — | — | — |
| `postFilter` absence: `LIMIT` pushdown, native `GROUP BY`, scan budget | Tasks 6, 7 (both spellings of match-all, both SQL backends) | — | deliberately per-backend | — |
| cross-module agreement (`match` ≡ sqlite) | Task 10 (`TestPrepared_SqliteParity_Smoke`) | — | — | — |
| tx read-your-own-writes oracle | — | — | Task 10 (`txsearchryw`, all three backends) | — |
| infra-failure precedence | Task 9 (model store down, `OR[$.age > 5, $.x IS_CHANGED]`, plus healthy-store control) | — | — | — |
| criterion behaviour change | — | Task 14 (400 `WORKFLOW_FAILED`, rollback, plus control) | waived — engine behaviour, storage not consulted | waived — same handlers, transport-blind engine |

**Coverage waivers, stated explicitly** (`.claude/rules/test-coverage.md`):

- *No endpoint status table.* No API or gRPC surface changes shape, no new error codes, and §5 moves no status code. There is nothing to tabulate — and correspondingly **no new `errors/<CODE>.md` topic is owed**, since no error code is added. `TestErrCode_Parity` should stay green untouched; if it does not, an error code was added by accident.
- *No gRPC row.* gRPC search funnels through the same `SearchService.Search` as HTTP. The §5 criterion change **is** reachable over gRPC — `internal/grpc/entity.go:52,101,226,290,354` call the same entity handlers — but the engine is transport-blind and both doors map the error through the same `classifyWorkflowError`, so a gRPC row would exercise the transport, not the change. Waived on that basis, not on unreachability.
- *No `e2e/parity` scenario.* The criterion change is engine behaviour evaluated identically on every backend; the storage layer is not consulted. Parity scenarios exist to catch backends disagreeing on one contract, and there is no backend-varying answer here to pin. The `postFilter` row is deliberately per-backend, because what it asserts is backend-specific by construction.
- *No second compile counter at `plugins/memory`.* It would need the `compileRegex` indirection **exported** from a public SPI that three backends implement, to prove something spec §7 already pins by naming every hoist site. This is the one gap the test suite does not close; Task 16 Step 6 makes it an explicit review obligation rather than papering over it.

---

## Self-Review

Checked against the spec, section by section.

- **§1 problem** — Tasks 1, 5–9 hoist every site named. §7's wrapper table maps one-to-one onto Tasks 5, 6, 7.
- **§2 shape** — two walkers, one kernel; both prepared leaf types carry an `Expansion` plus their own addressing (Tasks 1, 3). Evaluators are not unified; that is a separate change and is not attempted here.
- **§3 SPI surface** — `Prepare`/`Match` added, `MatchFilter`/`EvalLeafString`/`Void` deleted (Tasks 1, 12). `ExpandLeaf` keeps its error. The expansion failure is encoded as an explicit `expanded bool`, not as reliance on iota ordering. `Coercion` is deliberately not carried. Zero-value table pinned in Task 1.
- **§4 cyoda-go surface** — `Prepare` returns an error here and not on the SPI side, for the stated reason. All five structural errors move, with exact message text (Task 3). One expansion per non-nil array position; `previousTransition` canonicalised before the field check; `matchSimple`'s per-row array routing preserved. Both "must NOT become errors" cases pinned by name.
- **§5 declared change** — Task 9 (precedence, with the correct non-short-circuiting condition and a healthy-store control), Task 14 (criterion, with a control). No status code changes anywhere; the `search/service.go` wrap text is preserved verbatim.
- **§6 eager preparation** — Task 3's `Prepare` doc states the leaf set matches today's exactly and why narrowing would be fail-open; Task 9 corrects the engine's now-wrong "loaded lazily" comment.
- **§7 call sites** — every wrapper and hoist point in the table has a task. `postFilter` nil-ness handled as §7 prescribes, including `postgres/searcher.go:220` called out explicitly with a `grep` verification step. All four stale-comment families corrected (Tasks 11, 12).
- **§8 testing** — every row has a task; see the matrix. The compile-counter test is not `t.Parallel()` and lives in an internal test file, while the concurrency test lives in the external one, so the indirection swap cannot overlap it.
- **§9 known divergence** — Task 13, comments at both evaluators with no issue number, plus an explicit check that the shipped help topics were not "corrected" back.
- **§10 landing** — Tasks 15, 16 in order: SPI merge → pin bump → docs → cyoda-go PR → Cassandra notification.
- **§11 not in scope** — nothing in the plan bounds regex program size, refuses impossible conditions, unifies the evaluators, adds work budgets, or chases the allocation target.

**Two places the plan departs from the spec, both deliberate:**

1. **Task order interleaves the repos.** The spec presents the SPI as step 1 and cyoda-go as step 2. Doing that literally means deleting `spi.MatchFilter` while cyoda-go still calls it, breaking the `go.work` build for the whole middle of the change. The plan therefore adds additively, migrates, then deletes — the commits still land on their own branches in the spec's order, and the SPI PR still merges first.
2. **§9's comment site.** The spec names `internal/match.matchTemporalMeta`. That function is deleted by Task 11; its field-identity guard lives in `prepareLifecycle`'s temporal arm, which is where the comment goes. Task 13 records this.

**One thing the spec asserts that the plan verifies rather than assumes:** Task 14 requires the workflow import step to succeed, and says explicitly that an import failure would mean the §5 change is unreachable and must be surfaced, not worked around.
