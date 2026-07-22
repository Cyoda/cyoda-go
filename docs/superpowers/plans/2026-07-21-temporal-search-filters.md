# Temporal Search Filters (#423) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Compare the temporally-typed meta fields `creationDate`/`lastUpdateTime` chronologically (not lexically) in search filters, type-soundly, consistently across memory/sqlite/postgres and the workflow-criteria path.

**Architecture:** Type-driven. A shared SPI kernel (`ParseTemporalMillis` + a `CompareTemporal` dispatcher) is delegated to by both Go evaluators (`internal/match`, `spi.MatchFilter`); the SQL planners mirror it (postgres `cyoda_epoch_millis` IMMUTABLE fn; sqlite µs→ms). A `Filter.Coercion` marker stamped in the domain layer (mirroring `OrderSpec.Kind`) routes temporal leaves. The meta filter vocabulary is reconciled to `sortableMetaFields` across all surfaces.

**Tech Stack:** Go 1.26, `github.com/tidwall/gjson`, pgx (postgres), modernc.org/sqlite, testcontainers-go (e2e), `cyoda-go-spi` (SPI, coordinated release).

**Spec:** `docs/superpowers/specs/2026-07-21-temporal-search-filters-design.md` — read it; this plan implements it. Section refs below (e.g. "spec §6.4") point there.

## Global Constraints

- **No operand-sniffing.** Comparison class is type-driven only; `String` fields are untouched (spec §2, §4 non-goals).
- **Canonical scalar = floored epoch-milliseconds** (`time.Time.UnixMilli`); matches `OrderTemporal` (spec §5).
- **Shared primitives, not per-evaluator copies.** Every temporal/numeric leaf decision lives in ONE SPI function both evaluators call — this is the first slice of the #431 convergence and must not be duplicated (spec §13).
- **SPI is a coordinated-release dependency.** Develop against the local checkout via `go.work` (gitignored; NO committed `replace`); pseudo-version-pin during the v0.8.3 window; SPI tag + final pin bump at milestone-end (spec §10, MAINTAINING.md).
- **No new error codes** — reuse `CONDITION_TYPE_MISMATCH` and `INVALID_FIELD_PATH` (both exist in `internal/common/error_codes.go` with help topics).
- **Go conventions:** `log/slog` only; wrap errors `fmt.Errorf("...: %w", err)`; `uuid.UUID` not string.
- **`isPushable` parity:** the pushable op set in `plugins/postgres/query_planner.go` and `plugins/sqlite/query_planner.go` must stay identical (do not add/remove ops).

---

## File Structure

**SPI (`/Users/paul/go-projects/cyoda-light/cyoda-go-spi`):**
- `filter.go` — add `FilterCoercion` type + `Filter.Coercion` field.
- `temporal.go` (new) — `ParseTemporalMillis`, `CompareTemporal`, `NumericFloat` (shared kernel).
- `filter_match.go` — temporal branch in `evalLeafFilter`; canonical meta keys in `extractFilterMetaValue`; numeric via `NumericFloat`.

**cyoda-go:**
- `internal/match/operators.go` — numeric alignment via `spi.NumericFloat`.
- `internal/match/match.go` — `matchLifecycle` temporal + canonical vocabulary.
- `internal/domain/search/filter_translate.go` — `ConditionToFilter(cond, fields)` + `Coercion` stamping; `lifecycleToFilter` canonicalization.
- `internal/domain/search/condition_type_validate.go` — temporal operator/operand validation + unknown-meta-field 400.
- `internal/domain/search/service.go` — thread `fields` to `ConditionToFilter`.
- `internal/domain/entity/grouped_stats_service.go` — pass `nil` fields to `ConditionToFilter`.
- `plugins/postgres/migrations/000005_temporal_epoch_millis.{up,down}.sql` (new) — `cyoda_epoch_millis`.
- `plugins/postgres/query_planner.go` — meta-key mapping + temporal SQL emission.
- `plugins/sqlite/query_planner.go` — meta-key mapping + temporal SQL emission.
- Tests: `internal/e2e/search_temporal_test.go` (new), `e2e/parity/search_temporal.go` (new) + `registry.go`, `internal/grpc/search_temporal_test.go` (new), plus per-package unit tests.
- Docs: `cmd/cyoda/help/content/cli/search.md` (or the search help topic), `CHANGELOG.md`, `COMPATIBILITY.md`.

---

## Task 0: Local SPI composition + clean baseline

**Files:**
- Create: `go.work` entry (gitignored, local-only) pointing at the SPI checkout.

- [ ] **Step 1: Add the local SPI to go.work**

The worktree already has a `go.work` with `.` and the three plugins. Append the SPI checkout (absolute path; go.work is gitignored):

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/fix-temporal-search-filters
go work edit -use /Users/paul/go-projects/cyoda-light/cyoda-go-spi
cat go.work   # verify the SPI line is present
```

- [ ] **Step 2: Verify baseline compiles against local SPI**

Run: `go build ./... && (cd plugins/postgres && go build ./...) && (cd plugins/sqlite && go build ./...)`
Expected: clean build (no changes yet, just composing against local SPI).

- [ ] **Step 3: Baseline unit tests green**

Run: `go test -short ./internal/match/... ./internal/domain/search/... && (cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./...)`
Expected: PASS. If anything fails, stop and report — do not build on a red baseline.

- [ ] **Step 4: Commit** (nothing to commit — go.work is gitignored; this task is environment setup only. Skip.)

---

## Task 1: SPI — `ParseTemporalMillis`

**Files:**
- Create: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/temporal.go`
- Test: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/temporal_test.go`

**Interfaces:**
- Produces: `func ParseTemporalMillis(s string) (int64, bool)` — floored epoch-ms from offset-bearing RFC3339; `ok=false` otherwise.

- [ ] **Step 1: Write the failing test**

```go
package spi

import "testing"

func TestParseTemporalMillis(t *testing.T) {
	cases := []struct {
		in   string
		ms   int64
		ok   bool
	}{
		{"2021-01-01T00:00:00Z", 1609459200000, true},
		{"2021-01-01T00:00:00.000Z", 1609459200000, true},   // same instant as above
		{"2021-06-01T14:00:00+02:00", 1622548800000, true},  // = 12:00Z
		{"2021-06-01T13:00:00Z", 1622552400000, true},       // 1h after the +02:00 one
		{"2021-01-01T00:00:00.5Z", 1609459200500, true},     // sub-second kept to ms
		{"2021-01-01T00:00:00", 0, false},                   // offset-less rejected
		{"2021-01-01", 0, false},                            // date-only rejected
		{"not-a-date", 0, false},
	}
	for _, c := range cases {
		ms, ok := ParseTemporalMillis(c.in)
		if ok != c.ok || (ok && ms != c.ms) {
			t.Errorf("ParseTemporalMillis(%q) = (%d,%v), want (%d,%v)", c.in, ms, ok, c.ms, c.ok)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestParseTemporalMillis ./...`
Expected: FAIL (undefined: ParseTemporalMillis).

- [ ] **Step 3: Write minimal implementation**

```go
package spi

import "time"

// ParseTemporalMillis parses an offset-bearing RFC3339 timestamp to floored
// epoch-milliseconds. Returns ok=false for any input that is not full RFC3339
// with an explicit offset (Z or ±hh:mm). The mandatory offset makes the value an
// absolute instant — which is what lets the SQL cyoda_epoch_millis be IMMUTABLE.
// Shared kernel: called by internal/match, spi.MatchFilter, and the SQL planners
// (to precompute operands). Do not duplicate this logic (#431).
func ParseTemporalMillis(s string) (int64, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestParseTemporalMillis ./...`
Expected: PASS.

- [ ] **Step 5: Commit** (in the SPI repo)

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add temporal.go temporal_test.go
git commit -m "feat: ParseTemporalMillis — canonical epoch-ms temporal scalar"
```

---

## Task 2: SPI — `Filter.Coercion` marker

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/filter.go:49-56`
- Test: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/filter_test.go` (add)

**Interfaces:**
- Produces: `type FilterCoercion int`; `const (CoerceNone FilterCoercion = iota; CoerceTemporal)`; `Filter.Coercion FilterCoercion`.

- [ ] **Step 1: Write the failing test**

```go
func TestFilterCoercionZeroValue(t *testing.T) {
	var f Filter
	if f.Coercion != CoerceNone {
		t.Fatalf("zero Filter.Coercion = %v, want CoerceNone", f.Coercion)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestFilterCoercionZeroValue ./...`
Expected: FAIL (undefined CoerceNone / Coercion).

- [ ] **Step 3: Implement**

In `filter.go`, add above the `Filter` struct:

```go
// FilterCoercion selects the comparison semantics for a leaf, mirroring
// OrderSpec.Kind for sort. CoerceNone (zero value) preserves the existing
// numeric/text/bool evaluation; CoerceTemporal compares as floored epoch-ms
// instants. The domain layer stamps this from the model schema / meta type;
// backends consume it without inspecting the value (#423). #137 adds no new value.
type FilterCoercion int

const (
	CoerceNone     FilterCoercion = iota
	CoerceTemporal
)
```

And add the field to `Filter`:

```go
type Filter struct {
	Op       FilterOp
	Path     string
	Source   FieldSource
	Value    any
	Values   []any
	Children []Filter
	Coercion FilterCoercion // #423: temporal comparison routing (zero = CoerceNone)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run TestFilterCoercionZeroValue ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add filter.go filter_test.go
git commit -m "feat: Filter.Coercion marker (CoerceNone/CoerceTemporal)"
```

---

## Task 3: SPI — `CompareTemporal` dispatcher + `NumericFloat` shared helper

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/temporal.go`
- Test: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/temporal_test.go`

**Interfaces:**
- Produces:
  - `func CompareTemporal(op FilterOp, storedMs int64, storedOK bool, loMs int64, hiMs int64, loHiOK bool) bool` — the single per-operator temporal decision incl. exclude/vacuous (spec §7.1). For non-BETWEEN ops only `loMs`/`loHiOK` are used (hi ignored).
  - `func NumericFloat(v any) (float64, bool)` — numeric coercion that does NOT parse strings (shared by both evaluators; #431 seed).

- [ ] **Step 1: Write the failing test**

```go
func TestCompareTemporal(t *testing.T) {
	const a = int64(1000) // stored
	// op, stored, storedOK, lo, hi, loHiOK -> want
	type c struct{ op FilterOp; stored int64; sok bool; lo, hi int64; lok, want bool }
	cases := []c{
		{FilterEq, 1000, true, 1000, 0, true, true},
		{FilterEq, 1000, true, 1001, 0, true, false},
		{FilterNe, 1000, true, 1000, 0, true, false},
		{FilterNe, 1000, true, 1001, 0, true, true},
		{FilterGt, 1000, true, 999, 0, true, true},
		{FilterGte, 1000, true, 1000, 0, true, true},
		{FilterLt, 1000, true, 1000, 0, true, false},
		{FilterLte, 1000, true, 1000, 0, true, true},
		{FilterBetween, 1000, true, 900, 1100, true, true},
		{FilterBetween, 1000, true, 1001, 1100, true, false},
		// stored not convertible -> exclude for positive, vacuous-true for NE
		{FilterEq, 0, false, 1000, 0, true, false},
		{FilterGt, 0, false, 1000, 0, true, false},
		{FilterNe, 0, false, 1000, 0, true, true},
	}
	_ = a
	for i, tc := range cases {
		got := CompareTemporal(tc.op, tc.stored, tc.sok, tc.lo, tc.hi, tc.lok)
		if got != tc.want {
			t.Errorf("case %d: CompareTemporal(%v,...) = %v want %v", i, tc.op, got, tc.want)
		}
	}
}

func TestNumericFloatNoStringParse(t *testing.T) {
	if _, ok := NumericFloat("20"); ok {
		t.Error("NumericFloat must NOT parse strings")
	}
	if f, ok := NumericFloat(float64(20)); !ok || f != 20 {
		t.Errorf("NumericFloat(20.0) = (%v,%v)", f, ok)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run 'TestCompareTemporal|TestNumericFloat' ./...`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement** (append to `temporal.go`)

```go
import "encoding/json" // add to existing imports as needed

// CompareTemporal is the single per-operator temporal decision, shared by both
// Go evaluators (#431). storedOK=false (stored value not a valid instant) →
// excluded for positive ops, vacuously true for NE. loMs is the (single) operand
// for non-BETWEEN ops; loMs..hiMs are the inclusive bounds for BETWEEN. loHiOK is
// false only if an operand failed to parse (validation makes this unreachable for
// validated callers; evaluators still degrade safely).
func CompareTemporal(op FilterOp, storedMs int64, storedOK bool, loMs, hiMs int64, loHiOK bool) bool {
	if !storedOK || !loHiOK {
		return op == FilterNe // vacuous-true for NE, exclude otherwise
	}
	switch op {
	case FilterEq:
		return storedMs == loMs
	case FilterNe:
		return storedMs != loMs
	case FilterGt:
		return storedMs > loMs
	case FilterLt:
		return storedMs < loMs
	case FilterGte:
		return storedMs >= loMs
	case FilterLte:
		return storedMs <= loMs
	case FilterBetween:
		return storedMs >= loMs && storedMs <= hiMs
	}
	return false
}

// NumericFloat coerces genuine numeric Go types to float64. It deliberately does
// NOT parse strings — this is the canonical numeric-leaf coercion both evaluators
// use (#423 numeric alignment / #431 seed). Mirrors the existing toFilterFloat64.
func NumericFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run 'TestCompareTemporal|TestNumericFloat' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add temporal.go temporal_test.go
git commit -m "feat: CompareTemporal dispatcher + NumericFloat shared coercion"
```

---

## Task 4: SPI — temporal branch + canonical meta vocabulary in `MatchFilter`

**Files:**
- Modify: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/filter_match.go` (`evalLeafFilter`, `extractFilterMetaValue`, `compareFilterValues`)
- Test: `/Users/paul/go-projects/cyoda-light/cyoda-go-spi/filter_match_test.go`

**Interfaces:**
- Consumes: `ParseTemporalMillis`, `CompareTemporal`, `NumericFloat`, `Filter.Coercion`.
- Produces: `spi.MatchFilter` honoring `CoerceTemporal` and the canonical meta keys.

- [ ] **Step 1: Write the failing test**

```go
func TestMatchFilter_TemporalMeta(t *testing.T) {
	meta := EntityMeta{CreationDate: time.Date(2021,1,1,0,0,0,0,time.UTC)}
	// stored 2021-01-01T00:00:00Z; operand 2021-01-01T00:00:00.000Z → same instant
	eq := Filter{Op: FilterEq, Source: SourceMeta, Path: "creationDate",
		Coercion: CoerceTemporal, Value: "2021-01-01T00:00:00.000Z"}
	if !MatchFilter(eq, nil, meta) {
		t.Error("EQUALS same-instant (mixed precision) should match")
	}
	gt := Filter{Op: FilterGt, Source: SourceMeta, Path: "creationDate",
		Coercion: CoerceTemporal, Value: "2020-12-31T23:59:59Z"}
	if !MatchFilter(gt, nil, meta) {
		t.Error("GREATER_THAN earlier instant should match")
	}
	// offset operand: 2021-01-01T01:00:00+01:00 == 00:00:00Z → equal
	eqOff := Filter{Op: FilterEq, Source: SourceMeta, Path: "creationDate",
		Coercion: CoerceTemporal, Value: "2021-01-01T01:00:00+01:00"}
	if !MatchFilter(eqOff, nil, meta) {
		t.Error("EQUALS with offset operand denoting same instant should match")
	}
}

func TestExtractFilterMetaValue_CanonicalKeys(t *testing.T) {
	meta := EntityMeta{ID: "e1", State: "S", TransitionForLatestSave: "t",
		TransactionID: "tx", CreationDate: time.Unix(1,0)}
	for _, k := range []string{"id", "state", "transitionForLatestSave", "transactionId", "creationDate", "lastUpdateTime"} {
		if _, ok := extractFilterMetaValue(k, meta); !ok {
			t.Errorf("extractFilterMetaValue(%q) not found", k)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test -run 'TestMatchFilter_TemporalMeta|TestExtractFilterMetaValue_CanonicalKeys' ./...`
Expected: FAIL (creationDate not mapped / no temporal branch).

- [ ] **Step 3: Implement**

In `extractFilterMetaValue`, add the canonical client-name cases (keep the existing storage-key cases — additive; see spec §6.5):

```go
	case "id":
		return meta.ID, true
	case "creationDate":
		return meta.CreationDate, true          // time.Time (temporal)
	case "lastUpdateTime":
		return meta.LastModifiedDate, true      // time.Time (temporal)
	case "transitionForLatestSave":
		return meta.TransitionForLatestSave, true
	case "transactionId":
		return meta.TransactionID, true
```

In `evalLeafFilter`, BEFORE the existing comparison switch, add a temporal branch gated on `Coercion`:

```go
	if f.Coercion == CoerceTemporal {
		return evalTemporalLeaf(f, val) // val already extracted above; found/null handled by the earlier guard
	}
```

Add the helper (converts stored `time.Time` or RFC3339 string → ms; operand(s) via `ParseTemporalMillis`; delegates to `CompareTemporal`):

```go
func evalTemporalLeaf(f Filter, val any) bool {
	storedMs, storedOK := toEpochMillis(val)
	if f.Op == FilterBetween {
		if len(f.Values) < 2 {
			return false
		}
		lo, lok := ParseTemporalMillis(fmt.Sprint(f.Values[0]))
		hi, hok := ParseTemporalMillis(fmt.Sprint(f.Values[1]))
		return CompareTemporal(FilterBetween, storedMs, storedOK, lo, hi, lok && hok)
	}
	op, ook := ParseTemporalMillis(fmt.Sprint(f.Value))
	return CompareTemporal(f.Op, storedMs, storedOK, op, 0, ook)
}

// toEpochMillis converts a stored leaf value to floored epoch-ms. time.Time →
// UnixMilli (meta path); RFC3339 string → ParseTemporalMillis (future #137 body
// text). Anything else is not a valid instant → ok=false (excluded per §7.1).
func toEpochMillis(v any) (int64, bool) {
	switch t := v.(type) {
	case time.Time:
		return t.UnixMilli(), true
	case string:
		return ParseTemporalMillis(t)
	}
	return 0, false
}
```

Finally, in `compareFilterValues` replace the local `toFilterFloat64` calls with `NumericFloat` (behaviour identical; now the shared symbol). Keep `toFilterFloat64` as a thin alias or delete its callers — do not change semantics.

Note: the `IsNull`/`NotNull` early-return path in `evalLeafFilter` is unchanged and must run before the temporal branch (a `CoerceTemporal` filter never uses those ops in practice, but presence semantics stay correct).

- [ ] **Step 4: Run to verify it passes**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./...`
Expected: PASS (new tests + no regressions).

- [ ] **Step 5: Commit**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add filter_match.go filter_match_test.go
git commit -m "feat: temporal leaf branch + canonical meta vocabulary in MatchFilter"
```

---

## Task 5: SPI — push a dev commit and pseudo-version-pin cyoda-go

**Files:**
- Modify (root + 3 plugins): `go.mod` `cyoda-go-spi` require line.

- [ ] **Step 1: Push the SPI dev commits**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git push origin HEAD:main   # or the SPI feature branch per MAINTAINING.md; capture the pushed SHA
```

- [ ] **Step 2: Pseudo-version-pin cyoda-go to the pushed SPI SHA**

Use `make repin-plugins` if applicable, or manually bump the `cyoda-go-spi` require line to the new pseudo-version in the root and all three `plugins/*/go.mod`, then `go mod tidy` in each. Verify:

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go/.claude/worktrees/fix-temporal-search-filters && make check-spi-pin-sync`
Expected: PASS (all four manifests agree).

- [ ] **Step 3: Commit the pin bump**

```bash
git add go.mod go.sum plugins/*/go.mod plugins/*/go.sum
git commit -m "chore: pin cyoda-go-spi to dev-window pseudo-version for #423 temporal kernel"
```

Note: local development still composes against the checkout via `go.work` (Task 0); the pin keeps CI resolvable. The SPI tag + final pin happen at milestone-end (spec §10).

---

## Task 6: Domain — `ConditionToFilter` stamps `Coercion`

**Files:**
- Modify: `internal/domain/search/filter_translate.go` (`ConditionToFilter`, `simpleToFilter`, `lifecycleToFilter`, `groupToFilter`, `arrayToFilter`)
- Modify: `internal/domain/search/service.go` (thread `fields`)
- Modify: `internal/domain/entity/grouped_stats_service.go` (pass `nil`)
- Test: `internal/domain/search/filter_translate_test.go`

**Interfaces:**
- Consumes: `spi.Filter.Coercion`, `classifyType`, `schema.FieldDescriptor`.
- Produces: `func ConditionToFilter(cond predicate.Condition, fields map[string]schema.FieldDescriptor) (spi.Filter, error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestConditionToFilter_StampsTemporalMeta(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_THAN", Value: "2021-01-01T00:00:00Z"}
	f, err := ConditionToFilter(c, nil)
	if err != nil { t.Fatal(err) }
	if f.Coercion != spi.CoerceTemporal {
		t.Errorf("creationDate leaf Coercion = %v, want CoerceTemporal", f.Coercion)
	}
}

func TestConditionToFilter_DataLeafStampsNone(t *testing.T) {
	c := &predicate.SimpleCondition{JsonPath: "$.name", OperatorType: "EQUALS", Value: "x"}
	f, _ := ConditionToFilter(c, nil) // no schema → CoerceNone
	if f.Coercion != spi.CoerceNone {
		t.Errorf("data leaf Coercion = %v, want CoerceNone", f.Coercion)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/search/ -run TestConditionToFilter_Stamps -v`
Expected: FAIL (ConditionToFilter arity / field).

- [ ] **Step 3: Implement**

Add the parameter and stamping. In `filter_translate.go`:

```go
// metaTemporalFields are the SourceMeta paths whose comparison is temporal.
var metaTemporalFields = map[string]bool{"creationDate": true, "lastUpdateTime": true}

func ConditionToFilter(cond predicate.Condition, fields map[string]schema.FieldDescriptor) (spi.Filter, error) {
	// ... existing nil guard ...
	switch c := cond.(type) {
	case *predicate.SimpleCondition:
		return simpleToFilter(c, fields)
	case *predicate.LifecycleCondition:
		return lifecycleToFilter(c), nil
	case *predicate.GroupCondition:
		return groupToFilter(c, fields)
	case *predicate.ArrayCondition:
		return arrayToFilter(c)
	// ... unchanged ...
	}
}

func simpleToFilter(c *predicate.SimpleCondition, fields map[string]schema.FieldDescriptor) (spi.Filter, error) {
	stripped, err := stripDollarDot(c.JsonPath)
	if err != nil { return spi.Filter{}, err }
	return spi.Filter{
		Op: mapOperator(c.OperatorType), Path: stripped, Source: spi.SourceData,
		Value: c.Value, Coercion: dataCoercion(c.JsonPath, fields),
	}, nil
}

// dataCoercion returns CoerceTemporal only if the schema classifies the field as
// temporal. Today classifyType never returns OrderTemporal for data → always
// CoerceNone. #137 flips scalarClass and this lights up with no change here.
func dataCoercion(jsonPath string, fields map[string]schema.FieldDescriptor) spi.FilterCoercion {
	if fields == nil { return spi.CoerceNone }
	fd, ok := fields[jsonPath]
	if !ok { return spi.CoerceNone }
	if kind, err := classifyType(fd.Types); err == nil && kind == spi.OrderTemporal {
		return spi.CoerceTemporal
	}
	return spi.CoerceNone
}
```

Update `lifecycleToFilter` to canonicalize the `previousTransition` alias and stamp temporal:

```go
func lifecycleToFilter(c *predicate.LifecycleCondition) spi.Filter {
	field := c.Field
	if field == "previousTransition" {
		field = "transitionForLatestSave"
	}
	co := spi.CoerceNone
	if metaTemporalFields[field] {
		co = spi.CoerceTemporal
	}
	return spi.Filter{Op: mapOperator(c.OperatorType), Path: field, Source: spi.SourceMeta, Value: c.Value, Coercion: co}
}
```

Update `groupToFilter(c, fields)` to thread `fields` into the recursive `ConditionToFilter(child, fields)` call. Update the two external callers: `service.go:179` → `ConditionToFilter(cond, fields)` (see Task 6b), `grouped_stats_service.go:105` → `ConditionToFilter(parsedCond, nil)`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/domain/search/ ./internal/domain/entity/ -run 'ConditionToFilter|Filter' -v && go build ./...`
Expected: PASS + build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/filter_translate.go internal/domain/search/service.go internal/domain/entity/grouped_stats_service.go internal/domain/search/filter_translate_test.go
git commit -m "feat(search): stamp Filter.Coercion from schema/meta type in ConditionToFilter"
```

---

## Task 6b: Domain — hoist `FieldsMap` into `Search` and pass to `ConditionToFilter`

**Files:**
- Modify: `internal/domain/search/service.go` (`Search`)
- Test: `internal/domain/search/service_test.go`

- [ ] **Step 1: Write the failing test** — an integration-style test on the memory backend asserting a `creationDate GREATER_THAN` search returns the chronologically-correct set (this is RED until the whole pushdown chain works; keep it as the driving test for Tasks 6b/12/13). Minimal version:

```go
func TestSearch_CreationDateGreaterThan_Memory(t *testing.T) {
	// build a SearchService over the memory factory; create 2 entities;
	// creationDate is engine-set, so assert GREATER_THAN "<between the two>"
	// returns exactly the later entity. (Use existing memory test harness.)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/search/ -run TestSearch_CreationDateGreaterThan_Memory -v`
Expected: FAIL (currently no-match).

- [ ] **Step 3: Implement** — in `Search`, load the FieldsMap once and pass it:

```go
	fields, _ := loadFieldsMap(ctx, modelStore, modelRef) // best-effort; nil-tolerant
	...
	filter, translateErr := ConditionToFilter(cond, fields)
```

(Reuse the existing `loadFieldsMap` helper; ignore its error the same way validation does — a nil map yields `CoerceNone` for data leaves, meta temporal still stamps via the static table.)

- [ ] **Step 4: Run to verify it passes** (memory backend now temporal-correct)

Run: `go test ./internal/domain/search/ -run TestSearch_CreationDateGreaterThan_Memory -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/service.go internal/domain/search/service_test.go
git commit -m "feat(search): thread schema FieldsMap into ConditionToFilter"
```

---

## Task 7: Domain — temporal operator/operand validation + unknown-meta 400

**Files:**
- Modify: `internal/domain/search/condition_type_validate.go` (lift lifecycle exemption for temporal; validate operator + operand + meta-field name)
- Test: `internal/domain/search/condition_type_validate_test.go`

**Interfaces:**
- Consumes: `spi.ParseTemporalMillis`, `metaTemporalFields`, the canonical meta set, `errConditionTypeMismatch`, `common.ErrCodeInvalidFieldPath`.

- [ ] **Step 1: Write the failing test**

```go
func TestValidate_TemporalRejectsStringOp(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "CONTAINS", Value: "2021"}
	if err := validateLifecycleType(c); !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("CONTAINS on creationDate should be CONDITION_TYPE_MISMATCH, got %v", err)
	}
}
func TestValidate_TemporalRejectsBadOperand(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "GREATER_THAN", Value: "not-a-date"}
	if err := validateLifecycleType(c); !errors.Is(err, errConditionTypeMismatch) {
		t.Errorf("non-RFC3339 operand on creationDate should be CONDITION_TYPE_MISMATCH, got %v", err)
	}
}
func TestValidate_UnknownMetaField(t *testing.T) {
	c := &predicate.LifecycleCondition{Field: "bogus", OperatorType: "EQUALS", Value: "x"}
	err := validateLifecycleType(c)
	if err == nil { t.Fatal("unknown meta field must be rejected") }
	// handler maps this to 400 INVALID_FIELD_PATH
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/domain/search/ -run TestValidate_ -v`
Expected: FAIL (lifecycle currently exempt).

- [ ] **Step 3: Implement**

Add a lifecycle type/operator validator and call it from `walkConditionTypes` (replacing the blanket `LifecycleCondition → return nil`):

```go
var comparisonOps = map[string]bool{
	"EQUALS": true, "NOT_EQUAL": true, "GREATER_THAN": true, "LESS_THAN": true,
	"GREATER_OR_EQUAL": true, "LESS_OR_EQUAL": true, "BETWEEN": true,
	"IS_NULL": true, "NOT_NULL": true,
}

// canonicalMetaFilterFields mirrors sortableMetaFields (+ previousTransition alias).
var canonicalMetaFilterFields = map[string]bool{
	"state": true, "creationDate": true, "lastUpdateTime": true,
	"transitionForLatestSave": true, "previousTransition": true, "transactionId": true, "id": true,
}

func validateLifecycleType(c *predicate.LifecycleCondition) error {
	if !canonicalMetaFilterFields[c.Field] {
		return fmt.Errorf("unknown meta filter field %q: %w", c.Field, errInvalidFieldPath)
	}
	field := c.Field
	if field == "previousTransition" { field = "transitionForLatestSave" }
	if metaTemporalFields[field] {
		if !comparisonOps[c.OperatorType] {
			return fmt.Errorf("operator %q is not valid on temporal field %q: %w", c.OperatorType, c.Field, errConditionTypeMismatch)
		}
		// operand(s) must be offset-bearing RFC3339 (skip for null-only ops)
		if c.OperatorType != "IS_NULL" && c.OperatorType != "NOT_NULL" {
			for _, v := range operandStrings(c.Value) {
				if _, ok := spi.ParseTemporalMillis(v); !ok {
					return fmt.Errorf("operand %q is not a valid timestamp for temporal field %q: %w", v, c.Field, errConditionTypeMismatch)
				}
			}
		}
	}
	return nil
}
```

Add `errInvalidFieldPath` sentinel (mapped by the handler to 400 `INVALID_FIELD_PATH`) and a small `operandStrings(any) []string` helper (handles scalar + BETWEEN `[]any`). Wire `walkConditionTypes`'s `*predicate.LifecycleCondition` case to call `validateLifecycleType(c)`.

Verify the HTTP + gRPC handlers translate `errInvalidFieldPath`/`errConditionTypeMismatch` to the right 400 codes (they already map `errConditionTypeMismatch`; add the `errInvalidFieldPath` mapping if absent — reuse the existing invalid-path 400).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/domain/search/ -run TestValidate_ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/search/condition_type_validate.go internal/domain/search/condition_type_validate_test.go
git commit -m "feat(search): type-sound operator/operand validation for temporal meta fields + unknown-field 400"
```

---

## Task 8: `internal/match` — `matchLifecycle` temporal + canonical vocabulary

**Files:**
- Modify: `internal/match/match.go` (`matchLifecycle`)
- Test: `internal/match/match_test.go`

**Interfaces:**
- Consumes: `spi.ParseTemporalMillis`, `spi.CompareTemporal`.

- [ ] **Step 1: Write the failing test**

```go
func TestMatchLifecycle_TemporalEquals(t *testing.T) {
	meta := spi.EntityMeta{CreationDate: time.Date(2021,1,1,0,0,0,0,time.UTC)}
	c := &predicate.LifecycleCondition{Field: "creationDate", OperatorType: "EQUALS", Value: "2021-01-01T00:00:00.000Z"}
	ok, err := matchLifecycle(c, meta)
	if err != nil || !ok { t.Errorf("EQUALS same-instant should match; ok=%v err=%v", ok, err) }
}
func TestMatchLifecycle_LastUpdateTime(t *testing.T) {
	meta := spi.EntityMeta{LastModifiedDate: time.Date(2021,6,1,12,0,0,0,time.UTC)}
	c := &predicate.LifecycleCondition{Field: "lastUpdateTime", OperatorType: "GREATER_THAN", Value: "2021-06-01T11:00:00Z"}
	ok, err := matchLifecycle(c, meta)
	if err != nil || !ok { t.Errorf("lastUpdateTime GT earlier should match; ok=%v err=%v", ok, err) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/match/ -run TestMatchLifecycle_ -v`
Expected: FAIL (lexical / errors on lastUpdateTime).

- [ ] **Step 3: Implement** — rewrite `matchLifecycle` to route temporal fields through the shared dispatcher and add the canonical string fields:

```go
func matchLifecycle(c *predicate.LifecycleCondition, meta spi.EntityMeta) (bool, error) {
	field := c.Field
	if field == "previousTransition" { field = "transitionForLatestSave" }
	switch field {
	case "creationDate":
		return matchTemporalMeta(c.OperatorType, meta.CreationDate, c.Value)
	case "lastUpdateTime":
		return matchTemporalMeta(c.OperatorType, meta.LastModifiedDate, c.Value)
	case "state":
		return applyStringLifecycle(c, meta.State)
	case "transitionForLatestSave":
		return applyStringLifecycle(c, meta.TransitionForLatestSave)
	case "transactionId":
		return applyStringLifecycle(c, meta.TransactionID)
	case "id":
		return applyStringLifecycle(c, meta.ID)
	default:
		return false, fmt.Errorf("unknown lifecycle field: %s", c.Field)
	}
}

func matchTemporalMeta(op string, stored time.Time, value any) (bool, error) {
	fop := mapOpToFilterOp(op) // small local map string->spi.FilterOp for the 7 comparison ops
	storedMs, storedOK := stored.UnixMilli(), !stored.IsZero()
	if op == "BETWEEN" || op == "BETWEEN_INCLUSIVE" {
		lo, hi, ok := twoTemporalBounds(value)
		return spi.CompareTemporal(spi.FilterBetween, storedMs, storedOK, lo, hi, ok), nil
	}
	ms, ok := spi.ParseTemporalMillis(fmt.Sprint(value))
	return spi.CompareTemporal(fop, storedMs, storedOK, ms, 0, ok), nil
}
```

`applyStringLifecycle` preserves the existing `applyOperator`-on-string behaviour (wrap value in the fake-gjson exactly as today). Add `mapOpToFilterOp` and `twoTemporalBounds` helpers. Note: validation (Task 7) already rejects invalid ops/operands at the boundary; `matchLifecycle` degrades safely if reached unvalidated.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/match/ -v`
Expected: PASS (new + existing).

- [ ] **Step 5: Commit**

```bash
git add internal/match/match.go internal/match/match_test.go
git commit -m "feat(match): chronological matchLifecycle for creationDate/lastUpdateTime + canonical meta vocabulary"
```

---

## Task 9: `internal/match` — numeric alignment (delegate to `spi.NumericFloat`)

**Files:**
- Modify: `internal/match/operators.go` (`opCompare`, `opEquals`)
- Test: `internal/match/operators_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestOpCompare_NoStringOperandCoercion(t *testing.T) {
	// numeric field value 100, string operand "20": must NOT be numeric (100>20),
	// aligning with spi.compareFilterValues (lexical) — matches pushdown.
	actual := gjson.Parse(`100`)
	got, _ := opCompare(actual, "20", func(a, b float64) bool { return a > b }, func(a, b string) bool { return a > b })
	// lexical "100" > "20" is false
	if got { t.Error("string operand must not be numerically coerced (align to spi)") }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/match/ -run TestOpCompare_NoStringOperandCoercion -v`
Expected: FAIL (currently parses "20"→numeric → 100>20 true).

- [ ] **Step 3: Implement** — in `opCompare` and `opEquals`, replace `toFloat64(expected)` (which parses strings) with `spi.NumericFloat(expected)` (no string parse). Keep the `actual.Type == gjson.Number` guard:

```go
func opCompare(actual gjson.Result, expected any, numCmp func(float64,float64) bool, strCmp func(string,string) bool) (bool, error) {
	if opIsNull(actual) { return false, nil }
	if expFloat, ok := spi.NumericFloat(expected); ok && actual.Type == gjson.Number {
		return numCmp(actual.Float(), expFloat), nil
	}
	expStr := fmt.Sprintf("%v", expected)
	return strCmp(actual.String(), expStr), nil
}
```

Apply the same change in `opEquals`. Leave `toFloat64` in place only if still used elsewhere (e.g. `opBetween`); if `opBetween` needs consistent behaviour, note it — but BETWEEN numeric semantics are out of #423's temporal scope; keep as-is unless a test requires otherwise.

Check existing `internal/match` tests for any that rely on string→numeric coercion; update them to the aligned behaviour (they were asserting the divergent path) and confirm this matches the intended contract.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/match/ ./internal/domain/... -v`
Expected: PASS (aligned; fix any test that encoded the old divergence).

- [ ] **Step 5: Commit**

```bash
git add internal/match/operators.go internal/match/operators_test.go
git commit -m "fix(match): align numeric leaf coercion to spi (no string-operand parsing) — #431 seed"
```

---

## Task 10: postgres — `cyoda_epoch_millis` migration

**Files:**
- Create: `plugins/postgres/migrations/000005_temporal_epoch_millis.up.sql`
- Create: `plugins/postgres/migrations/000005_temporal_epoch_millis.down.sql`
- Test: `plugins/postgres/cyoda_epoch_millis_test.go`

- [ ] **Step 1: Write the failing test** (Docker required)

```go
func TestCyodaEpochMillis(t *testing.T) {
	db := newTestDB(t) // existing postgres test harness
	q := func(s string) (sql.NullInt64) { var v sql.NullInt64; db.QueryRow("SELECT cyoda_epoch_millis($1)", s).Scan(&v); return v }
	if v := q("2021-01-01T00:00:00Z"); !v.Valid || v.Int64 != 1609459200000 { t.Errorf("Z: %+v", v) }
	if v := q("2021-01-01T00:00:00.000Z"); !v.Valid || v.Int64 != 1609459200000 { t.Errorf("ms: %+v", v) }
	if v := q("2021-06-01T14:00:00+02:00"); !v.Valid || v.Int64 != 1622548800000 { t.Errorf("offset: %+v", v) }
	if v := q("2021-01-01T00:00:00"); v.Valid { t.Error("offset-less must be NULL") }
	if v := q("not-a-date"); v.Valid { t.Error("garbage must be NULL") }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./plugins/postgres/ -run TestCyodaEpochMillis -v`
Expected: FAIL (function absent).

- [ ] **Step 3: Implement** — write the up migration (spec §8.1):

```sql
CREATE OR REPLACE FUNCTION cyoda_epoch_millis(t text) RETURNS bigint AS $$
DECLARE result bigint;
BEGIN
  IF t IS NULL OR t !~ '\A\d{4}-\d{2}-\d{2}T.+(Z|[+-]\d{2}:?\d{2})\Z' THEN
    RETURN NULL;
  END IF;
  BEGIN
    result := floor(extract(epoch from t::timestamptz) * 1000)::bigint;
  EXCEPTION WHEN others THEN
    RETURN NULL;
  END;
  RETURN result;
END;
$$ LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE;
```

Down migration: `DROP FUNCTION IF EXISTS cyoda_epoch_millis(text);`

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./plugins/postgres/ -run TestCyodaEpochMillis -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/migrations/000005_temporal_epoch_millis.up.sql plugins/postgres/migrations/000005_temporal_epoch_millis.down.sql plugins/postgres/cyoda_epoch_millis_test.go
git commit -m "feat(postgres): cyoda_epoch_millis IMMUTABLE NULL-safe RFC3339->epoch-ms fn"
```

---

## Task 11: postgres — meta-key mapping + temporal SQL emission

**Files:**
- Modify: `plugins/postgres/query_planner.go` (`fieldExpr`, `leafToSQL`/`orderingOp`)
- Test: `plugins/postgres/query_planner_test.go`

- [ ] **Step 1: Write the failing test** — assert the generated SQL for a `CoerceTemporal` meta leaf uses `cyoda_epoch_millis(doc->'_meta'->>'creation_date')` and binds an int64 ms operand; and that `fieldExpr` maps `creationDate`→`creation_date`.

```go
func TestPlan_TemporalMetaEmitsEpochMillis(t *testing.T) {
	f := spi.Filter{Op: spi.FilterGt, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	plan := planQuery(f)
	if !strings.Contains(plan.where, "cyoda_epoch_millis(doc->'_meta'->>'creation_date')") {
		t.Errorf("where = %q", plan.where)
	}
	if plan.args[0] != int64(1609459200000) { t.Errorf("arg = %v", plan.args[0]) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./plugins/postgres/ -run TestPlan_TemporalMeta -v`
Expected: FAIL.

- [ ] **Step 3: Implement**
- In `fieldExpr`, for `SourceMeta`, map the path through `metaJSONKey` (import/reference the map from `searcher.go`), and `id`→`entity_id` (directMetaColumns). Fall through to raw path otherwise (unreachable post-validation).
- In `leafToSQL`, before the numeric/text branches, handle `f.Coercion == CoerceTemporal`: emit `(cyoda_epoch_millis(<fieldExpr>) IS NOT NULL AND cyoda_epoch_millis(<fieldExpr>) <op> $N)` with `$N` bound to the Go-computed `int64` from `spi.ParseTemporalMillis(fmt.Sprint(f.Value))`; for `BETWEEN`, two placeholders from `f.Values`. For `NE`, use the `IS NULL OR … != …` form (mirror the existing NE 3VL).

```go
if f.Coercion == spi.CoerceTemporal {
	col := "cyoda_epoch_millis(" + fieldExpr(f) + ")"
	switch f.Op {
	case spi.FilterBetween:
		lo, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[0]))
		hi, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[1]))
		p1, p2 := nextPlaceholder(counter), nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN %s AND %s)", col, col, p1, p2), []any{lo, hi}
	case spi.FilterNe:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		p := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NULL OR %s != %s)", col, col, p), []any{ms}
	default:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		p := nextPlaceholder(counter)
		return fmt.Sprintf("(%s IS NOT NULL AND %s %s %s)", col, col, sqlOpFor(f.Op), p), []any{ms}
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./plugins/postgres/ -run 'TestPlan_TemporalMeta|TestField' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/postgres/query_planner.go plugins/postgres/query_planner_test.go
git commit -m "feat(postgres): temporal meta filter via cyoda_epoch_millis + meta-key mapping"
```

---

## Task 12: sqlite — meta-key mapping + temporal SQL emission

**Files:**
- Modify: `plugins/sqlite/query_planner.go` (`fieldExpr`, `leafToSQL`)
- Test: `plugins/sqlite/query_planner_test.go`

- [ ] **Step 1: Write the failing test** — `CoerceTemporal` meta leaf emits `(json_extract(json(meta),'$.creation_date') / 1000) <op> ?` with int64 ms arg; `fieldExpr` maps `creationDate`→`creation_date`.

```go
func TestSqlitePlan_TemporalMetaDividesMicros(t *testing.T) {
	f := spi.Filter{Op: spi.FilterGt, Source: spi.SourceMeta, Path: "creationDate", Coercion: spi.CoerceTemporal, Value: "2021-01-01T00:00:00Z"}
	sql, args := leafToSQL(f)
	if !strings.Contains(sql, "/ 1000") || !strings.Contains(sql, "creation_date") {
		t.Errorf("sql = %q", sql)
	}
	if args[0] != int64(1609459200000) { t.Errorf("arg = %v", args[0]) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./plugins/sqlite/ -run TestSqlitePlan_TemporalMeta -v`
Expected: FAIL.

- [ ] **Step 3: Implement**
- In `fieldExpr`, for `SourceMeta`, map through `metaBlobKey` (+ `id`→entity_id); fall through to raw otherwise.
- In `leafToSQL`, handle `f.Coercion == CoerceTemporal` for `SourceMeta`: the meta value is µs-int, so emit `(<fieldExpr> / 1000)` as the column expression and compare against the int64 ms operand(s). Mirror the NULL/3VL guards already used (`IS NOT NULL` for positive, `IS NULL OR` for NE), two placeholders for BETWEEN.

```go
if f.Coercion == spi.CoerceTemporal {
	col := "(" + fieldExpr(f) + " / 1000)"
	switch f.Op {
	case spi.FilterBetween:
		lo, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[0]))
		hi, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Values[1]))
		return fmt.Sprintf("(%s IS NOT NULL AND %s BETWEEN ? AND ?)", col, col), []any{lo, hi}
	case spi.FilterNe:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		return fmt.Sprintf("(%s IS NULL OR %s != ?)", col, col), []any{ms}
	default:
		ms, _ := spi.ParseTemporalMillis(fmt.Sprint(f.Value))
		return fmt.Sprintf("(%s IS NOT NULL AND %s %s ?)", col, col, sqlOpFor(f.Op)), []any{ms}
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./plugins/sqlite/ -run 'TestSqlitePlan_TemporalMeta|TestField' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add plugins/sqlite/query_planner.go plugins/sqlite/query_planner_test.go
git commit -m "feat(sqlite): temporal meta filter via micros->ms + meta-key mapping"
```

---

## Task 13: e2e coverage (running postgres)

**Files:**
- Create: `internal/e2e/search_temporal_test.go`
- Test docs: covers the §11.0 error table + §11.1 matrix rows on a real backend.

- [ ] **Step 1: Write the failing tests** — one test function per matrix row (GT/LT/GE/LE/EQ/NE/BETWEEN chronological on `creationDate`; `lastUpdateTime`; string-op-on-temporal→400; bad-operand→400; unknown-meta→400; valid creationDate→200). Use the existing e2e harness (`TestMain` starts postgres + httptest server). Create entities with a controlled time gap; assert the chronologically-correct result set and the documented status codes. Full code follows the existing `internal/e2e/search_test.go` patterns.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/e2e/ -run TestSearchTemporal -v` (Docker required)
Expected: FAIL (RED against current behaviour).

- [ ] **Step 3: Implement** — no production code here (already built in Tasks 4–12); this task makes the e2e assertions concrete and green.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/e2e/ -run TestSearchTemporal -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/search_temporal_test.go
git commit -m "test(e2e): temporal creationDate/lastUpdateTime filters + 400 validation cases"
```

---

## Task 14: cross-backend parity scenarios

**Files:**
- Create: `e2e/parity/search_temporal.go`
- Modify: `e2e/parity/registry.go`
- Test: runs across memory/sqlite/postgres (+ commercial).

- [ ] **Step 1: Write the failing scenarios** — `RunSearchTemporalCreationDate` (GT/LT/GE/LE/EQ/NE/BETWEEN chronological, mixed-precision operands), `RunSearchTemporalLastUpdateTime`, `RunSearchUnknownMetaField400`, and `RunSearchStringMetaVocabulary` (transitionForLatestSave/transactionId/id resolve identically). Follow `e2e/parity/search.go` patterns; register each in `registry.go`.

- [ ] **Step 2: Run to verify they fail** on the reference backends

Run: `go test ./e2e/parity/... -run TestParity -v` (memory), plus per-plugin parity as configured.
Expected: FAIL (RED) before the fix is wired in each backend; GREEN after.

- [ ] **Step 3: Implement** — scenarios only (production code already built).

- [ ] **Step 4: Run to verify they pass** on memory + sqlite + postgres

Run: `make test-all` (Docker required) — confirm parity green across backends.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add e2e/parity/search_temporal.go e2e/parity/registry.go
git commit -m "test(parity): chronological temporal date-filter scenarios across backends"
```

---

## Task 15: gRPC coverage

**Files:**
- Create: `internal/grpc/search_temporal_test.go`

- [ ] **Step 1: Write the failing tests** — assert the gRPC search envelope (`Success`, `Error.Code`) for: `creationDate GREATER_THAN` chronological result; `EQUALS` same-instant; string-op-on-temporal → `CONDITION_TYPE_MISMATCH`; bad operand → `CONDITION_TYPE_MISMATCH`; unknown meta field → `INVALID_FIELD_PATH`. Follow `internal/grpc/search_test.go`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/grpc/ -run TestSearchTemporal -v`
Expected: FAIL.

- [ ] **Step 3: Implement** — tests only.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/grpc/ -run TestSearchTemporal -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/grpc/search_temporal_test.go
git commit -m "test(grpc): temporal filter chronological result + 400 envelopes"
```

---

## Task 16: workflow-criteria coverage + fallback/pushdown agreement

**Files:**
- Create/Modify: `internal/e2e/` or `internal/domain/workflow/` test asserting a workflow **criterion** on `creationDate` (EQUALS precision, GT) fires correctly at a transition; and a test asserting the memory `GetAll` fallback and `spi.MatchFilter` pushdown return the identical set for `creationDate EQUALS` across the precision boundary.

- [ ] **Step 1: Write the failing tests** (per §11.1 rows: criterion EQUALS/NE/ordering; invalid-op-on-temporal rejected at workflow import; fallback==pushdown).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/domain/workflow/ ./internal/e2e/ -run 'Temporal|Criterion' -v`
Expected: FAIL.

- [ ] **Step 3: Implement** — production already built (Tasks 8/7); wire the workflow-import validation to reject invalid temporal-criterion operators (confirm the import path calls the shared validator; if not, route it through `validateLifecycleType`).

- [ ] **Step 4: Run to verify they pass**

Run: same as Step 2.
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "test(workflow): temporal criterion chronological + import-time operator rejection; fallback/pushdown parity"
```

---

## Task 17: docs (Gate 4)

**Files:**
- Modify: the search help topic under `cmd/cyoda/help/content/` (document that `creationDate`/`lastUpdateTime` are temporal and support only comparison operators; list the canonical filterable meta fields).
- Modify: `CHANGELOG.md` (entry under the v0.8.3 section).
- Modify: `COMPATIBILITY.md` (note the SPI surface addition — `ParseTemporalMillis`, `Filter.Coercion` — if the SPI pin is being advanced).
- Verify: `CONDITION_TYPE_MISMATCH.md` and `INVALID_FIELD_PATH.md` help topics already exist (they do) — no new `errors/<CODE>.md` needed; `TestErrCode_Parity` stays green.

- [ ] **Step 1** Update the search help topic (keep prose compact — actionable core only).
- [ ] **Step 2** Add the CHANGELOG entry.
- [ ] **Step 3** Update COMPATIBILITY.md if the SPI pin advanced this cycle.
- [ ] **Step 4** Run: `go test ./... -run TestErrCode_Parity -v` — Expected: PASS (no new codes).
- [ ] **Step 5: Commit**

```bash
git add cmd/cyoda/help CHANGELOG.md COMPATIBILITY.md
git commit -m "docs(search): temporal meta filter semantics + changelog (#423)"
```

---

## Task 18: full verification

- [ ] **Step 1** Root + plugins tests: `make test-all` (Docker required) — Expected: all green.
- [ ] **Step 2** SPI tests: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./...` — green.
- [ ] **Step 3** Vet: `go vet ./...` and per-plugin vet — clean.
- [ ] **Step 4** Race (one-shot before PR): `make race` — green.
- [ ] **Step 5** `make check-spi-pin-sync` — green (pin consistent). Then proceed to requesting-code-review → security-review → PR.

---

## Coverage matrix carry-forward (spec §11)

| Spec matrix row | Task(s) |
|---|---|
| GT/LT/GE/LE/EQ/NE/BETWEEN chronological on `creationDate` — unit | 4 (spi), 8 (match), 11 (pg), 12 (sqlite) |
| — running-backend e2e | 13 |
| — cross-backend parity | 14 |
| — gRPC | 15 |
| `lastUpdateTime` suite | 4, 8, 13, 14 |
| creationDate accepted 200 | 13, 15 |
| string/pattern op → 400 CONDITION_TYPE_MISMATCH | 7 (unit), 13 (e2e), 15 (grpc) |
| non-RFC3339 operand → 400 | 7, 13, 15 |
| unknown meta field → 400 INVALID_FIELD_PATH | 7, 13, 14, 15 |
| string meta vocabulary resolves identically | 4, 8, 11, 12, 14 |
| workflow criterion chronological + import rejection | 16 |
| fallback == pushdown (no split) | 16 |
| Kernel unit tests (ParseTemporalMillis, cyoda_epoch_millis) | 1, 10 |
