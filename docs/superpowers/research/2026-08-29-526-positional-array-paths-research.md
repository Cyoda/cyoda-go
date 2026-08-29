# #526 — positional array paths: research

Factual record for issue #526 (row 9 of #516). Everything below was **measured**
on this branch (`c880cc9`, SPI pin `82d5096`), not inferred from reading code.
Written before any design so the ruling rests on facts rather than on the
issue's own premises — two of which turned out to be wrong.

## 1. What the mechanism is

`spi.arrayToFilter` (`condition_filter.go:322`) is the **only** producer in the
SPI of a `Filter.Path` containing a synthesised numeric segment:

```go
Path: fmt.Sprintf("%s.%d", basePath, i),
```

An `ArrayCondition{JsonPath: "$.tags", Values: ["A"]}` becomes
`Filter{Op: eq, Path: "tags.0", Value: "A"}`. `Filter.Path`'s grammar
(`filter.go:96-115`) sanctions this: *"an array position is addressed as an
ordinary numeric segment (`tags.0`), which is what ConditionToFilter produces
for an ArrayCondition."*

The grammar admits a numeric segment for **two different meanings** and marks
neither:

- an **array index** — synthesised by `arrayToFilter`;
- an **object key literally spelled `"0"`** — a legal model field name under
  `docs/cloud-parity/model-field-name-grammar.md` (`segment = ALPHA / DIGIT /
  "_" / "-"`), reaching the plugin from an ordinary `SimpleCondition`.

## 2. The two JSON dialects: measured

Neither SQL dialect has one expression that serves both meanings. They are
**mutually exclusive**, in both dialects, in the same direction.

### SQLite 3.51.0 (`/usr/bin/sqlite3`)

| expression | on `{"tags":["A"]}` | on `{"o":{"0":"A"}}` |
|---|---|---|
| `json_extract(…, '$.X.0')` (dotted) | `NULL` | `A` |
| `json_extract(…, '$.X[0]')` (bracket) | `A` | `NULL` |

### PostgreSQL 17.10 (`postgres:17-alpine`)

| expression | on `{"tags":["A"]}` | on `{"o":{"0":"A"}}` |
|---|---|---|
| `-> 'X' ->> '0'` (text key — what `jsonbExtractText` emits) | `NULL` | `A` |
| `-> 'X' ->> 0` (integer index) | `A` | `NULL` |

### gjson (the SPI kernel, and `internal/match`)

gjson resolves **both**, on the same dotted syntax:

| doc | path | result |
|---|---|---|
| `{"tags":["A"]}` | `tags.0` | `A` |
| `{"o":{"0":"A"}}` | `o.0` | `A` |
| `{"arr":[{"x":"A"}]}` | `arr.0.x` | `A` |
| `{"o":{"0":{"x":"A"}}}` | `o.0.x` | `A` |

gjson decides from the **data's shape**. That is precisely what
`path-grammar.md` forbids ("decided by the declared shape and never
by the stored value"), so the kernel is non-conformant here too — it just
happens to give the answer §10 wants in every case measured, because the
declared shape and the stored shape agree.

## 3. End-to-end reproduction

A throwaway parity scenario (logs, does not assert; registered, run, removed)
against a model whose sample is `{"name":"S","tags":["A","B"],"obj":{"0":"Z"}}`,
with one matching entity, via `POST /api/search/direct/`:

| condition | memory | sqlite | postgres |
|---|---|---|---|
| `{"type":"array","jsonPath":"$.tags","values":["A"]}` | **1** | **0** | **0** |
| `{"type":"array","jsonPath":"$.tags","values":[null,"B"]}` | **1** | **0** | **0** |
| `$.tags[0] EQUALS "A"` (SimpleCondition) | 1 | 1 | 1 |
| `$.tags.0 EQUALS "A"` (SimpleCondition) | 400 | 400 | 400 |
| `$.obj.0 EQUALS "Z"` (SimpleCondition) | 1 | 1 | 1 |
| `$.tags[*] EQUALS "A"` (SimpleCondition) | 1 | 1 | 1 |
| `$.name EQUALS "S"` (control) | 1 | 1 | 1 |

Two findings that the issue and #516 do not have:

### 3a. #526 is LIVE today, not dormant

#516 row 9 says the defect is *"dormant only because any subscript routes the
condition away from SQL"*. That is true of the **bracket** spelling
(`$.tags[0]`), which `ConditionToFilter` refuses with a plain error and every
call site reads as "evaluate in memory". It is **not** true of the
`ArrayCondition`, whose own `jsonPath` is `$.tags` and carries no subscript at
all: it translates cleanly, pushes down, and misses. Rows 1 and 2 of the table
above are a shipped under-select on both SQL backends today.

### 3b. The dotted spelling is already closed at the boundary

`$.tags.0` as a `SimpleCondition` is rejected `400 INVALID_FIELD_PATH` on all
three backends. `schema.CanonicalFieldPath` (`canonical_path.go:31`)
canonicalises **bracketed** subscripts only, so `$.tags.0` is looked up
verbatim; `FieldsMap` records an array's element under `$.tags[*]` and has no
`$.tags.0` entry, and no recorded leaf has `$.tags.0` as a prefix, so
`isPathKnown` reports it unknown.

Consequence: the numeric segment reaching a SQL planner is **unambiguous in
practice**, just not in the type. If it came through path validation it is an
object key; if it was synthesised by `arrayToFilter` it is an array index. The
information exists at the producer and is discarded by the representation.

### 3c. The object-numeric-key case works on all three backends today

`$.obj.0` matches everywhere. This is the fact that decides against the issue's
option (1).

## 4. Why the residual cannot recover it

`leafExact(FilterEq)` is false, so `planQuery` reinstalls the full filter as
`postFilter` and the kernel re-checks each candidate. But the WHERE clause
**narrows**: a row whose extract returned `NULL` never leaves the database, so
there is nothing to re-check. Over-selection is recoverable under the superset
contract; under-selection is not. `arrayToFilter` emits only `FilterEq`, so the
error is always in the under-select direction — never a wrong row returned,
always a right row withheld.

## 5. Assessment of the issue's two candidate fixes

### Option 1 — "each SQL plugin's `fieldExpr` renders a numeric segment as its dialect's array index"

**Not implementable as stated.** The issue frames the object-key case as a
"genuinely ambiguous corner needing a rule". §2's measurements show it is not a
corner: on both dialects the two renderings are mutually exclusive, and the
object-key case **works today on all three backends** (§3c). Rewriting every
numeric segment to a dialect index would flip `$.obj.0` from matching to
missing on sqlite and postgres — trading one under-select for a second one of
exactly the same shape, and breaking a case that currently has cross-backend
parity.

For a plugin to choose correctly it would have to know the **container's**
declared shape. It does not have it: `Filter.Declared` carries the *leaf's*
declared types (for `tags.0` those come from `$.tags[*]`, the element type), not
the container's kind, and a plugin has no route to the model schema — nor may it
acquire one (#477: no search path may materialise the model).

### Option 2 — "mark a path containing a numeric segment non-pushable, so it goes residual"

Strictly correct and immediately so, and §12 sanctions it (translatability is a
query-plan property and carries no semantics). Two variants, which the issue
does not separate:

- **2a, per-plugin** — each SQL planner treats a numeric-segment leaf as
  untranslatable. Every backend must implement it independently, the commercial
  backend included, and a backend that forgets is silently wrong. That is the
  heterogeneous landscape this project treats as a defect in itself.
- **2b, in the SPI** — `ConditionToFilter` refuses to translate an
  `ArrayCondition`, returning the plain (non-`ErrInvalidFilterPath`) error every
  call site already reads as "fall back to in-memory". One place, every backend,
  no per-plugin obligation.

**2b collides with row 13.** #477's acceptance criteria are that a translation
failure against a `Searcher`-implementing store becomes a `400`, and that no
search on the four in-house backends reaches the in-process fallback. Under
#477 as written, 2b would turn a legal `ArrayCondition` into a `400` rather
than a slower correct answer. So 2b is correct only until row 13, and row 13
would then have to reopen it.

### Option 3 — not in the issue: carry the subscript in `Filter.Path`

Widen the `Filter.Path` grammar to admit the bracketed positional subscript
(`tags[0]`), so the producer's knowledge survives to the plugin: a bracket is an
array index, a dotted numeric segment is an object key. Each dialect then has an
unambiguous rendering for both meanings, both are correct, and pushdown is kept.

Observations bearing on it, not conclusions:

- Row 11 (spi#32 + spi#46) must put `[*]` into the filter representation for the
  array-wildcard quantifier regardless. `[0]` and `[*]` are the same widening of
  the same grammar.
- `Filter.Path`'s grammar is also the SQL **injection guard** ("every character
  that could terminate a quoted JSON-path literal is outside it"). Admitting
  `[` and `]` widens the guard, so `validateJSONPath` in both plugins
  (`path_validation.go`) must be widened in lock-step and re-tested.
- It is an SPI change and therefore carries a commercial-backend obligation and
  a pin bump.

## 6. Surfaces the defect reaches

Every surface that translates a condition through `ConditionToFilter` against a
`Searcher` store, i.e. every one that accepts an `ArrayCondition`:

- `POST /api/search/direct/` and `/api/search/async/` (HTTP), and the gRPC
  equivalents;
- grouped stats (`grouped_stats_service.go:211`);
- conditional delete (`entity/service.go:1078`).

Workflow criteria are **always** served by `internal/match`, which resolves an
`ArrayCondition` positionally through gjson (`prepared.go:325`) and so matches.
A criterion and a search carrying the same `ArrayCondition` therefore disagree —
the §12 violation, on top of the backend divergence.

## 7. What is NOT wrong

- The wire grammar. `$.tags` with positional `values` is a coherent condition
  shape and is validated correctly.
- `internal/match`'s answer. It matches, on every backend.
- The memory plugin. It routes through the kernel and matches.
- `Filter.Path`'s narrowness as an injection guard. That property is sound and
  any widening has to preserve it deliberately.
