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

The kernel is a **leaf comparator only.** Each caller keeps its own tree-walk and
representation-specific logic — `internal/match`'s array-wildcard **ANY-match**
(`matchArrayWildcard`), lifecycle routing, and `FunctionCondition`; `spi.MatchFilter`'s
flat `spi.Filter` walk. Only the per-op leaf comparison is shared and deleted from
both. (This convergence also fixes a **live bug**: `filter_translate.go`'s
`mapOperator` has no `BETWEEN_INCLUSIVE` case and currently falls through to regex.)

Algorithm (mirrors Cloud, collapsed for single-value storage):
1. **Expand** the operand once per query (at the point the model is available, §7):
   for each declared type, `parseStringOrNull` (port of Cloud's per-type parse) +
   numeric-bucket range/rounding + temporal-resolution conversions → the set of
   `(type, typedValue, op')` sub-conditions. Reject 400 iff **none** parse. The
   operand string must be captured **losslessly** (§3.5).
2. **Classify** the stored leaf into a `DataType` from its **`gjson.Raw`** (not
   `.Value()` — see §3.5), via the schema classifier.
3. **Select by assignability + compare**: pick the sub-condition(s) whose type `U`
   the stored value's classified type `T` is **assignable to** (`schema.IsAssignableTo`,
   coercing `T`→`U` for the compare) — *not* exact `T==U`. This mirrors Cloud
   coercing a value into its declared slot at ingest (`5` in a `[LONG]` field
   matches). Compare **same-type**, precisely (§3.4). The Cloud "OR over slots"
   collapses to "the assignable branch." If no sub-condition's type is assignable
   from the stored type → **non-match** (Cloud's absent-slot).

**Null / absent uniformity.** A missing or JSON-null leaf **never matches any
binary op, including negatives** — `NOT_EQUAL`/`NOT_CONTAINS`/`INOT_*` are **not**
`!positive`; they null-guard to non-match, exactly like their positive twins. (Cloud:
"null never matches"; also CouchDB Mango, SQL, Postgres JSONB. Only MongoDB differs,
and it is a documented footgun.) `IS_NULL` = leaf absent or JSON-null; `NOT_NULL` =
present and non-null — handled directly, not via classify-then-branch.

### 3.2 SQL pushdown = EXACT vs SOUND-SUPERSET (per-leaf contract)
The kernel is authoritative; pushdown narrows. But "the kernel re-checks every
candidate" is **not** true on today's fast path: when a filter is fully pushable,
sqlite/postgres return SQL rows **verbatim** and apply `LIMIT/OFFSET` (and grouped-
stats `GROUP BY`) in SQL, with no Go re-check. An over-selecting pushdown would then
leak false positives *and* truncate pages wrong. So soundness is **correctness**-
critical, not a free perf lever, and needs an explicit contract:

- Each pushed leaf is classified **EXACT** (the SQL predicate matches the kernel
  bit-for-bit) or **SOUND-SUPERSET** (may over-select; kernel must re-check).
- The **fast path** — skip the Go re-check, push `LIMIT/OFFSET`, push `GROUP BY`
  aggregation — is permitted **only when every pushed leaf is EXACT.**
- If **any** leaf is SOUND-SUPERSET (or non-pushable), the planner retains the
  **full filter as residual**, the Go kernel re-checks every candidate, and
  `LIMIT/OFFSET`/paging happens **in Go** (as the residual path already does). For
  grouped-stats, a non-EXACT leaf makes the planner **opt out of native SQL GROUP-BY
  aggregation** (`ErrAggregationNotPushdownable`) and **fall back to the existing
  `Iterate`+Welford streaming tally**, where the kernel filters per row before
  tallying. **No query is rejected** — this is an execution-strategy switch to a
  path that already exists (and already fires today for regex/PIT/non-pushable
  filters); the only effect is per-row tally vs native aggregation. (Residual scans
  remain subject to the pre-existing `SCAN_BUDGET_EXHAUSTED` guard — unchanged.)
- Within that contract each backend narrows **as tightly as it can** (best-effort:
  `jsonb_typeof`/`typeof` gating, numeric/temporal helpers, prefix-LIKE ranges) —
  reducing false positives is a pure perf lever. sqlite's lack of bignums just means
  fewer EXACT leaves there; results never diverge because the kernel decides.

The memory backend already re-checks every candidate (`spi.MatchFilter` per row), so
it is soundness-safe by construction — **but that means memory-only tests mask
pushdown-soundness bugs; parity + a per-backend soundness property are required (§11).**

### 3.3 (reserved — merged into 3.1/3.2)

### 3.4 Precise numerics
The kernel compares numbers with the schema layer's existing precise types
(`big.Int` / `schema.Decimal`, which has exact cross-scale `Cmp` and `ParseDecimal`),
not `float64` — matching Cloud and correct beyond 2⁵³. `spi.NumericFloat`-based
comparison is retired from the leaf path. A **fast path** for the common
monomorphic-numeric / monomorphic-string leaf avoids bignum work per row.

### 3.5 Precision capture (both ends — an asymmetry to respect)
- **Operand (currently lossy — must fix):** `predicate/parse.go` decodes with plain
  `json.Unmarshal`, so a JSON number operand becomes `float64` **before** the kernel
  runs — a 20-digit int or `1e20` is already rounded. Fix at the source: decode the
  operand with `UseNumber` (→ `json.Number`) or retain the raw token, and carry the
  lossless string to the expansion. Do **not** re-stringify a `float64`.
- **Stored value (recoverable):** classify/compare from `gjson.Result.Raw`
  (the original JSON text) via `schema.ParseDecimal`, **not** `.Value()` (which is
  `float64`, and which `InferDataType` would misclassify as `String`).

### 3.6 Void
"Void" (operand is a number but no declared numeric type can hold a matching value —
e.g. `EQUALS 12.5` against an `INTEGER`-only field) is decided at **leaf
construction** over the **full** declared-type expansion (Cloud's `EmptyResult`), and
is observable **only under `NOT`**. cyoda-go's group walk supports **AND/OR only**
(`matchGroup` errors on `NOT`; `NOT` is not in the wire contract). Under monotone
AND/OR, void is observationally identical to **false**, so cyoda-go implements void as
plain **non-match** now — no three-valued machinery. **If `NOT` is ever added**, void
must become Cloud's construction-time whole-leaf form (OR drops it, AND annihilates);
noted for that future. **Reject 400** remains: operand parses into no declared type.

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
  coercion in favour of the declared-type-driven path.
  - **Meta-temporal must accept coarse operands.** Cloud runs meta fields
    (monomorphic `ZONED_DATE_TIME`) through the same expansion, so a coarse operand
    **upscales** to the instant: `creationDate >= "2024-09-09"` (or `"2024"`) is
    accepted and compared as an instant. Do the same — parse the meta-temporal
    operand as **any** temporal subtype and upscale via the resolution graph;
    do **not** restrict to strict offset-RFC3339 (that would 400 queries Cloud
    accepts). This subsumes/relaxes the #423 offset-mandatory rule for meta fields.
- **Numeric-bucket + temporal-resolution engine is its own phase** (§14) with the
  §6/`entity-search.md` worked examples wired as the executable oracle — it is the
  most substantial port, not a "same logic, adapt" footnote.

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
- **equality/ordering**: same-type `compareTo`; **null/absent never matches**
  (including negatives — §3.1); numbers compared precisely; UUID time-comparator for v1.
- **negatives** (`NOT_EQUAL/NOT_CONTAINS/INOT_*`): null-guard to **non-match**, not
  `!positive` (§3.1). Verify against the core-libs `NotEquals`/`NotContains` matcher
  during implementation; note the behavior change in migration + cloud-parity.
- **string ops** case-sensitive substring/prefix/suffix; `I*` case-fold both sides;
  apply only when the stored value is textual (same-type gate) — a string op against
  a non-textual stored value is a **non-match** (see §6 for the `CONTAINS`-on-numeric
  caveat).
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
under the same-type string-op gate it returns empty, not an error). *Caveat:* whether
Cloud's core-libs `Contains` stringifies a numeric slot (making it match) is not
determinable from OSS source — **verify against the `com.cyoda.core.conditions.Contains`
matcher**; cyoda-go's principled choice is non-match, recorded in the cloud-parity doc.
Reuse existing error codes; **no new codes.** Enforce at the search boundary
(HTTP+gRPC) and workflow-criterion import, using the model (available at both). Keep
the multi-node schema-refresh; unknown path after refresh → reject.

**Note — this replaces the existing validation.** Today's `validateSimpleConditionType`
(`IsAssignableTo`-based *value-type* checking) is removed in favour of the parse-based
rule; some existing E2E error-table tests will change which requests return 400 (a
migration cost — §13, and re-baseline the error-table coverage).

**`searchInStrings` dual-slot is out of scope.** Cloud can optionally store a textual
value in both its typed slot and the `strings` slot (`alsoSaveInStrings`/`searchInStrings`,
both default **off**, and off on production paths). The single-classification collapse
(§3.1) structurally cannot express matching a value under two type-branches, so this
capability is **not** ported; record the scope-out in the cloud-parity doc.

## 7. Where expansion happens

- **Search** has the model at the `SearchService` boundary → expand there
  (analogue of Cloud's `coerceConditionValueForReport`) so the pushed/residual
  condition already carries typed sub-conditions; the kernel re-checks every
  candidate unless all pushed leaves are EXACT (§3.2). Operand parsing happens
  once here, not per row.
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
| void leaf (`eq 12.5` on int field) → non-match under AND/OR | ✓ | ✓ | — | — |
| **negative op on absent field → non-match** (`NOT_EQUAL`/`NOT_CONTAINS`/`INOT_*`) | ✓ | ✓ | ✓ | — |
| stored-type reconciled by assignability (`5` matches `[LONG]` field) | ✓ | ✓ | ✓ | — |
| precision at both ends: 20-digit operand + stored value, no float rounding | ✓ | ✓ | ✓ | — |
| LIKE anchored escaped glob, case-sensitive | ✓ | ✓ | ✓ | — |
| string ops case sensitivity + I-variants; string op on non-textual → non-match | ✓ | ✓ | ✓ | — |
| BETWEEN exclusive / inclusive, precise bounds; `BETWEEN_INCLUSIVE` no longer regex | ✓ | ✓ | ✓ | ✓ |
| IS_NULL/NOT_NULL absent vs present-null | ✓ | ✓ | ✓ | — |
| **pushdown soundness**: EXACT fast-path vs SOUND-SUPERSET residual; over-select + kernel re-check; LIMIT/pagination correct under residual; grouped-stats GROUP-BY disqualified by non-EXACT leaf (per backend) | — | ✓ | ✓ | — |
| validation: operand parses no type → 400; **error-table re-baselined** vs old value-type validation | — | ✓ | ✓ | ✓ |
| validation: null operand / bad arity → 400 | — | ✓ | ✓ | ✓ |
| meta temporal (`creationDate`) chronological **incl. coarse operand** (`>= "2024"`) | ✓ | ✓ | ✓ | — |

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
   (and arity); **no** operator-class rejections. This **replaces** the current
   `IsAssignableTo` value-type validation, so some existing error-table tests change
   which requests 400 (re-baseline them).
6. **Negative operators on an absent/null field now return non-match** (were matched
   via `!positive`) — aligns Cloud/CouchDB/SQL; MongoDB differs. Verify vs core-libs.
7. `BETWEEN_INCLUSIVE` is fixed (was silently evaluated as a regex on `Searcher`
   backends via the `mapOperator` fall-through).
8. `IS_CHANGED/IS_UNCHANGED` remain unimplemented (explicitly dropped).

## 14. Staging (delivery phases, each its own PR under redefined #431)

Ordering constraint from review: the phase that makes the kernel **authoritative**
for evaluation must land **together with** the pushdown EXACT/SOUND-SUPERSET contract
— otherwise, mid-release, a fully-pushable query runs old SQL semantics (no re-check)
while a residual query runs new kernel semantics, so the same query differs by
pushability. So Phase 3 bundles the wire-up **and** the pushdown contract.

1. **Precision capture (prerequisite):** operand losslessly via `UseNumber`/raw token
   in `predicate/parse.go`; stored-value classification/compare from `gjson.Raw`.
   No behaviour change; unblocks precise compare.
2. **Kernel + type porting (SPI):** `parseStringOrNull` per type, precise compare,
   assignability reconciliation, null/negative uniformity, void, operator semantics;
   kernel unit table (seeded from `entity-search.md` examples). Not yet wired.
3. **Numeric-bucket + temporal-resolution engine (SPI):** the substantial port
   (range/rounding/NOT_NULL/drop; six temporal subtypes + resolution graph; data
   temporal lit up; meta coarse-operand upscale). Subsumes #137. Executable-oracle tests.
4. **Wire both evaluators to the kernel + pushdown contract (together):** delete
   duplicated comparison code; fix `BETWEEN_INCLUSIVE`; classify each pushed leaf
   EXACT/SOUND-SUPERSET; fast-path (skip re-check / SQL LIMIT / GROUP-BY) only when
   all-EXACT, else full residual + Go paging + GROUP-BY disqualification; per-backend
   soundness property; e2e + parity. **This is the kernel-authoritative switch.**
5. **Validation** alignment (parse-based; drop operator-class matrix); error table;
   re-baseline error-table tests; boundary + import.
6. **Pushdown tightening** per backend (reduce false positives: `jsonb_typeof`/`typeof`,
   more EXACT leaves) — pure perf, guarded by the soundness property.
7. **Docs + cloud-parity + Cassandra issue + SPI tag.**

## 15. Resolved decisions
- **`IS_CHANGED/IS_UNCHANGED`: dropped** — not relevant to search; not implemented.
- **`BYTE`/`SHORT`/`FLOAT`: moot.** cyoda-go builds schemas by **data discovery**
  (the `importer` package imports *data*, not a foreign declared-type schema) and
  its inference never produces these types; there is no path by which a Cloud model
  declaring them enters cyoda-go. If a foreign-schema import is ever added, map them
  to `INTEGER`/`DOUBLE` (search-lossless) at that point — not now.
- **`MATCHES_PATTERN` regex dialect:** accept the bounded RE2-vs-Java divergence;
  document it in the `predicates` help topic. Not reconciled.

Review-driven (independent design review, 2 fresh-context lenses):
- **Negative op on absent field → non-match** (aligns Cloud/CouchDB/SQL/Postgres;
  MongoDB is the outlier). §3.1/§5/§13.
- **Void → non-match now** (three-valued void deferred until `NOT` is supported). §3.6.
- **Stored-type reconciliation by assignability**, not exact membership. §3.1.
- **Pushdown EXACT vs SOUND-SUPERSET contract** — fast path only when all-EXACT; else
  residual + Go paging; grouped-stats GROUP-BY disqualified by any non-EXACT leaf. §3.2.
- **Lossless precision capture** at both ends (`UseNumber` operand, `gjson.Raw` stored). §3.5.
- **`searchInStrings` dual-slot: out of scope** (default-off; collapse can't express it). §6.
- **Meta-temporal accepts coarse operands** (upscale), not strict offset-RFC3339 only. §4.
