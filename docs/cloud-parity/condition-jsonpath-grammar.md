# Condition `jsonPath` grammar — Cloud twin-alignment spec

cyoda-go leads this contract. A condition's `jsonPath` is JSON Path
nomenclature; the `$.` leader is **required**, and a path outside the grammar is
rejected **400 `INVALID_FIELD_PATH`** at the API boundary, before anything
executes. Cloud aligns to the same accept/reject set and the same error code.

This is a **wire-contract tightening**: input that works today stops working.

## The grammar

```
jsonPath  = "$." segment ( "." segment )*
segment   = name subscript*
name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
subscript = "[" ( "*" / 1*DIGIT ) "]"
```

Rejected with 400 `INVALID_FIELD_PATH`:

- **no `$.` leader** — `amount`, `address.city`
- empty, `$` on its own, `$.` on its own
- an empty or trailing segment — `$..a`, `$.a.`
- **bracket-quoted property access** — `$['x']`, `$.['x']`, `$.a["b"]`,
  `$.a[0]['x']` (write `$.x`)
- **a bracket spelling outside the two supported subscript forms** — unclosed
  or unmatched (`$.a[`, `$.a[0`, `$.a]`, `$.a].b`), no field name before it
  (`$.[0]`, `$.[*]`), empty (`$.a[]`), negative or signed (`$.a[-1]`,
  `$.a[+1]`), exponent (`$.a[1e2]`), slice (`$.a[0:2]`), union (`$.a[0,1]`),
  filter expression (`$.a[?(@.x)]`), whitespace inside (`$.a[ 0]`, `$.a[0 ]`),
  and a non-index chained subscript (`$.tags[*][x]`, `$.a[0][-1]`)
- any character outside the segment set — whitespace, quotes, `;`, `/`, `*`,
  `:`, `@`, `$` inside a segment, control bytes, every non-ASCII rune —
  **including one that follows a well-formed subscript** (`$.a[0]b`,
  `$.a[0];DROP`, `$.a[0]/etc`, `$.a[0].xé`, `$.a[*]..b`, `$.a[*].`)

**Accepted, and this is load-bearing:** a path with a **well-formed** array
subscript — the wildcard `[*]` or a non-negative index — is valid JSON Path
that simply cannot be expressed as a pushdown filter (`$.tags[*]`,
`$.tags[*].name`, `$.arr[0]`, `$.arr[12].a.b`, `$.matrix[*][*]`,
`$.matrix[0][1]`, `$.a[0][*].b`, `$.orders[*].lines[*].sku`). These must keep
working — they are served by the in-memory evaluator. Only *malformed* bracket
spellings are rejected; the accept set above is unchanged by this tightening.

The index arm delegates to the same `IsArrayIndex` predicate the in-memory
evaluator's own subscript rewriter consults, so "the boundary accepts it" and
"an evaluator resolves it" are the same question by construction. A second,
independent spelling of that test could admit a subscript the rewriter then
copies verbatim — which resolves to nothing, the exact failure this closes.

Metadata is not addressed through `jsonPath` at all: a `lifecycle` condition
names a member of the closed meta vocabulary directly and is not subject to this
grammar. A *data* path that happens to spell `$._meta.state` is an ordinary
dotted path and is accepted as one.

## Why: the rejection existed but was inert

The translator (`spi.ConditionToFilter`) already refused a bare path. It made no
difference, because every engine call site treats a translate failure as "not
pushdownable, fall back to in-memory evaluation" — and the in-memory evaluator
resolves a bare path happily.

So `{"jsonPath":"amount","operatorType":"GREATER_THAN","value":50}` returned
**200 with correct-looking results**, having silently abandoned the pushdown
plan for a full scan. The mistake never surfaced, and the cost was invisible.
Bracket-quoted access was worse: no evaluator in the stack resolves it, so it
answered an **empty page for a field that exists**.

Malformed *subscripts* were the same failure by a different route. The boundary
scan used to stop at the first `[` and accept the remainder unread, and
`ConditionToFilter` short-circuited identically — so `$.a[0:2]`, `$.a[-1]` and
`$.a[0];DROP` all classified as "unpushdownable" and fell through to the
in-memory evaluator, which resolves none of them. On the two surfaces with no
downstream schema backstop — a grouped-stats `condition`, validated against a
nil model, and workflow-criterion import, which is grammar-only — that surfaced
as a **200 with wrong buckets** and a **criterion that silently never fires**.
Both classifiers now scan the whole path.

Project ruling: *"`variantId` isn't a JSON path and therefore simply should
error. We have a model syntax that is based on JSON Path nomenclature. Invalid
paths are invalid paths. We shouldn't try to accommodate incorrect syntax."*

## Where the rejection lives, and why it matters for Cloud

Two error classes must stay distinguishable, and conflating them is the
implementation trap:

| Class | Example | Translator error | Correct response |
|---|---|---|---|
| **Invalid path** | `amount`, `$['x']`, `$.a.`, `$.a[-1]`, `$.a[0]b` | wraps `spi.ErrInvalidFilterPath` | **400** |
| **Not pushdownable** | `$.tags[*].name`, `$.arr[0]` | plain error | **fall back**, answer 200 |

Note which side a malformed subscript falls on: **invalid**, not
"unpushdownable". Only a *well-formed* subscript earns the fall-back.

Rejecting *every* translate failure would turn working array-subscript queries
into 400s. Rejecting none leaves the tightening inert. cyoda-go therefore
validates at the single model-independent condition boundary
(`search.ValidateCondition`), which every search-shaped entry point funnels
through, rather than at the four separate translate sites; the boundary scans
the whole path, subscripts included, exactly as the translator does, so the two
classify identically.

An engine-side companion matters as much as the boundary: a plugin's own
backstop returning `spi.ErrInvalidFilterPath` must map to **400
`INVALID_FIELD_PATH`**, not a 500 with a support ticket. cyoda-go does this in
`search.ClassifyStoreQueryError`, applied on *both* store branches — the
bounded `Search` call and the unbounded `Iterate` drain. Classifying only one
of them leaves malformed input answering 500 depending on whether the request
carried a positive `limit`.

Surfaces covered, all four reaching a condition→filter translation:

- `POST /search/direct/…` (sync search)
- `POST /search/async/…` (async submit — a rejected submit issues no job)
- `DELETE /entity/{entityName}/{modelVersion}` (conditional delete)
- `POST /entity/stats/…/query` (the `condition` field)

HTTP and gRPC both funnel through the same service boundary, so both reject
identically. On gRPC the refusal is an envelope error (`success=false`,
`Error.Code = CLIENT_ERROR`, `INVALID_FIELD_PATH` in the message) — never an
empty stream, which a client would read as "no matches".

## Path validation must run, or the request fails

Grammar is checked without the model; **existence** is checked against it. The
model's schema decides whether a condition's paths are real, so a schema that
cannot be loaded is not a reason to skip the check — it is a reason to fail the
request, per `.claude/rules/correctness-over-availability.md`.

- **Schema load fails** (model store unreachable, stored schema unparseable):
  **5xx with a ticket id.** Never a result set. An async job whose schema
  becomes unreadable between submit and execution ends `FAILED`, not
  `SUCCESSFUL` with a short page.
- **Model declares no fields:** every data path the condition names is unknown
  → **400 `INVALID_FIELD_PATH`**. A schema-less model is a model in which
  nothing is declared, not a model that accepts anything.
- **Lifecycle-only conditions are exempt.** A meta leaf takes its type from the
  static meta vocabulary, not from the model schema, so such a request is
  answerable without the schema and must not be failed when it cannot be
  loaded.

Answering without the schema is a *wrong* answer, not merely an unvalidated
one. With no fields map the translator stamps an empty declared-type set on
every leaf, which collapses the eight comparison and ordering operators to a
non-match while the other eighteen keep matching — so rows that should have
matched are dropped and the short page looks complete.

**Path shape is held to the model.** `$.items[*].sku` asserts `items` is an
array (or polymorphic with an array member); `$.items.sku` asserts `items` is
an object. The fields-map keys carry the `[*]` hops, so the two spellings are
distinct lookups and the one contradicting the model is rejected
`400 INVALID_FIELD_PATH`. A positional subscript canonicalises to the wildcard
key (`$.arr[0]` resolves against `$.arr[*]`) — see
`positional-subscript-path.md`.

### Where cyoda-go implements this today

Stated precisely, because a contract this document overstates is one Cloud
would build in more places than cyoda-go has it.

| surface | grammar | path existence | schema-load failure |
|---|---|---|---|
| `POST /search/direct/…` | yes | yes | 5xx |
| `POST /search/async/…` | yes | yes | 5xx (submit), job `FAILED` (execution) |
| `DELETE /entity/{entityName}/{modelVersion}` | yes | yes | 5xx |
| `POST /entity/stats/…/query` | yes | **no** | 5xx |

Grouped stats validates the condition's grammar, its patterns and its
model-independent operand types, but performs **no schema-membership check** —
on the condition, the `groupBy` paths or the aggregate fields. A condition
naming a field the model does not declare is accepted there and answered from a
filter whose comparison leaves annihilate. Cloud should not read the rows above
as licence to skip it; cyoda-go intends to close it, and the gap is recorded
here rather than papered over.

**Workflow criteria are out of scope of this document.** Criteria evaluate
through the in-process predicate evaluator, never through `ConditionToFilter`,
so a bare criterion `jsonPath` is unaffected by this change. They are covered by
their own tightening, at their own boundary — see
`workflow-criterion-jsonpath-grammar.md`, which rejects one at workflow import
with `400 VALIDATION_FAILED`.

## Caller migration

Add the leader; replace bracket access with dotted access.

```diff
- {"type":"simple","jsonPath":"amount",      "operatorType":"GREATER_THAN","value":50}
+ {"type":"simple","jsonPath":"$.amount",    "operatorType":"GREATER_THAN","value":50}

- {"type":"simple","jsonPath":"$['amount']", "operatorType":"GREATER_THAN","value":50}
+ {"type":"simple","jsonPath":"$.amount",    "operatorType":"GREATER_THAN","value":50}
```

Replace a malformed subscript with one of the two supported forms. There is no
rewrite for a slice, union, filter expression or negative index — no evaluator
in the stack ever resolved them, so a caller using one was already getting an
empty page; enumerate the elements, or use the wildcard and filter on the
result.

```diff
- {"type":"simple","jsonPath":"$.items[-1].sku", "operatorType":"EQUALS","value":"x"}
+ {"type":"simple","jsonPath":"$.items[*].sku",  "operatorType":"EQUALS","value":"x"}
```

## Test surface

- `internal/domain/search/jsonpath_grammar_test.go` — reject table (including
  the full malformed-bracket class), positive control table (including chained
  and nested well-formed subscripts), and
  `TestValidateCondition_PathGrammarMatchesSPI`, which cross-checks every entry
  against `spi.ConditionToFilter` itself so the boundary and the translator
  cannot drift apart.
- `internal/e2e/search_jsonpath_grammar_test.go` — status + error code over real
  HTTP on postgres for sync search, async submit and conditional delete, plus
  the array-subscript accept control and a nested-in-group case (the translator
  short-circuits on the first failing child, so a non-pushdownable sibling must
  not mask a malformed path behind it).
- `internal/grpc/search_jsonpath_grammar_test.go` — envelope shape for the
  direct and snapshot surfaces.
- `e2e/parity/search_path_key.go` —
  `RunSearchPathRequiresJSONPathLeader`, `RunSearchArraySubscriptPathStillServed`,
  `RunSearchPathTypeMismatch400` and `RunGroupedStatsPathRequiresJSONPathLeader`
  (the `groupBy`/`field` surface — `400 INVALID_GROUP_BY_PATH` /
  `INVALID_AGGREGATION_FIELD`, see `grouped-stats-path-grammar.md`).
- `e2e/parity/criterion_path.go` —
  `RunWorkflowCriterionPathRequiresJSONPathLeader` (workflow import, `400
  VALIDATION_FAILED` — see `workflow-criterion-jsonpath-grammar.md`) and
  `RunPositionalSubscriptPathResolves`, which is the accept side's teeth: a
  subscripted path is accepted here, so something must prove it then *resolves*
  rather than silently matching nothing (see `positional-subscript-path.md`).

All of the above are registered in `e2e/parity/registry.go`.

The parity scenarios matter because a bare path is exactly the input that used
to differ per backend: whether pushdown was even attempted depends on the
backend and the query shape, and the fallback silently produced a different
plan. Rejecting at the boundary makes all backends agree by construction, and
`RunSearchPathTypeMismatch400` additionally pins that `INVALID_FIELD_PATH` and
`CONDITION_TYPE_MISMATCH` stay distinct codes — a bare path is not a type
mismatch, because the request never named a field.
