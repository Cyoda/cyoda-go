# Type admission on ingestion: judge the value, not its label

Status: proposed.

## 1. Terms

Three moments. This document never uses one to mean another.

- **Registration** — sample data is imported while the model is `UNLOCKED`.
  cyoda-go reads the values and records each field's declared types.
- **Ingestion** — an entity is written against a `LOCKED` model. The model's
  configured `changeLevel` is a ceiling on how much the model may change to
  accept the write. A model with no `changeLevel` may not change at all.
- **Search** — a query. Conditions are evaluated by the SPI kernel
  (`EvalLeaf`); SQL pushdown is a narrowing pre-filter whose rows the kernel
  re-checks.

Two more terms:

- **Label** — the DataType cyoda-go assigns by inspecting only the value: the
  narrowest type containing it. `1000` → `INTEGER`, `2147483648` → `LONG`,
  `"2026-03-01"` → `LOCAL_DATE`, `"hello"` → `STRING`. Computed by
  `schema.InferDataType`.
- **Declared types** — the DataType set a model field records.

`changeLevel` is a ceiling, not a budget (`levelPermits` is a static rank
comparison). Nothing is "consumed" or "spent".

## 2. The problem

Ingestion decides whether a value is acceptable by computing its **label** and
comparing that label to the field's **declared types**. The label is computed
without reference to the declaration, so the comparison answers a different
question from the one that matters. And the query side reads the same
literals by a third rule again. Three measured consequences.

**(a) A whole number is refused by a `DOUBLE` field.** Field declared `DOUBLE`,
write `1000`. `1000` is a valid double. Its label is `INTEGER`, and
`INTEGER ∉ {DOUBLE}`, so ingestion treats it as a type change and refuses it on
any model below `TYPE`:

```
400 VALIDATION_FAILED: change level violation:
    type change at .amount requires TYPE level, but level is "ARRAY_LENGTH"
```

**(b) A date-shaped string is refused by a `STRING` field.** Field declared
`STRING` (its sample value was `"hello"`), write `"2026-03-01"`. That is a valid
string. Its label is `LOCAL_DATE`, and `LOCAL_DATE ∉ {STRING}`, so it is refused
— including under strict validation, where no `changeLevel` exists at all:

```
400 INCOMPATIBLE_TYPE: validation failed:
    note: value of type LOCAL_DATE is not compatible with [STRING]
```

**(c) A query cannot find a value it wrote.** A leaf declares `INTEGER` and holds
`5`. `EQUALS 5` finds it; `EQUALS 5.0` does not. Ingestion strips trailing zeros
before classifying, so it read `5.0` as the whole number 5; the query side does
not strip, so it reads the same literal as fractional and drops the integer
comparison entirely.

(b) is the most severe of the three: it needs no `changeLevel` to reproduce, and
it rejects a string from a string field.

## 3. Three faces of one defect

The three failures in this document are the same mistake in three places: the
same value is judged differently depending on which moment does the judging.
They need different fixes, because the judgement goes wrong for different
reasons.

### (b) is value-independent

Every JSON string is a `STRING`, whatever it looks like inside. That is a fact
about the value's *kind*, not its contents. A kind-aware rule settles it, and it
needs no parse machinery and no change to search: `evalCompare`'s String branch
already engages for any stored string, so a date-shaped string in a `[STRING]`
leaf is findable today.

### (a) is value-dependent, and no label rule can decide it

Two values with the same label have different answers:

```
2147483648         label = LONG   valid DOUBLE?  yes   (10 significant digits)
9007199254740993   label = LONG   valid DOUBLE?  no    (16 significant digits)
```

`DOUBLE` is a precision envelope — precision ≤ 15 significant digits,
|scale| ≤ 292 (`spi/numeric.go`, `isDoubleEnvelope`) — and it applies to whole
numbers as well as fractional ones. Whether a whole number is a valid double is
therefore a property of the number.

A label rule has one answer per (label, declared type) pair; this needs two.
Adding `LONG → DOUBLE` to the widening lattice would admit `9007199254740993`
into a field that cannot represent it. Omitting it refuses `2147483648`, which
is a perfectly good double.

**The divergence is bounded.** Over 89 values × 7 numeric targets: no case where
the label admits and the value test refuses, and 56 the other way — all confined
to two targets, `DOUBLE` (26) and `BIG_DECIMAL` (30). For every other numeric
type the two tests give identical answers.

### (c) The query side and the ingestion side disagree about the same literal

Ingestion strips trailing zeros before classifying, so `5.0` is a whole number
and classifies `INTEGER`. The query side does not: `foldToInt` tests integrality
with `value.Scale() <= 0` on the unstripped decimal, so the operand `5.0` reads
as fractional. Under `EQUALS`, which is not an ordering operator, that drops the
entire integer family and the comparison matches nothing.

Measured, on a leaf declaring `INTEGER` holding the value `5`:

```
EQ 5      -> 1 hit
EQ "5"    -> 1 hit
EQ 5.0    -> 0 hits     wrong: 5.0 and 5 are the same number
```

This is the same defect as (a) and (b) — one literal, two answers, depending on
which side of the system reads it — and it contradicts cyoda-go's own rule that
classification is value-based and independent of spelling.

### The numeric collapse is not at fault

When a leaf must hold two numeric types, `TypeSet.Add` collapses them to one via
`CollapseNumeric`, widening as far as needed. That is deliberate: A.1's design
states the goal as "one collapsed numeric type per field, not a polymorphic
numeric set", accepts that the collapse is monotone-up-only, and defers
narrowing. No narrowing op exists in the catalog by construction, because the
catalog is widening-only to preserve validation-monotonicity (A.2 §I3).

The collapse behaves correctly. The defect is what the label test hands it:
writing `2147483648` to a `[DOUBLE]` leaf at `TYPE` level makes the gate report
that the field must now also hold `LONG`, and `Merge` then widens the leaf to
`UNBOUND_DECIMAL` — a correct response to a false premise. The field never
needed to hold a new type, because the value was already a double. Under §4 that
premise never arises, so the widening never happens.

The rule in §4 preserves A.1's invariants. **I4** requires the collapsed type to
represent every value observed at the field; a value admitted under §4 is an
instance of an already-declared type, so I4 holds unchanged. **I5** (cross-kind
polymorphism preserved) is untouched. **I3** monotonicity is untouched: §4 only
changes which values are admitted, never the direction in which a schema may
move.

## 4. The rule

> **A value is admitted at ingestion when its JSON kind matches a declared
> type's kind *and* the value is an instance of that declared type. When it is,
> the model does not change and the write is permitted at any `changeLevel`.
> When it is not, the model must change to hold the value; that change is
> recorded and permitted only at the model's configured level.**

The kind gate is normative. Per JSON kind:

| JSON kind | Admitted by a declared type T when |
|---|---|
| number | T is numeric **and the value fits T's envelope/range** |
| string | T is `STRING` (always), or T is temporal and the string's classification is exactly T |
| boolean | T is `BOOLEAN` |
| null | any T — the nullable marker, unchanged |

Only the number row is value-based. The string row's temporal case is a
classification test, for the reasons in §7.

The invariant:

> **Admitted with no model change ⟹ the declared types are byte-identical
> afterwards ⟹ the value is still findable under a declared type.**

### Case table

| Declared | Value | Admitted? | Result | Today |
|---|---|---|---|---|
| `DOUBLE` | `1000`, `1000.0`, `1e3` | yes | model unchanged | refused below `TYPE` |
| `DOUBLE` | `2147483648` | yes | model unchanged | refused below `TYPE`; widens the leaf at `TYPE`; needs §7 |
| `DOUBLE` | `9007199254740993` | no | model change, gated | gated — correct |
| `BIG_DECIMAL` | `1.5` | yes | model unchanged | refused; needs §7 |
| `STRING` | `"2026-03-01"` | yes | model unchanged | refused even strictly |
| `STRING` | `"hello"` | yes | model unchanged | admitted |
| `STRING` | `5` | no | model change, gated | gated — correct |
| `STRING` | `true` | no | model change, gated | gated — correct |
| `BOOLEAN` | `"true"` | no | model change, gated | gated — correct |
| `INTEGER` | `"2024"` | no | model change, gated | gated — correct |
| `INTEGER` | `13.111` | no | model change, gated | gated — correct |
| `ZONED_DATE_TIME` | `"2026-03-01T10:00:00Z"` | yes | model unchanged | admitted |
| `ZONED_DATE_TIME` | `"2026"` | no | model change, gated | gated — correct |
| `LOCAL_DATE_TIME` | `"2026-03-01T10:00:00+05:00"` | no | model change, gated | gated — correct, see §7 |

`STRING` admits any JSON *string*, because every JSON string is a string. A JSON
number or boolean is not a string, so it still costs a type change.

## 5. `Extend` stops merging the whole document

`extend.go` today:

```go
changed, err := checkAndExtend(existing, incoming, level, "", spi.ChangeLevelType)
if !changed { return existing, nil }
return Merge(existing, incoming), nil
```

`Merge` runs over the **entire** walked document whenever any node changed, and
it works from labels alone — by the time it runs, the values are gone. Measured:
model `{x:[DOUBLE], y:[INTEGER]}` at `TYPE`, write `{"x": 2147483648, "y": 1.5}`
→ `y` changes, `Merge` runs, and `x` becomes `UNBOUND_DECIMAL`, which the gate
never asked for. The same `x` value yields a different model depending on what
else was in the document.

**Required:** `Extend` builds its result from the gate's own per-leaf decisions,
merging only the leaves the gate ruled changed.

### What `Merge` is for afterwards

`Merge` remains the type-union engine everywhere the model genuinely must hold
both types. Its call sites after this change:

- `internal/domain/model/service.go:155` — re-importing a model while unlocked.
- `internal/domain/model/importer/sample_documents.go:44` — folding several
  sample documents into one model.
- `internal/domain/model/importer/walker.go:124` — unioning array element types
  *within* a single document.
- `internal/domain/model/schema/apply.go:79,174,181` — replaying a schema delta
  when folding the extension log.
- `merge.go:48,64` — its own recursion.
- The ingestion path, per-leaf, for leaves the gate ruled changed.

What it stops doing is being applied to a whole document on a verdict about one
leaf.

## 6. Polymorphic leaves

A leaf may declare several types, and a value may satisfy more than one of them.

At ingestion this does not matter: the rule asks whether the value is an
instance of *at least one* declared type. It is an existence question and
nothing is chosen. Because cyoda-go stores the raw document and derives types at
read time, no type has to be picked at write time.

At search it is already handled. `expandCompare` buckets the declared types and
builds one comparison per declared type, OR'd together. A stored value readable
as two declared types is tried as both, and any match wins.

## 7. Search

### What is already correct, and stays

The operand machinery — `ExpandNumericOperand` and the per-type folding,
range-classification and directional rounding beneath it — is mathematically
correct and is not being removed. It answers out-of-range and fractional
operands exactly as arithmetic requires. Measured:

```
[DOUBLE]  <  3e100   stored 10.5  -> true    every double is below 3e100
[DOUBLE]  >  3e100   stored 10.5  -> false
[DOUBLE]  == 3e100   stored 10.5  -> false
[INTEGER] <  3e100   stored 5     -> true
[INTEGER] >  -3e100  stored 5     -> true
[INTEGER] >  12.5    stored 13    -> true    integers above 12.5 are 13, 14, …
[INTEGER] >  12.5    stored 12    -> false
[INTEGER] == 12.5    stored 12    -> false   no integer equals 12.5
```

`>` floors the operand and `<` ceilings it, so the rounded bound admits exactly
the same values as the original. An operand beyond a type's magnitude window
becomes "everything in this bucket qualifies", or nothing. That design is also
what lets one logical predicate be expressed as one predicate per type bucket,
which a store with per-type index tables needs.

### Numbers — two changes

**(i) The stored-value filter.** Admitting `2147483648` into a `[DOUBLE]` leaf
stores the raw JSON. At search time the kernel derives that stored value's
**label** — `classifyStoredNumeric` gives `LONG` — then checks
`IsAssignableTo(LONG, DOUBLE)`, gets false, and does not match the row. The
kernel must judge a stored number the way ingestion does: against the declared
type's envelope. This applies to **both `DOUBLE` and `BIG_DECIMAL`**, in **both**
`evalCompare` and `evalBetween`. Measured today:

```
declared=[DOUBLE]      value=2147483648            admitted by value=true  findable=false
declared=[BIG_DECIMAL] value=1.5                   admitted by value=true  findable=false
declared=[BIG_DECIMAL] value=12345678901234567890  admitted by value=true  findable=false
```

Because numerics collapse, a leaf declares **at most one** numeric type, so this
filter is never choosing between buckets. It is only asking whether the stored
value belongs to the single declared numeric type — and under §4 the answer is
yes by construction.

**(ii) Operand normalisation — §2(c).** `foldToInt` decides integrality with
`value.Scale() <= 0` on the unstripped decimal, so `5.0` reads as fractional and
`EQUALS` drops the whole integer family. Strip trailing zeros first, as
`classifyNumber` already does on the ingestion side. One literal, one meaning.

### What must not change: the fail-closed gate

Two different gates live near each other and only the first moves.

- *Does this leaf declare a numeric type at all?* This decides whether a numeric
  comparison is built. **Keep it.** A leaf with no declared numeric type — an
  unresolvable path, say — must yield a non-match, not a comparison. Removing it
  would make an unresolvable path start matching rows, which is fail-open and
  contradicts `correctness-over-availability.md`.
- *Does the stored value belong to the declared numeric type?* This is (i), and
  it is the only one that changes.

### Temporal strings — no change, and why

cyoda-go has six temporal types: `YEAR`, `YEAR_MONTH`, `LOCAL_DATE`,
`LOCAL_TIME`, `LOCAL_DATE_TIME`, `ZONED_DATE_TIME`. A temporal value arrives as
a JSON string.

Ask whether `"2026-03-01T10:00:00+05:00"` is an instance of `LOCAL_DATE_TIME`.
There are two ways to answer, and they disagree.

**By classification.** Read the string on its own terms. It carries an offset,
`+05:00`, so it denotes an instant, and its type is `ZONED_DATE_TIME`. It is not
a `LOCAL_DATE_TIME`. Answer: **no**.

**By parsing.** Hand the string to a `LOCAL_DATE_TIME` parser and see whether it
succeeds. The SPI's `ParseTemporalSubtype` tries RFC3339 first and then keeps
only the wall-clock fields, discarding the offset. It succeeds, yielding
`2026-03-01T10:00:00`. Answer: **yes**.

The parsing answer is wrong to build on, for two separate reasons.

*It loses information.* The stored text still reads `+05:00`; the parse decided
it meant 10:00 with no zone. Those denote different instants — 10:00+05:00 is
05:00 UTC.

*It produces data nothing can find.* If a value like this were admitted free
into a `[LOCAL_DATE_TIME]` leaf, it would be stored raw, offset included. At
search time the stored side classifies it (the first method) and gets
`ZONED_DATE_TIME`, which is not the declared type, so no comparison engages.
Measured: neither `EQ "2026-03-01T10:00:00"` nor
`EQ "2026-03-01T10:00:00+05:00"` finds it.

Making the search side parse instead of classify does not rescue it — it makes
things worse. A query for `10:00` would then match a stored value that is
actually `05:00` UTC, because both sides would have discarded the offset. Two
different instants would compare equal.

So for temporal types, "is an instance of T" must mean **classify the string and
require the result to be exactly T**. That is what `ClassifyTemporalString`
does, and it is what the code already uses on both sides. The temporal part of
this design is therefore no change at all: `"2026"` is a `YEAR` and not a
`ZONED_DATE_TIME`; `"2026-03-01T10:00:00+05:00"` is a `ZONED_DATE_TIME` and not
a `LOCAL_DATE_TIME`. Both remain gated type changes, and the stored-side
exact-match rule stays as it is.

### Pushdown

Pushdown is a narrowing pre-filter: the kernel re-checks every row the SQL
returns and is authoritative, so the SQL only has to be a superset.

- **postgres** builds the WHERE clause from the *operand*. A numeric operand
  becomes `cyoda_try_float8(doc->>'path') = $1::float8`; anything else is a text
  comparison, and `->>` stringifies every stored value. `cyoda_try_float8`
  already parses `2147483648`, so the row is already returned and the SQL stays
  a valid superset as the kernel loosens. No migration.
- **sqlite** picks its bind type from the leaf's **declared** type family
  (`comparisonBind`), because `json_extract` preserves the stored value's
  storage class and SQLite's `30 = '30'` is false. The §4 kind gate is what
  keeps this sound: without it a `[STRING]`-declared leaf could hold both `"42"`
  and `42`, and sqlite would TEXT-bind and return one row where postgres returns
  two — an under-return the residual re-check cannot recover, and a backend
  divergence.
- Temporal data comparisons are not pushed on either backend
  (`isLeafPushable` refuses them) and stay residual.

## 8. Registration and ingestion answer different questions

Registration discovers types; ingestion checks against them. They therefore give
different declared sets for the same value, and that is the design:

- Registering `{"note":"hello"}` and then `{"note":"2026-03-01"}` yields
  `{STRING, LOCAL_DATE}`.
- Locking after `{"note":"hello"}` and then ingesting `"2026-03-01"` leaves
  `{STRING}`.

Both accept the value. The registered model additionally supports temporal
predicates on that field, because it declares `LOCAL_DATE` and search buckets
per declared type.

Registration is unchanged by this design. Absorbing `LOCAL_DATE` into `STRING`
at registration would remove temporal search from genuinely temporal fields.

## 9. Error and status codes

No new error codes. Cases move from error to success; every remaining error
keeps its code, message shape and Props.

| Endpoint | Scenario | Today | Under this rule |
|---|---|---|---|
| `POST /api/entity/JSON/{name}/{ver}` | value is an instance of a declared type, no `changeLevel` | `400 INCOMPATIBLE_TYPE` | **`200`** |
| `POST /api/entity/JSON/{name}/{ver}` | value is an instance of a declared type, level below `TYPE` | `400 VALIDATION_FAILED` | **`200`** |
| `POST /api/entity/JSON/{name}/{ver}` | value is an instance of none, no `changeLevel` | `400 INCOMPATIBLE_TYPE` | unchanged |
| `POST /api/entity/JSON/{name}/{ver}` | value is an instance of none, level below `TYPE` | `400 VALIDATION_FAILED` (level named) | unchanged |
| `POST /api/entity/JSON/{name}/{ver}` | value is an instance of none, level ≥ `TYPE` | `200`, model widened | unchanged |
| `POST /api/entity/JSON/{name}/{ver}` | wrong JSON kind for every declared type | `400` | unchanged — kind gate |
| `POST /api/entity/JSON/{name}/{ver}` | kind mismatch (array into scalar, …) | `400 VALIDATION_FAILED` | unchanged |
| `PATCH /api/entity/JSON/{id}` | as strict validation above | `400 INCOMPATIBLE_TYPE` | **`200`** when admitted |
| `POST /api/model/import/…/SAMPLE_DATA/…` | any | unchanged | unchanged |
| gRPC `EntityCreateRequest` | mirrors the HTTP rows | `CLIENT_ERROR` envelope | **`Success: true`** where HTTP becomes `200` |

Unique keys keep their guard: a write making a unique-key leaf non-scalar still
returns `422 INVALID_UNIQUE_KEY_DEFINITION`.

## 10. Coverage matrix

| Scenario | Unit | Running-backend e2e | Cross-backend parity | gRPC |
|---|---|---|---|---|
| whole number into `DOUBLE`, all four levels | ✓ | ✓ | ✓ | ✓ |
| `1000` / `1000.0` / `1e3` identical | ✓ | ✓ | ✓ | — |
| `2147483648` into `DOUBLE`: admitted, model unchanged | ✓ | ✓ | ✓ | ✓ |
| `2147483648` into `DOUBLE`: then found by search | ✓ | ✓ | ✓ | — |
| `1.5` into `BIG_DECIMAL`: admitted, then found | ✓ | ✓ | ✓ | — |
| `9007199254740993` into `DOUBLE` still gated | ✓ | ✓ | ✓ | — |
| `EQUALS 5.0` finds a stored `5` on an `INTEGER` leaf | ✓ | ✓ | ✓ | — |
| `EQUALS 12.5` still finds nothing on an `INTEGER` leaf | ✓ | ✓ | — | — |
| out-of-range operands: `< 3e100` all rows, `> 3e100` none | ✓ | ✓ | ✓ | — |
| an admitted write does not widen the leaf | ✓ | ✓ | ✓ | — |
| date-shaped string into `STRING`, strict | ✓ | ✓ | ✓ | ✓ |
| date-shaped string into `STRING`, each level | ✓ | ✓ | ✓ | — |
| date-shaped string into `STRING` then found by search | ✓ | ✓ | ✓ | — |
| JSON number into `STRING` still gated | ✓ | ✓ | ✓ | ✓ |
| JSON boolean into `STRING` still gated | ✓ | ✓ | ✓ | — |
| JSON string `"2024"` into `INTEGER` still gated | ✓ | ✓ | ✓ | — |
| `"2026"` into `ZONED_DATE_TIME` still gated | ✓ | ✓ | ✓ | — |
| `"…T10:00:00+05:00"` into `LOCAL_DATE_TIME` still gated | ✓ | ✓ | ✓ | — |
| polymorphic leaf, value satisfies two declared types | ✓ | ✓ | ✓ | — |
| polymorphic leaf, value satisfies none | ✓ | ✓ | — | — |
| array element: same rule at `ARRAY_ELEMENTS` | ✓ | ✓ | ✓ | — |
| unique-key leaf unaffected | ✓ | ✓ | — | — |
| sqlite pushdown superset with mixed-kind data | ✓ | ✓ | ✓ | — |
| postgres pushdown superset | ✓ | ✓ | ✓ | — |
| **property: admitted ⟹ declared types byte-identical, in a document that also carries a gated change in another field** | ✓ | ✓ | ✓ | — |
| **property: admitted ⟹ findable** | ✓ | ✓ | ✓ | — |

The last two are the design's invariant and must be property tests over the full
type × value matrix. The first must use documents that carry a gated change in
another field, or it passes vacuously against the §5 defect.

## 11. Work

**cyoda-go**
1. `Extend`: build the result from per-leaf gate decisions; stop merging the
   whole document (§5). Precondition for everything else.
2. One shared kind-gated admission predicate (§4), used by both
   `schema.Validate`'s leaf check and `schema.Extend`'s leaf gate, so the two
   write doors agree by construction.
3. Registration unchanged.
4. Docs: `cmd/cyoda/help/content/models.md`, `docs/numeric-classification.md`,
   `CHANGELOG.md`.
5. Gate 7: reconcile with cyoda-cloud and log the contract change in
   `docs/cloud-parity/`.

**SPI (`cyoda-go-spi`)**
6. `eval_leaf.go`: judge a stored number against the declared type's envelope
   for `DOUBLE` and `BIG_DECIMAL`, in both `evalCompare` and `evalBetween`.
7. Tag, one pin commit (`MAINTAINING.md`, `make repin-plugins`,
   `COMPATIBILITY.md`).

`IsAssignableTo` and the widening lattice stay where they are used legitimately:
merging types the model must genuinely hold both of, and `CollapseNumeric`.

## 12. Not changing

- Registration semantics (§8).
- The `changeLevel` contract: the model never changes without permission.
- The numeric collapse, its monotone-up-only direction, and the widening-only op
  catalog (A.1 §5, A.2 §I3).
- Temporal semantics on both the ingestion and search sides (§7).
- Raw-document storage. Values are never rewritten on the way in.

## 13. Status of the open question

The earlier open question was whether to take the SPI change at all, or confine
everything to cyoda-go and accept that `2147483648` cannot be written to a
`DOUBLE` field. Three findings have closed it.

- The operand machinery is mathematically correct, so this is a targeted change
  rather than a rewrite (§7).
- The change is one filter plus one normalisation, not a redesign of numeric
  comparison (§7 (i), (ii)).
- The out-of-tree Cassandra plugin does not use the SPI's numeric typing, so
  there is no cross-repo index breakage to design around.

What remains is sequencing, not design. The kind gate and §2(b) ship entirely in
cyoda-go; §7 needs an SPI tag, a pin commit and a plugin repin. §5 is required
before either. Splitting is possible and needs no rework, since the numeric arm
of the admission predicate is the only part that differs.

## 14. Adjacent, not in scope

The SQL pre-filter can drop rows the kernel would match — three defects, tracked
separately. They are in the pushdown layer, not in type admission, and neither
this design nor the current behaviour causes them. Fixing them is independent of
this work in both directions.
