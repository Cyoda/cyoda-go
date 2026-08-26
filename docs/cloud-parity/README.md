# docs/cloud-parity — Cloud-facing implementation contracts

cyoda-go **defines** the API and integration contract; Cyoda Cloud follows and
mirrors it. This folder holds the Cloud-facing implementation specs — contracts
written precisely enough for the Cyoda Cloud team to implement twin-alignment.

There is no shared ticketing system across the two streams yet; this folder is
the interim coordination surface. Each file in this folder describes a feature
or behaviour that Cloud must implement to stay aligned with cyoda-go.

## Relationship to cloud-divergences.md

`../cyoda/cloud-divergences.md` is the **inverse vector**: fields cyoda-go
*declares* in its OpenAPI spec but does not yet implement on the OSS server side.
This folder is the opposite direction — behaviour cyoda-go *has implemented* that
Cloud needs to adopt.

## Contents

| File | Covers |
|---|---|
| `entity-patch.md` | PATCH single-entity contract (RFC 7386 merge patch) |
| `openapi-conformance.md` | OpenAPI operation status, live common ground, tolerant-reader obligation, deferred open questions (E6, D2) |
| `search-sort.md` | Search result sorting — HTTP `sort` grammar, gRPC `orderBy`, canonical ordering semantics |
| `processor-criteria-annotations.md` | Processor `annotations` + workflow/transition `criterionAnnotations`, well-known renderer keys, schema 1.1 → 1.2 |
| `processor-attach-entity-default.md` | Processor `config.attachEntity` defaults to `true` on import, aligning with the function callouts |
| `nested-join-tx-serialisation.md` | Per-tx gate must release across external dispatch so depth-2+ nested joined cascades commit atomically instead of deadlocking |
| `criterion-stoppage-reason.md` | Criteria `reason` field: 400-response delivery + durable audit `data.reason` on automated/skip paths |
| `scheduled-transitions.md` | Scheduled-transition runtime: arm/cancel atomicity, one-shot criterion, grace-band expiry, explicit-fire reject, audit events, settled-interval reset |
| `tx-aware-search.md` | In-transaction `Search` is RYW-correct pushdown (no full-model fallback); the `pointInTime` carve-out, where the ambient transaction is ignored and only committed state is returned; `trackingRead` opt-in read-set recording; tx-owner co-location requirement |
| `authcontext-attribution.md` | AuthContext `authtype`/`authid`/`authclaims` contract (`service_account` retired → `service`); attributed/executor pair on change history; attribution semantics for cascade/scheduled/CBD-detached follow-on actions |
| `431-search-semantics.md` | Type-directed same-type predicate comparison, precise numerics; deliberate divergences (negative-op null handling, mixed object-or-scalar field searchability, meta-temporal coarse-operand upscale); Cloud quirks not replicated (BETWEEN representations/widening/UUID comparator, `Matches` NPE); accepted RE2-vs-Java regex divergence; `searchInStrings` out of scope; pushdown EXACT/SOUND-SUPERSET contract |
| `function-condition-search-rejection.md` | `function` clauses are criteria-only: every search-shaped entry point rejects one at any depth with `400 INVALID_CONDITION`; `FunctionConditionDto` retained only in the workflow/transition `criterion` unions |
| `search-queue-full-backpressure.md` | Async submit sheds load with `503 SEARCH_QUEUE_FULL` (retryable) from either engine worker-pool saturation or the per-tenant in-flight cap; unreachable on a self-executing backend, which owes the equivalent bound under the same status/code |
| `direct-search-bounded-or-fail.md` | Direct search is bounded-or-fail on every backend: positive `limit` caps the matched set and fails rather than truncates when exceeded; non-positive `limit` is unbounded and must not be re-defaulted by a plugin; `Offset` is removed; `MergePage` renamed `MergeBounded` |
| `unevaluable-criterion-fails-save.md` | A workflow criterion carrying an operator nobody can evaluate now fails the triggering save with `400 WORKFLOW_FAILED` and rolls back, instead of silently never firing for entities that short-circuit past it |
| `delete-not-converged.md` | Batched delete (`transactionSize`, no `pointInTime`) is capped at a fixed number of selection cycles and fails `409 DELETE_NOT_CONVERGED` (retryable) when matching entities keep being created; committed batches stay deleted; distinct from `CONFLICT` |
| `transaction-control-params.md` | `transactionTimeoutMillis`/`transactionSize`/search `timeoutMillis` now honored (opt-in); fictional spec defaults removed; new `408 TRANSACTION_TIMEOUT`/`SEARCH_TIMEOUT`; joined-request `400` rejection; batched-delete partial-commit + version-guard semantics; `EntityDeleteAllRequest.TransactionSize` becomes `*int` |
| `grouped-stats-path-grammar.md` | Grouped-stats `groupBy` path and aggregation `field` validated at the API boundary against the JSON Path grammar (`$.` leader required; bracket forms and array subscripts rejected); `400 INVALID_GROUP_BY_PATH` / `INVALID_AGGREGATION_FIELD` replace a backend-dependent `500`-or-wrong-`200` split |
| `condition-jsonpath-grammar.md` | A condition's `jsonPath` must be JSON Path — `$.` leader required, bracket-quoted access rejected — `400 INVALID_FIELD_PATH` on search, async search, conditional delete and grouped-stats `condition`; array-subscripted paths stay served by the in-memory fallback |
| `workflow-criterion-jsonpath-grammar.md` | The same grammar now governs a workflow/transition `criterion` `jsonPath` (`simple` and `array` clauses, any nesting depth), rejected at workflow import with `400 VALIDATION_FAILED`; array subscripts stay valid because criteria evaluate in memory |
| `model-field-name-grammar.md` | A model field name must be spellable as a single wire-`jsonPath` segment (`A-Za-z0-9_-`, ASCII, non-empty, no `.`); enforced at the one walker both field-set-establishing paths share — sample-data import and ChangeLevel-driven schema extension on a write — with `400 VALIDATION_FAILED`. Strict validation and PATCH are unaffected; pre-existing non-conforming models are not migrated |
| `positional-subscript-path.md` | A path addressing one array element by position (`$.arr[0]`) must resolve to that element's value on search, conditional delete, grouped-stats `condition` and workflow criteria; declared type comes from the `$.arr[*]` schema entry, so a mistyped operand is `400 CONDITION_TYPE_MISMATCH` rather than an empty page. A self-executing backend owes its own implementation |
| `like-glob-grammar.md` | `LIKE` is matched directly as a glob, not translated into a regex: no regex metacharacter is meaningful, `%`/`_` reach newlines, `\` escapes ANY following character, a trailing unpaired `\` is invalid, and literals compare bytewise. Also carries the **boundary contract**: a malformed `LIKE` or `MATCHES_PATTERN` operand is rejected up front (`400`), while the evaluator below it still treats one as a leaf that never matches — both halves are required. Pinned by `spitest`'s `Searcher/Pattern/LikeGrammar` + `MalformedLike`; a backend carrying its own LIKE→regex fork must adopt the kernel |
| `trailing-wildcard-path.md` | A path whose last hop is an array wildcard (`$.tags[*]`, `$.orders[*].lines[*].sku`) addresses the array's ELEMENTS, so a leaf on it holds when some element satisfies it — it used to resolve to the array's length. Existential, so vacuously false on an empty array; `400 INVALID_FIELD_PATH` when the element is a pure object. A self-executing backend owes its own implementation |
