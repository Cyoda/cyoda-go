# How Cyoda Cloud entity search works (the reference model)

This document describes how **entity search evaluates predicates in Cyoda Cloud**
(the Kotlin "digital twin"), so that cyoda-go can align its search/predicate
semantics with it. It is a faithful account of the source, not a proposal; the
cyoda-go design derived from it lives in a separate spec.

Provenance (read directly from source):
- **cyoda-cloud** (`/Users/paul/dev/cyoda`): the tree-node search layer
  (`tree-node/tree-node-backend/.../com/cyoda/tdb/search/…`) and the entity/type
  layer (`client/.../org/cyoda/entity/…`).
- **cyoda-platform core-libs** (`/Users/paul/dev/cyoda-platform/core-libs`, pulled
  in as a binary dependency `com.cyoda.core.conditions.*`): the `Operation` enum
  and the concrete condition/matcher classes (`Equals`, `Like`, `Between`, …).
- The ordered **range index** itself is Cassandra, in a binary DAO
  (`cyoda-platform-dao`); the OSS tree only *marks* range-indexability.

## 0. The one idea that changes everything

Cyoda **never compares values across types at runtime.** Instead:

1. **Storage is type-segregated.** Every leaf value is stored under a
   **type-specific** slot — physically, one map per Java/`DataType` per node.
2. **The model declares each leaf field's type(s).** A field may be
   **polymorphic** — a *set* of `DataType`s (e.g. `[INTEGER, STRING]`), and each
   value is stored in the slot for the type it actually is.
3. **Queries are expanded at plan time.** The operand always arrives as a
   **string**; for each declared type of the field, Cyoda parses the string into
   that type and emits a **same-type** sub-condition on that type's slot, then
   **OR**s them.
4. **Comparison is always same-type**, against the typed slot. A sub-condition
   whose type-slot doesn't hold this entity's leaf simply doesn't match (the slot
   is absent) — no error, no cross-type coercion.
5. **Validation is parse-based, not operator-based.** A predicate is rejected
   (400) only if the operand parses into *none* of the field's declared types
   (plus arity checks). There is **no** "this operator isn't valid on this field
   type" rule.

Everything below is the detail behind these five points.

## 1. The `DataType` system

`org.cyoda.entity.model.DataType` (`client/.../entity/model/DataType.kt:33-83`).
Declaration order is load-bearing: when two types share an acceptor class, the
narrower is declared first (used by the polymorphic sort order, §6).

**Value-based (leaf) types** (`isValueBased()`):

| Group | Types (narrow→wide) |
|---|---|
| Stringy | `STRING`, `CHARACTER` |
| Whole numbers | `BYTE`, `SHORT`, `INTEGER`, `LONG`, `BIG_INTEGER`, `UNBOUND_INTEGER` |
| Decimals | `FLOAT`, `DOUBLE`, `BIG_DECIMAL`, `UNBOUND_DECIMAL` |
| Boolean | `BOOLEAN` |
| Temporal | `LOCAL_DATE`, `LOCAL_DATE_TIME`, `ZONED_DATE_TIME`, `YEAR`, `YEAR_MONTH`, `LOCAL_TIME` |
| UUID | `TIME_UUID_TYPE`, `UUID_TYPE` |
| Binary | `BYTE_ARRAY` |

**Structural sentinels** (not leaf values): `NULL`, `OBJECT`, `ARRAY`,
`ARRAY_ELEMENT`, `TYPE_REFERENCE`, `POLYMORPHIC`. Note there is **no plain
`DATE`** — dates are `LOCAL_DATE`.

**`parseStringOrNull(s): Any?`** (`DataType.kt:125-166`) — parse a string operand
into the type; any exception → `null`. Salient rules:
- `STRING` — **identity, always succeeds.** (So a field whose declared set
  includes `STRING` can never fail parse-validation.)
- `BYTE/SHORT/INTEGER/LONG/BIG_INTEGER/UNBOUND_INTEGER` — via `BigDecimal`;
  accepts `"1.0"`/`"2.00"`/`"1E2"` as whole numbers, rejects any fractional part
  or out-of-range value.
- `FLOAT/DOUBLE/BIG_DECIMAL/UNBOUND_DECIMAL` — precision/scale-checked
  (`FLOAT`: precision ≤ 6; `DOUBLE`: ≤ 15; `BIG_DECIMAL`: Trino Int128 bounds).
- `BOOLEAN` — **strict, case-sensitive** `"true"`/`"false"` only
  (`toBooleanStrictOrNull`); `"TRUE"`, `"1"`, `"yes"` → null.
- Temporals — Java **ISO-8601** formatters: `LOCAL_DATE` `2026-07-23`,
  `LOCAL_DATE_TIME` `2026-07-23T10:15:30`, `LOCAL_TIME` `10:15:30`,
  `ZONED_DATE_TIME` requires offset/zone (`…Z` or `+01:00[Zone]`), `YEAR` `2026`,
  `YEAR_MONTH` `2026-07`. (Offset required only for `ZONED_DATE_TIME`.)
- `UUID_TYPE` any RFC UUID; `TIME_UUID_TYPE` only version-1; `BYTE_ARRAY` Base64.

**Numeric groupings & lattice** (`DataType.kt:177-309`): `isIntegerType()`,
`isDecimalType()`; a widening map drives `isAssignableFrom`/`findCommonDataType`
(fallback join type is `STRING`). `UNBOUND_INTEGER`/`UNBOUND_DECIMAL` are the
"too big for a native/Trino number, kept as string" sinks; `UNBOUND_DECIMAL` is
the top of the numeric lattice. Precision limits live in
`entity/parsing/ParserFunctions.kt:28-59`.

## 2. How the model declares (polymorphic) leaf types

`ModelDataType` (`client/.../entity/model/structure/items/ModelDataType.kt`) is
the per-leaf type carrier: `private val types: Set<DataType>`;
`isPolymorphic() = types.size > 1`. It serializes as `"STRING"` (homomorphic) or
`"[INTEGER, STRING]"` (polymorphic). Leaf fields are
`PrimitiveFieldModel(fieldKey, fieldType: ModelDataType)`.

`Polymorphic` (`entity/model/Polymorphic.kt`) is the runtime type-set with
compatibility rules (a `TreeSet<ComparableDataType>`): `add/addAll` **reject
incompatible combinations**, but the only declared incompatibility is
`LOCAL_DATE_TIME` ⟷ `ZONED_DATE_TIME`. So **`[INTEGER, STRING]` is a valid,
storable polymorphic field** (common type `STRING`).

**Type discovery on ingest** (`entity/parsing/ParserFunctions.kt:118-178`,
`model/ValueDetectionFunctions.kt`): JSON numbers are bucketed by precision;
JSON strings are type-*discovered* in a fixed priority order
(UUID→TIME_UUID→LOCAL_DATE_TIME→ZONED_DATE_TIME→LOCAL_DATE→YEAR_MONTH→LOCAL_TIME→STRING).
**Numbers stored as JSON strings are deliberately not re-typed to numeric.**
Each ingested entity's discovered leaf types **merge** into the model's field
type set, so a field seen as both integer and string becomes `[INTEGER, STRING]`.

## 3. Storage: type-segregated value maps

An entity is a tree (`TreeNodeEntity` → `members: List<NodeInfo>`); each node's
`value` is a `PersistedValueMaps`
(`tree-node/.../model/treenode/PersistedValueMaps.kt:24-50`) — **one
`MutableMap<String, T>` per type**, keyed by the leaf's JSONPath:

```
strings, chars, doubles, floats, bytes, shorts, longs, ints,
localDates, localDateTimes, localTimes, zonedDateTimes, years, yearMonths,
bigDecimals, unboundDecimals, bigIntegers, unboundIntegers, booleans,
uuids, timeuuids,                       // range-indexable
@NoneRange byteArrays, others, nulls,   // not range-indexable
@NoneRange typeReferences: Map<JSONPath, DataType>   // "what type is this leaf"
```

**Write side** (`client/.../entity/parsing/JacksonParser.kt:307-330`): a leaf is
coerced to its model `DataType` (`value.coerceOrNull(fieldType.getPolymorphic())`),
then `typeReferences[path] = type` and `getTypeMap(type)[path] = typedValue`. An
optional **string-shadow** copy is written into `strings` when opted in.

**Read side** (`tree-node/.../search/TreeNodeEntityConditionProvider.kt:103-120`):
`getValuePathForValueMapKey(fieldPath, type)` builds a column path addressing
exactly one per-type slot, e.g. `('$.amount', INTEGER)` →
`members.[*]@…PersistedValueMaps.ints[$.amount]`. `DataType.getTypeMapVariableName`
is the exact inverse of the write-side `getTypeMap` — same type selects the same
map on both sides; the map key is the JSONPath.

**Meta/lifecycle fields are NOT in the value maps.** `id` (`UUID_TYPE`), `state`
(`STRING`), `creationDate`/`lastUpdateTime` (`ZONED_DATE_TIME`),
`transitionForLatestSave`/`previousTransition` (`STRING`), and the model-id are
**first-class typed columns** on the entity, monomorphic, addressed by identity
paths (`TreeNodeEntityConditionProvider.kt:34-101`). Temporal meta values are
compared as `java.util.Date`. A **model-id equality** (`Equals(metadataClassIdPath,
modelId, true)`) is AND-appended to every search, and the entity `id` is always a
returned column.

## 4. The wire contract

Schema: `client/src/main/resources/api/openapi-common.yml`. Polymorphic
`QueryCondition` with a **`type` discriminator** ∈ `{group, simple, lifecycle,
array, function}`. (tree-node search maps group/simple/lifecycle/array; `function`
is not handled there.)

- **group**: `{ "type":"group", "operator":"AND|OR|NOT", "conditions":[…] }` —
  nests arbitrarily.
- **simple** (data field): `{ "type":"simple", "jsonPath":"$.category",
  "operatorType":"EQUALS", "value": <any JSON scalar> }`. The operator property is
  `operatorType` (aliased `operation`). **`value` is any JSON scalar** (string /
  number / boolean / 2-element array for range) — it is coerced to a **string
  internally** via `.asText()` before type-parsing.
- **lifecycle** (meta): `{ "type":"lifecycle", "field":"state",
  "operatorType":"EQUALS", "value":… }` — `field` is one of the fixed lifecycle
  names, not a `jsonPath`.
- **array** (Trino-only): positional `value: List<String>`.

**Wire operators** (`OperatorType`, 28 exposed): `EQUALS, NOT_EQUAL, IEQUALS,
INOT_EQUAL, IS_NULL, NOT_NULL, GREATER_THAN, GREATER_OR_EQUAL, LESS_THAN,
LESS_OR_EQUAL, CONTAINS, NOT_CONTAINS, STARTS_WITH, NOT_STARTS_WITH, ENDS_WITH,
NOT_ENDS_WITH, ICONTAINS, ISTARTS_WITH, IENDS_WITH, INOT_CONTAINS,
INOT_STARTS_WITH, INOT_ENDS_WITH, MATCHES_PATTERN, LIKE, BETWEEN,
BETWEEN_INCLUSIVE, IS_UNCHANGED, IS_CHANGED`. (The internal `Operation` enum has
~69 members — `*_FIELD`, `REGEXP`, `IN_SET`, `INSTANCE_OF`, the `*_PATTERN`
family, etc. — but only these 28 are search-reachable.)

## 5. Operators: arity and semantics

Arity is classified by the `Predicate` table
(`tree-node/.../search/TreeNodeConditionUtils.kt:56-104`), the only
classification actually used:

- **Unary** (field only): `IS_NULL`, `NOT_NULL`.
- **Range/ternary** (field + two bounds): `BETWEEN` (both bounds **exclusive**),
  `BETWEEN_INCLUSIVE` (both **inclusive**).
- **Binary** (field + one operand): the six comparables
  (`EQUALS/NOT_EQUAL/GREATER_THAN/GREATER_OR_EQUAL/LESS_THAN/LESS_OR_EQUAL`), all
  string ops (`CONTAINS/STARTS_WITH/ENDS_WITH` + `NOT_*` + case-insensitive
  `I*`/`INOT_*`), `IEQUALS/INOT_EQUAL`, `MATCHES_PATTERN`, `LIKE`, and
  `IS_CHANGED/IS_UNCHANGED` (operand = a change-generation number).

Each `Operation` maps to a concrete condition class
(`getConditionForLeafField`/`getConditionForBinaryCondition`,
`TreeNodeConditionUtils.kt:112-266`) — `Equals`, `GreaterThanEquals`, `Contains`,
`IContains`, `Like`, `Matches`, `Between`, `IsNull`, … The **authoritative
matcher** is each class's `evaluateSimple` in `com.cyoda.core.conditions`:

**Equality / ordering** (`SimpleRangeCondition.evaluateSimple`):
- **null never matches** (condition-null or stored-null → false).
- same class → `compareTo`; both `Number` (different classes) →
  **lossless `BigDecimal`** comparison; both UUID → `TimeUUIDComparator` only when
  both are version-1, else natural; otherwise → false.
- `Equals` `==0`, `GreaterThan` `>0`, `GreaterThanEquals` `>=0`, etc.

**String ops** (case-**sensitive**): `CONTAINS` = `String.contains`
(**substring**), `STARTS_WITH` = `startsWith`, `ENDS_WITH` = `endsWith`;
`NOT_*` negate. Case-**insensitive** `I*` upper-case both sides
(`IEQUALS` = `equalsIgnoreCase`). All null-guard the stored value.

**LIKE** (`queryable/Like.java`): `%`→regex `.*?`, `_`→regex `.`; **all other
regex metacharacters are escaped** (treated literally); `\` is the escape char
(`\%`,`\_`,`\\`); **whole-string anchored, case-sensitive**. Range-index only for
a pure literal (`value` → equals) or a single trailing `%` (`value%` →
starts-with); anything else is a residual regex scan.

**MATCHES_PATTERN** (`nonqueryable/Matches.java`): Java `Pattern.matches` —
**whole-string anchored, case-sensitive**, no flags (use inline `(?i)`).

**BETWEEN** `from < v < to` (exclusive); **BETWEEN_INCLUSIVE** `from <= v <= to`.

**IS_NULL** = stored value absent/null; **NOT_NULL** = present and non-null.

## 6. The core: polymorphic type-directed expansion

For a binary/range leaf on a data field, `TDBValueMapCondition.convertBinaryCondition`
(`TDBCondition.kt:76-86`) calls
`parseToPolymorphicConditions(possibleTypes, valueString, operation)`
(`polymorphic/PolymorphicTypeConversions.kt:37-78`) then `.processResult(…)`.

**Step 1 — parse per declared type.** For each declared `DataType`, try
`parseStringOrNull(value)`. Types that parse directly become sub-conditions with
the *original* operation. Types that fail direct parse are bucketed into
numeric / temporal / other for conversion.

**Step 2 — numeric expansion** (`PolymorphicNumberConversions.kt`). If any
*numeric* declared type failed direct parse but the operand parses as a
`BigDecimal`, spread it across the numeric buckets. Per bucket, classify the
value vs the bucket's `[floor, ceiling]`:
- **IN_RANGE** → a typed condition; **floats** round imprecise values in the
  direction that preserves the operation (`>=`/`<` → CEILING, `<=`/`>` → FLOOR);
  **`EQUALS` on an imprecise value → dropped** (the bucket can't hold it).
- **ABOVE ceiling** + a less-than op → the bucket degenerates to `NOT_NULL`
  (every stored value of that type satisfies it); ABOVE + greater-than → dropped.
- **BELOW floor** + a greater-than op → `NOT_NULL`; BELOW + less-than → dropped.
- The `UNBOUND_*` bucket is verbatim (holds any value).

**Step 3 — temporal expansion** (`PolymorphicTemporalConversions.kt`). Convert a
parsed temporal seed to each unparsed temporal target via a resolution
down/upscale graph. Downscaling truncates (floor-like); to preserve set
semantics on an imprecise value it mutates the op (`>=`→`>`, `<`→`<=`); **`EQUALS`
on an imprecise value → dropped**. (`2024-09-09 >= ` on a `YEAR` slot becomes
`> 2024`.)

**Step 4 — combine / outcome** (`PolymorphicTypeConversions.kt:70-141`):
- **Success** (≥1 sub-condition): one leaf, or **`GroupCondition.or(…)`** across
  types, each on its type-specific path.
- **EmptyResult → `null` (void leaf)**: the operand *is* a number but no numeric
  bucket can match (e.g. `EQUALS` a fractional value against only integer types).
  In group composition, **OR drops a void child; AND is annihilated by it**
  (`TDBCondition.kt:33-44`).
- **Failure → 400**: the operand parsed into *no* declared type.
  `InvalidTypesInClientConditionException` (single type) or
  `InvalidPolymorphicTypesInClientConditionException` (multiple). Impossible when
  `STRING` is among the types (it parses anything).

**Evaluation** (`SimpleCondition.evaluate`): each OR-branch reads its
type-specific slot; a branch whose slot is **absent** (`!isColumnPathExists()`)
is simply false — so only the branch matching the entity's actual stored type can
match, harmlessly.

### Worked examples
- `[INTEGER, STRING]`, `EQUALS "30"` → `OR(ints[$.f]=30, strings[$.f]="30")`.
- `[INTEGER, STRING]`, `EQUALS "hello"` → `strings[$.f]="hello"` (INTEGER fails
  parse, contributes nothing; no 400 because STRING parsed).
- `[FLOAT, INTEGER]`, `GREATER_OR_EQUAL "12.78"` →
  `OR(floats[$.f]>=12.78, ints[$.f]>=13)` (CEILING).
- `[BYTE]`, `LESS_THAN "300"` → `NotNull(bytes[$.f])` (300 above BYTE range; every
  byte is `< 300`).
- `[YEAR, LOCAL_DATE]`, `GREATER_OR_EQUAL "2024-09-09"` →
  `OR(localDates[$.f]>=2024-09-09, years[$.f]>2024)`.
- `[STRING]`, `LIKE "foo%"` → `Like(strings[$.f], "foo%")` (starts-with range).
- `[INTEGER]`, `EQUALS "12.5"` → **void** (`EmptyResult`): OR drops it, AND kills
  the group. `[INTEGER]`, `EQUALS "abc"` → **400**.

## 7. Index vs residual

Each condition declares `canUseInRangeQuery()`/`canUseInIndexQuery()`. Only
range-capable ops push to the ordered (Cassandra) index: `Equals` (also
index-query), ordering, `StartsWith`, `Like`-as-prefix, `Between`. Every
**nonqueryable** op — `Contains`, all `I*`, all negations, `Matches` (regex),
`IsNull/NotNull` — cannot use the index and is a **residual post-filter** (scan
candidates and run `evaluateSimple`). The index narrows candidates; the per-class
`evaluateSimple` is the authoritative matcher.

## 8. Validation & error mapping

**There is no operator-vs-field-type validation.** (`supportedTypesMap` exists but
is referenced only by a test — dead in production, verified.) A predicate is
rejected only for **parse failure** or **arity**. So `CONTAINS 5` on a numeric
field is *accepted* (`"5"` parses to `5`, a `Contains(numericSlot, 5)` is built);
`GREATER_THAN true` on a boolean field is *accepted*. Rejection requires the
operand to parse into no declared type.

Handler: `TdbRestControllerAdvice` → RFC-7807 `application/problem+json`; a
`TdbException`'s `@ResponseStatus` sets the code, non-`TdbException` (e.g.
`IllegalArgumentException`) → **500 + ticket UUID**.

| Trigger | Exception | Status |
|---|---|---|
| `jsonPath` not in model | `InvalidJsonPathInClientConditionException` | 400 |
| Operand parses to no (single) field type | `InvalidTypesInClientConditionException` | 400 |
| Operand parses to none of a polymorphic field's types | `InvalidPolymorphicTypesInClientConditionException` | 400 |
| Binary/range op with `null` operand | `InvalidNullOperandsInClientConditionException` | 400 |
| Object value, or array where scalar required | `InvalidComplexTypeInClientConditionException` | 400 |
| Range op operand not a 2-element array | `InvalidArraySizeInClientRangeConditionException` | 400 |
| Model not found / not LOCKED | `EntityModelNotFoundException` | 404 |
| Report timed out | `InMemoryReportTimedOutException` | 408 |
| Unknown operator name / malformed body | Jackson → `HttpMessageNotReadableException` | 400 |
| Unknown lifecycle `field`; `NOT` on multi-cond group; `function` type | `IllegalArgumentException`/`UnsupportedOperationException` | 500 + ticket |

Lifecycle validation is the same parse-into-fixed-DataType check (e.g.
`creationDate` value must parse as `ZONED_DATE_TIME`; `id` as UUID; `state` is
`STRING` so any string passes).

## 9. Cloud quirks (do not blindly replicate)

The core-libs matcher has a few inconsistencies worth *not* copying verbatim; the
cyoda-go design should do the principled thing and note the divergence:
- **Two non-identical BETWEEN representations** — a real `Between` object
  (double-based bounds) vs an AND-of-two-comparisons (`createTDBCondition`,
  BigDecimal-based). High-precision numbers can disagree between them.
- **double vs BigDecimal** — single-value comparisons widen cross-type numerics
  losslessly via `BigDecimal`, but `BETWEEN` uses `double` (lossy for large
  long/BigInteger/BigDecimal).
- **UUID comparator** — single-value ops gate `TimeUUIDComparator` on version-1;
  `BETWEEN` does not.
- **`Matches` null handling** reaches a match via a caught NPE rather than a clean
  null branch (net effect: null → no match, like the others).

## 10. Summary for cyoda-go alignment

The alignment target is the **semantics**, since cyoda-go's storage (JSON
documents over memory/sqlite/postgres) can't literally be Cassandra type-maps:
1. Drive comparison from the model's declared per-leaf types (a set — polymorphic).
2. Treat the operand as a string; parse it into each declared type; build a
   same-type sub-condition per successful parse; OR them.
3. Match a sub-condition only against a stored leaf **of that type** (cyoda-go's
   analogue of the type-slot: the stored JSON value's own type, cross-checked
   against the model type).
4. Reproduce numeric bucket range/rounding and temporal resolution conversions
   (semantics-preserving op mutation; imprecise-`EQUALS` drop; out-of-range →
   `NOT_NULL`/drop).
5. Void (unsatisfiable-number) vs 400 (parses into no type): OR drops void, AND is
   annihilated; 400 only when nothing parses.
6. **No operator-vs-field-type validation** — reject only on parse failure and
   arity. (This reverses the operator-class-matrix idea from the earlier draft.)
7. Per-operator comparison semantics per §5 (same-type; null never matches
   ordering/equality; LIKE = anchored escaped glob, case-sensitive; CONTAINS =
   substring; BETWEEN exclusive, BETWEEN_INCLUSIVE inclusive).
