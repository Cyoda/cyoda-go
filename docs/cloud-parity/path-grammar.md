# Path grammar — Cloud twin-alignment spec

cyoda-go leads this contract. Cloud aligns to the same accept set, the same
addressing rules and the same error codes.

A `jsonPath` addresses data inside an entity. This document defines how a path
is written, what each form addresses, which surfaces accept which forms, how a
path is checked against the model, and how a path is represented inside the
engine.

`operator-semantics.md` defines what an operator means once a path is resolved.
The two documents divide as follows: this one is the left side of a predicate,
that one is the operator and the right side.

## 1. Field names

A field name is one or more of `A-Za-z0-9_-`, ASCII only. The empty name is not
a name.

A name may be entirely digits. `0` is a field name, and `{"obj":{"0":"Z"}}` is a
model with a field named `0` inside the object `obj`.

Model import holds a field name to this set, so every declared field is
addressable by a path. `model-field-name-grammar.md` defines that door.

## 2. The grammar

```
jsonPath  = "$." segment ( "." segment )*
segment   = name subscript*
name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
subscript = "[" ( "*" / 1*DIGIT ) "]"          ; the digit run must fit an int32
```

The `$.` leader is required. A path without it is not a path.

A positional index's digit run is bounded to what fits an `int32` — not a
Go `int` (`int64` on every supported platform), and deliberately narrower:
int32 is the intersection every in-tree backend can address a position
through. PostgreSQL renders a positional index as a jsonb operand, and
`jsonb -> bigint` does not exist — an index above `int32` fails to parse
rather than answering a result, which without a backend-specific error
classifier surfaces as an unclassified `5xx` instead of a `400`. No entity
array is ever long enough for a larger index to address a real position, so a
digit run that overflows is rejected, not truncated or wrapped.

Accepted, including every well-formed subscript and every chain of them:

```
$.amount            $.address.city      $.obj.0
$.tags[*]           $.tags[0]           $.tags[12]
$.tags[*].name      $.items[0].sku      $.orders[*].lines[*].sku
$.matrix[*][*]      $.matrix[0][1]      $.a[0][*].b
```

Rejected:

| Form | Examples |
|---|---|
| no `$.` leader | `amount`, `address.city` |
| leader only, or empty | `$`, `$.`, `` |
| empty or trailing segment | `$..a`, `$.a.` |
| bracket-quoted property access | `$['x']`, `$.['x']`, `$.a["b"]`, `$.a[0]['x']` |
| unclosed or unmatched bracket | `$.a[`, `$.a[0`, `$.a]`, `$.a].b` |
| no name before a subscript | `$.[0]`, `$.[*]` |
| empty subscript | `$.a[]` |
| negative or signed index | `$.a[-1]`, `$.a[+1]` |
| index too large to fit an int32 | `$.a[2147483648]`, `$.a[99999999999999999999]` |
| exponent index | `$.a[1e2]` |
| slice | `$.a[0:2]` |
| union | `$.a[0,1]` |
| filter expression | `$.a[?(@.x)]` |
| whitespace in a subscript | `$.a[ 0]`, `$.a[0 ]` |
| non-index chained subscript | `$.tags[*][x]`, `$.a[0][-1]` |
| any character outside the name set, including after a well-formed subscript | `$.a b`, `$.a;DROP`, `$.a/etc`, `$.xé`, `$.a[0]b`, `$.a[0];DROP`, `$.a[*]..b`, `$.a[*].` |

A rejected path is refused before anything executes. Section 9 gives the code
per surface.

There is no escape for a character outside the name set. A JSON key containing
one is legal JSON but is not addressable, which is why model import refuses such
a key rather than accepting data no query can reach.

Metadata is not addressed by a path. A `lifecycle` clause names a member of the
closed meta vocabulary directly.

`_meta` is not a declared field, so a data path spelling `$._meta.state` names a
field no model declares, and every condition surface rejects it
`400 INVALID_FIELD_PATH`.

A backend that keeps its own metadata inside the same stored document as domain
data holds the two apart. A data path resolves against domain data only. A model
that declares a field named `_meta` reads that field, never the engine's block.

## 3. What a path addresses

**The declared shape of the field decides what a path addresses. The shape of
the stored value never decides it.** Two entities of one model give the same
predicate the same meaning.

| Path | Addresses | Valid when the declaration has |
|---|---|---|
| `$.a` | the value at `a` | any branch |
| `$.a.b` | the value at `b` inside the object at `a` | an object branch |
| `$.a[*]` | every element of the array at `a` | an array branch |
| `$.a[0]` | the element at index 0 of the array at `a` | an array branch |

Every array hop counts, wherever it sits. The rule is about the values the path
addresses, not about where the brackets are:

| Path | Addresses |
|---|---|
| `$.tags[*]` | each element of `tags` |
| `$.matrix[*][*]` | each element of each inner array |
| `$.a[*].b[*]` | each element of each `b` |
| `$.orders[*].lines[*].sku` | each `sku`, across both hops |

A leaf on a path that addresses more than one value holds when **some** addressed
value satisfies it.

**A trailing wildcard never resolves to the array's length.** For
`{"tags":["red","blue"]}`:

| Condition on `$.tags[*]` | Result |
|---|---|
| `EQUALS "red"` | matches |
| `EQUALS "blue"` | matches |
| `EQUALS "purple"` | no match |
| `EQUALS 2` | **no match** — 2 is the length, not an element |
| `CONTAINS "lu"` | matches |
| `NOT_NULL` | matches |

### A dot and a number is a field name, not an index

The two forms address different things, and both are valid:

| Path | Addresses |
|---|---|
| `$.obj.0` | the field named `0` inside the object `obj` |
| `$.tags[0]` | element 0 of the array `tags` |

A field can be declared as an object branch and an array branch at once. Then
both paths above are valid statements about the same field, and each applies to
the entities whose data is that branch.

### Divergence from SQL/JSON `lax`

SQL/JSON `lax` mode routes on the data. It wraps a scalar so `$.a[*]` matches a
scalar value, and unwraps an array so a bare `$.a` compares against elements.

cyoda-go does neither.

## 4. Polymorphic fields — the union rule

A field is declared as a set of branches, because different entities of one
model may hold different shapes at one path.

**A path is accepted when it is a valid statement for at least one declared
branch. It is rejected only when it is valid for no declared branch.**

Per entity, the predicate applies to the branch that entity's data is. Where the
path is not a valid statement for that branch, the entity does not match. That is
a non-match, not an error.

For a field `a` declared `string | array-of-string`:

| Condition | `{"a":"A"}` | `{"a":["A","B"]}` |
|---|---|---|
| `$.a EQUALS "A"` | matches — valid for the string branch | no match — not a valid statement for the array branch |
| `$.a[*] EQUALS "A"` | no match — `[*]` is not a valid statement for the string branch | matches |
| `$.a NOT_NULL` | matches | matches — valid for both |

Neither condition is rejected. Each is valid for one branch.

A path observed as both an object and a bare scalar is searchable at that path
through its scalar branch. The entities holding an object stay reachable through
the child paths beneath it.

## 5. Vacuity — empty arrays, absent fields, absent keys

For a field `a` declared array-of-string:

| Stored | `$.a NOT_NULL` | `$.a IS_NULL` | `$.a[*] NOT_NULL` | `$.a[*] IS_NULL` | `$.a[0] IS_NULL` |
|---|---|---|---|---|---|
| `{"a":["A"]}` | true | false | true | false | false |
| `{"a":[]}` | **true** | false | **false** | **false** | true |
| `{"a":null}` | false | true | false | false | true |
| absent | false | true | **false** | **false** | true |

Three consequences follow, and each follows from section 3 rather than from a
special case.

**A bare path addresses the array itself, which exists when it is empty.** So
`$.a NOT_NULL` holds for `[]`.

**A wildcard path never answers the array's own nullness.** `$.a[*]` addresses
elements and nothing else. An empty array, an explicit `null` and an absent field
are three states of the array, and all three present no elements to a wildcard
path. Both presence tests answer false for all three. Ask about the array itself
with `$.a`, which separates them. On a wildcard path `IS_NULL` and `NOT_NULL` are
therefore **not** complements. They are complements only where at least one
element exists.

**A positional path differs from a wildcard path.** `$.a[0]` addresses exactly
one position, which may be absent. `$.a[*]` addresses a set, which may be empty.
An absent single value is null, so `$.a[0] IS_NULL` holds over `[]`. An empty set
has nothing to satisfy either test, so `$.a[*] IS_NULL` does not.

**Elements missing the key are evaluated, not dropped.** For `$.items[*].sku`
over `[{"sku":"A"},{}]`, the element without `sku` supplies an absent value, and
absent is null. So `IS_NULL` holds on that element.

## 6. Validity against the model

Grammar is checked without the model. Everything in this section is checked
against it.

**A path the model does not declare is rejected** `400 INVALID_FIELD_PATH`. A
positional path resolves against the wildcard key, because the model records an
array's element once, under `$.a[*]`, and has no per-index entry. `$.a[0]` is
that field.

**A container path carries no scalar comparison.** A path that is structural —
it has child fields and was never observed holding a bare scalar — is not
comparable to a scalar operand. A scalar comparison on it is
`400 INVALID_FIELD_PATH`. Navigate to a child path instead. The two presence
tests carry no scalar operand and stay valid on a container path.

The same rule decides a wildcard path over an array, at the `[*]` key:

| Declared element | `$.x[*]` with a scalar operand |
|---|---|
| array of scalars | valid |
| array of objects, also observed as a bare scalar | valid — the scalar occurrences are comparable |
| array of objects only | `400 INVALID_FIELD_PATH` — the element has substructure and no scalar form |

The two presence tests carry no scalar operand, so `IS_NULL` and `NOT_NULL` stay
valid on an array of objects. "Does some element exist and is it non-null" is a
meaningful question about an object.

**The model is required.** A condition addressing a data path is answerable only
against the schema that declares the field:

| State | Result |
|---|---|
| schema loads, path declared | the condition executes |
| schema loads, path not declared | `400 INVALID_FIELD_PATH` |
| model carries no schema | `400 INVALID_FIELD_PATH` — a model declaring nothing declares no path |
| schema cannot be loaded or parsed | `5xx` with a ticket id — never a result set |

A condition addressing no data path — one built only of `lifecycle` clauses —
takes its types from the static meta vocabulary and is answerable without the
schema. It is not failed when the schema cannot be loaded.

Answering without the schema is a wrong answer, not an unchecked one. With no
declared types, eight of the twenty-six operators — the comparison and ordering
operators — collapse to a non-match while the other eighteen keep matching. The
result set is short, and it looks complete.

Every surface applies one bounded schema refresh before it refuses, so a field a
peer node has just added is not falsely rejected by a node holding an older
cached schema.

## 7. Where each form is accepted

Three path surfaces exist. They accept different subsets of the grammar, and the
difference is deliberate.

| Surface | Field | Subscripts | Model check | Rejection code |
|---|---|---|---|---|
| search condition | `jsonPath` in a `simple` or `array` clause | yes | yes | `400 INVALID_FIELD_PATH` |
| workflow criterion | `jsonPath` in a `simple` or `array` clause | yes | grammar at import; model membership at evaluation | `400 VALIDATION_FAILED` (grammar, at import); `400 WORKFLOW_FAILED`, rolled back (undeclared field, at evaluation) |
| grouped-stats grouping | `groupBy` entry | **no** | yes | `400 INVALID_GROUP_BY_PATH` outside the grammar; `400 INVALID_FIELD_PATH` undeclared |
| grouped-stats aggregation | aggregation `field` | **no** | yes | `400 INVALID_AGGREGATION_FIELD` outside the grammar; `400 INVALID_FIELD_PATH` undeclared |
| sort key | `orderBy` path | **no** | yes | `400` — see `search-sort.md` |

The grammar check and the model check answer different codes on the two
grouped-stats surfaces. A path outside the grammar names the surface it was
written on; a well-formed path the model does not declare is an unknown field
like any other.

The three surfaces that reject subscripts use the grammar of section 2 with the
`subscript` production removed. There is one scanner, and it varies only on
whether that production is admitted. Two scanners drift, and a surface that
accepts a form no resolver serves answers an empty page for data that is present.

A grouping dimension or a sort key must name one scalar value per entity. A
wildcard path names a set, and a set has no single value to group or order by, so
the subscript forms are outside those surfaces.

**An array position is therefore not a grouping dimension, an aggregation field
or a sort key.** Those three surfaces admit no subscript, and a dotted numeric
segment is a field name. There is no other spelling for a position.

The grouped-stats validator does not rewrite a path. An accepted path is echoed
back verbatim in the response `groupKey[].path`, so a client reads back the
spelling it sent.

The reserved `groupBy` token `state` names the lifecycle state. It is a token,
not a path, and carries no leader. It is accepted for `groupBy` only. There is no
aggregate over lifecycle state, so `state` as an aggregation `field` is rejected
as a path with no leader.

Grouped stats holds three path surfaces in one request — the condition's leaves,
the `groupBy` entries and the aggregate fields. All three are checked. A partial
check returns an answer that looks real: an undeclared condition leaf yields no
buckets, an undeclared `groupBy` puts every entity under a `null` key, and an
undeclared aggregate reports a `null` total beside a correct count.

### A criterion's grammar is checked at import; its model membership at evaluation

A criterion is stored verbatim and read back on every entity write that
touches its transition, and a model may legitimately be declared after the
workflow that references it. Path grammar, operator names, lifecycle
type-soundness and pattern operands are therefore checked once, at import
(`400 VALIDATION_FAILED`), and not re-checked on a stored workflow: a
workflow carrying a path outside the grammar fails on its next import, and a
criterion naming a field the model has not yet declared is accepted at
import regardless.

**Model membership is checked at evaluation instead, not at import.** A query
never executes against a field the model does not declare — there is no such
thing as an undeclared field to query. So when a transition's criterion is
evaluated, the field it names must be declared by the model at that point;
if it is not, the save that triggered the evaluation is aborted and rolled
back with `400 WORKFLOW_FAILED` — no entity write, no state transition, no
partial effect. This applies to all 26 operators, not only the ones that need
a declared type. See `unevaluable-criterion-fails-save.md`.

A field that no entity has ever written must be declared explicitly through
`POST /model/import/...` — the model does not grow from a criterion alone. A
criterion on such a field is supported once the field is declared; declaring
it is the modelling step that makes it so. The evaluation-time check applies
one bounded schema refresh before it refuses, matching section 6, so a field
a peer node has just declared is not falsely refused by a node holding an
older cached schema.

## 8. Condition clauses

Five clause types exist. Two carry a path.

| Clause | Carries a path | Addresses |
|---|---|---|
| `simple` | `jsonPath` | one data path |
| `array` | `jsonPath` | one data path — see below |
| `group` | no | recursed into; every leaf beneath it is checked — operator `AND`, `OR`, or `NOT` (`NOT` recurses into exactly one child; see `negation.md`) |
| `lifecycle` | no | `field` names a member of the closed meta vocabulary |
| `function` | no | dispatched to a compute member |

### The `array` clause

An `array` clause tests elements of an array by position. Its `jsonPath` **must**
carry a trailing array wildcard, and the array's element must be a primitive:

```json
{"type":"array","jsonPath":"$.tags[*]","values":["A",null,"C"]}
```

Each non-null entry of `values` tests the element at that index. A `null` entry
tests nothing at that index. The clause above means: element 0 equals `"A"` and
element 2 equals `"C"`.

The engine reads the clause as an `AND` of positional comparisons:

```json
{"type":"group","operator":"AND","conditions":[
  {"type":"simple","jsonPath":"$.tags[0]","operatorType":"EQUALS","value":"A"},
  {"type":"simple","jsonPath":"$.tags[2]","operatorType":"EQUALS","value":"C"}
]}
```

The two forms are the same condition and answer identically. An `array` clause
whose `values` are all null tests nothing and matches every entity.

Because the clause is read as positional comparisons, every rule in this document
applies to it without exception — the model check of section 6, the union rule of
section 4, and the type check of `operator-semantics.md`.

A bare path (`$.tags`) is not accepted for an `array` clause. A bare path
addresses the array itself, not its elements, so it cannot carry a positional
test. An array of objects is not accepted either, for the reason section 6 gives:
an element with substructure and no scalar form is not comparable to a scalar.
Both are rejected with the code section 7 gives for the surface.

This restricts the `array` clause only. On a `simple` clause a bare path still
addresses the array itself, and section 6 decides what may be asked of it. On a
field declared only as an array that is the two presence tests and nothing else:
a scalar comparison on the bare path is `400 INVALID_FIELD_PATH`, because a
container has no scalar branch. Where the field carries a scalar branch as well,
a comparison is valid for that branch.

The bare path therefore stays accepted. Removing it would take `$.tags IS_NULL`
and `$.tags NOT_NULL` with it, and section 5 requires both.

## 9. The internal form

A storage plugin does not receive a `jsonPath`. It receives a **filter path**:
the same grammar with the leader removed.

```
filterPath = segment ( "." segment )*
segment    = name subscript*
name       = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
subscript  = "[" ( "*" / 1*DIGIT ) "]"          ; the digit run must fit an int32
```

`$.tags[0]` becomes `tags[0]`. `$.obj.0` becomes `obj.0`. An empty filter path is
legal and carries no field: `AND`, `OR` and `NOT` nodes hold one.

The magnitude bound on a positional index carries through unchanged: a digit
run that does not fit an `int32` is rejected here exactly as it is at the wire
boundary in section 2, because this is the same grammar with the leader
removed.

**The filter path keeps the distinction the wire form makes.** A bracket is an
array index. A dot and a number is a field name. A plugin that collapses the two
answers one of them wrongly, and no later re-check can recover it: a plugin's
query narrows the candidate set, so a row it drops is never re-examined.

Each backend renders each form in its own dialect:

| Filter path | SQLite | PostgreSQL |
|---|---|---|
| `obj.0` | `json_extract(data,'$.obj.0')` | `doc->'obj'->>'0'` |
| `tags[0]` | `json_extract(data,'$.tags[0]')` | `doc->'tags'->>0` |

The two renderings are not interchangeable in either dialect. A text key against
an array yields null, and an integer index against an object yields null.

**A backend rejects a filter path outside this grammar** with its
`ErrInvalidFilterPath` sentinel, which the engine reports as
`400 INVALID_FIELD_PATH`. A backend must not answer an empty result set instead:
a mistyped path and a predicate that genuinely matched nothing are different
answers.

The grammar is also the injection guard on SQL backends. Every character that
could terminate a quoted path literal is outside it. A backend that needs a wider
form widens this grammar; it does not bypass its own validator.

**Validation walks the whole tree, on any node carrying children — not a fixed
list of recognised branch operators.** A validator that recurses only into
`AND` and `OR` leaves a `NOT` node's child unvalidated, since `NOT` carries no
`Path` of its own. The consequence is worse than under `AND`: an unvalidated
malformed path resolves as a never-match leaf, and `NOT` inverts that into
matches-everything — a superset on search, and a destructive one on
`DELETE /entity/...`. The property that decides whether a node needs
recursing into is "this node has children", not which operator it is, so a
later branch operator is covered by construction rather than by remembering
to add it to a case list.

## 10. One resolver

**There is exactly one path resolver.** It reads the value or values a path
addresses out of one entity's data.

- A predicate's answer does not depend on which execution path served it.
  Pushing a condition into a backend query narrows the candidate set; it never
  decides the answer.
- A condition is answered identically whether or not some other leaf in the same
  condition can be pushed into a query. Whether a leaf can be pushed is a
  property of the query plan and carries no meaning.
- Two resolvers with different array handling is a defect. Two individually
  defensible resolvers are still a defect.

Three maps exist between the path forms, and there is exactly one of each:

| Map | From | To |
|---|---|---|
| wire → filter path | `$.tags[0]` | `tags[0]` |
| wire → model key | `$.tags[0]` | `$.tags[*]` |
| filter path → resolver syntax | `tags[0]` | the resolver's own spelling |

**What the boundary accepts is exactly what the resolver resolves.** The two
decisions consult one definition of a well-formed subscript. Written twice they
drift, and the boundary then admits a form the resolver copies through
unresolved — which answers an empty page for data that is present.

The model key map folds a positional subscript to the wildcard, because the model
records an array's element once. It is a lookup key only; every diagnostic names
the path the request actually sent.

## 11. Errors

| Code | Meaning | Surfaces |
|---|---|---|
| `INVALID_FIELD_PATH` | the path is outside the grammar, or the model does not declare it, or it is a container compared to a scalar | search condition, conditional delete, grouped-stats condition |
| `CONDITION_TYPE_MISMATCH` | the path is known; the operand does not fit its declared type | the same |
| `INVALID_GROUP_BY_PATH` | a `groupBy` entry is outside the grammar | grouped stats |
| `INVALID_AGGREGATION_FIELD` | an aggregation `field` is outside the grammar | grouped stats |
| `VALIDATION_FAILED` | a criterion path is outside the grammar | workflow import |
| `WORKFLOW_FAILED` | a criterion names a path the model does not declare, discovered at evaluation | the transition attempt that evaluates the criterion; rolled back |

A well-formed `groupBy` entry or aggregation `field` that the model does not
declare answers `INVALID_FIELD_PATH`, like any other unknown field.

`VALIDATION_FAILED` is the one code the workflow import endpoint answers for
every content rule, so `detail` locates the fault: it names the offending
workflow, state and transition. A caller distinguishing which rule failed reads
`detail`.

`INVALID_FIELD_PATH` and `CONDITION_TYPE_MISMATCH` stay distinct. A path outside
the grammar never names a field, so it is not a type mismatch.

Surfaces that carry a condition:

- `POST /search/direct/{entityName}/{modelVersion}`
- `POST /search/async/{entityName}/{modelVersion}` — a rejected submit issues no
  job
- `DELETE /entity/{entityName}/{modelVersion}` — the conditional delete
- `POST /entity/stats/{entityName}/{modelVersion}/query` — the `condition` field
- a workflow or transition `criterion`, checked at
  `POST /api/model/{entityName}/{modelVersion}/workflow/import`

**A backend's own rejection carries the same code.** A backend that refuses a
filter path answers its `ErrInvalidFilterPath` sentinel, and the engine reports
that as `400 INVALID_FIELD_PATH` rather than a `5xx` with a ticket id. The
classification applies on every route to the backend — the bounded search call
and the unbounded streaming drain alike. Classifying one route only makes the
answer depend on whether the request carried a positive `limit`.

**A path that is well formed but that a backend cannot translate is not an
error.** It is answered by the resolver instead. Only a path outside the grammar
is rejected. Refusing every translation failure turns working queries into `400`;
refusing none leaves the grammar unenforced.

**An async job fails rather than answering short.** A submitted job whose schema
becomes unreadable between submit and execution ends `FAILED`. It does not end
`SUCCESSFUL` with a page that is missing rows.

HTTP and gRPC funnel through one service boundary and reject identically. Over
gRPC a rejection from that boundary is an envelope error with `success=false` and
`error.code = CLIENT_ERROR`, the message carrying the code above. It is never an
empty stream, which a client reads as "no matches". A payload that cannot be
parsed at all is refused before the boundary and carries a bare gRPC status.

Workflow import is HTTP only. The gRPC CloudEvents surface carries entity-model
import, not workflow import.

## 12. Cloud obligations

Cloud implements every section above. Three points need implementing rather than
inheriting, because cyoda-go's in-tree backends satisfy them through shared code
that a self-executing backend does not run:

- **Path resolution, for every form.** A backend that executes a search itself
  resolves bare, positional and wildcard paths, and resolves them by the declared
  shape as section 3 requires.
- **The declared-type lookup**, including the fold from a positional path to the
  wildcard model key.
- **The field-existence check** of section 6, including the container rules.
- **The filter-path grammar**, including the bracket forms, the magnitude
  bound on a positional index, and the rejection sentinel of section 9.

The cross-backend parity suite runs against every backend wired into it, so a
backend that has not implemented these fails on its next dependency update rather
than diverging silently.

## Test surface

- The filter-path model and its resolver, in `cyoda-go-spi`: `filter_path_test.go`
  (the grammar — sections 1, 2 and 9), `filter_path_resolve_test.go` (`ResolvePath`
  against section 3's addressing table and section 5's vacuity table),
  `prepared_filter_test.go` and `condition_filter_test.go` (the `array` clause's
  desugaring into positional comparisons, section 8).
- The engine's own resolver, kept aligned to the SPI kernel it must agree with:
  `internal/match/resolver_parity_test.go` and
  `internal/match/prepared_equivalence_test.go`.
- Each plugin's path validator and query planner, against the grammar and the
  bracket-versus-dot rendering of section 9: `plugins/memory/path_validation_test.go`,
  `plugins/sqlite/path_validation_test.go`, `plugins/sqlite/query_planner_test.go`,
  `plugins/postgres/path_validation_test.go`, `plugins/postgres/query_planner_test.go`.
- The domain-layer surfaces of section 7 — search conditions, `groupBy`,
  aggregation fields and sort keys: `internal/domain/search/jsonpath_grammar_test.go`,
  `internal/domain/search/sortparam_test.go`,
  `internal/domain/search/sortkey_refresh_test.go`,
  `internal/domain/search/condition_type_validate_test.go`,
  `internal/domain/search/array_condition_validate_test.go`, and, for the
  workflow-criterion door of section 7's second row,
  `internal/domain/workflow/criterion_path_test.go`.
- Cross-backend parity, run through the full HTTP stack on every wired backend:
  `e2e/parity/path_addressing.go` (`RunArrayClausePositional`,
  `RunPathAddressingByDeclaredShape`, `RunPathVacuity`), registered in
  `e2e/parity/registry.go`.
- End to end, on a running backend: `internal/e2e/workflow_criterion_array_clause_test.go`
  (the trailing-`[*]` requirement at import) and `internal/e2e/criterion_prepare_test.go`
  (a criterion's path resolved the same way a search condition's is).
