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
- The **type-classification seam** for filters: `ConditionToFilter` consults the
  schema (`FieldsMap`) and stamps a comparison-coercion marker on `spi.Filter`
  (mirroring `OrderSpec.Kind`); backends consume the marker. Built generically
  (data + meta) so #137 flips a single classifier decision, not the seam.
- The **instant-comparison kernel**: `spi.ParseTemporalMillis` (canonical
  epoch-ms scalar) and `cyoda_epoch_millis` (postgres `IMMUTABLE` SQL function).
- The meta-key mapping fix in each SQL backend's filter `fieldExpr` (also
  incidentally repairs `transitionForLatestSave`/`transactionId` meta filters,
  which are broken the same way — Gate 6).
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
- No change to the numeric/boolean/text comparison paths — the coercion marker
  only ever distinguishes `temporal` from `none` (existing behaviour).

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

`ConditionToFilter` gains a `fields map[string]schema.FieldDescriptor` parameter
(the service already loads it for path validation; callers that lack one pass
`nil`). For each leaf it stamps `Coercion`:

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
`grouped_stats_service.go:105` (pass `fields` if loaded, else `nil`), and the
recursive `groupToFilter` child call (thread `fields` through).

### 6.3 The predicate path (no schema at the comparison point)

`internal/match` (used by **workflow criteria** via `engine.go:826`, the search
`GetAll` fallback, and grouped-stats residual) never goes through
`ConditionToFilter`. Detection there is **field-identity-driven, still not
operand-driven**: `matchLifecycle` already special-cases `creationDate` (and we
add `lastUpdateTime`), so those field names *are* the type signal. Ordering ops
on those fields compare chronologically; other ops keep their current string
behaviour on the RFC3339 form.

## 7. Per-surface implementation

The comparison the engine performs becomes, uniformly:

```
  <stored value → epoch-ms int64>   <op>   <operand → epoch-ms int64>
```

The operand is parsed once in Go via `ParseTemporalMillis`. The stored value is
converted to epoch-ms from that backend's physical form.

| Surface | Trigger | Stored-value conversion | Operand |
|---|---|---|---|
| `internal/match` `matchLifecycle` | field is `creationDate`/`lastUpdateTime` | `meta.CreationDate.UnixMilli()` | `ParseTemporalMillis(op)` |
| `spi.MatchFilter` (memory Search + residuals) | `Filter.Coercion == CoerceTemporal` | stored value → ms: `time.Time`→`UnixMilli`; string→`ParseTemporalMillis` | `ParseTemporalMillis(op)` |
| postgres planner | `Filter.Coercion == CoerceTemporal` | `cyoda_epoch_millis(<fieldExpr>)` (RFC3339 text → ms) | `$N` = int64 ms |
| sqlite planner | `Filter.Coercion == CoerceTemporal`, `SourceMeta` | `json_extract(meta,'$.creation_date') / 1000` (µs → ms) | `?` = int64 ms |

Notes:
- `spi.extractFilterMetaValue` gains `creationDate` → `meta.CreationDate` and
  `lastUpdateTime` → `meta.LastModifiedDate` (returned as `time.Time`); the
  temporal branch in `evalLeafFilter` converts `time.Time`→`UnixMilli` and
  string→`ParseTemporalMillis`. Existing `created_at`/`updated_at` (µs) keys are
  left untouched for grouped-stats callers.
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

This reuses the existing missing-value branch in `evalLeafFilter`, and the SQL
`IS NOT NULL` guard already emitted by the planners mirrors it exactly, so Go and
SQL agree row-for-row. If the **operand** does not parse
(`ParseTemporalMillis` ok=false), the filter cannot be evaluated as intended —
the leaf matches nothing for positive ops / everything for `NE` (same rule),
consistent across surfaces.

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
  -- so ::timestamptz never falls back to the session TimeZone.
  IF t IS NULL OR t !~ '\A\d{4}-\d{2}-\d{2}[Tt].+(?:[Zz]|[+-]\d{2}:?\d{2})\Z' THEN
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

### 8.2 Meta-key mapping in filter `fieldExpr`

Both SQL planners currently interpolate the raw `f.Path` for `SourceMeta`
filters, so canonical names whose blob key differs (`creationDate`→`creation_date`,
`lastUpdateTime`→`last_modified_date`, `transitionForLatestSave`→`transition`/
`transition_for_latest_save`, `transactionId`→`transaction_id`) resolve to
NULL. Apply the existing `metaJSONKey`/`metaBlobKey` maps (today sort-only) in
the filter `fieldExpr` path too. This is what makes `creationDate` resolve at
all, and it repairs the sibling string-meta filters as a Gate-6 by-product.

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
4. **The exclude/vacuous semantics + ms floor (§7.1):** inherited unchanged.

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

No new endpoints, no new error codes — this is a behaviour fix on existing search
entry points. The search filter/validation error codes are unchanged; the one
error-adjacent behaviour to assert is that a **malformed operand** does not 500
(it evaluates to the empty/vacuous set per §7.1) and that a `creationDate` filter
is accepted (200) rather than rejected.

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
| `BETWEEN` inclusive bounds, mixed precision | ✓ | ✓ | ✓ | — |
| Same, on `lastUpdateTime` | ✓ | — | ✓ | — |
| Malformed operand ⇒ empty set (positive) / vacuous (NE), no 500 | ✓ | ✓ | — | ✓ |
| `creationDate` filter is accepted (200), not rejected | — | ✓ | — | ✓ |
| Workflow **criterion** on `creationDate` ordering compares chronologically | ✓ | ✓ | — | — |

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
