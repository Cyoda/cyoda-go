# Research: how type admission works, in Cyoda Cloud and in cyoda-go

Date: 2026-09-04. Factual current-state record — no design decisions here.
The design built on it is `../specs/2026-09-04-544-type-admission-design.md`.

Sources:
- Cyoda Cloud: `/Users/paul/dev/cyoda` @ `d5ce7c1c`.
- cyoda-go: `release/v0.8.4` @ `f5ce7de`.
- SPI: `github.com/cyoda-platform/cyoda-go-spi@v0.8.4-0.20260903130721-1d3b6ed501f0`.

Every "measured" line below was produced by running code against those trees.
Claims read from source but not executed are marked *read, not run*.

## 0. Vocabulary

- **Label** — the DataType assigned by inspecting only a value: the narrowest
  type containing it. cyoda-go: `schema.InferDataType`. Cloud:
  `String.dataTypeValueFromValue` / node-kind resolution.
- **Declared types** — the DataType set a model field records.
- Three moments, never conflated: **registration** (sample import, model
  unlocked), **ingestion** (entity write against a locked model), **search**.

## 1. Cloud's type model

A field's types live in `ModelDataType`, holding a plain `Set<DataType>`
(`client/.../structure/items/ModelDataType.kt:11`). `Polymorphic`
(`client/.../model/Polymorphic.kt:17`) wraps them in a `TreeSet<ComparableDataType>`
— a *sorted* set, ordered by `ComparableDataType.compareTo`.

`ComparableDataType.compareTo` (`client/.../model/ComparableDataType.kt:56-68`)
orders by widening commonality, with `STRING` as the maximum:

```kotlin
// Anything can be a String, so we are always lesser
if (other.type == STRING) return -1
if (this.type == STRING) return 1
val myMap = DataType.wideningConversionMap[type]
    ?: return this.type.toString().compareTo(other.type.toString())
return if (myMap.contains(other.type)) -1 else 1
```

`ComparableDataType` also declares exactly one incompatible pair —
`LOCAL_DATE_TIME` ↔ `ZONED_DATE_TIME` (`:35`, `:40`).

## 2. Where Cloud's widening map is actually used

`DataType.wideningConversionMap` (`client/.../model/DataType.kt:240-287`) has
three consumers, and **none of them is ingestion admission**:

1. `DataType.isAssignableFrom` (`DataType.kt:173-175`) — whose *only* call site
   in the whole repo is `Polymorphic.findCommonDataType` (`Polymorphic.kt:57`).
   Every other `isAssignableFrom` hit in the tree is `java.lang.Class`'s
   unrelated method.
2. `ComparableDataType.compareTo` (`ComparableDataType.kt:63`) — sort order only.
3. `DataType.findCommonDataType(other)` (`DataType.kt:293-310`) — referenced
   only from a test helper.

`Polymorphic.findCommonDataType()` collapses a set to one type and is reached
via `getHomomorphic()` from `ModelDataType.kt:49-50` and `ArrayModel.kt:167-168`.
Its call sites are **projection**: the Trino/SQL column type
(`TreeNodeCyodaEntityBusinessModelSupport.kt:277`, `TrinoSchemaGenerator.kt:125`)
and a last-resort accessor fallback (`TreeNodeEntityAccessor.kt:177`, used only
when the per-entity `typeReferences` entry is absent).

Note `isAssignableFrom` returns true whenever the target is `STRING`
(`this == STRING`), and `findCommonDataType` falls back to `STRING` when no
common type exists (`Polymorphic.kt:60-62`). That fallback is what
`docs/numeric-classification.md` records as a deliberate cyoda-go divergence.

## 3. Cloud's ingestion path — two gates

```
TreeNodeEntityService.parsedFlow
  → JsonParserService.parseToTreeNodeEntity(..., structureModel)   ← model reaches the PARSER
      → JacksonParser.handleDefault()                              ← GATE A: coerce into declared types
  → TreeNodeEntityService.completeAndValidate
      → TreeNodeEntityValidatorByModel.validateEntity
          → EntityStructureModel.applyLayerItem → … → field.merge  ← GATE B: set union + ChangeLevel
```

### Gate A — coerce into the already-declared types

`client/.../parsing/JacksonParser.kt:291-320`:

```kotlin
modelType?.let {
    val coerced = value.coerceOrNull(it)
    if (!typeExtensionAllowed && coerced == null)
        throw FoundIncompatibleTypeWitEntityModelException(newPath, it, value)
    else
        coerced
}
} ?: value.parseLeaf()
```

`ParserFunctions.kt:105-112`:

```kotlin
internal fun String.coerceOrNull(possibleTypes: Polymorphic): DataTypeValue<*>? {
    // It is assumed that the possible types are sorted according to their priority.
    for (possibleType in possibleTypes.getPossibleTypes()) {
        val parsedValue = this.parseToDataTypeValueOrNull(possibleType)
        if (parsedValue?.value != null) return parsedValue
    }
    return null
}
```

For a non-textual node (`ParserFunctions.kt:74-96`) it first looks for a declared
type equal to the node's own label, then falls back to parsing `asText()` against
each declared type in sorted order.

**Consequence:** a value that parses as an already-declared type takes that type,
is stored in that type's bucket, and the model never changes — no ChangeLevel
involved. `1000` into a `[DOUBLE]` field coerces via
`"1000".parseToDoubleOrNull()` and is accepted at any level. *Read, not run* —
the Kotlin tree was not executed.

`typeExtensionAllowed` (`JacksonParser.kt:54-55`) is true only at `TYPE` or
`STRUCTURAL`; below that, a value coercing to nothing is a hard
`FoundIncompatibleTypeWitEntityModelException` → HTTP 400.

### Gate B — set union, charged at TYPE

Only values Gate A could not coerce reach the merge.
`client/.../structure/items/EntityFieldModel.kt:85-95`:

```kotlin
val newFieldType = fieldType.merge(newField.fieldType)
if (fieldType != newFieldType) {
    recordChange(ChangeLevel.TYPE,"Extended $fieldType to $newFieldType")
    fieldType = newFieldType
}
```

`ModelDataType.merge` (`ModelDataType.kt:55-60`) is a plain set union with a NULL
carve-out — `this.types + other.types`. No widening, no collapse. The type set
only ever grows at ingestion.

## 4. Cloud's ChangeLevel

Same four values as cyoda-go (`ChangesRecordingStructure.kt:84-89`). The check is
`totalChange > ignoreBelow` (`:40-49`), i.e. the configured level is a **ceiling**;
`null` tolerates no change at all.

`recordChange` sites and what each gates:

| Level | Site | Change |
|---|---|---|
| `STRUCTURAL` | `EntityStructureModel.kt:81` | new node path |
| `STRUCTURAL` | `ObjectStructureNode.kt:77` | new field on an object (suppressed when the value is NULL) |
| `STRUCTURAL` | `ObjectStructureNode.kt:57`, `ArrayStructureNode.kt:39` | object↔array shape change |
| `TYPE` | `EntityFieldModel.kt:92` | new DataType on an existing scalar field |
| computed | `EntityFieldModel.kt:165`, `ArrayStructureNode.kt:54` | array spec, via `ArrayModel.analyzeChanges` (`:59-64`, `:144-149`): width only → `ARRAY_LENGTH`; layout, same type union → `ARRAY_ELEMENTS`; element types changed → `TYPE` |

## 5. Cloud's search

Established by a fresh-context sweep of `tree-node/.../search/`; call sites given,
*read, not run*.

- The condition builder takes the **full** declared set, not a collapse:
  `TreeNodeConditionUtils.kt:331` uses `getPossibleTypes()`.
- `PolymorphicTypeConversions.kt:37-78` parses the operand against **every**
  declared type with `DataType.parseStringOrNull` and `partition`s, keeping all
  that parse; for those that do not, it synthesises converted conditions —
  numeric range/rounding (`PolymorphicNumberConversions.kt`) and temporal
  up/down-scaling (`PolymorphicTemporalConversions.kt`).
- Result: an **OR of per-bucket predicates**, one per `ValueMaps` type map, with
  the bucket baked into the Cassandra column path
  (`TreeNodeEntityConditionProvider.kt:103-120`).
- `findCommonDataType` is **not** on this path. Trino predicate pushdown also
  uses the full set (`EntityStructureNode.kt:21` `getFieldTypesMap()`); only the
  declared *column type* is collapsed.
- `wideningConversionMap` is not consulted for stored-value matching; matching is
  exact-per-bucket, and cross-bucket reach comes from the operand conversions above.
- The string shadow (`alsoSaveInStrings`, `ParsingSpec.kt:27`) defaults **false**
  with no production caller setting it true, and only shadows values textual in
  the source JSON. So "every leaf is also stored as a string" does **not** hold.

## 6. cyoda-go current state — measured

### 6.1 Which mechanism each phase uses

| Phase | Mechanism | Widening applied |
|---|---|---|
| Registration | `Merge` → `TypeSet.Add` | numerics collapse; everything else accumulates |
| Ingestion (strict, no changeLevel; and PATCH) | `Validate` → `matchesScalarBranch` → `IsAssignableTo` | numerics only |
| Ingestion (changeLevel set) | `Extend` gate, then `Merge` | numerics only, then registration's rule |
| Search — operand admission | `ExpandLeaf` — parse-test | everything (parses/upscales) |
| Search — stored-side matching | numerics: `IsAssignableTo`; temporals: exact subtype; strings: lexical | numerics only |

Measured, one row per (declared, value):

```
declared          value              label            P1 import              P2 strict  P3 @ARRAY_LENGTH  P4 operand
DOUBLE            1000               INTEGER          [DOUBLE]               accept     [DOUBLE]          accept
DOUBLE            2147483648         LONG             [UNBOUND_DECIMAL]      REJECT     REJECT            accept
ZONED_DATE_TIME   "2026"             YEAR             [ZONED_DATE_TIME YEAR] REJECT     REJECT            accept
ZONED_DATE_TIME   "2026-03-01"       LOCAL_DATE       [LOCAL_DATE ZDT]       REJECT     REJECT            accept
LOCAL_DATE        "2026"             YEAR             [LOCAL_DATE YEAR]      REJECT     REJECT            accept
STRING            "2026-03-01"       LOCAL_DATE       [STRING LOCAL_DATE]    REJECT     REJECT            accept
STRING            5                  INTEGER          [INTEGER STRING]       REJECT     REJECT            accept
STRING            true               BOOLEAN          [STRING BOOLEAN]       REJECT     REJECT            accept
```

Two facts fall out. **Search's operand path accepts every row** that ingestion
refuses. And **registration accepts every row too** — a model built by importing
both `{"note":"hello"}` and `{"note":"2026-03-01"}` declares `{STRING, LOCAL_DATE}`
and accepts both forever; import only the first and the second is refused for the
model's life.

### 6.2 The label/value gap

`DOUBLE` is a decimal envelope — precision ≤ 15 significant digits, |scale| ≤ 292
(`spi/numeric.go`, `isDoubleEnvelope`), applied to whole numbers as well as
fractional (`parse_typed.go`, `parseDecimalType`).

```
literal                          label        value fits DOUBLE   label widens to DOUBLE
1000                             INTEGER      true                true
2147483647                       INTEGER      true                true
2147483648                       LONG         true                FALSE
9007199254740992                 LONG         false               false
9007199254740993                 LONG         false               false
123456789012345678901234567890   BIG_INTEGER  false               false
```

Same label, different answers. Over 89 values × 7 numeric targets: no case where
the label admits and the value test refuses; 56 the other way, confined entirely
to two targets — `DOUBLE` (26) and `BIG_DECIMAL` (30). For every other numeric
type the two tests are extensionally equal.

### 6.3 Search stored-side reachability

```
declared                  stored          operand       found
[DOUBLE]                  1000            1000          true
[ZONED_DATE_TIME, YEAR]   "2026"          2026          true
[ZONED_DATE_TIME]         "2026"          2026          FALSE
[ZONED_DATE_TIME]         "2026-03-01"    2026-03-01    FALSE
[STRING]                  "2026-03-01"    2026-03-01    true
[DOUBLE]                  2147483648      2147483648    FALSE
[BIG_DECIMAL]             1.5             1.5           FALSE
[BIG_DECIMAL]             12345678901234567890  …       FALSE
```

Rows 3–4: a model declaring only the wider temporal type cannot find a coarser
stored value — the stored side requires an exact subtype match
(`eval_leaf.go`, `evalCompare`'s String branch) while the operand side upscales.
Rows 6–8: the numeric band of §6.2, unreachable today.

### 6.4 The ZONED→LOCAL parse overlap

`spi/temporal_subtype.go`, `ParseTemporalSubtype(_, LocalDateTime)` tries
`time.RFC3339Nano` **first** and keeps only wall-clock fields (`wallOf` discards
the offset). Measured:

```
"2026-03-01T10:00:00+05:00"  parses as LOCAL_DATE_TIME = true
                             natural subtype           = ZONED_DATE_TIME
  as a stored value in a [LOCAL_DATE_TIME] leaf:
    EQ "2026-03-01T10:00:00"        found = false
    EQ "2026-03-01T10:00:00+05:00"  found = false
```

So `ParseTemporalSubtype` is **not** a safe "is this an instance of T" test for
temporals: it succeeds for a value whose natural subtype is a different type, and
silently drops the offset. `ClassifyTemporalString(s) == T` does not have this
property.

### 6.5 `Extend` merges the whole tree

`internal/domain/model/schema/extend.go`:

```go
changed, err := checkAndExtend(existing, incoming, level, "", spi.ChangeLevelType)
if !changed { return existing, nil }
return Merge(existing, incoming), nil
```

`Merge` runs on the **ingestion** path, over the entire walked document, whenever
any node changed — and it is label-only, the values being gone by then. Measured:
model `{x:[DOUBLE], y:[INTEGER]}` at `TYPE`, incoming `x`→`LONG`-labelled,
`y`→`DOUBLE`-labelled:

```
after Extend:  x = [UNBOUND_DECIMAL]   y = [DOUBLE]
Merge(DOUBLE, LONG) = [UNBOUND_DECIMAL]
```

Today this is harmless because the gate and `TypeSet.Add` agree exactly (verified
across all 724,201 declared × incoming pairs, both directions). It becomes
load-bearing for any value-aware gate.

### 6.6 Pushdown

- **postgres** (`plugins/postgres/query_planner.go`): the WHERE clause is built
  from the **operand**. `isNumericValue(f.Value)` → `cyoda_try_float8(doc->>'p')`
  compared as `float8`; otherwise text comparison. `->>` stringifies every stored
  value, so storage class never enters.
- **sqlite** (`plugins/sqlite/query_planner.go:396-401`): `comparisonBind` picks
  the bind type from the leaf's **declared** type family, because `json_extract`
  preserves the stored scalar's storage class and SQLite's `30 = '30'` is false.
  `:338` refuses polymorphic declared sets for that reason.
- Temporal *data* comparisons are not pushed on either backend
  (`isLeafPushable`); they go residual for the kernel's subtype resolution.
- Pushdown is a narrowing pre-filter; the kernel re-checks every returned row and
  is authoritative. The SQL must therefore be a superset.

The declared types touch pushdown in one place: `dataCoercion`
(`spi/condition_filter.go:393-405`) reads them to decide whether a leaf is
temporal, which is what routes it residual.

### 6.7 What consumes declared types elsewhere

Storage plugins are schema-blind — entity documents are opaque JSON/JSONB.
Unique-key signatures tokenise by the value's runtime Go type, not the declared
type. Exporters render the model only. The real consumers of the declared-type
gate are all reached through `spi.ConditionToFilter`'s `Declared` stamping:
search, delete-by-condition (`internal/domain/entity/service.go`), and workflow
criteria (`internal/match/prepared.go`). Two second-order spots: sort-class
derivation (`orderresolve.go`) feeding postgres's raw `::boolean` cast, and
`dataCoercion` above. *Agent-sourced sweep; call sites spot-checked, not run.*

## 7. Structural differences that matter

| | Cloud | cyoda-go |
|---|---|---|
| Stored form | value **converted** to a declared type, filed in a per-type `ValueMaps` bucket | raw JSON document; type re-derived at read time |
| Admission | parse/coerce against declared types (Gate A) | label compared against declared types |
| Ingestion merge | plain set union, only for values that coerced to nothing | eager numeric collapse via `TypeSet.Add` |
| Collapse to one type | at projection (`findCommonDataType`) | none — no projection step exists |
| Search matching | per-bucket, exhaustive OR over declared types | classify stored value, compare against declared types |

Because Cloud converts on the way in, it must choose one type when a value fits
several; because cyoda-go stores raw, it never chooses.

## 8. Defects observed in Cloud, for the record

Not cyoda-go's to fix; recorded because they bear on how far Cloud is a reference.

1. `ModelDataType.merge` unions raw `Set<DataType>` without going through
   `Polymorphic.add`, bypassing its `require(isCompatibleWith(...))`. A field
   observing both `LOCAL_DATE_TIME` and `ZONED_DATE_TIME` merges fine and then
   throws `IllegalArgumentException` from `getPolymorphic()` on read — surfacing
   as HTTP 500 via `TdbRestControllerAdvice.kt:127-138`. *Read, not run.*
2. `TreeNodeEntityService.kt:116` — `persistedModel?.allowedChangeLevel ?: MODEL_BY_ENTITY_CL`
   turns a persisted `null` change level (meaning "no changes allowed") into
   `STRUCTURAL` for the validator. Enforcement for such models rests entirely on
   the parser's narrower check. Verified in source.
3. `ChangeLevel`'s KDoc (`ChangesRecordingStructure.kt:76`) says "all levels above
   it in the enum are also allowed"; the code (`totalChange > ignoreBelow`) allows
   the named level and everything **below**. The doc is inverted.
4. `defaultDiscoverableTypes` (`ValueDetectionFunctions.kt:59-71`) tries
   `LOCAL_DATE_TIME` **before** `ZONED_DATE_TIME`, and `LocalDateTimeKSerializer`
   parses with `DateTimeFormatter.ISO_DATE_TIME`, which accepts an offset and
   discards it. So an offset-bearing timestamp classifies as `LOCAL_DATE_TIME`
   with the zone silently lost — the same hazard shape as §6.4, but in Cloud it
   is in the classification path. Verified in source.

## 9. Classification divergences, cyoda-go vs Cloud

From §6 measurements against `ValueDetectionFunctions.kt:59-71`:

- cyoda-go sniffs `YEAR` (`"2026"` → `YEAR`); Cloud does not list `YEAR` as
  discoverable.
- Cloud sniffs `UUID_TYPE` and `TIME_UUID_TYPE`; cyoda-go does not
  (a UUID string measures as `STRING`).
- Neither sniffs numbers out of strings — Cloud says why at
  `ValueDetectionFunctions.kt:71-77`.
- Neither sniffs `BYTE_ARRAY`; Cloud's list has it commented out explicitly.
