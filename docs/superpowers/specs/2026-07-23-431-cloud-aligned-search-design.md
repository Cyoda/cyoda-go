# #431 (redefined) — Align cyoda-go search with the Cloud type-directed model

**Status:** architecture agreed with Paul; this is the design of record.
**Supersedes:** `2026-07-23-431-evaluator-convergence-design.md` (cross-type paradigm — wrong).
**Reference:** `docs/cyoda/entity-search.md` — how Cyoda Cloud search actually works.
**Subsumes:** #137 (polymorphic/data-field temporal typing) — folded in here.
**Milestone:** v0.8.3 line. **Branch:** `feat/431-evaluator-convergence` (off `release/v0.8.3`).

## 1. What and why

Cyoda Cloud is the reference "digital twin"; cyoda-go must evaluate the **same
search semantics**. Cloud does **type-directed, same-type-only** comparison:
storage is segregated by declared type, and a query is expanded at plan time into
an OR of same-type sub-conditions (one per declared type the string operand parses
into), each hitting its type's slot; comparison never crosses types; validation is
**parse-based only** (reject iff the operand parses into none of the field's
declared types, plus arity). See `docs/cyoda/entity-search.md` for the full account.

cyoda-go today compares raw `gjson` values via a `float64` coercion, with untyped
`any` operands and a single Temporal/None coercion bit — a different paradigm that
drifts from Cloud. **This effort aligns the semantics.** Storage is **not** changed
(cyoda-go keeps raw JSON documents; we do **not** adopt Cloud's per-type ValueMaps).

### What cyoda-go already has (reuse, don't rebuild)
- 19 of Cloud's 21 `DataType`s (`schema/types.go`); only `BYTE`/`SHORT`/`FLOAT`
  are deliberately absent (fine — see §4).
- **Precise** numeric classification (`big.Int` + a custom `Decimal`), a widening
  lattice, and `IsAssignableTo` matching Cloud's envelopes (`schema/numeric.go`).
- Per-leaf polymorphic type sets `FieldDescriptor.Types` (`schema/field.go`) — the
  analogue of Cloud's `Polymorphic`, built additively on ingest.
- The shared temporal instant kernel (`spi.ParseTemporalMillis`/`CompareTemporal`, #423).
- The `spi.FilterCoercion` seam (currently 1 bit) — the natural insertion point.

The gap is purely that **none of this reaches the evaluator**. This effort carries
declared types into evaluation.

## 2. Governing principle

Comparison is **same-type only**, driven by two type signals:
1. the **model's declared types** for the leaf field (`FieldDescriptor.Types`), and
2. the **stored JSON value's own type**, classified at eval time and reconciled
   with (1).

There is **no cross-type comparison** and **no operator-vs-field-type validation**.
The operand is treated as a string and parsed into each declared type (Cloud's
`.asText()` → `parseStringOrNull`). A predicate is rejected only when the operand
parses into **no** declared type (or fails arity). This matches Cloud exactly.

## 3. Architecture

### 3.1 One authoritative type-directed kernel (SPI)
A single kernel in `cyoda-go-spi` is the **source of truth** for leaf evaluation.
Conceptually:

```
EvalLeaf(op, operandString, declaredTypes []DataType, stored gjson.Result) -> (bool, error)
```

Algorithm (mirrors Cloud, collapsed for single-value storage):
1. **Classify** the stored value into a `DataType` using the existing schema
   classifier (`InferDataType`/`ClassifyInteger`/`ClassifyDecimal`), intersected
   with `declaredTypes`. (A stored value whose type isn't in the declared set can
   never match a declared-type branch → non-match, like Cloud's absent slot.)
2. **Expand** the operand: for each declared type, `parseStringOrNull` (port of
   Cloud's per-type parse) + numeric-bucket range/rounding + temporal-resolution
   conversions → the set of `(type, typedValue, op')` sub-conditions. Reject 400 if
   none parse; a numeric-but-unsatisfiable expansion is **void** (§3.4).
3. **Select + compare**: evaluate the sub-condition(s) whose type equals the stored
   value's classified type, **same-type**, with precise comparison (§3.3). The
   Cloud "OR over slots" collapses to "the branch matching the stored value's type."

Both Go evaluators delegate to it — this is the original #431 convergence, now
type-directed:
- `internal/match` (workflow criteria, search `GetAll` fallback, grouped-stats
  residual) keeps only its tree-walk over `predicate.Condition`.
- `spi.MatchFilter` (memory search, residual post-filter, iterate, streaming-tally)
  keeps only its tree-walk over `spi.Filter`.
Delete the duplicated per-op comparison code in both.

### 3.2 SQL pushdown = best-effort **sound** narrowing (not the authority)
This mirrors Cloud (its Cassandra index narrows; `evaluateSimple` is authoritative).

- The **kernel re-checks every candidate** a backend returns; correctness lives
  solely in the kernel, so **results never diverge across backends.**
- Each backend's planner narrows **as tightly as it can efficiently express**
  (best-effort, per-backend — type-directed WHERE via `jsonb_typeof`/`typeof`,
  numeric/temporal helpers, prefix-LIKE ranges, etc.), subject to **one invariant:
  soundness — the pushed predicate must be a *superset* of true matches** (may
  over-select, must never drop a real match). A backend that cannot express a
  branch tightly (e.g. sqlite has no bignum) simply narrows less; the kernel still
  decides. No divergence, no sqlite-precision blocker.
- Backends may be optimized independently over time to reduce false positives; that
  is a pure performance lever with zero semantic risk.

This resolves the two problems that would otherwise sink a faithful port
(cross-backend result divergence; sqlite precision).

### 3.3 Precise numerics
The kernel compares numbers with the schema layer's existing precise types
(`big.Int` / `Decimal`), not `float64` — matching Cloud (lossless `BigDecimal`) and
correct beyond 2⁵³. Enabled by 3.2 (kernel is Go-only; it can afford precision even
where a backend's SQL cannot). `spi.NumericFloat`-based comparison is retired from
the leaf path.

### 3.4 Void vs reject (Cloud's trichotomy)
- **Success** → the matching same-type branch decides.
- **Void** (operand is a number but no declared numeric type can hold a matching
  value — e.g. `EQUALS 12.5` against an `INTEGER`-only field) → the leaf is a void
  predicate: in a tree-walk, **OR drops it, AND is annihilated** (Cloud's null-leaf
  composition).
- **Reject 400** → operand parses into no declared type.

## 4. Type system porting

- **`parseStringOrNull` per type** — port Cloud's parse rules (`DataType.kt`):
  strict `BOOLEAN` (`true`/`false`), ISO-8601 temporal per subtype, UUID variants,
  numeric via precise Decimal with range/precision checks. cyoda-go already has the
  numeric classifiers and `Decimal`; add the per-type string→value entrypoints and
  the temporal-subtype parsers.
- **Numeric buckets** (`PolymorphicNumberConversions` analogue): parse operand as
  the broadest number, then per declared numeric type do range-classify
  (IN_RANGE / ABOVE / BELOW), semantics-preserving rounding for imprecise values
  (`>=`/`<` → ceiling, `<=`/`>` → floor), out-of-range → `NOT_NULL`/drop, and
  imprecise-`EQUALS` → drop. Adapted to cyoda-go's numeric set (no BYTE/SHORT/FLOAT
  — fewer buckets, same logic). `UNBOUND_*` verbatim.
- **Missing `BYTE`/`SHORT`/`FLOAT`**: cyoda-go never infers them, so a field is
  never declared as them; no operand needs a bucket for them. Confirm the importer
  accepts a Cloud model that declares them by widening to `INTEGER`/`DOUBLE` (import
  concern, `importer/walker.go`), and document the mapping. No eval bucket needed.
- **Temporal subtypes (subsumes #137)** — port Cloud's six-type parsing +
  resolution down/upscale graph (`PolymorphicTemporalConversions` analogue) with
  precision-aware op mutation (`>= 2024-09-09` on `YEAR` → `> 2024`) and
  imprecise-`EQUALS` drop. Light up **data-field** temporal (today dormant:
  `classifyType` never returns `OrderTemporal` for data fields). Reconcile with the
  existing meta-field instant path (`creationDate`/`lastUpdateTime`) so meta and
  data temporal share the subtype machinery. Retire the single-bit temporal
  coercion in favour of the declared-type-driven path (keep the meta instant
  behaviour as the `ZONED_DATE_TIME`/instant case).

## 5. Operators

Support the Cloud search-reachable set (`docs/cyoda/entity-search.md` §5):
- **unary**: `IS_NULL`, `NOT_NULL`.
- **range**: `BETWEEN` (exclusive), `BETWEEN_INCLUSIVE` (inclusive).
- **binary**: `EQUALS/NOT_EQUAL/GREATER_THAN/GREATER_OR_EQUAL/LESS_THAN/LESS_OR_EQUAL`,
  `CONTAINS/STARTS_WITH/ENDS_WITH` + `NOT_*` + case-insensitive `I*`/`INOT_*`,
  `IEQUALS/INOT_EQUAL`, `MATCHES_PATTERN`, `LIKE`. **`IS_CHANGED/IS_UNCHANGED` are
  dropped** — change-generation ops, not relevant to search here; cyoda-go does not
  implement them and won't.

Per-operator semantics (authoritative in the kernel), matching Cloud:
- **equality/ordering**: same-type `compareTo`; **null never matches**; numbers
  compared precisely; UUID time-comparator for v1.
- **string ops** case-sensitive substring/prefix/suffix; `I*` case-fold both sides;
  operate on the stored value's string form only when the stored value is textual
  (same-type gate).
- **LIKE**: `%`→`.*?`, `_`→`.`, all other regex metachars escaped, `\` escape,
  **whole-string anchored, case-sensitive** (Cloud `Like`). This *changes* cyoda-go's
  current LIKE (rune-regex, no escape) and SQL LIKE (wildcard-neutered) — align both
  to Cloud's grammar; malformed escape handling per Cloud.
- **MATCHES_PATTERN**: Go RE2, whole-string anchored (Cloud uses `Pattern.matches`).
  RE2-vs-Java regex dialect differences are an **accepted bounded divergence**,
  documented in `cyoda help` (the `predicates` topic) — not reconciled.
- **BETWEEN** exclusive / **BETWEEN_INCLUSIVE** inclusive; precise numeric bounds
  (avoid Cloud's double-based BETWEEN quirk — do the principled precise thing).

**Cloud quirks we deliberately do *not* replicate** (do the principled thing;
note in the parity doc): the two non-identical BETWEEN representations; BETWEEN's
`double` widening; the BETWEEN UUID-comparator inconsistency; `Matches` null-via-NPE.

## 6. Validation (parse-based; operator-class matrix dropped)

Align to Cloud: reject a leaf iff
- the `jsonPath`/field is unknown to the model → `INVALID_FIELD_PATH` (400);
- the operand parses into **none** of the field's declared types →
  `CONDITION_TYPE_MISMATCH` (400) (Cloud's `Invalid[Polymorphic]TypesInClientConditionException`);
- arity: a binary/range op with a null operand, a range op whose value isn't a
  2-element array, or an object/complex operand → `INVALID_CONDITION` (400)
  (Cloud's `InvalidNullOperands`/`InvalidArraySize`/`InvalidComplexType`).

**No operator-vs-field-type validation** (Cloud has none; the earlier operator-class
matrix is dropped). `CONTAINS 5` on a numeric field is accepted (parses as a number;
simply won't match a numeric stored value under same-type string-op rules → empty,
not an error). Reuse existing error codes; no new codes. Enforce at the search
boundary (HTTP+gRPC) and workflow-criterion import, using the model (available at
both). Keep the multi-node schema-refresh; unknown path after refresh → reject.

## 7. Where expansion happens

- **Search** has the model at the `SearchService` boundary → expand there
  (analogue of Cloud's `coerceConditionValueForReport`) so the pushed/residual
  condition already carries typed sub-conditions; the kernel re-checks.
- **Single-entity evaluation** (workflow criteria; residual per-entity) has the
  actual entity + model → the kernel classifies the stored value and evaluates the
  matching branch directly (the OR collapses to one branch); no pre-expansion
  needed. Same kernel, same semantics.

## 8. Operand contract

cyoda-go operands arrive as JSON-parsed `any` (`predicate.Condition.Value`,
`spi.Filter.Value`), not strings. Normalize to the **string form** at the expansion
boundary and parse per declared type (Cloud's `.asText()`), so a JSON number `30`
and JSON string `"30"` are treated identically (both parse-tested against every
declared type). Preserve `Values` for BETWEEN.

## 9. Error / status-code table

| Surface | Scenario | Status | Code |
|---|---|---|---|
| `/search` (HTTP+gRPC), grouped-stats, workflow import | operand parses into no declared type | 400 | `CONDITION_TYPE_MISMATCH` |
| " | unknown field / bad path | 400 | `INVALID_FIELD_PATH` |
| " | null operand on binary/range; range operand not 2-array; object operand | 400 | `INVALID_CONDITION` |
| " | unknown operator / malformed body | 400 | `INVALID_CONDITION` (or parse 400) |
| " | valid condition, no matches (incl. same-type miss, void leaf) | 200 | — |
| " | model/schema unavailable | 5xx + ticket | generic |

## 10. Coverage matrix (scenario × layer)

Layers: **U** = SPI kernel unit test; **E** = running-backend e2e (`internal/e2e`);
**P** = cross-backend parity (`e2e/parity`, memory+sqlite+postgres, +commercial);
**G** = gRPC.

| Scenario | U | E | P | G |
|---|---|---|---|---|
| polymorphic `[INTEGER,STRING]` eq "30" matches int-30 and string-"30" | ✓ | ✓ | ✓ | — |
| polymorphic eq "hello" → only string branch; no 400 | ✓ | ✓ | ✓ | — |
| numeric bucket rounding (`>=12.78` on int field → `>=13`) | ✓ | ✓ | ✓ | — |
| out-of-range bound → NOT_NULL / drop | ✓ | ✓ | ✓ | — |
| precise big-int/decimal compare beyond 2^53 | ✓ | ✓ | ✓ | — |
| temporal subtype + resolution (`>=2024-09-09` on YEAR → `>2024`) | ✓ | ✓ | ✓ | — |
| temporal imprecise EQUALS dropped | ✓ | ✓ | ✓ | — |
| void leaf (`eq 12.5` on int field): OR drops, AND annihilates | ✓ | ✓ | — | — |
| LIKE anchored escaped glob, case-sensitive | ✓ | ✓ | ✓ | — |
| string ops case sensitivity + I-variants | ✓ | ✓ | ✓ | — |
| BETWEEN exclusive / inclusive, precise bounds | ✓ | ✓ | ✓ | ✓ |
| IS_NULL/NOT_NULL absent vs present-null | ✓ | ✓ | ✓ | — |
| pushdown soundness: SQL over-selects, kernel re-checks (per backend) | — | ✓ | ✓ | — |
| validation: operand parses no type → 400 | — | ✓ | ✓ | ✓ |
| validation: null operand / bad arity → 400 | — | ✓ | ✓ | ✓ |
| meta temporal (`creationDate`) still chronological | ✓ | ✓ | ✓ | — |

Concurrency tests isolated (not parity). The parity matrix doubles as the
Cloud-parity guard.

## 11. Test harness
- SPI **kernel unit table**: every op × declared-type(s) × stored-type × operand,
  incl. polymorphic expansion, numeric buckets, temporal resolution, void/reject.
  Seed directly from `docs/cyoda/entity-search.md` worked examples.
- **`e2e/parity` type-directed matrix** across memory+sqlite+postgres (+commercial).
- A **pushdown-soundness** property per backend: for a random condition + corpus,
  the pushed candidate set ⊇ the kernel's true match set.
- Migrate today's postgres-only string-op tests into the cross-backend matrix.

## 12. Cloud-parity, SPI release, docs
- `docs/cloud-parity/431-search-semantics.md` recording the aligned semantics + the
  deliberate non-replications (§5 quirks) and the RE2-vs-Java-regex note.
- Kernel + type-porting land in `cyoda-go-spi` on the v0.8.3 line (pseudo-pin, real
  tag at milestone-end; `make repin-plugins`); local `go.work` composition.
- **Docs (Gate 4):** new `cmd/cyoda/help/content/predicates.md` (operator catalog +
  type-directed semantics + LIKE grammar + validation), correct `search.md`'s stale
  operator descriptions, cross-ref `workflows.md`, broaden `CONDITION_TYPE_MISMATCH.md`.
- File a `cyoda-go-cassandra` issue referencing this design so the commercial
  backend aligns + passes the new parity matrix.

## 13. Migration / behaviour changes (observable)
1. Comparison becomes same-type + type-directed (numeric-looking strings, cross-type
   operands behave per Cloud, not lexical coincidence).
2. Precise numeric comparison (big-int/decimal) replaces float64.
3. LIKE becomes anchored escaped glob, case-sensitive, on all backends (SQL LIKE
   un-neutered; sqlite case-sensitive).
4. Data-field temporal lights up (subtypes + resolution); meta temporal unchanged in
   result.
5. Validation is parse-based: new 400 only when operand parses into no declared type
   (and arity); **no** operator-class rejections.
6. `IS_CHANGED/IS_UNCHANGED` remain unimplemented (explicitly dropped).

## 14. Staging (delivery phases, each its own PR under redefined #431)
1. **Kernel + type porting (SPI):** `parseStringOrNull` per type, numeric buckets,
   precise compare, void/reject, operator semantics; kernel unit tests. No behaviour
   change until wired.
2. **Wire `internal/match` + `spi.MatchFilter` to the kernel;** delete duplicated
   comparison code; single-entity + residual paths aligned; e2e + parity.
3. **Temporal subtypes (subsumes #137):** per-subtype parse + resolution graph; data
   temporal lit up; meta reconciled.
4. **Validation** alignment (parse-based; drop operator-class matrix); error table;
   boundary + import.
5. **Pushdown best-effort narrowing** per backend (sqlite/postgres), soundness
   property; optimize false positives.
6. **Docs + cloud-parity + Cassandra issue + SPI tag.**

## 15. Resolved decisions
- **`IS_CHANGED/IS_UNCHANGED`: dropped** — not relevant to search; not implemented.
- **`BYTE`/`SHORT`/`FLOAT`: moot.** cyoda-go builds schemas by **data discovery**
  (the `importer` package imports *data*, not a foreign declared-type schema) and
  its inference never produces these types; there is no path by which a Cloud model
  declaring them enters cyoda-go. If a foreign-schema import is ever added, map them
  to `INTEGER`/`DOUBLE` (search-lossless) at that point — not now.
- **`MATCHES_PATTERN` regex dialect:** accept the bounded RE2-vs-Java divergence;
  document it in the `predicates` help topic. Not reconciled.
