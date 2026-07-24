# #431 — Converge the predicate evaluators onto one leaf-comparison kernel + one query semantics

**Status:** design agreed (Paul), independently reviewed (two fresh-context lenses), folded validation in.
**Milestone:** v0.8.3. **Base branch:** `release/v0.8.3`. **Worktree/branch:** `feat/431-evaluator-convergence`.
**Seeded by:** #423 (temporal + numeric shared primitives) — this spec *extends* those, deletes nothing #423 built.

## 1. Problem

"Does entity field `value <op> operand` match?" is implemented **four** times, and they have drifted, so the same logical query returns different results depending on which path ran:

1. `internal/match/operators.go` — `applyOperator` over `gjson.Result`+`any`. Used by workflow criteria (`workflow/engine.go`), search `GetAll` fallback (`search/service.go`), grouped-stats residual (`entity/grouped_stats_service.go`).
2. `spi.MatchFilter`/`evalLeafFilter` (`cyoda-go-spi/filter_match.go`) over `any`. Used by memory search, sqlite/postgres residual post-filter, iterate, streaming-tally.
3. `plugins/sqlite/query_planner.go` — SQL translation of the pushable `spi.Filter` subset.
4. `plugins/postgres/query_planner.go` — same for Postgres.

The two *representations* (`predicate.Condition` richer than `spi.Filter`) are justified; the duplicated *leaf-comparison logic* is not. #423 already pulled temporal + numeric coercion into shared SPI primitives (`ParseTemporalMillis`, `CompareTemporal`, `NumericFloat`) that (1) and (2) both call, and built a type-validation seam (`ValidateConditionValueTypes`, `CONDITION_TYPE_MISMATCH`). This spec finishes the convergence and makes all four paths agree on **one canonical query semantics**, guarded by a cross-backend parity matrix.

### Confirmed drifts today
- **LIKE:** (1) rune→regex, no `\` escape, glob; (2) byte-based, `\` escape, glob; (3)/(4) push `LIKE ? ESCAPE '\'` but run the operand through `escapeLike`, escaping the user's `%`/`_` → **pushed-down LIKE is a literal full-string match**. SQLite LIKE is **case-insensitive** by default; Postgres/Go are case-sensitive.
- **BETWEEN:** (1) forces float (errors on non-numeric strings); (2) lexical for strings; (3)/(4) numeric-vs-lexical decided by three different rules (affinity / operand-type / stored+operand-type).
- **Numeric eq/ordering on numeric-looking *stored strings*:** Postgres coerces via `cyoda_try_float8` (stored `"30.0"` eq operand `30` matches); Go + sqlite do not.
- **Cross-type scalar comparison:** memory/criteria lexically coincide (`fmt.Sprint(float64(30))=="30"`) — an implicit string↔number coercion that no SQL backend can faithfully reproduce.
- **`EQUALS null` operand / present JSON null:** (1) `null==null`→true and JSON-null counts as null; (2) present JSON null is neither null nor not-null (`IS_NULL` returns `!found`).
- **Operator-class validation gap:** `CONTAINS 5` on a numeric field is accepted today (validation checks operand *value* type, not *operator* class) and silently stringifies.

## 2. Goals / non-goals

**Goals.** One Go leaf-comparison kernel in the SPI, called by both Go evaluators; the two SQL planners mirror it, guarded by a new cross-backend leaf-op × stored-type parity matrix; one canonical, logical, backend-consistent query semantics; operator-class-vs-field-type validation folded in (strict against the authoritative model, fail-closed); SPI coordinated release; Cloud-parity reconciliation.

**Non-goals (kept out, with rationale).**
- **Big-number exactness.** All paths canonicalize numerics through IEEE-754 `float64` (`NumericFloat`, `cyoda_try_float8`, SQLite `REAL`). Values beyond 2⁵³ in `BigInteger`/`BigDecimal`/`Unbounded*` compare imprecisely — but **identically on all four paths**, so it is a consistent, documented limitation, not a convergence bug. Exactness (`json.Number`/`big`) is a separate concern.
- **Making type-aware SQL sargable.** The type-guarded predicates wrap the column in a function (already true for the numeric path today), so eq/ordering may scan on large models. Correctness over availability (design philosophy): accepted and documented; expression indexes are a later optimisation.
- **Data-field temporal typing** stays #137 (`InferDataType` never yields a temporal `DataType` for a JSON data leaf — dates are `String`; #423's temporal handling is meta/lifecycle-only).
- **Pushing case-insensitive / regex ops into SQL.** They stay residual-only (always `spi.MatchFilter`) → automatically consistent.

## 3. Canonical leaf semantics (the contract)

**Governing principle.** The *richest available type signal* governs comparison, and **there is no string→number parsing, ever.** JSON's native type (number / string / bool / null) governs where it exists; the declared/inferred schema type governs only where JSON is lossy (temporal — dates are strings). This is why temporal needs a `Coercion` marker and numeric does not.

The **Go kernel** (§4) is the executable reference. The two SQL planners MUST reproduce it bit-for-bit, proven by the §9 parity matrix.

### 3.1 Type model
A stored JSON leaf has exactly one of: **number** (gjson `.Value()` → `float64`), **string**, **bool**, **null** (present-null), or **absent** (missing). An operand likewise has a JSON type (numbers arrive as `float64`). **Comparison happens only between same-type values.**

### 3.2 Cross-type rule (keystone)
Any comparison whose stored value and operand are of **different** JSON types is a **non-match**, uniformly:
- `eq` → false; `ne` → true (negative-op vacuous truth); `gt/lt/gte/lte/between` → false (excluded); string ops → false; `ieq` → false, `ine` → true.

Rationale: it is the only rule that is both logically defensible ("no coercion") and faithfully mirrorable in SQL (`jsonb_typeof(x)='number' AND …` numeric branch; else string branch; type mismatch → the branch simply doesn't fire). This **replaces** today's lexical cross-type coincidence — an observable behavior change on memory/criteria too (see §13). Strict validation (§5) rejects a type-mismatched operand at the boundary, so at evaluation this rule is **defense-in-depth** (paths that skip validation) and the guarantee that all four backends agree.

### 3.3 Per-op canonical semantics

| Op(s) | Canonical meaning |
|---|---|
| `eq` / `ne` | same-type only. number↔number numeric equality; string↔string byte-equal; bool↔bool equal. Cross-type per §3.2. |
| `gt/lt/gte/lte` | same-type only. number↔number numeric order; string↔string lexical (`strings.Compare` on the raw string values); bool → ordering undefined (validation rejects it on bool fields; defense-in-depth eval → non-match). Cross-type per §3.2. |
| `between` | numeric iff stored **and both bounds** are numbers; else string iff stored **and both bounds** are strings (lexical); else non-match. Malformed arity (<2 bounds) → validation 400 (already enforced) / fail-closed in eval. |
| `contains/starts_with/ends_with` | substring on the string value (`strings.Contains/HasPrefix/HasSuffix`; byte/rune agree on valid UTF-8). Non-string stored value → non-match (cross-type). |
| `like` | **rune-based glob, case-sensitive**: `%`=any run of characters, `_`=one character, `\` escapes. Escape grammar: `\` MUST be followed by `%`, `_`, or `\`; any other `\x` or a trailing `\` is a **malformed pattern → 400 `INVALID_CONDITION`** at validation. Non-string stored value → non-match. |
| `matches_regex` | Go RE2, unanchored, on the string form. Non-pushable → residual-only. |
| `ieq/ine` | **equality class**, type-gated exactly like `eq/ne`: number↔number numeric equality (case-fold is a no-op on numbers), string↔string `EqualFold`, bool↔bool equal, cross-type per §3.2. This is why they are validation-allowed on numeric/bool fields (twins of `eq/ne`), while `icontains/istarts/iends` are string-class. |
| `icontains/istarts_with/iends_with` (+ `inot_*`) | **string class**, case-folded substring. |
| `is_null` | `!found || val==nil` (absent OR present-JSON-null). |
| `not_null` | `found && val != nil`. |
| negative-op vacuous truth | on absent/null field, the negatives (`ne`, `ine`, `inot_contains/starts/ends`, and the tree-walk `NOT_CONTAINS/NOT_STARTS_WITH/NOT_ENDS_WITH`) are vacuously **true**; all other ops **false**. |
| null operand | `eq/ieq` with a literal-null operand ≡ `is_null`; `ne/ine` with literal-null ≡ `not_null` (normalized in the kernel — preserves current criteria intent, removes the corner). |

### 3.4 Null / presence precedence
The kernel evaluates in this order: (1) `is_null`/`not_null` (presence, before any value use); (2) null-operand normalization (§3.3); (3) absent/null-field guard → negative-op vacuous truth or false; (4) temporal branch (`Coercion==CoerceTemporal`, unchanged from #423); (5) the per-op comparison of §3.3.

## 4. Architecture — the kernel and its callers

### 4.1 The kernel (in `cyoda-go-spi`)
```go
// EvalLeaf is the single source of truth for scalar leaf comparison.
// val/found come from the caller's own extraction (gjson for internal/match,
// already-any for the Filter path). operands carries BETWEEN bounds; operand
// the scalar operand. coercion routes temporal (unchanged from #423).
func EvalLeaf(op FilterOp, coercion FilterCoercion, val any, found bool, operand any, operands []any) bool
```
It owns §3.4 in full. `evalLeafFilter` is reduced to: extract `(val, found)` via `extractFilterValue` → `EvalLeaf(...)`. `compareFilterValues`, `matchFilterLike` (now **rune-based**), `opMatchesPattern` become internal helpers of `EvalLeaf`. The cross-type rule (§3.2) is implemented by type-gating each comparison branch (number branch requires both `NumericFloat` ok; string branch requires both non-numeric-typed strings; bool branch requires both bool).

### 4.2 `internal/match` tree-walk (keeps only representation-specific logic)
- `matchSimple` extracts `gjson.Result` → `(val any, found bool)` where `found = result.Exists()`, and `val = nil` for `Type==Null`, else `result.Value()`; then `applyOperator` → `EvalLeaf`.
- `applyOperator` becomes a thin mapper: predicate op-string → `(spi.FilterOp, negate bool)`, then `res := EvalLeaf(positiveOp, …); return res != negate`. The 3 case-sensitive negatives (`NOT_CONTAINS/NOT_STARTS_WITH/NOT_ENDS_WITH`, absent from `spi.FilterOp`) map to `(FilterContains/StartsWith/EndsWith, negate=true)`. **Negation stays inside `applyOperator`, i.e. per array element** — `matchArrayWildcard` ORs `applyOperator` results, preserving `ANY(¬contains)` (no De Morgan flip).
- `matchLifecycle` keeps its temporal/string routing (already shared via `CompareTemporal`); its string lifecycle fields route through `applyOperator` → `EvalLeaf` with the field value as `val`.
- **Deleted from `operators.go`:** `opEquals`, `opCompare`, `opBetween`, `opContains`, `opStartsWith`, `opEndsWith`, `opLike`, `opMatchesPattern`, `opIEquals`, `opIsNull`, `toFloat64`.

### 4.3 SQL planners mirror the kernel (§6)

## 5. Validation contract (folded in)

Operator-class-vs-field-type validation extends `validateSimpleConditionType` (`search/condition_type_validate.go`), reusing **`CONDITION_TYPE_MISMATCH`** (no new error code). It complements the existing operand-*value*-type check.

### 5.1 Operator classes
- **equality:** `EQUALS/NOT_EQUAL/IEQUALS/INOT_EQUAL` (+ `IS_NULL/NOT_NULL`, always allowed).
- **ordering:** `GREATER_THAN/LESS_THAN/GREATER_OR_EQUAL/LESS_OR_EQUAL/BETWEEN`.
- **string:** `CONTAINS/STARTS_WITH/ENDS_WITH/LIKE/MATCHES_PATTERN` + negatives + case-insensitive `ICONTAINS/ISTARTS_WITH/IENDS_WITH` + their negatives.

### 5.2 Matrix (field model type → allowed op classes)
| Field type | equality | ordering | string |
|---|---|---|---|
| string | ✓ | ✓ (lexical) | ✓ |
| number | ✓ | ✓ (numeric) | ✗ → 400 |
| boolean | ✓ | ✗ → 400 | ✗ → 400 |
| temporal (meta only) | ✓ | ✓ | ✗ (already #423) |

Beyond operator class, the **operand value type** must be assignable to the field's model type (numeric widening honored via `IsAssignableTo`); a non-assignable operand → `400 CONDITION_TYPE_MISMATCH`. This now includes e.g. a **numeric operand on a string field** — under §3.2 it can never match, so it is rejected, not silently empty. Unknown data-field paths → `400 INVALID_FIELD_PATH`. `IS_NULL`/`NOT_NULL` are valid on every field. There is no permissive row: see §5.3.

### 5.3 Validation is strict, authoritative, and fail-closed

The entity model is the **authoritative** definition of every leaf field and its type. Data cannot diverge from it: a write that would make a field a second type is rejected at ingest (`schema.ErrPolymorphicSlot`), as are unknown paths and shape violations. So at query time a field's type is **definite** — there is no "maybe it's also a string" and no "not seen yet, so accept". *How* the schema is populated (by discovery today) is irrelevant; what the model **currently defines** is the contract, and any predicate that does not conform to it is rejected. There is **no best-effort/permissive mode**.

Concretely, every data-field predicate is validated against the model, and rejected 4xx when:
- the path is absent from the model → `400 INVALID_FIELD_PATH`;
- the operator's class is invalid for the field's type (§5.2) → `400 CONDITION_TYPE_MISMATCH`;
- the operand's type is not assignable to the field's type → `400 CONDITION_TYPE_MISMATCH`;
- a LIKE pattern has a malformed escape (§3.3) → `400 INVALID_CONDITION`.

**Fail closed, not open.** If the model/schema cannot be loaded (model-store unavailable, schema decode failure), the operation **fails** (5xx + ticket) rather than proceeding unvalidated. This is the correctness-over-availability rule (`.claude/rules/correctness-over-availability.md`) and it **replaces** the current fail-open "log and proceed so the matcher surfaces an error" branches in `validateConditionPaths` and `validateConditionTypes`/`loadModelNode`. The legitimate multi-node schema-propagation refresh (one bounded `RefreshAndGet` when a path looks absent, for a peer's freshly-extended schema) is **kept** — that is eventual-consistency handling, not permissiveness; after the refresh a still-unknown path is rejected.

Enforced **uniformly** at every surface that admits a predicate — each reaches the model via its `ModelStore`:

| Surface | Data-field matrix + value-type | Lifecycle/temporal (meta) |
|---|---|---|
| `/search` (HTTP + gRPC) | full — single `SearchService.validateConditionTypes` boundary | full (already #423) |
| grouped-statistics | **newly enabled** — thread the model in (today passes `ValidateConditionValueTypes(nil, …)`) | full (already) |
| workflow-criterion import | **newly enabled** — load the model (the workflow handler already holds a `ModelStore`) and validate data-field predicates, not only lifecycle | full (already) |

Gate-6 clean-ups riding along: `ArrayCondition` value-type validation (currently skipped — `walkConditionTypes` returns nil for it); and the fail-open branches above (a correctness-over-availability fix in its own right).

### 5.4 Help-topic hygiene (Gate 4)
Broaden `errors/CONDITION_TYPE_MISMATCH.md` DESCRIPTION to cover operator-class mismatch (not only value-type), and correct the over-promising "both /search and grouped-statistics enforce" line to reflect §5.3.

## 6. SQL planner changes

### 6.1 Postgres (`plugins/postgres/query_planner.go`)
- eq/ne/gt/lt/gte/lte/between: type-gate the numeric branch on the **stored** value — `jsonb_typeof(<fieldExpr>) = 'number'` AND operand numeric → `cyoda_try_float8` numeric compare; string operand → `jsonb_typeof=… 'string'` text compare; otherwise the predicate does not match (cross-type §3.2). Stop coercing numeric-looking stored strings.
- `like`: stop `escapeLike`-neutering the user's `%`/`_`; pass the (validated) pattern with `ESCAPE '\'`. Postgres LIKE is already case-sensitive and character-based. Malformed escapes are rejected upstream (§3.3) so 22025 is unreachable.
- boolean: render/compare via `jsonb_typeof='boolean'` so a stored bool compares only to a bool operand (`->>` gives `"true"/"false"`; the kernel compares `bool==bool`; align the text forms).
- temporal (`Coercion`): unchanged (`cyoda_epoch_millis`, migration 000005).

### 6.2 SQLite (`plugins/sqlite/query_planner.go`, `store_factory.go`)
- eq/ne/ordering/between: type-gate on `json_type(<data>, <path>)` (`'integer'`/`'real'` → numeric; `'text'` → string; `'true'`/`'false'` → boolean) so numeric compare only fires on stored numbers and cross-type doesn't match. This removes reliance on affinity.
- `like`: set case-sensitivity via the ncruces DSN `_pragma=case_sensitive_like(true)` (robust per-connection; `SetMaxOpenConns(1)` today, but DSN form is future-proof); stop `escapeLike`-neutering wildcards. `instr`/`substr` (contains/starts/ends) are unaffected by the PRAGMA.
- boolean: `json_extract` coerces JSON bool→`1`/`0`; use `json_type` to detect and compare the boolean distinctly so it matches the kernel (bool↔bool only).
- temporal: unchanged (`/1000` floor).

### 6.3 `isPushable`
Unchanged set; `like` stays pushable (now correct). Regex + case-insensitive stay non-pushable.

## 7. Error / status-code table

All 4xx carry full domain detail + error code; no new codes.

| Endpoint | Scenario | Status | Code |
|---|---|---|---|
| `POST /search` (+ gRPC Search) | string-class op on numeric field / any op on boolean field per §5.2 | 400 | `CONDITION_TYPE_MISMATCH` |
| `POST /search` (+ gRPC) | ordering on boolean field | 400 | `CONDITION_TYPE_MISMATCH` |
| `POST /search` (+ gRPC) | malformed LIKE escape grammar | 400 | `INVALID_CONDITION` |
| `POST /search` (+ gRPC) | operand value-type mismatch (existing #423) | 400 | `CONDITION_TYPE_MISMATCH` |
| `POST /search` (+ gRPC) | unknown meta field / bad path (existing) | 400 | `INVALID_FIELD_PATH` |
| `POST /search` (+ gRPC) | malformed BETWEEN arity (existing) | 400 | `INVALID_CONDITION` |
| `POST /search` (+ gRPC) | numeric operand on string field (now rejected, was accepted) | 400 | `CONDITION_TYPE_MISMATCH` |
| grouped-statistics endpoints | same matrix as /search (newly model-aware) | 400 | `CONDITION_TYPE_MISMATCH` / `INVALID_CONDITION` / `INVALID_FIELD_PATH` |
| workflow import | lifecycle/temporal (existing) + **data-field** operator-class/value-type + LIKE grammar (newly model-aware) | 400 (import reject) | `INVALID_CONDITION` / `CONDITION_TYPE_MISMATCH` / `INVALID_FIELD_PATH` |
| `POST /search` / grouped-stats | model/schema unavailable or undecodable (now **fails closed**, was proceed) | 5xx + ticket | generic (5xx) |
| all above | valid condition, no matches (incl. cross-type non-match) | 200 | — (empty result) |

## 8. Coverage matrix (scenario × layer)

Layers: **U** = SPI `EvalLeaf` unit test; **E** = running-backend e2e (`internal/e2e`, real Postgres); **P** = cross-backend parity (`e2e/parity`, memory+sqlite+postgres, picked up by commercial); **G** = gRPC (`internal/grpc`).

| Scenario | U | E | P | G |
|---|---|---|---|---|
| eq/ne same-type number / string / bool | ✓ | ✓ | ✓ | — |
| eq/ne **cross-type** (number vs string, bool vs string, …) → non-match | ✓ | ✓ | ✓ | — |
| ordering same-type number (numeric) / string (lexical) | ✓ | ✓ | ✓ | — |
| ordering cross-type → excluded | ✓ | — | ✓ | — |
| between numeric / string / mixed-bound → non-match / malformed→400 | ✓ | ✓ | ✓ | ✓ |
| contains/starts/ends string / non-string→non-match | ✓ | ✓ | ✓ | — |
| **like** glob `%`/`_`, case-sensitive, `\` escape | ✓ | ✓ | ✓ | — |
| like malformed escape → 400 | ✓(grammar) | ✓ | ✓ | ✓ |
| like on non-ASCII (`_` = one char) | ✓ | ✓ | ✓ | — |
| matches_regex (residual) | ✓ | ✓ | ✓ | — |
| ieq/ine (equality-class, allowed on number) | ✓ | ✓ | ✓ | — |
| icontains/istarts/iends (+negatives) | ✓ | ✓ | ✓ | — |
| is_null / not_null: absent vs **present JSON null** | ✓ | ✓ | ✓ | — |
| negative-op vacuous truth on absent field (ne/ine/inot_*/NOT_CONTAINS…) | ✓ | ✓ | ✓ | — |
| null-operand eq/ne ≡ is_null/not_null | ✓ | ✓ | ✓ | — |
| array-wildcard `[*]` with NOT_CONTAINS over mixed array (ANY vs ALL) | ✓ | ✓ | — | — |
| validation: string-op on numeric field → 400 | — | ✓ | ✓ | ✓ |
| validation: any/ordering op on boolean field → 400 | — | ✓ | ✓ | ✓ |
| validation: numeric operand on string field → 400 (now rejected) | — | ✓ | ✓ | ✓ |
| validation: unknown data-field path → 400 INVALID_FIELD_PATH | — | ✓ | ✓ | — |
| validation: grouped-stats now model-aware → 400 | — | ✓ | ✓ | — |
| validation: model/schema unavailable → fails closed (5xx), not proceed | — | ✓ | — | — |
| workflow-import: data-field op-class/value-type + lifecycle rejected | — | ✓ | — | — |

Concurrency: none in parity (per rule) — leaf semantics are deterministic.

## 9. Test harness

- **SPI `EvalLeaf` unit table** (`cyoda-go-spi/filter_match_test.go` + `_internal_test.go`): the kernel contract — every op × stored-type × operand-type, incl. cross-type, null/present-null, null-operand, negatives, LIKE grammar.
- **`e2e/parity` leaf-op × stored-type matrix** (new scenario file, registered in `registry.go`): every op against number/string/bool/null/absent stored values × matching/mismatching operand types, asserted identical across memory+sqlite+postgres (and thereby the commercial backend). Seed each field with a definite model type so validation-reject scenarios trigger. This matrix is the refactor's safety net: RED on each §1 divergence, GREEN as each §6/§4 change lands.
- **Reject parity** reuses the existing 400-asserting harness (`SearchBetweenArity400` pattern).
- Migrate the string-operator scenarios that are currently **postgres-only** in `internal/e2e/search_test.go` into the cross-backend matrix.

## 10. SPI coordinated release

Kernel + rune-based `matchFilterLike` land in `cyoda-go-spi` on the **v0.8.3 accumulation line** (currently pseudo-pinned `v0.8.3-0.…-930c293` on `main`). Per `MAINTAINING.md` §3: SPI commits first, then a single pin bump across all four `go.mod` manifests + plugin repin (`make repin-plugins`), composed locally via `go.work` (uncommitted SPI `use` line — never `git add -A`). Real `cyoda-go-spi v0.8.3` tag at milestone-end, not per-issue.

## 11. Cloud parity

Operator-class validation and the cross-type-non-match semantics change the integration contract Cloud mirrors. Per Gate 7 (cyoda-go leads): add `docs/cloud-parity/431-leaf-comparison-semantics.md` recording the canonical §3 semantics + the §5 validity matrix, reconciled with Cloud's `InvalidTypesInClientConditionException` (which today targets value-type, not operator-class). Note the observable result change for tenants storing numbers-as-strings (Postgres stops coercing).

## 12. Documentation (Gate 4)

The predicate/operator semantics are shared by **search** and **workflow criteria** (both evaluate `predicate.Condition`), so they are documented **once** in a new dedicated help topic and referenced from both.

- **New topic `cmd/cyoda/help/content/predicates.md`** — the canonical reference:
  - The five condition types (simple / lifecycle / group / array / function) at a glance (structure lives in `search`; this topic owns *semantics*).
  - **Operator catalog** with the §3.3 canonical meaning of each op (not the current stale "numeric-aware EQUALS" wording).
  - **Type model** (§3.1) and the **cross-type = non-match** rule (§3.2), with a worked example (`number 30` vs string `"30"` → no match).
  - **LIKE grammar**: rune-based glob, case-sensitive, `%`/`_` wildcards, `\` escape (`\%`,`\_`,`\\` only; malformed → `INVALID_CONDITION`).
  - **Null / presence** handling (`is_null`/`not_null`, absent vs present-JSON-null, null-operand normalization, negative-op vacuous truth).
  - **Operator-vs-field-type validity matrix** (§5.2) and its strict, fail-closed enforcement against the authoritative model (§5.3), with the `400` codes.
  - Register in the help topic tree/index; satisfy the help-completeness test.
- **Update `cmd/cyoda/help/content/search.md`** — keep the CONDITION DSL *structure*; replace the now-incorrect per-operator descriptions (lines ~69–94: "numeric-aware EQUALS", bare "LIKE = % / _") with a pointer to `predicates`; add `predicates` to `see_also`.
- **Update `cmd/cyoda/help/content/workflows.md`** — where criteria/conditions are described, cross-reference `predicates` (criteria use the same operators + validity rules; import-time validation is strict against the model per §5.3).
- **`errors/CONDITION_TYPE_MISMATCH.md`** — broaden DESCRIPTION to include operator-class mismatch; correct the "both /search and grouped-statistics enforce" line per §5.3 (§5.4).
- **README / OpenAPI**: verify the search request schema description doesn't restate stale operator semantics; point to the topic. Keep prose compact (state the rule, not every nuance — detail lives in this spec).

Help topics are consumed downstream by cyoda-docs (`cyoda help` topic actions), so `predicates` becomes the single upstream source for predicate semantics.

## 13. Migration / behavior changes (observable)

1. **LIKE now globs on SQL backends** (was literal). `%`/`_` in a LIKE operand become wildcards on sqlite/postgres.
2. **SQLite LIKE becomes case-sensitive** (was ASCII-case-insensitive).
3. **Postgres stops coercing numeric-looking stored strings** — stored `"30.0"` no longer eq operand `30`.
4. **Cross-type comparisons stop coinciding** (memory/criteria too): number vs string operand → non-match, not lexical.
5. **BETWEEN on strings stops erroring in workflow criteria** (was a float-parse error) → lexical.
6. **`EQUALS null` / present JSON null** normalized (eq-null ≡ is_null; present JSON null is null).
7. **New 400s (strict validation, §5):** string-op on numeric field; any/ordering op on boolean field; **numeric operand on a string field** (was accepted); malformed LIKE escape; unknown data-field path (now uniform across surfaces).
8. **Validation is now strict + fail-closed** — data-field type validation runs at grouped-statistics and workflow-criterion import too (not just `/search`), and a **model/schema load failure now fails the request** (5xx) instead of proceeding unvalidated. Correctly rejects predicates that never matched anyway, but is an observable tightening for callers who relied on the old permissive/fail-open behavior.

## 14. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Refactor changes results subtly | The §9 parity matrix + `EvalLeaf` unit table pin every op before the walkers are collapsed (RED/GREEN discipline). |
| Type-aware SQL non-sargable → scans | Accepted per design philosophy; documented; expression-index a later optimisation. |
| Commercial (Cassandra) backend red on the new matrix | File a tracking issue in `cyoda-go-cassandra` referencing §3/§5; it reconciles on its next dependency bump (Paul's call). |
| SQLite bool `1/0` coercion vs kernel `bool` | `json_type`-gated bool branch; parity bool cells verify. |
| Strict validation rejects queries that used to be accepted (e.g. numeric operand on string field; unknown path at a surface that used to skip) | These predicates never matched under the canonical semantics anyway (§3.2). Called out in §13; a `400` is clearer than a silent empty result. |
| Fail-closed on schema-load failure reduces availability | Mandated by correctness-over-availability; the multi-node refresh handles legitimate schema-propagation lag, so only genuine store/decode failures fail — which should fail. |
| Big-number float64 imprecision | Consistent on all paths; documented non-goal. |

## 15. Out of scope / follow-ups
- Big-number exact comparison (`json.Number`/`big`).
- Expression indexes for type-aware predicates.
- Data-field temporal typing (#137).
- Commercial-backend reconciliation (issue filed in `cyoda-go-cassandra`).
