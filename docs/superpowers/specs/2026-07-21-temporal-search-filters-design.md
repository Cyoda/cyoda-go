# Chronological comparison of temporally-typed fields in search filters (#423)

Status: design — approved for spec review
Issue: #423 (milestone v0.8.3)
Blocks: #137 (polymorphic temporal-subtype detection + temporal range indexes)

## 1. Problem

Search **filter** comparison treats temporally-typed field values as opaque
strings and compares them **lexically**, not chronologically. RFC3339 strings do
not sort lexically the way instants order chronologically:

- **Precision:** `2021-01-01T00:00:00Z` and `2021-01-01T00:00:00.000Z` are the
  *same instant*, but as text `…00Z` sorts **after** `…00.000Z` (`Z` > `.`).
- **Offset:** `2021-06-01T14:00:00+02:00` is `12:00Z` — an hour **before**
  `2021-06-01T13:00:00Z` — yet as text `"14…"` sorts **after** `"13…"`.

You cannot repair this by massaging the text; the operands must be compared as
**instants**.

## 2. Logical architecture (the governing principle)

**A field's declared type governs its comparison semantics. Nothing else.** A
`String` field is compared with string semantics (lexical); a temporally-typed
field is compared chronologically. Detection is **type-driven** — never derived
from the shape of the operand value. This mirrors how sort already works:
`resolveOrderBy` → `classifyType(fd.Types)` → an `OrderKind` stamped on
`OrderSpec.Kind`, which the plugins consume blindly.

### What is temporally typed *today*

`schema.inferDataType` (internal/domain/model/schema/validate.go) maps **every**
`string` value to `String`. The temporal `DataType`s (`LocalDate`,
`LocalDateTime`, `ZonedDateTime`, `YearMonth`, `LocalTime`) exist in the enum but
the classifier never assigns them, and `scalarClass` collapses them all to
`OrderText`. Assigning those subtypes to body fields is exactly **#137**, which
**depends on #423**.

Therefore the only temporally-typed fields that exist today are the engine meta
timestamps **`creationDate`** and **`lastUpdateTime`** (already typed
`OrderTemporal` in `sortableMetaFields`; genuine instants). A body date field is
a `String`; comparing it lexically is *correct* behaviour, not a bug, until #137
makes it temporally typed.

This spec fixes the chronological comparison for the fields that are temporally
typed today, and builds the reusable **type-classification seam** and
**instant-comparison kernel** that #137 extends to body fields.

## 3. What is broken today (all three reference backends)

A `creationDate`/`lastUpdateTime` filter is a `LifecycleCondition` →
`lifecycleToFilter` → `spi.Filter{Source: SourceMeta, Path: "creationDate"}` →
the plugin `Searcher.Search` pushdown path (not the `internal/match` fallback).
Meta timestamps are physically stored three different ways, and every backend is
broken:

| Backend | Physical storage of `creationDate` | Today's filter behaviour |
|---|---|---|
| memory (spi) | `EntityMeta.CreationDate` — a `time.Time` | `extractFilterMetaValue` has no `creationDate` case → **no-match** |
| sqlite | `meta` blob key `creation_date` = µs int64 | filter `fieldExpr` extracts `$.creationDate` (wrong key; `metaBlobKey` is sort-only) → **NULL → no-match** |
| postgres | `doc._meta` key `creation_date` = RFC3339Nano text | filter `fieldExpr` extracts `->>'creationDate'` (wrong key; `metaJSONKey` is sort-only) → **NULL → no-match** |

So the fix has two layers: (a) make these filters *resolve* (meta-key mapping),
and (b) compare the resolved instants *chronologically*.

## 4. Scope

### In scope
- Chronological filter comparison for the temporally-typed meta fields
  `creationDate` and `lastUpdateTime`, for `EQUALS`, `NOT_EQUAL`, `GREATER_THAN`,
  `LESS_THAN`, `GREATER_OR_EQUAL`, `LESS_OR_EQUAL`, `BETWEEN`, across all four
  evaluation surfaces.
- **Type-sound operator handling on temporal fields (§6.4):** a temporal field
  supports only the comparison + null operators above; string/pattern/
  case-insensitive operators (`CONTAINS`, `STARTS_WITH`, `ENDS_WITH`, `LIKE`,
  `MATCHES_PATTERN`, `I*`) are **rejected 400**, not silently lexically
  evaluated. A temporal operand that is not offset-bearing RFC3339 is likewise
  **rejected 400** (fail-loud), not silently emptied.
- The **type-classification seam** for filters: `ConditionToFilter` consults the
  schema (`FieldsMap`) and stamps a comparison-coercion marker on `spi.Filter`
  (mirroring `OrderSpec.Kind`); backends consume the marker. Built generically
  (data + meta) so #137 flips a single classifier decision, not the seam.
- The **instant-comparison kernel**: `spi.ParseTemporalMillis` (canonical
  epoch-ms scalar) and `cyoda_epoch_millis` (postgres `IMMUTABLE` SQL function).
- **Meta filter vocabulary reconciliation (§6.5):** make the filter path resolve
  the full canonical `sortableMetaFields` vocabulary (`state`, `creationDate`,
  `lastUpdateTime`, `transitionForLatestSave` [alias `previousTransition`],
  `transactionId`, `id`) **consistently across all three backends and
  `match.Match`**, via the generalized meta-key mapping (§8.2) and matching
  `extractFilterMetaValue`/`matchLifecycle` keys. An unknown meta filter field is
  **rejected 400** on every surface (unifying today's error-vs-no-match split).
- **Numeric-coercion evaluator alignment (§6.6):** remove the string-operand
  parsing divergence between `match` and `spi.MatchFilter` by delegating both to
  one shared coercion — the first slice of the evaluator convergence (§13).
- **Shared leaf primitives (§13):** the temporal (and now numeric) leaf logic is
  authored *once* in the SPI and delegated to by both Go evaluators, so it is
  reused — not replaced — by the convergence successor.
- Full test coverage (unit per surface, running-backend e2e, cross-backend
  parity, gRPC) — see §11.

### Out of scope → #137
- Teaching `inferDataType`/the classifier to assign `ZONED_DATE_TIME` etc.
- Changing `scalarClass`/`classifyType` to return a temporal class for those.
- Body-field temporal comparison (undefined until a body field can *be*
  temporally typed) and its parity scenarios.
- Parsing the bracketed IANA-zone form `…+01:00[Europe/Paris]`.
- Temporal **range indexes** and the commercial-backend typed index.
- `simple_view.go` exporter changes.

### Explicit non-goals
- **No operand-sniffing.** The operand's shape never determines whether a
  comparison is temporal. `String` fields are untouched.
- The comparison-class marker only ever distinguishes `temporal` from `none`;
  the numeric/boolean/text *evaluation* paths are unchanged.

### Also fixed here: the numeric-coercion evaluator divergence
The two Go evaluators disagree on string-encoded numerics —
`internal/match.opCompare`/`opEquals` parse a string operand to a float while
`spi.compareFilterValues` does not — so `numericField > "20"` can differ between
the `GetAll`+`match.Match` fallback and the `spi.MatchFilter` pushdown/memory
path. #423 removes the divergence by **aligning `match` down to `spi`**: `match`
stops parsing string operands as floats and delegates numeric comparison to the
same shared coercion `spi` uses (§6.6). This is done as *deletion of a divergent
copy*, not a second implementation — see §6.6 for why it is the first slice of
the evaluator-convergence successor, not throwaway work.

### Related, but NOT here (a distinct contract decision)
A separate gap remains: there is no **operator-vs-field-type validation** — string
operators on **numeric/boolean** fields silently coerce (`CONTAINS 0` matches
numeric `100`), ordering ops on booleans evaluate lexically. #423 fixes the
*temporal* slice of operator validity (§6.4) because handling temporal fields is
its remit, but the numeric/boolean slice is a product/contract decision (it
touches Cloud's `InvalidTypesInClientConditionException`) and is left for an
explicit future decision — not silently deferred, just genuinely out of this
change's remit. It is orthogonal to the evaluator convergence below.

## 5. The canonical scalar and parser

Add to the SPI:

```go
// ParseTemporalMillis parses an offset-bearing RFC3339 timestamp to floored
// epoch-milliseconds. Returns ok=false for any input that is not full RFC3339
// with an explicit offset (Z or ±hh:mm). The mandatory offset makes the value
// an absolute instant, which is what lets cyoda_epoch_millis be IMMUTABLE.
func ParseTemporalMillis(s string) (int64, bool) {
    t, err := time.Parse(time.RFC3339, s) // RFC3339 layout requires an offset
    if err != nil {
        return 0, false
    }
    return t.UnixMilli(), true // floors sub-ms toward the epoch
}
```

- **Resolution:** floored epoch-**milliseconds**, matching the established
  `OrderTemporal` sort canonical and the cross-engine (Cassandra-HLC) floor.
- **Offset mandatory:** `time.Parse(time.RFC3339, …)` rejects offset-less input;
  `creationDate`/`lastUpdateTime` are always stored `…Z`, so this always
  succeeds for them. The mandatory offset is load-bearing for #137's indexes
  (§9).

This helper parses the **query operand** on every surface, and (in #137) body
temporal **values**.

## 6. The type-classification seam

### 6.1 The coercion marker on `spi.Filter`

Add one field to `spi.Filter`, mirroring `OrderSpec.Kind`:

```go
type FilterCoercion int
const (
    CoerceNone     FilterCoercion = iota // existing numeric/text/bool behaviour
    CoerceTemporal                        // compare as floored epoch-ms instants
)
// Filter gains: Coercion FilterCoercion
```

`CoerceNone` (the zero value) preserves today's behaviour for every existing
filter. Only `CoerceTemporal` is new. #137 needs **no new marker value**.

### 6.2 Stamping (domain layer, where the schema lives)

`ConditionToFilter` gains a `fields map[string]schema.FieldDescriptor` parameter;
callers that lack a schema pass `nil`. Plumbing note: `Search` currently loads
the `FieldsMap` only inside `validateConditionPaths`/`resolveSortKeys` (local
scope); the plan must hoist a `loadFieldsMap` call into `Search` so the resolved
map is in scope at the `ConditionToFilter` call site (service.go:179). For each
leaf `ConditionToFilter` stamps `Coercion`:

- **meta leaf:** static table — `creationDate`, `lastUpdateTime` → `CoerceTemporal`;
  everything else → `CoerceNone`.
- **data leaf:** `classifyType(fd.Types)` (reused verbatim). Map its result:
  `OrderTemporal` → `CoerceTemporal`; everything else → `CoerceNone`. With
  `fields == nil` or the path absent → `CoerceNone`.

Today `classifyType` never returns `OrderTemporal` for a data field, so **data
leaves always stamp `CoerceNone`** — the seam is wired but dormant for data.
**#137's only interaction with this seam:** make the classifier assign
`ZONED_DATE_TIME` and make `scalarClass` return `OrderTemporal` for it; a data
ZonedDateTime leaf then stamps `CoerceTemporal` automatically. No change to
`ConditionToFilter`, the marker, or any backend consumer.

Call sites to update: `service.go:179` (pass resolved `fields`),
`grouped_stats_service.go:105` (passes `nil` — it loads no schema, so meta
temporal still stamps correctly via the static table; data-temporal coercion
stays dormant there until #137 additionally teaches grouped-stats to load a
`FieldsMap` — a one-line forward-compat gap, acceptable since grouped-stats on a
body temporal field is meaningless until #137 anyway), and the recursive
`groupToFilter` child call (thread `fields` through).

### 6.3 The predicate path (no schema at the comparison point)

`internal/match` (used by **workflow criteria** via `engine.go:826`, the search
`GetAll` fallback, and grouped-stats residual) never goes through
`ConditionToFilter`. Detection there is **field-identity-driven, still not
operand-driven**: `matchLifecycle` already special-cases `creationDate` (and we
add `lastUpdateTime` — it currently *errors* on that field), so those field
names *are* the type signal.

For those fields, **all comparison ops** — `EQUALS`, `NOT_EQUAL`,
`GREATER_THAN`, `LESS_THAN`, `GREATER_OR_EQUAL`, `LESS_OR_EQUAL`, `BETWEEN` —
compare chronologically (epoch-ms), not just the ordering ops. This is required
for two reasons: (1) the motivating §1 bug is a *precision-equality* case
(`…00Z` vs `…00.000Z` same instant), so `EQUALS` itself must be chronological;
(2) the memory `GetAll` fallback (`service.go:222`) evaluates via `match.Match`
while the pushdown path evaluates via `spi.MatchFilter` — if the criteria path
left `EQUALS` lexical, the *same query* could return different results through
fallback vs pushdown. String/pattern/case-insensitive operators on these fields
are **rejected**, not evaluated (§6.4).

### 6.4 Type-sound operators on temporal fields

A temporal field is not a string; it does not offer string predicates. The
**valid** operators on `creationDate`/`lastUpdateTime` are exactly `EQUALS`,
`NOT_EQUAL`, `GREATER_THAN`, `LESS_THAN`, `GREATER_OR_EQUAL`, `LESS_OR_EQUAL`,
`BETWEEN`, `IS_NULL`, `NOT_NULL`. Any other operator (`CONTAINS`, `STARTS_WITH`,
`ENDS_WITH`, `LIKE`, `MATCHES_PATTERN`, and the `I*` case-insensitive variants)
is **rejected with 400** (`ErrCodeConditionTypeMismatch`) — it is not silently
evaluated as a lexical string op.

Additionally, for a comparison op the **operand must be offset-bearing RFC3339**
(`ParseTemporalMillis` ok); a non-temporal operand on a temporal field is a type
mismatch → **400** (fail-loud, at validation time), never a silent empty result.
(The *stored*-value non-parseable case is different — it is per-row runtime data,
handled by exclude/vacuous in §7.1.)

Enforcement point: this operator-and-operand check lives in the shared condition
validator (`ValidateCondition`/a temporal-aware companion) which
`walkConditionTypes` currently exempts for lifecycle fields — the exemption is
lifted for the two temporal fields. It runs on every entry point that validates
conditions (HTTP, gRPC) and the workflow-criteria import validation path, so a
temporal criterion with an invalid operator is rejected at import, not silently
mis-evaluated at transition time.

This is deliberately scoped to temporal fields; the equivalent operator-vs-type
soundness for numeric/boolean fields is the separate issue noted in §4.

### 6.5 Meta filter vocabulary reconciliation

The canonical set of filterable meta fields is exactly `sortableMetaFields`
(orderclass.go — "canonical client names"): `state`, `creationDate`,
`lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`, with
`previousTransition` an accepted alias of `transitionForLatestSave`. Today the
filter path is inconsistent — canonical names whose physical key differs resolve
to NULL/no-match on the SQL backends, are absent from `extractFilterMetaValue`
(memory), and `match.Match` *errors* on some. #423 makes all four surfaces agree:

- `lifecycleToFilter` normalizes `previousTransition`→`transitionForLatestSave`
  and stamps the canonical name as `Filter.Path`.
- Each backend maps the canonical name → its physical storage (§8.2):
  postgres/sqlite via the existing `metaJSONKey`/`metaBlobKey` (+ `id`→entity_id),
  spi `extractFilterMetaValue` via added canonical-name cases
  (`creationDate`/`lastUpdateTime`→`time.Time` temporal; `state`/
  `transitionForLatestSave`/`transactionId`→string; `id`→`meta.ID`). The
  pre-existing storage-key cases (`created_at`, `entity_id`, …) are left in place
  — no current filter producer emits them (only `lifecycleToFilter` produces
  `SourceMeta` filters), so this is purely additive.
- `matchLifecycle` is aligned to the same canonical vocabulary (adds
  `lastUpdateTime`, `transactionId`, `id`; keeps `state`,
  `transitionForLatestSave`/`previousTransition`; temporal ones chronological).
- **Unknown meta field → 400** (`ErrCodeInvalidFieldPath`) at validation, on
  every surface — replacing the current error-vs-no-match split with one
  fail-loud response. `cyoda-go` leads this contract (Gate 7).

### 6.6 Numeric-coercion evaluator alignment

`internal/match.opCompare`/`opEquals` parse a **string** operand to a float
(`toFloat64` accepts strings), so `numericField > "20"` compares numerically;
`spi.compareFilterValues` deliberately does not, so the same clause compares
lexically. The two Go evaluators therefore disagree on string-encoded numerics,
and the disagreement is reachable on any path that skips value-type validation
(notably **workflow criteria**). #423 removes it by **aligning `match` down to
`spi`**: `match` stops treating a string operand as numeric — numeric comparison
applies only when the operand is a genuine numeric type, exactly as `spi` does.

Implementation discipline (this is what makes it convergence-seed, not throwaway):
the numeric leaf comparison is delegated to a **single shared coercion helper**
that both evaluators call, rather than edited in two places. The common case
(numeric operand on a numeric field) is unchanged; only a string-encoded numeric
operand changes, and it changes *toward* the primary pushdown behaviour. Verify
against existing `internal/match` tests and the Cloud contract before landing the
direction; a RED test pins the fallback-vs-pushdown agreement.

## 7. Per-surface implementation

The comparison the engine performs becomes, uniformly:

```
  <stored value → epoch-ms int64>   <op>   <operand → epoch-ms int64>
```

The operand is parsed once in Go via `ParseTemporalMillis`. The stored value is
converted to epoch-ms from that backend's physical form. **Both Go evaluators
(`matchLifecycle` and `evalLeafFilter`) convert only their own value form to ms
and then call one shared temporal dispatcher** — `spi.CompareTemporal(op,
storedMs, storedOK, operandMs)` (or equivalent) — which encodes the per-operator
result and the exclude/vacuous rule (§7.1) exactly once. Neither evaluator gets
its own temporal branch to later reconcile (§13).

| Surface | Trigger (all comparison ops: EQ/NE/GT/LT/GE/LE/BETWEEN) | Stored-value conversion | Operand(s) |
|---|---|---|---|
| `internal/match` `matchLifecycle` | field is `creationDate`/`lastUpdateTime` | `meta.CreationDate.UnixMilli()` | `ParseTemporalMillis(op)` (both bounds for BETWEEN) |
| `spi.MatchFilter` (memory Search + residuals) | `Filter.Coercion == CoerceTemporal` | stored value → ms: `time.Time`→`UnixMilli`; string→`ParseTemporalMillis` | `ParseTemporalMillis(op)` (both bounds for BETWEEN) |
| postgres planner | `Filter.Coercion == CoerceTemporal` | `cyoda_epoch_millis(<fieldExpr>)` (RFC3339 text → ms) | `$N` = int64 ms (two placeholders for BETWEEN) |
| sqlite planner | `Filter.Coercion == CoerceTemporal`, `SourceMeta` | `json_extract(meta,'$.creation_date') / 1000` (µs → ms) | `?` = int64 ms (two for BETWEEN) |

Notes:
- `spi.extractFilterMetaValue` gains the canonical vocabulary (§6.5), including
  `creationDate` → `meta.CreationDate` and `lastUpdateTime` →
  `meta.LastModifiedDate` (returned as `time.Time`); the temporal branch in
  `evalLeafFilter` converts `time.Time`→`UnixMilli` and string→`ParseTemporalMillis`.
  The pre-existing storage-key cases (`created_at`/`updated_at`/`version`/… — a
  historical mirror of the sqlite post-filter keyset) are left in place but are
  emitted by no current filter producer (§6.5); this change is purely additive.
- **sqlite meta is µs-int → `/1000`**, *not* `cyoda_epoch_millis` (which parses
  text). The `cyoda_epoch_millis(text)` form and its sqlite registration are
  **deferred to #137**, where the temporal *data-text* path first needs them.
  Postgres uses `cyoda_epoch_millis` now because its meta is RFC3339 text.
- The predicate path's `matchLifecycle` converts `meta.CreationDate` directly
  (`UnixMilli`); no RFC3339 round-trip.

### 7.1 Comparison / exclude semantics (defined once)

For a temporal comparison where the stored value does not convert to ms
(non-parseable / absent / null):

- `EQUALS`/`GREATER_THAN`/`LESS_THAN`/`GREATER_OR_EQUAL`/`LESS_OR_EQUAL`/`BETWEEN`
  → **excluded** (no match).
- `NOT_EQUAL` → **vacuously true** (matches).

For a truly **missing/null** value the existing `!found || val == nil` branch in
`evalLeafFilter` already yields this, and the SQL `IS NOT NULL` guard mirrors it.
But a value that is **present yet does not convert to ms** does *not* hit that
branch — so the temporal branch needs its **own** unparseable→exclude/vacuous
handling (it must not fall through to a lexical compare). For #423 this is
belt-and-suspenders (engine meta `time.Time` always converts); it becomes
load-bearing for #137 body text. The SQL side gets this for free: a
non-convertible stored value makes `cyoda_epoch_millis(...)`/`(…/1000)` NULL,
which the `IS NOT NULL` guard excludes (and the `IS NULL OR` form matches for
`NE`) — so Go and SQL still agree row-for-row.

`BETWEEN` carries **two** operands (`f.Values[0]`, `f.Values[1]`); each is parsed
via `ParseTemporalMillis`, and the SQL planners bind two placeholders.

The **operand** side is validated **up-front** (§6.4): a non-RFC3339 operand on a
temporal field is a 400 at validation, so by the time a leaf reaches the
evaluators the operand always parses. The evaluators therefore never face an
unparseable operand; the only runtime "can't convert" case is a *stored* value,
handled by exclude/vacuous above. (The evaluators still guard defensively — an
unparseable operand degrades to the same exclude/vacuous rule — but the boundary
validation means that path is unreachable for validated entry points.)

## 8. `cyoda_epoch_millis` (postgres) and the meta-key mapping fix

### 8.1 Migration `000005_temporal_epoch_millis`

```sql
-- Offset-bearing RFC3339 text → floored epoch-milliseconds, or NULL.
-- IMMUTABLE (required for the functional indexes #137 will build on it):
-- the mandatory offset means the instant is timezone-independent, so the
-- result does not depend on the session TimeZone. Modeled on cyoda_try_float8.
CREATE OR REPLACE FUNCTION cyoda_epoch_millis(t text) RETURNS bigint AS $$
DECLARE result bigint;
BEGIN
  -- Reject anything without an explicit offset (Z or ±hh:mm) BEFORE the cast,
  -- so ::timestamptz never falls back to the session TimeZone. Uppercase T/Z
  -- only, to match Go's time.RFC3339 parse (avoids a Go/SQL disagreement that
  -- would otherwise surface for #137 body text; engine meta is always uppercase).
  IF t IS NULL OR t !~ '\A\d{4}-\d{2}-\d{2}T.+(?:Z|[+-]\d{2}:?\d{2})\Z' THEN
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

(The regex is a pre-filter; the `EXCEPTION` net catches anything that passes the
regex but still fails to cast. Final anchoring uses `\A…\Z`. Exact regex to be
validated against the RFC3339 forms in tests.)

Down migration drops the function.

### 8.2 Meta-key mapping in filter `fieldExpr` (generalized — item 12)

Both SQL planners currently interpolate the raw `f.Path` for `SourceMeta`
filters, so any canonical name whose physical key differs resolves to NULL. Apply
the existing `metaJSONKey`/`metaBlobKey` maps (today sort-only) in the filter
`fieldExpr` path for the **whole** canonical vocabulary (§6.5), with `id` →
`entity_id` (direct column). Because the memory side (`extractFilterMetaValue`)
and `match.Match` are reconciled to the *same* vocabulary in the same change
(§6.5), generalizing the SQL mapping introduces **no** cross-backend divergence —
all four surfaces resolve the identical canonical set. A name not in the map is
already rejected 400 at validation (§6.5), so `fieldExpr` never sees an unknown
meta path; defensively it falls through to the raw path (unreachable for
validated callers).

## 9. Forward-compatibility contract with #137

#423 leaves these artifacts for #137 to reuse **without touching the comparison
kernel**:

1. **The classification seam (§6):** #137 flips `inferDataType` +
   `scalarClass`; a data ZonedDateTime leaf then stamps `CoerceTemporal` and
   flows through unchanged.
2. **The canonical scalar + parser (§5):** reused verbatim for ZonedDateTime
   values; #137 extends only the parse front-end for the bracketed IANA-zone
   form.
3. **The indexable coercion expression (§8):** `cyoda_epoch_millis` is
   `IMMUTABLE`; the postgres planner already emits
   `cyoda_epoch_millis(<fieldExpr>) <op> $N` for temporal leaves, so #137's
   `CREATE INDEX … (cyoda_epoch_millis(doc->>'field'))` is sargable with no
   planner change. (sqlite: #137 registers `cyoda_epoch_millis` as a
   deterministic scalar function — required for sqlite expression indexes — and
   adds the data-text planner branch.)
4. **The exclude/vacuous semantics + ms floor (§7.1):** inherited unchanged —
   including the present-but-unparseable→exclude handling in the temporal branch,
   which is dormant for meta (`time.Time` always converts) but is exactly what
   #137's body text needs.

What #137 still owns: classifier assignment, `scalarClass` temporal mapping, the
per-subtype decision that **local** types (`LocalDate`, `YearMonth`,
`LocalDateTime`, `LocalTime`) stay lexical (their uniform ISO form already sorts
chronologically — they never need the instant kernel), bracketed-zone parsing,
sqlite `cyoda_epoch_millis` registration + data-text planner branch, the range
indexes + commercial coordination, and the exporter.

## 10. SPI change & coordinated release

Touches `cyoda-go-spi`: `ParseTemporalMillis`, the `Filter.Coercion` field +
`FilterCoercion` type, and the temporal branch in `filter_match.go`
(`evalLeafFilter`/`extractFilterMetaValue`). Per MAINTAINING.md
"Coordinated release across sibling repos":

- Develop against the local SPI checkout via `go.work` (gitignored; **no
  committed `replace` directives**).
- During the v0.8.3 window, pseudo-version-pin `cyoda-go-spi` to its pushed HEAD
  across the root and all `plugins/*/go.mod` (`make check-spi-pin-sync` green);
  the SPI tag + final pin bump happen at milestone-end.
- `plugins/postgres` and `plugins/sqlite` and `internal/match` all consume the
  new SPI symbols.

No `cyoda-go` binary-version ↔ SPI-version coupling is implied.

## 11. Test coverage

No new endpoints and **no new error codes** — reuses `ErrCodeConditionTypeMismatch`
and `ErrCodeInvalidFieldPath`. But there are new *behaviours* on the existing
search + workflow-criteria entry points, including new 400s (§6.4/§6.5).

### 11.0 Error / status-code table (search endpoints + workflow-criteria import)

| Condition on a temporal / meta field | HTTP | Error code |
|---|---|---|
| Valid comparison op + valid RFC3339 operand on `creationDate`/`lastUpdateTime` | 200 | — |
| String/pattern/case-insensitive op on a temporal field | 400 | `CONDITION_TYPE_MISMATCH` |
| Non-RFC3339 (non-temporal) operand on a temporal field | 400 | `CONDITION_TYPE_MISMATCH` |
| Unknown/unsupported meta filter field | 400 | `INVALID_FIELD_PATH` |
| Valid string op (`EQUALS`, `CONTAINS`, …) on a string meta field (`state`, `transitionForLatestSave`, `transactionId`, `id`) | 200 | — |
| Same invalid conditions in a **workflow criterion** (rejected at import) | 400 | as above |

(Exact code constants per `internal/common/error_codes.go`; the HTTP handler
already maps `errConditionTypeMismatch`→400 `CONDITION_TYPE_MISMATCH` and invalid
paths→400 `INVALID_FIELD_PATH`.)

### 11.1 Behaviour matrix (scenario × layer)

Layers: **U** = unit (per surface), **E** = running-backend e2e (`internal/e2e`,
real postgres), **P** = cross-backend parity (`e2e/parity`, memory+sqlite+postgres
+commercial), **G** = gRPC (`internal/grpc`).

| Scenario (on `creationDate`, mixed precision `…Z` vs `…000Z` = same instant) | U | E | P | G |
|---|---|---|---|---|
| `GREATER_THAN` returns the chronologically-later set (not lexical) | ✓ | ✓ | ✓ | ✓ |
| `LESS_THAN` | ✓ | ✓ | ✓ | — |
| `GREATER_OR_EQUAL` boundary (`…Z` vs `…000Z` same instant ⇒ included) | ✓ | ✓ | ✓ | — |
| `LESS_OR_EQUAL` boundary | ✓ | ✓ | ✓ | — |
| `EQUALS`: two different strings, same instant ⇒ equal | ✓ | ✓ | ✓ | ✓ |
| `NOT_EQUAL`: same instant ⇒ not-matched; different ⇒ matched | ✓ | ✓ | ✓ | — |
| `BETWEEN` inclusive bounds, mixed precision (both bounds parsed) | ✓ | ✓ | ✓ | — |
| Same suite on `lastUpdateTime` (also asserts it no longer 500s) | ✓ | ✓ | ✓ | — |
| `creationDate` filter with valid op+operand is accepted (200) | — | ✓ | — | ✓ |
| String/pattern op on a temporal field ⇒ **400** `CONDITION_TYPE_MISMATCH` | ✓ | ✓ | — | ✓ |
| Non-RFC3339 operand on a temporal field ⇒ **400** `CONDITION_TYPE_MISMATCH` | ✓ | ✓ | — | ✓ |
| Unknown meta filter field ⇒ **400** `INVALID_FIELD_PATH` (all surfaces agree) | ✓ | ✓ | ✓ | ✓ |
| String meta filters (`transitionForLatestSave`, `transactionId`, `id`) resolve identically across backends (item 12) | ✓ | ✓ | ✓ | — |
| Workflow **criterion** on `creationDate` — **EQUALS** (precision), NE, and ordering all chronological | ✓ | ✓ | — | — |
| Workflow **criterion** with invalid op on a temporal field ⇒ rejected at import (400) | ✓ | ✓ | — | — |
| Memory `GetAll` fallback and pushdown agree on `creationDate` EQUALS (no split) | ✓ | ✓ | — | — |

Each cell that pins chronological semantics is **RED against the current lexical
/ no-match implementation and GREEN after**. New parity scenarios registered in
`e2e/parity/registry.go`. Concurrency is not relevant here (read-only
comparison), so no isolated concurrency test.

Note on mixed-offset data: `creationDate`/`lastUpdateTime` are engine-generated
and always stored `…Z`, so the *stored* side is never offset-varying. Mixed
**offset** on the operand side (e.g. querying with `+02:00`) is covered by the
`EQUALS`/ordering scenarios. Mixed-offset *stored* data is a body-field concern →
#137.

### 11.2 Kernel unit tests
- `ParseTemporalMillis`: `…Z`, `….000Z`, `+02:00`, offset-less (rejected),
  garbage (rejected), sub-ms flooring.
- `cyoda_epoch_millis` (postgres): same set; offset-less → NULL; non-timestamp →
  NULL; equal instants across precision/offset → equal bigints.

## 12. Risks & mitigations

- **postgres/Go parse agreement.** postgres parses via `::timestamptz`; sqlite
  and memory use `ParseTemporalMillis`. All floor to ms and require an offset, so
  they agree; the parity suite verifies it directly on mixed precision/offset.
- **`IMMUTABLE` honesty.** Guaranteed only because the function rejects
  offset-less input (§8.1). This must be enforced by the regex pre-filter; a unit
  test asserts offset-less → NULL.
- **Seam dormant for data.** Intentional — the seam is exercised for meta today
  and unit-tested to stamp `CoerceNone` for data leaves (guarding against an
  accidental early activation before #137).
- **grouped-stats `nil` fields.** `ConditionToFilter(cond, nil)` must still
  classify meta temporal correctly (static table) and stamp `CoerceNone` for
  data — unit-tested.
- **Fallback/pushdown agreement.** Because `matchLifecycle` (fallback) and
  `spi.MatchFilter` (pushdown/memory) are separate implementations, a unit +
  e2e test asserts they return the identical set for a `creationDate` `EQUALS`
  across the precision boundary — guarding MAJOR-1's split.
- **postgres regex vs Go parse.** `cyoda_epoch_millis` is restricted to
  uppercase `T`/`Z` to match Go's `time.RFC3339` (§8.1). Immaterial for #423
  (only well-formed engine meta flows through it) but prevents a Go/SQL
  disagreement once #137 routes body text through the same function.

## 13. Relationship to the evaluator-convergence successor

Root cause of both the temporal (MAJOR-1) split and the numeric divergence: two
independent Go leaf-comparison implementations — `internal/match` (over
`predicate.Condition`) and `spi.MatchFilter` (over `spi.Filter`). The two
*representations* are justified (`predicate.Condition` is strictly more
expressive — `FunctionCondition` callouts, array-wildcards — and is the only form
workflow criteria have), but the duplicated *leaf-comparison logic* is not: how a
single `value <op> operand` behaves has no reason to differ, and its drift is the
bug source. A third semantics — the SQL the planners emit — genuinely cannot
share Go code and is kept in line by the parity suite, not by reuse.

The agreed successor effort (its own spec/plan, immediately after #423) converges
the two Go evaluators onto **one shared leaf-comparison kernel**: each keeps only
its own tree-walk, delegating every leaf op to the shared kernel; the SQL planners
mirror it under parity guard. That structurally removes the whole class of
fallback-vs-pushdown divergence.

**#423 is the first slice of that convergence, not a patch it discards.** Its
temporal dispatch (§7) and its numeric alignment (§6.6) are authored as shared
SPI primitives that both evaluators already delegate to. The convergence
*continues* by moving the remaining leaf ops (`CONTAINS`/`STARTS_WITH`/`ENDS_WITH`/
`LIKE`/`MATCHES_PATTERN`/`IS_NULL`/case-insensitive) into the same kernel and
collapsing the walkers — deleting nothing #423 built. Implementation constraint
for #423: **do not add a temporal or numeric branch that lives only inside one
evaluator** — every leaf-comparison decision #423 introduces must be in a shared
primitive both call.

### Execution & context plan
Design is complete and captured here; the spec + the `writing-plans` output are
the durable handoff. #423 is implemented via `subagent-driven-development`
(fresh-context subagent per task, reading spec + plan), so implementation detail
does not accumulate in the design session. The convergence is a fresh effort
afterwards, reusing #423's shared primitives.
