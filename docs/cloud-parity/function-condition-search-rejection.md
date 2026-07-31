# FUNCTION conditions are criteria-only — Cloud twin-alignment spec

cyoda-go leads this contract. A `{"type":"function", …}` clause is valid in a
**workflow or transition `criterion`** and invalid in a **search-shaped request
body**. Cloud aligns to that split.

## The rule

Every search-shaped entry point rejects a `function` clause, **at any nesting
depth**, with `400 INVALID_CONDITION`, before any store access:

- `POST /search/direct/{entityName}/{modelVersion}`
- `POST /search/async/{entityName}/{modelVersion}` — no job is created
- `POST /entity/stats/{entityName}/{modelVersion}/query`
- the conditional form of delete-by-model — no transaction is opened
- the gRPC direct-search and snapshot-search entry points, as a `success:false`
  envelope carrying the same domain code

Criteria are unaffected: `engine.evaluateCriterion` still intercepts a
whole-criterion `FunctionCondition` and dispatches it as an
`EntityCriteriaCalculationRequest`. A `function` clause nested inside a `group`
criterion is not dispatched and fails the evaluation — the interception only
sees the top level.

## Why the split is real, not an implementation gap

The `Condition` DSL is shared between search and criteria, which is why the
shape parses in both. Everything downstream of the parse is not shared:

- `predicate.FunctionCondition` carries **no fields** — `ParseCondition`
  discards `function.name` and `function.config`. The engine only works because
  it keeps the raw criterion bytes and re-parses them at dispatch time. A
  search evaluator holding the parsed value has nothing to route on.
- The predicate kernel is a pure, synchronous, in-process function with no
  `context.Context`. A function clause is a blocking network callout. Giving a
  leaf I/O capability changes the contract of every caller.
- `spi.Filter` has no callout op and `spi.Searcher.Search` takes only a
  `Filter`, so a function clause can never narrow a scan — it forces a full
  scan plus post-filter.
- `EntityCriteriaCalculationRequest` carries a single `entityId` and a single
  `payload`. There is no batch criteria shape, so evaluating one per candidate
  entity is unbounded fan-out against a compute member.

Accepting the clause and failing later is the one option ruled out: a
client-supplied condition must never produce a 5xx, and on the async path it
produced something worse — `200` plus a job that ended `FAILED` with no reason
surfaced on either poll.

## Contract surface

- `FunctionConditionDto` is removed from the request unions of the three
  search-shaped operations and retained in `TransitionDefinitionDto.criterion`
  and the workflow-level `criterion`.
- `cyoda help search` catalogues `FunctionCondition` under the shared DSL and
  marks it criteria-only; `cyoda help errors INVALID_CONDITION` names the
  rejection.

## Cloud action

Reject the clause at the same boundary with the same code rather than
attempting it. If Cloud today evaluates function predicates inside search, that
is a divergence to reconcile toward this document — including the fan-out and
batching questions above, which any future "function conditions in search"
design must answer before the split can be revisited.
