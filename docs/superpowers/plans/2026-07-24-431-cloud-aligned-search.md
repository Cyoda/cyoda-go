# #431 — Cloud-aligned type-directed search — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make cyoda-go evaluate search predicates with Cyoda Cloud's type-directed, same-type-only semantics — one authoritative leaf-comparison kernel in the SPI, precise numerics, polymorphic operand expansion (numeric buckets + temporal-subtype resolution, subsuming #137), parse-based validation, and a per-leaf EXACT/SOUND-SUPERSET SQL-pushdown contract — with all backends agreeing because the kernel decides.

**Architecture:** A single kernel `EvalLeaf(op, operandString, declaredTypes, stored gjson.Result) → (bool, error)` lives in `cyoda-go-spi`, building on the type-core relocated there in Phase 0 (`spi.DataType`, `spi.Decimal`, `spi.ClassifyInteger`/`ClassifyDecimal`, `spi.IsAssignableTo`, `spi.ParseTemporalMillis`/`CompareTemporal`). The kernel and its two engines (numeric-bucket, temporal-subtype) are built **dormant and unit-tested first**, then a **single atomic switch** wires both existing evaluators (`internal/match` and `spi.MatchFilter`) to the kernel, flips operand capture to lossless `json.Number`, and installs the pushdown EXACT/SOUND-SUPERSET contract on postgres+sqlite — so behaviour changes exactly once, consistently across all backends. Storage is unchanged (raw JSON documents; no per-type ValueMaps).

**Tech Stack:** Go 1.26+, two modules (`github.com/cyoda-platform/cyoda-go`, `github.com/cyoda-platform/cyoda-go-spi`) composed locally via `go.work`; `gjson` for stored-value access; `math/big` + `spi.Decimal` for precise numerics; testcontainers-go Postgres for e2e.

**Reference documents:**
- Spec (design of record): `docs/superpowers/specs/2026-07-23-431-cloud-aligned-search-design.md`.
- Executable oracle: `docs/cyoda/entity-search.md` (worked examples = kernel unit-table seed).
- Cloud source (read-only reference): `/Users/paul/dev/cyoda` (tree-node search + client DataType) and `/Users/paul/dev/cyoda-platform/core-libs` (conditions matchers, `Operation` enum).

## Global Constraints

- **Correctness/consistency over availability; fail closed.** A leaf that cannot be evaluated correctly is not substituted with a fallback. The kernel is the single source of truth; a backend diverging from it is a bug, not an accepted difference.
- **cyoda-go is primarily multi-node.** Do not descope cluster correctness (schema-refresh at the search boundary, cross-node residual eval) on proportionality grounds.
- Go 1.26+. `log/slog` only. Wrap errors `fmt.Errorf("...: %w", err)`. `uuid.UUID` not `string`.
- **Coordinated SPI release** (`MAINTAINING.md`): SPI commits land FIRST on branch `feat/431-cloud-aligned-search` (already exists, HEAD `79e6c34`), THEN a single pseudo-version pin bump across all four `go.mod` files (`make repin-plugins`). Compose locally via `go.work`; the local SPI `use` line stays **uncommitted** — **never `git add -A`** (stage explicitly; `go.work`'s absolute path would break CI). Real SPI tag is deferred to milestone-end (one v0.8.3 tag). A pinned SPI commit must be **pushed** before `go build` resolves it, even under `go.work`. `GOPRIVATE=github.com/cyoda-platform/*` for standalone (`GOWORK=off`) resolution.
- **No issue IDs (`#NNN`) in shipped artefacts** (code, comments, error messages, help/OpenAPI content). Issue refs live only in commits/PR bodies/spec docs.
- **Precise numerics only** on the leaf path — `spi.Decimal`/`big.Int`, never `float64`. `spi.NumericFloat` is retired from leaf comparison.
- **Kernel is leaf-only.** Each caller keeps its own tree walk and representation logic: `internal/match`'s array-wildcard ANY-match (`matchArrayWildcard`), lifecycle routing, `FunctionCondition`; `spi.MatchFilter`'s flat `spi.Filter` walk + presence contract. Only per-op leaf comparison is shared and deleted from both.
- **Null/absent uniformity (cyoda-go's principled choice, §3.1):** a missing or JSON-null leaf **never matches any binary op, including negatives** (`NOT_EQUAL`/`NOT_CONTAINS`/`INOT_*` are null-guarded to non-match, NOT `!positive`). This is a **deliberate divergence** from Cloud's verified core-libs behaviour (Cloud's negatives are `!positive` guarded by *type-slot presence*, so a present-but-null slot matches a negative). Implement the non-match rule; record the divergence in the cloud-parity doc. Do not "fix" it toward Cloud.
- **Void → non-match now** (three-valued void deferred until `NOT` is ever added; the group walk is AND/OR only).
- **The atomic switch is one unit.** Tasks 7–14 (wire-up + pushdown contract + validation) change observable behaviour and must land together in the branch — a fully-pushable query must not run old SQL semantics while a residual runs new kernel semantics.

## File Structure

**cyoda-go-spi (new, all dormant until the switch):**
- `parse_typed.go` — `ParseStringOrNull(operand string, t DataType) (any, bool)` per-type operand parsing (numeric/boolean/uuid/string/character; temporal delegated to the temporal engine).
- `numeric_bucket.go` — the numeric-bucket conversion engine (range-classify + rounding + NOT_NULL/drop).
- `temporal_subtype.go` — the six-subtype parse + resolution-graph engine (subsumes #137).
- `eval_leaf.go` — the `EvalLeaf` kernel: operand expansion, stored-value classification from `gjson.Raw`, assignability selection, precise same-type compare, per-op semantics.
- Matching `_test.go` files, incl. `eval_leaf_oracle_test.go` (the entity-search.md unit table).

**cyoda-go (modified at the switch):**
- `cyoda-go-spi/predicate/parse.go` — lossless operand capture (`UseNumber`).
- `internal/match/operators.go`, `match.go` — delete leaf comparators, delegate to `EvalLeaf`.
- `cyoda-go-spi/filter_match.go` — delete leaf comparators, delegate to `EvalLeaf`.
- `internal/domain/search/filter_translate.go` — fix `BETWEEN_INCLUSIVE`; carry typed sub-conditions.
- `internal/domain/search/condition_type_validate.go` — parse-based validation replacing `IsAssignableTo` value-type check.
- `plugins/postgres/query_planner.go`, `plugins/sqlite/query_planner.go` — `sqlPlan.exact` + per-leaf soundness classification; `isNumericValue` json.Number fix.
- `plugins/{postgres,sqlite}/searcher.go`, `plugins/{postgres,sqlite}/grouped_stats.go` — gate fast-paths on `plan.exact`.

**Docs:** `cmd/cyoda/help/content/predicates.md` (new), `.../search.md`, `.../workflows.md`, `.../errors/CONDITION_TYPE_MISMATCH.md`; `docs/cloud-parity/431-search-semantics.md`; `COMPATIBILITY.md`, `CHANGELOG.md`.

---

## CLUSTER 1 — SPI foundation: lossless operand capture + per-type parse (dormant)

### Task 1: Lossless operand capture in `predicate/parse.go`

**Files:**
- Modify: `cyoda-go-spi/predicate/parse.go` (parseSimple ~58, parseLifecycle ~77, parseArray ~119).
- Modify: `cyoda-go-spi/predicate/parse_test.go` (the `float64(18)` assertion ~92).

**Interfaces:**
- Produces: `SimpleCondition.Value` / `LifecycleCondition.Value` / `ArrayCondition.Values` now hold `json.Number` for JSON-number operands (a lossless string form), not `float64`. String/bool/null operands unchanged. `MarshalJSON` round-trips `json.Number` as a bare number (verified: `encoding/json` marshals it faithfully).

- [ ] **Step 1: Write the failing test — a 20-digit and a `1e20` operand survive losslessly**

Add to `cyoda-go-spi/predicate/parse_test.go`:
```go
func TestParseSimple_NumericOperandLossless(t *testing.T) {
	body := []byte(`{"type":"simple","jsonPath":"$.n","operatorType":"EQUALS","value":12345678901234567890}`)
	c, err := ParseCondition(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sc := c.(*SimpleCondition)
	num, ok := sc.Value.(json.Number)
	if !ok {
		t.Fatalf("want json.Number, got %T (%v)", sc.Value, sc.Value)
	}
	if num.String() != "12345678901234567890" {
		t.Fatalf("lossy operand: got %q", num.String())
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./predicate/ -run TestParseSimple_NumericOperandLossless -v`
Expected: FAIL — `want json.Number, got float64` (current `json.Unmarshal` path).

- [ ] **Step 3: Switch the three parsers to `json.Decoder` with `UseNumber`**

Add a helper and use it in `parseSimple`/`parseLifecycle`/`parseArray` in place of `json.Unmarshal(body, &raw)`:
```go
// unmarshalNumberAware decodes body into v with JSON numbers preserved as
// json.Number (lossless) rather than coerced to float64. Search operands
// must survive beyond float64 precision (§3.5).
func unmarshalNumberAware(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return dec.Decode(v)
}
```
Replace each `if err := json.Unmarshal(body, &raw); err != nil {` in `parseSimple`, `parseLifecycle`, `parseArray` with `if err := unmarshalNumberAware(body, &raw); err != nil {`. Add `"bytes"` to imports. (Leave the envelope/group parsers on `json.Unmarshal` — they carry no operand.)

- [ ] **Step 4: Update the existing assertion that codified the lossy behaviour**

In `TestParseGroupWithNestedSimple` (~line 92) change `if c0.Value != float64(18)` to compare the `json.Number`:
```go
if n, ok := c0.Value.(json.Number); !ok || n.String() != "18" {
	t.Fatalf("expected json.Number(18), got %T %v", c0.Value, c0.Value)
}
```

- [ ] **Step 5: Run parse + marshal tests to verify green (incl. round-trip)**

Run: `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./predicate/ -v`
Expected: PASS — new lossless test green, marshal round-trip unaffected (`json.Number` marshals as bare number).

- [ ] **Step 6: Commit (SPI repo)**

```bash
cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi
git add predicate/parse.go predicate/parse_test.go
git commit -m "feat(predicate): capture numeric search operands losslessly as json.Number

Decode operands with json.Decoder.UseNumber so a 20-digit int or 1e20 is not
rounded to float64 before the type-directed kernel runs. Dormant: no consumer
reads the extra precision yet.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: `ParseStringOrNull` per type (numeric / boolean / uuid / string / character)

**Files:**
- Create: `cyoda-go-spi/parse_typed.go`, `cyoda-go-spi/parse_typed_test.go`.

**Interfaces:**
- Produces: `ParseStringOrNull(operand string, t DataType) (value any, ok bool)` — Cloud's `DataType.parseStringOrNull` (`/Users/paul/dev/cyoda/client/.../model/DataType.kt:125-166`). `ok=false` ⇒ the operand does not parse as `t` (drop that type-branch; never an error here). Returns: numeric types → `spi.Decimal` (whole types validated integral + in range) or `*big.Int`; `Boolean` → `bool`; `String` → the string; `Character` → `rune`; `UUIDType`/`TimeUUIDType` → `uuid.UUID`. **Temporal types are handled by the temporal engine (Task 4), not here** — `ParseStringOrNull` returns `ok=false` for temporal `t` and callers route temporal separately.
- Consumes: `spi.ParseDecimal`, `spi.ClassifyInteger`, `spi.Decimal` methods (`StripTrailingZeros`, `Scale`, `IsInt128`, `Precision`).

**Algorithm (faithful port; cite Cloud on each rule):**
- Whole types (`DataType.kt:132-137`, `NumberParsing.kt:29-50`): parse operand as `Decimal`; `StripTrailingZeros`; require `Scale() <= 0` (integral — so `"1.0"`, `"2.00"`, `"1E2"` accepted, `"1.5"` rejected); take the integer value; range-check: `Integer` → int32 range, `Long` → int64, `BigInteger` → `IsInt128`, `UnboundInteger` → unbounded. Out of range ⇒ `ok=false`.
- Decimal types (`DataType.kt:140-143`, `NumberParsing.kt:70-73`, `ParserFunctions.kt:35-59`): parse as `Decimal`; `StripTrailingZeros`; `Double` → require `Precision()<=15 && |Scale()|<=292` (matches `spi.ClassifyDecimal`'s DOUBLE envelope); `BigDecimal` → require `IsInt128Decimal` (the `scale<=18` definite/loose test already encoded in `spi.ClassifyDecimal`/`numeric.go:60-96` — reuse it); `UnboundDecimal` → any parseable number.
- `Boolean` (`DataType.kt:145`): exactly `"true"`/`"false"`, **case-sensitive**; else `ok=false`.
- `String`: always `ok=true`, value = operand. `Character`: exactly one rune else `ok=false`.
- `UUIDType` (`ValueDetectionFunctions.kt:22-25`): lowercase RFC regex `^[0-9a-f]{8}-...$`; `TimeUUIDType` additionally requires version==1.

- [ ] **Step 1: Write the failing table test (seed from oracle C.7 + Cloud rules)**

Create `cyoda-go-spi/parse_typed_test.go`:
```go
package spi

import "testing"

func TestParseStringOrNull(t *testing.T) {
	cases := []struct {
		operand string
		typ     DataType
		wantOK  bool
	}{
		{"30", Integer, true}, {"1.0", Integer, true}, {"1E2", Integer, true},
		{"1.5", Integer, false}, {"2147483648", Integer, false}, // int32 overflow → Long only
		{"2147483648", Long, true}, {"170141183460469231731687303715884105728", BigInteger, false}, // 2^127 → unbound
		{"170141183460469231731687303715884105727", BigInteger, true},
		{"12.78", Double, true}, {"12.5", Integer, false},
		{"true", Boolean, true}, {"false", Boolean, false /*parses, but value false*/},
		{"TRUE", Boolean, false}, {"yes", Boolean, false},
		{"hello", String, true}, {"", String, true},
		{"a", Character, true}, {"ab", Character, false},
		{"12345678-1234-1234-1234-123456789abc", UUIDType, true},
		{"abc", Integer, false},
	}
	for _, c := range cases {
		_, ok := ParseStringOrNull(c.operand, c.typ)
		if ok != c.wantOK {
			t.Errorf("ParseStringOrNull(%q,%v)=%v want %v", c.operand, c.typ, ok, c.wantOK)
		}
	}
}
```
Note: fix the `false`/`Boolean` row — `"false"` parses (ok=true, value=false); assert value separately in a dedicated sub-case rather than folding into `wantOK`. Split boolean value-assertions into their own test.

- [ ] **Step 2: Run to verify it fails** — `go test ./ -run TestParseStringOrNull -v` → FAIL (undefined `ParseStringOrNull`).

- [ ] **Step 3: Implement `parse_typed.go`** per the algorithm above, reusing `ParseDecimal`, `ClassifyInteger`, and the `numeric.go` envelope predicates. Import `github.com/google/uuid`.

- [ ] **Step 4: Run to verify green** — `go test ./ -run TestParseStringOrNull -v` → PASS.

- [ ] **Step 5: Vet + commit (SPI)** — `go vet ./...`; commit `feat(types): add ParseStringOrNull per-type operand parsing (Cloud DataType.parseStringOrNull)`.

---

## CLUSTER 2 — SPI numeric-bucket engine (dormant)

### Task 3: Numeric-bucket conversion engine

**Files:**
- Create: `cyoda-go-spi/numeric_bucket.go`, `cyoda-go-spi/numeric_bucket_test.go`.

**Interfaces:**
- Produces: `ExpandNumericOperand(value Decimal, declaredNumeric []DataType, op FilterOp) []NumericSubCondition` where `type NumericSubCondition struct { Type DataType; Value Decimal; ValueIsBigInt bool; IntValue *big.Int; Op FilterOp; NotNull bool }`. Empty slice ⇒ every numeric branch dropped (contributes void). Mirrors Cloud `PolymorphicNumberConversions.parseNumberConditionToPolyType` (`/Users/paul/dev/cyoda/tree-node/.../polymorphic/PolymorphicNumberConversions.kt:25-36`).
- Consumes: `spi.Decimal` (`Scale`, `StripTrailingZeros`, `Cmp`, `SetScale`, rounding), `spi.NumericFamily`, `spi.FilterOp`.

**Algorithm (faithful port; `PolymorphicNumberConversions.kt` cited):**
Split `declaredNumeric` into int-family and decimal-family targets (`NumericFamily`).
- **Decimal family** (`fltConversions`, default sink `UnboundDecimal`, `:163-187`): for each target, `toRange(value)` = `BELOW` if `value < floor`, `ABOVE` if `value > ceiling`, else `IN_RANGE`, using bounds: `Double` → `±typeMaxValue(15,292)`; `BigDecimal` → `±INT128/10^18`; `UnboundDecimal` → always IN_RANGE.
- **Int family** (`intConversions`, default sink `UnboundInteger`, `:91-116`): first fold `value`→BigInteger **once** (`fltToIntConverter.produceInRangeCondition`, `:120-147`): if `Scale()<=0` (whole) → exact BigInteger, keep op; else if `op ∈ {>,>=,<,<=}` → round to integer with the direction table, keep op; else (EQUALS/other on fractional) → **drop the entire int family** (return no int sub-conditions). Then range-classify the BigInteger per int target with bounds `Integer`→int32, `Long`→int64, `BigInteger`→INT128, `UnboundInteger`→∞.
- **Per-target range→action** (`NumberConverterMap.valueToParsedConditions`, `:40-64`): the default/unbounded sink always emits `(sink, value, op)`. For each concrete target:
  - `IN_RANGE` → emit `(type, convertedValue, op)`. Decimal targets round imprecise values per the table below; `BigDecimal` **never rounds** (magnitude-only, `:180-186`); int targets already folded (no further rounding).
  - `ABOVE` **and** `op ∈ {<,<=}` → emit `(type, NotNull)`.
  - `BELOW` **and** `op ∈ {>,>=}` → emit `(type, NotNull)`.
  - every other (position × op) → **emit nothing** (drop).
- **Rounding direction** (`:130-135`): `>=` → CEILING, `<` → CEILING, `<=` → FLOOR, `>` → FLOOR, `=`/other on imprecise → **drop that bucket**.

⚠️ **BIG_DECIMAL bucket asymmetry (intentional, `:178-179`):** the *bucket* path uses magnitude-only bounds and `isPrecise=true` (never rounds), so a high-scale-but-in-magnitude value that `ParseStringOrNull(BigDecimal)` rejected (scale>18) is still emitted here as a raw BigDecimal condition. Reproduce it — the scale≤18 limit is a Trino-storage constraint irrelevant to a search condition. Add a comment citing this.

- [ ] **Step 1: Write the failing oracle table (seed from entity-search.md C.2 + spec §6 examples)**

Create `numeric_bucket_test.go` with rows asserting the (op, value, targets) → sub-conditions, e.g.:
```go
// >=12.78 on [INTEGER] → CEILING → INTEGER >= 13 (+ UNBOUND_INTEGER sink >=12.78-as-int? no: int-fold rounds once)
// <300 on [INTEGER] where 300 > int-max? no; use a real overflow: <(2^40) on [INTEGER] → ABOVE + < → INTEGER NOT_NULL
// >(2^40) on [INTEGER] → ABOVE + > → dropped (no INTEGER sub-condition)
// =12.5 on [INTEGER] → fractional EQUALS → int family dropped → empty (void)
// =5 on [LONG] → LONG =5 (+ UNBOUND_INTEGER sink)
```
Encode ~12 rows covering: CEILING/FLOOR rounding on `>=`/`<`/`<=`/`>`, ABOVE+less→NOT_NULL, ABOVE+greater→drop, BELOW+greater→NOT_NULL, BELOW+less→drop, fractional-EQUALS→drop, UNBOUND_* verbatim, BigDecimal magnitude-only.

- [ ] **Step 2: Run → FAIL** (undefined `ExpandNumericOperand`). `go test ./ -run TestExpandNumericOperand -v`.

- [ ] **Step 3: Implement `numeric_bucket.go`** per the algorithm, using `Decimal.SetScale(0, mode)`-style rounding helpers (add a `Decimal.Round(mode)` if not present — check `decimal.go` first; if absent, implement CEILING/FLOOR via `SetScale(0, ...)`).

- [ ] **Step 4: Run → PASS.** Then add a property check: every emitted `NumericSubCondition.Type` is in `declaredNumeric` (or the family sink); no branch is emitted for a dropped bucket.

- [ ] **Step 5: Vet + commit (SPI)** — `feat(search): numeric-bucket operand expansion (Cloud PolymorphicNumberConversions)`.

---

## CLUSTER 3 — SPI temporal-subtype engine (dormant; subsumes #137)

### Task 4: Six-subtype temporal parse + resolution graph

**Files:**
- Create: `cyoda-go-spi/temporal_subtype.go`, `cyoda-go-spi/temporal_subtype_test.go`.

**Interfaces:**
- Produces: `ParseTemporalSubtype(operand string, t DataType) (TemporalValue, bool)` (per-subtype ISO-8601 parse) and `ExpandTemporalOperand(operand string, declaredTemporal []DataType, op FilterOp) []TemporalSubCondition` where `TemporalSubCondition{ Type DataType; Millis int64; Op FilterOp }`. Bridges to the existing instant kernel: each sub-condition's `Millis` feeds `spi.CompareTemporal`.
- Consumes: `time` (Go `java.time` analogues), `spi.FilterOp`, `spi.ParseTemporalMillis` (reused for meta instants).

**Algorithm (faithful port; `PolymorphicTemporalConversions.kt` cited):**
- **Six subtypes** (`DataType.kt:65-74`), each backed by a Go time representation: `LocalDate`, `LocalDateTime`, `LocalTime`, `ZonedDateTime`, `Year`, `YearMonth`. Parse rules (`LeafFieldParser.kt`): `LocalDate` ISO_DATE `2024-09-09`; `LocalDateTime` ISO_DATE_TIME; `LocalTime` ISO_TIME; **`ZonedDateTime` requires an offset** (`ISO_ZONED_DATE_TIME`, offset-less → fail); `Year` = `2024`; `YearMonth` = `2024-09`. A full offset-datetime parses as both `LocalDateTime` and `ZonedDateTime` (ambiguity is irrelevant, `:60-64`).
- **Resolution** (`convert()`, `:49-76`): try downscale path (fine→coarse) first, else upscale (coarse→fine); DFS composes multi-hop paths.
  - **Downscale edges** (`:22-38`) — floor the value **every hop**; on an **imprecise** hop where `modifyOperationOnNonPrecise` is true, mutate the op: `GREATER_OR_EQUAL→GREATER_THAN`, `LESS_THAN→LESS_OR_EQUAL`, everything else unchanged (idempotent across hops). `isPrecise` per edge: `YEAR_MONTH→YEAR` month==1; `LOCAL_DATE→YEAR_MONTH` day==1; `LOCAL_DATE_TIME→LOCAL_DATE` midnight; `LOCAL_DATE_TIME→LOCAL_TIME` date==EPOCH **and `modifyOperationOnNonPrecise=FALSE`** (the sole exception — value floored, op untouched); `ZONED_DATE_TIME→LOCAL_DATE_TIME` always precise (zone dropped, no tz math).
  - **Imprecise-EQUALS drop** (`:54-56`): on downscale, if any hop is imprecise **and** `op == EQUALS`, drop the whole type-branch (return not-ok).
  - **Upscale edges** (`:39-45`) — floor to start-of-period (`YEAR→YEAR_MONTH` Jan; `YEAR_MONTH→LOCAL_DATE` day 1; `LOCAL_DATE→LOCAL_DATE_TIME` midnight; `LOCAL_TIME→LOCAL_DATE_TIME` date=EPOCH; `LOCAL_DATE_TIME→ZONED_DATE_TIME` at UTC). Op is **never mutated, never dropped** on upscale (a deliberate asymmetry — reproduce it, do not "fix" to end-of-period).
- **Meta fields** (`creationDate`/`lastUpdateTime`) are monomorphic `ZonedDateTime`; a coarse operand (`>= "2024"`) parses as `Year` then upscales to the instant. This subsumes/relaxes the #423 offset-mandatory rule **for meta fields** (data `ZonedDateTime` still requires an offset when the operand parses directly as ZDT). Convert the final value to epoch millis for `CompareTemporal`.

- [ ] **Step 1: Write the failing table (seed from oracle C.3 + spec §6 + Cloud golden test)**

Create `temporal_subtype_test.go`:
```go
// ">=","2024-09-09",[YEAR] → downscale LD→YM→YEAR, imprecise → op mutates >= to > , value 2024 → {YEAR, >, millis(2024-01-01)}
// "<","2024-09-09",[YEAR] → downscale imprecise LESS_THAN→LESS_OR_EQUAL → {YEAR, <=, ...}
// "=","2024-09-09",[YEAR] → imprecise EQUALS → dropped (empty)
// ">=","2024",[ZONED_DATE_TIME] (meta) → upscale YEAR→ZDT start-of-year, op UNCHANGED → {ZDT, >=, millis(2024-01-01T00:00:00Z)}
// ">=","2024-09-09",[YEAR,LOCAL_DATE] → {LOCAL_DATE >= 2024-09-09} + {YEAR > 2024}
```
Cover: downscale op-mutation both directions, LDT→LT no-mutation exception, imprecise-EQUALS drop, upscale-no-mutation, multi-declared expansion, ZONED offset-required (data) vs coarse-accepted (meta).

- [ ] **Step 2: Run → FAIL.** `go test ./ -run 'Temporal' -v`.

- [ ] **Step 3: Implement `temporal_subtype.go`.** Model the six subtypes + the two edge-sets + DFS path composition + op-mutation. Use `time.Time` with explicit granularity tracking. Verify against the Cloud golden test file `PolymorphicTemporalConversionsKtTest.kt` cases.

- [ ] **Step 4: Run → PASS.**

- [ ] **Step 5: Vet + commit (SPI)** — `feat(search): temporal subtype parse + resolution graph (subsumes temporal-search; Cloud PolymorphicTemporalConversions)`.

---

## CLUSTER 4 — SPI kernel assembly (dormant)

### Task 5: `EvalLeaf` kernel

**Files:**
- Create: `cyoda-go-spi/eval_leaf.go`, `cyoda-go-spi/eval_leaf_test.go`.

**Interfaces:**
- Produces:
  - `ExpandLeaf(op FilterOp, operand string, values []string, declared []DataType) (Expansion, error)` — parse+bucket+temporal expansion, once per query (§7). `Expansion` holds the typed sub-conditions (numeric/temporal/string/bool/uuid branches) or a `Void` flag (no branch, but ≥1 type parsed) or triggers the `error` (parses into no declared type → the caller maps to `CONDITION_TYPE_MISMATCH`). Arity errors (null operand on binary/range, non-2 range array, object operand) also surface here.
  - `EvalLeaf(exp Expansion, stored gjson.Result) bool` — classify stored from `stored.Raw`, select the assignable sub-condition, compare same-type precisely; null/absent → non-match for all ops incl. negatives; unary `IS_NULL`/`NOT_NULL` handled directly.
  - A convenience `EvalLeafString(op, operand, values, declared, stored)` for single-entity callers (expand+eval in one).
- Consumes: Tasks 2/3/4 + `spi.ParseDecimal`, `spi.ClassifyInteger`/`ClassifyDecimal`, `spi.IsAssignableTo`, `spi.CompareTemporal`, `gjson`.

**Algorithm (spec §3.1):**
1. **Expand** (once): for each declared type, route to numeric-bucket (Task 3), temporal (Task 4), or `ParseStringOrNull` (Task 2). Collect sub-conditions. If ≥1 numeric branch dropped as void and no branch survives → `Void`. If **no** declared type parsed the operand → error (`CONDITION_TYPE_MISMATCH`).
2. **Classify stored** from `stored.Raw` (the raw JSON text — NOT `.Value()`): number → `ParseDecimal` then `ClassifyInteger`/`ClassifyDecimal`; string → `String`; bool → `Boolean`; temporal is driven by the declared branch (stored temporal is stored as string/number, classified by the matching branch). Absent/JSON-null → handled by the null rule.
3. **Select + compare:** pick the sub-condition whose type `U` the stored classified type `T` is **assignable to** (`IsAssignableTo(T,U)`, not `T==U`), coerce `T→U`, compare same-type precisely (`Decimal.Cmp`, temporal via `CompareTemporal`, string via the op's rule). If no sub-condition's type is assignable from the stored type → **non-match**.
4. **Null/absent:** missing or JSON-null leaf → **non-match for every binary op, including negatives** (Global Constraints). `IS_NULL` = absent or JSON-null; `NOT_NULL` = present and non-null.
5. **Operators** (spec §5, Cloud matchers): equality/ordering same-type `Cmp`; string ops case-sensitive substring/prefix/suffix on a **textual** stored value only (string op on non-textual → non-match; `CONTAINS 5` never stringifies a numeric slot); `I*`/`INOT_*` case-fold; `MATCHES_PATTERN` Go RE2 whole-string anchored; `LIKE` = `%`→`.*?`, `_`→`.`, other regex metachars escaped, `\` escape, whole-string anchored, case-sensitive (Cloud `Like.java`); `BETWEEN` exclusive / `BETWEEN_INCLUSIVE` inclusive, precise bounds.

- [ ] **Step 1: Write the failing oracle unit table** — `eval_leaf_test.go` seeded from **all** oracle rows C.1, C.5, C.6, C.8 (polymorphic expansion, negative-on-absent→non-match, assignability `5`↦`[LONG]`, string-op-on-non-textual→non-match, LIKE grammar, BETWEEN inclusive/exclusive, IS_NULL/NOT_NULL). Use `gjson.Parse` to build stored `gjson.Result`s. ~40 rows.

- [ ] **Step 2: Run → FAIL.**

- [ ] **Step 3: Implement `eval_leaf.go`** assembling Tasks 2/3/4. Add a **fast path** for the common monomorphic-numeric / monomorphic-string leaf (skip bignum when the single declared type and stored type are both the same simple kind).

- [ ] **Step 4: Run → PASS.**

- [ ] **Step 5: Vet + commit (SPI)** — `feat(search): EvalLeaf type-directed kernel (assignability select, precise same-type compare, null/void uniformity)`.

---

### Task 6: Kernel oracle table — full `entity-search.md` coverage

**Files:**
- Create: `cyoda-go-spi/eval_leaf_oracle_test.go`.

**Interfaces:** none (tests only). This is the executable oracle guarding every spec §10 kernel-layer (`U`) scenario.

- [ ] **Step 1: Encode every worked example** from `docs/cyoda/entity-search.md` (oracle rows C.1–C.9) as table cases: polymorphic `[INTEGER,STRING]` eq "30"; eq "hello" → string branch only, no error; `>=12.78` on int → `>=13`; out-of-range bound → NOT_NULL/drop; precise compare beyond 2^53; temporal `>=2024-09-09` on YEAR → `>2024`; temporal imprecise EQUALS dropped; void `eq 12.5` on int → non-match; negative on absent → non-match; assignability `5`↦`[LONG]`; 20-digit precision both ends; LIKE anchored escaped glob; string-op I-variants + non-textual non-match; BETWEEN exclusive/inclusive; IS_NULL/NOT_NULL absent-vs-present-null; meta temporal coarse operand `>= "2024"`.
- [ ] **Step 2: Run → PASS** (Task 5 already implements the behaviour; this task is the comprehensive lock). Any red row is a Task-5 defect — fix in `eval_leaf.go`, not by weakening the oracle.
- [ ] **Step 3: Commit (SPI)** — `test(search): entity-search.md executable oracle for the leaf kernel`.

---

### Task 6b: Push the SPI branch + bump the pin (compose for the switch)

**Files:** `go.mod` (root) + `plugins/{memory,postgres,sqlite}/go.mod`; `go.work` (local, uncommitted).

The switch tasks (7+) consume the kernel from cyoda-go, so the SPI branch must be pushed and pinned first.

- [ ] **Step 1: Push the SPI feature branch.** `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && git push origin feat/431-cloud-aligned-search` (inline credential helper with `GH_TOKEN`; SPI branch push is OK — no `main` merge, no tag without sign-off).
- [ ] **Step 2: Ensure `go.work` composes the local SPI** (uncommitted): confirm the `use ../../../../cyoda-go-spi` line is present; **do not stage `go.work`**.
- [ ] **Step 3: Bump the pin** to the new SPI HEAD across all four `go.mod`: `make repin-plugins SPI_REF=<sha>` (or manual pseudo-version `v0.8.3-0.<ts>-<sha>` matching existing format). `git status --short go.work` must be empty.
- [ ] **Step 4: Build the composed workspace** — `go build ./... && for p in memory postgres sqlite; do (cd plugins/$p && go build ./...); done`. Expected: build green (compiles). Full-suite green is Task 6c (below), because the pinned SPI now carries the live `json.Number` operand change. Commit the pin together with Task 6c.

---

### Task 6c: Keep numeric operands numeric across the pin (mandatory, prevents backend divergence)

**Files:** Modify `plugins/postgres/query_planner.go` (`isNumericValue` ~258), `plugins/sqlite/query_planner.go` (bind sites ~229-251).

**Why now, not Task 11:** the pin (Task 6b) makes operands `json.Number` live for the composed cyoda-go build. `json.Number` is a string-kind, so postgres `isNumericValue` returns false → numeric `Eq/Ne/ordering/Between` flip to lexical text comparison, and sqlite binds `json.Number` as TEXT → storage-class ordering. Either diverges the SQL pushdown from the memory/SPI kernel — a backend-divergence bug. This task restores today's numeric behaviour (numeric operands stay numeric) at the moment `json.Number` goes live, keeping the composed suite green through the wire-up tasks. The EXACT/SOUND-SUPERSET classification is layered on later (Task 11).

- [ ] **Step 1: Failing test (per backend)** — postgres `query_planner_test.go`: `isNumericValue(json.Number("5"))` is true, and a numeric `Eq` leaf emits the `cyoda_try_float8(...)::float8` numeric branch (not `textArg`). sqlite `query_planner_test.go`: a numeric `Eq` leaf binds a numeric value (REAL affinity), asserted via `TestSearcher_EqNumeric`-style result parity with memory. Run → FAIL (json.Number currently misroutes).
- [ ] **Step 2:** postgres — extend `isNumericValue` to return true for `json.Number` (add the `case json.Number:` to the type switch). sqlite — convert a `json.Number` operand to its numeric Go value (`int64`/`float64` via `.Int64()`/`.Float64()`) before binding in the Eq/Ne/ordering/Between sites, so REAL/INTEGER affinity holds; mirror the conversion helper in both if shared logic emerges.
- [ ] **Step 3:** Run both plugins' `query_planner_test.go` + `searcher_test.go` (esp. postgres `_EqNumeric`/`_NeNumeric`/`_ContainsNumericValue`) → PASS. Then the composed cyoda-go suite: `go test -short ./... && for p in memory postgres sqlite; do (cd plugins/$p && go test -short ./...); done` → PASS.
- [ ] **Step 4: Commit the pin + bind fix together** (cyoda-go). Stage explicitly (never `git add -A`; `go.work` stays unstaged): `git add go.mod plugins/*/go.mod plugins/postgres/query_planner.go plugins/sqlite/query_planner.go plugins/postgres/query_planner_test.go plugins/sqlite/query_planner_test.go` then commit `refactor(deps): pin the search-kernel SPI; keep json.Number operands numeric in postgres/sqlite pushdown`.

---

## CLUSTER 5 — The atomic switch (behaviour goes live; Tasks 7–13 land together)

> From here behaviour changes. These tasks share the branch and must all be green before the branch is considered shippable — a half-applied switch would let a pushable query run old SQL semantics while a residual runs the kernel. Each task still has its own test cycle for reviewability.

### Task 7: Expand at the SearchService boundary + carry typed sub-conditions

**Files:**
- Modify: `internal/domain/search/filter_translate.go` (`ConditionToFilter`, `simpleToFilter`), and the `SearchService` entry (`internal/domain/search/service.go` where the model/FieldsMap is loaded, ~193).
- Add: an operand-expansion step that, given the model's `FieldDescriptor.Types` for the leaf, calls `spi.ExpandLeaf` once and attaches the typed expansion to the pushed/residual `spi.Filter` (extend `spi.Filter` with an `Expansion` field, or carry it alongside — check `filter.go`).

**Interfaces:**
- Consumes: `spi.ExpandLeaf` (Task 5), `FieldDescriptor.Types` (via `loadFieldsMap`).
- Produces: pushed/residual filters whose leaves carry the pre-parsed typed sub-conditions, so the kernel re-check (residual) and the SQL translation (pushdown) both work from the same expansion; operand parsing happens **once** per query, not per row.

- [ ] **Step 1: Failing test** — an e2e/integration test (`internal/domain/search`) asserting a polymorphic `[INTEGER,STRING]` field with operand `"30"` matches both an int-30 and a string-"30" entity (memory backend), driving the expansion wiring. Run → FAIL.
- [ ] **Step 2:** Wire `ExpandLeaf` into `simpleToFilter` (model available), attach expansion to the leaf. For single-entity/workflow-criterion callers (no pre-expansion), the kernel expands on demand via `EvalLeafString`.
- [ ] **Step 3:** Run → PASS. **Step 4:** commit `feat(search): expand operands into typed sub-conditions at the search boundary`.

### Task 8: Wire `internal/match` (E1) to the kernel; delete duplicated leaf comparators

**Files:**
- Modify: `internal/match/operators.go` (delete `opEquals`/`opIEquals`/`opCompare`/`opBetween`/`opContains`/`opStartsWith`/`opEndsWith`/`opMatchesPattern`/`opLike`/`opIsNull`/`toFloat64`; replace `applyOperator`'s body with a call to the kernel), `internal/match/match.go` (keep `matchArrayWildcard`, `matchArray`, `matchLifecycle`/`applyStringLifecycle`, `matchTemporalMeta`, `matchGroup`, `FunctionCondition` error).

**Interfaces:** Consumes `spi.EvalLeafString`/`EvalLeaf`. `applyOperator(result gjson.Result, op string, expected any, path...)` now normalizes operand→string + declared-types (from the caller's model context) and delegates.

**Behaviour changes (intended):** negatives on absent/null → **non-match** (was `!positive` → matched); LIKE → Cloud grammar (was rune-regex); numeric compare → precise (was float64). These are the migration items — expected, covered by the oracle + re-baselined e2e.

- [ ] **Step 1: Failing test** — `internal/match` unit test: `NOT_EQUAL` on an absent field → non-match (currently true). Run → FAIL (current `!positive` returns true).
- [ ] **Step 2:** Delete the comparators; delegate `applyOperator` to the kernel. Preserve `matchArrayWildcard` ANY-match by calling the kernel per element. Keep temporal-meta on the shared `CompareTemporal` path (or route through the kernel's temporal branch).
- [ ] **Step 3:** Run `go test ./internal/match/... -v` → PASS (update any unit tests that encoded old float64/`!positive`/rune-LIKE behaviour — these are the domain-level re-baseline).
- [ ] **Step 4:** commit `refactor(match): delegate leaf comparison to the SPI kernel; delete duplicated comparators`.

### Task 9: Wire `spi.MatchFilter` (E2) to the kernel; delete duplicated comparators

**Files:**
- Modify: `cyoda-go-spi/filter_match.go` — delete `compareFilterValues`, `matchFilterLike`, the op-switch body, the ported `opMatchesPattern`/`opIsNull`, `toGjsonResult`; keep `MatchFilter`/`evalFilter` (AND/OR walk + group identity), `extractFilterValue`/`extractFilterMetaValue` (path + meta vocabulary + presence contract), and `evalTemporalLeaf`/`toEpochMillis` (or fold temporal into the kernel). Change stored read from `.Value()` (float64-lossy) to passing the `gjson.Result` (via `.Raw`) into `EvalLeaf`.

**Interfaces:** `evalLeafFilter(f Filter, data []byte, meta EntityMeta) bool` now resolves the leaf `gjson.Result` and calls `EvalLeaf(f.Expansion, stored)`.

**Behaviour changes (intended):** E2's hardcoded 5-op vacuous-true negative set → uniform non-match on absent/null; LIKE byte-based → Cloud grammar; `.Value()` float64 → precise via `.Raw`. Aligns E2 to E1 (they now share the kernel).

- [ ] **Step 1: Failing test** — extend `filter_match` tests (or `match_filter_sqlite_parity_test.go`): a numeric-ordering case on stringy-numeric data + a negative-on-null case, asserting E1==E2==kernel. Run → FAIL.
- [ ] **Step 2:** Delegate `evalLeafFilter` to the kernel; delete the duplicated comparators.
- [ ] **Step 3:** Run `cd /Users/paul/go-projects/cyoda-light/cyoda-go-spi && go test ./... -v` → PASS.
- [ ] **Step 4:** commit (SPI) `refactor(filter): delegate MatchFilter leaf comparison to EvalLeaf; drop float64 path`.

### Task 10: Fix `BETWEEN_INCLUSIVE` in `filter_translate.go`

**Files:** Modify `internal/domain/search/filter_translate.go` (`mapOperator` ~202-251, `betweenValues` ~67-76).

**The bug (confirmed):** `mapOperator` has no `"BETWEEN_INCLUSIVE"` case → falls to `default: return spi.FilterMatchesRegex`; `betweenValues` then returns nil (op≠FilterBetween) and the two bounds get compiled as a regex — a near-never-match. E1 in-memory handles it (aliased to BETWEEN); the translated Filter degrades. The kernel must treat `BETWEEN_INCLUSIVE` as an inclusive range.

- [ ] **Step 1: Failing test** — `filter_translate` unit test: `BETWEEN_INCLUSIVE` with `[10,20]` maps to an inclusive-range Filter (not `FilterMatchesRegex`) and `betweenValues` returns the 2-element slice. Run → FAIL.
- [ ] **Step 2:** Add the `spi.FilterOp` for inclusive-between if not present (check `filter.go`); add the `mapOperator` case; make `betweenValues` populate `Values` for both between ops.
- [ ] **Step 3:** Run → PASS. **Step 4:** commit `fix(search): map BETWEEN_INCLUSIVE to an inclusive range, not a regex fall-through`.

### Task 11: Pushdown EXACT / SOUND-SUPERSET contract (postgres + sqlite, mirrored)

**Files:**
- Modify: `plugins/postgres/query_planner.go` (add `exact` to `sqlPlan` ~17; `leafToSQL`/`temporalLeafToSQL` return a soundness class), `plugins/sqlite/query_planner.go` (mirror: `exact` on `sqlPlan` ~13; classify leaves), and the aggregation in `toSQL`/`dissect` (AND of EXACT = EXACT; any SOUND-SUPERSET → SOUND-SUPERSET). (The `isNumericValue`/sqlite-bind numeric fixes already landed in Task 6c.)
- Modify: `plugins/postgres/searcher.go` (~104), `plugins/sqlite/searcher.go` (~71) — gate the `plan.postFilter == nil` fast-path (skip re-check, push LIMIT/OFFSET) **additionally on `plan.exact`**.

**Interfaces:** `leafToSQL(...) (sql string, args []any, exact bool)`. Classification: NULL checks + byte-string ops (`Contains`/`StartsWith`/`EndsWith` via `strpos`/`instr`/`substr`) → **EXACT**; numeric-coercion `Eq`/`Ne`/ordering/`Between` and `LIKE` → **SOUND-SUPERSET** (SQL coercion/collation can over-select vs the precise kernel); temporal → EXACT modulo the documented floor caveat. sqlite (no bignums) → fewer EXACT numeric leaves than postgres; that's fine — results never diverge because the residual kernel re-checks.

- [ ] **Step 1: Failing test (per backend)** — `query_planner_test.go`: a numeric `Eq` leaf classifies SOUND-SUPERSET (forces residual), a `StartsWith` leaf classifies EXACT (fast-path eligible). Run → FAIL.
- [ ] **Step 2:** Implement the classification + `exact` aggregation + the fast-path gate, **identically mirrored** in both planners (the files are deliberate mirrors; a divergence breaks `e2e/parity`).
- [ ] **Step 3:** Run both `query_planner_test.go` + `searcher_test.go` (both plugins) → PASS.
- [ ] **Step 4:** commit `feat(pushdown): per-leaf EXACT/SOUND-SUPERSET contract; fast-path only when all leaves EXACT`.

### Task 12: grouped-stats GROUP-BY disqualification by any non-EXACT leaf

**Files:** Modify `plugins/postgres/grouped_stats.go` (~230), `plugins/sqlite/grouped_stats.go` (~301), and `internal/domain/entity/grouped_stats_service.go` (~201 native-vs-streaming decision).

**Rationale (spec §3.2):** a SOUND-SUPERSET WHERE pushed into a native `GROUP BY` corrupts per-bucket counts irrecoverably. So native aggregation must decline (`ErrAggregationNotPushdownable`) on **any non-EXACT pushed leaf**, not merely on residual presence — falling back to the existing `Iterate`+Welford streaming tally (kernel filters per row before tallying). **No query is rejected** — an execution-strategy switch to a path that already exists.

- [ ] **Step 1: Failing test** — parity/integration: grouped-stats with a numeric-condition (SOUND-SUPERSET) leaf declines native GROUP-BY and streams, giving counts identical to the residual/memory path. Run → FAIL (currently pushes and may miscount).
- [ ] **Step 2:** Add `|| !plan.exact` to the `plan.postFilter != nil` decline in both `GroupedAggregate` paths; ensure the service falls through to `tallyStreaming`.
- [ ] **Step 3:** Run grouped-stats tests (both plugins) → PASS. **Step 4:** commit `fix(grouped-stats): decline native GROUP-BY on any non-EXACT pushed leaf`.

### Task 13: Per-backend pushdown-soundness property test

**Files:** Create a property test under `e2e/parity` (or per-backend isolated e2e) asserting: for a random condition + corpus, the SQL-pushed candidate set ⊇ the kernel's true match set (no false negatives), and after residual re-check the final set == the memory-kernel set exactly.

- [ ] **Step 1:** Write the property (generate random simple/group conditions over a seeded corpus; run each backend's searcher + the memory kernel; assert superset + post-recheck equality). Run → it should PASS given Tasks 11–12 (if it fails, a leaf is mis-classified EXACT — fix the classification, that's the point of the property).
- [ ] **Step 2:** Register + commit `test(pushdown): per-backend soundness property (pushed ⊇ kernel; recheck == kernel)`.

---

## CLUSTER 6 — Validation (parse-based; behaviour)

### Task 14: Replace value-type validation with parse-based validation; re-baseline error tables

**Files:**
- Modify: `internal/domain/search/condition_type_validate.go` — replace `checkSingleValueType`/`inferValueDataType` (the `IsAssignableTo` value-type check, ~226-279) with a parse-based check: reject a leaf iff `spi.ExpandLeaf` parses the operand into **none** of the field's declared types (→ `CONDITION_TYPE_MISMATCH`). Keep `validateBetweenArity` (arity → `INVALID_CONDITION`), field-path checks (`INVALID_FIELD_PATH`), and `validateLifecycleType` (meta/temporal). Remove the now-dead `float64` branch + stale comments.
- Modify: e2e/gRPC error-table tests (below) that assert the *old* value-type semantics.

**Behaviour change (intended, spec §13.5):** e.g. a string operand on an `[INTEGER]`-only field still 400s (parses no type), but on an `[INTEGER,STRING]` field it now **200s** (STRING parses). `CONTAINS 5` on a numeric field 200s (parses as number; same-type gate returns empty, not error). No operator-class rejections.

- [ ] **Step 1: Failing tests** — update `internal/e2e/handler_condition_type_test.go`: `TestSearch_ConditionType_IntegerFieldWithStringValue_Rejects` becomes a two-case test (int-only field → 400; `[INTEGER,STRING]` field → 200). Also re-baseline the domain unit tests in `condition_type_validate_test.go` (the `Between_TypeMismatch`/`In_TypeMismatch`/`ObjectValue_Rejects` group) to the parse-based rule. Write them to the new expectation; run → FAIL against current code.
- [ ] **Step 2:** Implement parse-based `validateConditionTypes` using `spi.ExpandLeaf` over `FieldDescriptor.Types`. Preserve arity + field-path + lifecycle checks and their exact error codes (`condition_type_validate.go:317/319`, `operators.go:132-144`). Grouped-stats keeps its model-nil path (only lifecycle/temporal checks).
- [ ] **Step 3:** Run the full search e2e + gRPC error suites (`internal/e2e/...`, `internal/grpc/...`) → PASS. Re-baseline `search_temporal_test.go` offset-less meta operand (`TestSearchTemporal_400_OffsetLessOperand`) — a coarse meta operand now **upscales and 200s** (spec §4 relaxes #423's offset-mandatory rule for meta fields); data ZDT still 400s offset-less. Update `zzz_errorcode_matrix_test.go` cells that change.
- [ ] **Step 4:** commit `refactor(search): parse-based condition validation replacing value-type assignability check`.

### Task 15: Importer BYTE/SHORT/FLOAT widening confirmation

**Files:** Inspect `internal/domain/model/importer/walker.go`; add a test confirming a foreign model declaring BYTE/SHORT/FLOAT widens to INTEGER/DOUBLE (search-lossless) — or, if cyoda-go's importer only ever infers from data (never these types), document that no path introduces them and add a guard test asserting inference never yields them.

- [ ] **Step 1:** Test the importer/inference behaviour for these types. **Step 2:** confirm/adjust. **Step 3:** commit `test(importer): confirm BYTE/SHORT/FLOAT never enter the model (widen on foreign import)`.

---

## CLUSTER 7 — Coverage (running-backend e2e, parity, gRPC)

> The spec §10 coverage matrix is the checklist. Every row needs its `U`/`E`/`P`/`G` cells. `U` (kernel unit) is delivered by Tasks 5–6. This cluster delivers `E`/`P`/`G`. Concurrency tests stay isolated, never in parity (`.claude/rules/test-coverage.md`).

### Task 16: Running-backend e2e (`internal/e2e`, real Postgres)

**Files:** New/extended tests in `internal/e2e/` covering each §10 scenario on the full HTTP stack: polymorphic eq match; numeric-bucket rounding; out-of-range→NOT_NULL; precise beyond 2^53; temporal subtype+resolution; temporal imprecise-EQUALS; void leaf; negative-on-absent→non-match; assignability `5`↦`[LONG]`; 20-digit precision both ends; LIKE anchored escaped glob; string-op case + I-variants + non-textual→non-match; BETWEEN exclusive/inclusive (**incl. `BETWEEN_INCLUSIVE` no longer regex**); IS_NULL/NOT_NULL; meta temporal coarse operand; the pushdown-soundness scenario (EXACT fast-path vs SOUND-SUPERSET residual; LIMIT/pagination correct under residual).

- [ ] **Step 1:** Write the e2e tests (happy path + each 400 from §9). Run → they drive/confirm the switch. **Step 2:** Green on real Postgres (`go test ./internal/e2e/... -v`, Docker required). **Step 3:** commit `test(e2e): type-directed search coverage on the running backend`.

### Task 17: Cross-backend parity matrix (`e2e/parity`)

**Files:** Add type-directed parity scenarios to `e2e/parity/` (bodies) + register in `e2e/parity/registry.go`, run across memory+sqlite+postgres (and picked up by the commercial backend). Migrate today's postgres-only string-op tests into the cross-backend matrix. Cover the backend-agnostic §10 `P` rows, incl. the grouped-stats GROUP-BY-disqualified-by-non-EXACT-leaf row per backend.

- [ ] **Step 1:** Write + register the parity scenarios (assert identical results across backends — the kernel-decides guarantee). Run `make test-all` (Docker). **Step 2:** Green. **Step 3:** commit `test(parity): cross-backend type-directed search matrix`.

### Task 18: gRPC error-class coverage (`internal/grpc`)

**Files:** `internal/grpc/` tests asserting the envelope (`Success`, `Error.Message` contains the code) for the §9 error classes reachable via gRPC search: `CONDITION_TYPE_MISMATCH` (operand parses no type), `INVALID_CONDITION` (null operand/bad arity/malformed), `INVALID_FIELD_PATH` (unknown path), and the `BETWEEN_INCLUSIVE` happy path (matrix row marks `G`).

- [ ] **Step 1:** Write the gRPC tests (re-baseline the existing `search_temporal_test.go` gRPC cases per Task 14's semantics). Run → PASS. **Step 2:** commit `test(grpc): search validation error-class coverage`.

---

## CLUSTER 8 — Docs, cloud-parity, release

### Task 19: Help topics + operator/validation docs (Gate 4)

**Files:**
- Create `cmd/cyoda/help/content/predicates.md` — operator catalog, type-directed same-type semantics, LIKE grammar, parse-based validation, the accepted RE2-vs-Java `MATCHES_PATTERN` divergence.
- Correct `cmd/cyoda/help/content/search.md` stale operator descriptions; cross-ref `workflows.md`.
- Rewrite `cmd/cyoda/help/content/errors/CONDITION_TYPE_MISMATCH.md` for parse-based semantics (drop the value-type-compatibility prose). Keep `INVALID_FIELD_PATH.md`/`INVALID_CONDITION.md` accurate.

- [ ] **Step 1:** Write the topics (compact — actionable core, detail lives in spec/PR). **Step 2:** Run `go test ./cmd/cyoda/help/...` incl. `TestErrCode_Parity` (bijection guard — no code added/removed here, so it stays green). **Step 3:** commit `docs(help): predicates topic + type-directed search semantics; fix search.md`.

### Task 20: Cloud-parity record

**Files:** Create `docs/cloud-parity/431-search-semantics.md` — the aligned semantics, and the **deliberate non-replications**: negative-on-present-null (cyoda-go non-match vs Cloud's verified `!positive`-slot-guarded match — record the core-libs verification), the two BETWEEN representations / double-widening / UUID-comparator inconsistency / `Matches`-null-via-NPE (we do the principled thing), `searchInStrings` dual-slot out-of-scope, RE2-vs-Java regex divergence, and the meta-temporal coarse-operand relaxation of the #423 offset rule.

- [ ] **Step 1:** Write it. **Step 2:** commit `docs(cloud-parity): type-directed search semantics + deliberate divergences`.

### Task 21: Cassandra issue + COMPATIBILITY/CHANGELOG + SPI pin finalize

**Files:** `COMPATIBILITY.md` (SPI pin bump row), `CHANGELOG.md` (the search-semantics change + migration items §13). File a `cyoda-go-cassandra` (commercial backend — never link the private repo) issue referencing this design so it aligns + passes the new parity matrix.

- [ ] **Step 1:** Update COMPATIBILITY + CHANGELOG for the SPI pin + behaviour change. **Step 2:** File the Cassandra issue (via `gh`, in the commercial repo). **Step 3:** commit `docs(compat): record SPI pin + search-semantics change`. **Note:** the real `cyoda-go-spi` tag is deferred to milestone-end (one v0.8.3 tag) — do **not** tag here.

---

## CLUSTER 9 — Pushdown tightening (SEVERABLE — pure perf, zero result change)

> Guarded by the Task-13 soundness property; changes no results, only widens EXACT-leaf coverage so more queries hit the fast path. Drop this cluster if it balloons — correctness is complete without it.

### Task 22: Widen EXACT coverage via type gating

**Files:** `plugins/postgres/query_planner.go` (add `jsonb_typeof` gating so a numeric leaf on genuinely-numeric JSON classifies EXACT), `plugins/sqlite/query_planner.go` (`typeof`/`json_type` gating). Mirror identically.

- [ ] **Step 1:** For each SOUND-SUPERSET numeric leaf, add a `jsonb_typeof(col)='number'` (pg) / `json_type` (sqlite) guard that makes the SQL match the kernel bit-for-bit → reclassify EXACT. Property test (Task 13) must stay green. **Step 2:** Assert (test) that a now-EXACT leaf takes the fast path. **Step 3:** commit `perf(pushdown): tighten numeric leaves to EXACT via json type gating`.

---

## Race + full-suite gate (pre-PR, one-shot)

- [ ] Full root suite incl. e2e: `go test ./... 2>&1 | tail -30` (Docker) → PASS.
- [ ] Cross-module: `make test-all` → PASS. SPI standalone: `cd ../../../../cyoda-go-spi && go test ./...`.
- [ ] Vet: `go vet ./...` + per-plugin. Race (one-shot): `make race` → PASS.
- [ ] `git status --short go.work` empty (local `use` line never staged).

---

## Self-Review

**1. Spec coverage.** Spec §3 (kernel, pushdown contract, precise numerics, precision capture, void) → Tasks 1,5,11–13; §4 (type porting: parseStringOrNull, numeric buckets, temporal subtypes, BYTE/SHORT/FLOAT) → Tasks 2,3,4,15; §5 (operators incl. LIKE grammar, negatives, BETWEEN) → Tasks 5,10; §6/§13.5 (parse-based validation, re-baseline) → Task 14; §7 (expansion boundary) → Task 7; §9 error table → Tasks 14,16,18; §10 coverage matrix `U/E/P/G` → Tasks 6,16,17,18; §11 harness (kernel table, parity, soundness property) → Tasks 6,13,17; §12 (cloud-parity, SPI release, docs) → Tasks 20,21,19; §13 migration items 1–8 → Tasks 8,9,10,14 (each observable change has a re-baselined test); §14 phases 1–7 all folded (phase 6 tightening = severable Cluster 9). **Gap check:** `IS_CHANGED/IS_UNCHANGED` explicitly dropped (no task — correct); `searchInStrings` out of scope (recorded in Task 20). No new error codes → `TestErrCode_Parity` untouched (no `errors/<CODE>.md` task needed); confirmed in Task 19.

**2. Placeholder scan.** Engine tasks (3,4,5) intentionally carry the *faithful algorithm + Cloud file:line citations + concrete oracle test rows* rather than full literal Go — the oracle tests are the executable contract (TDD), and the algorithm is pinned to Cloud source, not left vague. Mechanical tasks (1,10,11 `isNumericValue`) carry literal code. No "TBD"/"handle edge cases"/"similar to Task N".

**3. Type consistency.** `EvalLeaf`/`ExpandLeaf`/`ExpandNumericOperand`/`ExpandTemporalOperand`/`ParseStringOrNull` names are used consistently across Tasks 2–9. `spi.Filter` gains an `Expansion` field (Task 7) consumed by Tasks 8,9,11. `sqlPlan.exact` (Task 11) consumed by Tasks 11,12. `NumericSubCondition`/`TemporalSubCondition` shapes defined in Tasks 3/4, consumed by Task 5.

**4. Coalescing / no-divergence safety.** The behaviour switch (Tasks 7–14) is one landing unit; the mandatory postgres/sqlite parity fixes (Task 11) travel with the operand `json.Number` change (Task 1 is dormant until then) so no backend ever diverges mid-branch. Coordinated-release safety: SPI pushed+pinned (Task 6b) before cyoda-go consumes it; `go.work` uncommitted; explicit staging.

## Execution Handoff

Two execution options:
1. **Subagent-Driven (recommended)** — fresh subagent per task, two-stage review between tasks, fast iteration.
2. **Inline Execution** — batch execution in this session with checkpoints.
