# Condition `jsonPath` grammar — Cloud twin-alignment spec

cyoda-go leads this contract. A condition's `jsonPath` is JSON Path
nomenclature; the `$.` leader is **required**, and a path outside the grammar is
rejected **400 `INVALID_FIELD_PATH`** at the API boundary, before anything
executes. Cloud aligns to the same accept/reject set and the same error code.

This is a **wire-contract tightening**: input that works today stops working.

## The grammar

```
jsonPath = "$." segment ( "." segment )*
segment  = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
```

Rejected with 400 `INVALID_FIELD_PATH`:

- **no `$.` leader** — `amount`, `address.city`
- empty, `$` on its own, `$.` on its own
- an empty or trailing segment — `$..a`, `$.a.`
- **bracket-quoted property access** — `$['x']`, `$.['x']` (write `$.x`)
- any character outside the segment set — whitespace, quotes, `;`, `/`, `*`,
  `:`, `@`, `$` inside a segment, control bytes, every non-ASCII rune

**Accepted, and this is load-bearing:** an array-subscripted path
(`$.tags[*].name`, `$.arr[0]`) is valid JSON Path that simply cannot be
expressed as a pushdown filter. It must keep working — it is served by the
in-memory evaluator.

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

Project ruling: *"`variantId` isn't a JSON path and therefore simply should
error. We have a model syntax that is based on JSON Path nomenclature. Invalid
paths are invalid paths. We shouldn't try to accommodate incorrect syntax."*

## Where the rejection lives, and why it matters for Cloud

Two error classes must stay distinguishable, and conflating them is the
implementation trap:

| Class | Example | Translator error | Correct response |
|---|---|---|---|
| **Invalid path** | `amount`, `$['x']`, `$.a.` | wraps `spi.ErrInvalidFilterPath` | **400** |
| **Not pushdownable** | `$.tags[*].name` | plain error | **fall back**, answer 200 |

Rejecting *every* translate failure would turn working array-subscript queries
into 400s. Rejecting none leaves the tightening inert. cyoda-go therefore
validates at the single model-independent condition boundary
(`search.ValidateCondition`), which every search-shaped entry point funnels
through, rather than at the four separate translate sites; the boundary check
short-circuits on the first `[`/`]` exactly as the translator does, so the two
classify identically.

Surfaces covered, all four reaching a condition→filter translation:

- `POST /search/direct/…` (sync search)
- `POST /search/async/…` (async submit — a rejected submit issues no job)
- `DELETE /entity/{entityName}/{modelVersion}` (conditional delete)
- `POST /entity/stats/…/query` (the `condition` field)

HTTP and gRPC both funnel through the same service boundary, so both reject
identically. On gRPC the refusal is an envelope error (`success=false`,
`Error.Code = CLIENT_ERROR`, `INVALID_FIELD_PATH` in the message) — never an
empty stream, which a client would read as "no matches".

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

## Test surface

- `internal/domain/search/jsonpath_grammar_test.go` — reject table, positive
  control table, and `TestValidateCondition_PathGrammarMatchesSPI`, which
  cross-checks every entry against `spi.ConditionToFilter` itself so the
  boundary and the translator cannot drift apart.
- `internal/e2e/search_jsonpath_grammar_test.go` — status + error code over real
  HTTP on postgres for sync search, async submit and conditional delete, plus
  the array-subscript accept control and a nested-in-group case (the translator
  short-circuits on the first failing child, so a non-pushdownable sibling must
  not mask a malformed path behind it).
- `internal/grpc/search_jsonpath_grammar_test.go` — envelope shape for the
  direct and snapshot surfaces.
- `e2e/parity/search_path_key.go` —
  `RunSearchPathRequiresJSONPathLeader`, `RunSearchArraySubscriptPathStillServed`
  and `RunSearchPathTypeMismatch400`, registered in `e2e/parity/registry.go`.

The parity scenarios matter because a bare path is exactly the input that used
to differ per backend: whether pushdown was even attempted depends on the
backend and the query shape, and the fallback silently produced a different
plan. Rejecting at the boundary makes all backends agree by construction, and
`RunSearchPathTypeMismatch400` additionally pins that `INVALID_FIELD_PATH` and
`CONDITION_TYPE_MISMATCH` stay distinct codes — a bare path is not a type
mismatch, because the request never named a field.
