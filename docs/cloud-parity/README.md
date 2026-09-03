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
| `negation.md` | `NOT` as a group operator: the one-condition rule, the truth table, the universal-quantifier reading over a list, the absent-field and presence-test asymmetries, why `ALL(P)` is not offered, and the errors |
| `path-grammar.md` | How a `jsonPath` is written and what each form addresses. The grammar and its full reject set; bare vs `[*]` vs `[0]` vs a dotted numeric segment, decided by the DECLARED shape and never by the stored value (deliberate divergence from SQL/JSON `lax`); the union rule for polymorphic fields; vacuity over empty/null/absent arrays; validity against the model (container vs leaf, array-of-objects, schema required); which surfaces accept subscripts and which reject them (condition and criterion accept, `groupBy`/aggregate/sort do not); the `array` clause's bracketed path and its reading as positional comparisons; the internal filter-path form, which keeps the index-vs-name distinction and is also the SQL injection guard; and the single-resolver requirement |
| `operator-semantics.md` | What an operator means once a path is resolved. Type-directed same-type comparison and precise numerics; null and absent never match any binary operator including negatives; coarse temporal operands upscale; the 26-operator set and which eight need a declared type; operand shape, `BETWEEN` arity and pattern compilability; RE2-vs-Java regex divergence; pushdown narrows but never decides, and never under-selects; Cloud behaviours deliberately not ported |
| `function-condition-search-rejection.md` | `function` clauses are criteria-only: every search-shaped entry point rejects one at any depth with `400 INVALID_CONDITION`; `FunctionConditionDto` retained only in the workflow/transition `criterion` unions |
| `search-queue-full-backpressure.md` | Async submit sheds load with `503 SEARCH_QUEUE_FULL` (retryable) from either engine worker-pool saturation or the per-tenant in-flight cap; unreachable on a self-executing backend, which owes the equivalent bound under the same status/code |
| `direct-search-bounded-or-fail.md` | Direct search is bounded-or-fail on every backend: `limit` is required and positive, caps the matched set, and fails rather than truncates when exceeded. A non-positive `limit` never reaches a backend — the service rejects one as a caller error and a backend MUST reject one rather than re-defaulting it (it used to mean "unbounded"); `Offset` is removed; `MergePage` renamed `MergeBounded` |
| `unevaluable-criterion-fails-save.md` | A workflow criterion carrying an operator nobody can evaluate now fails the triggering save with `400 WORKFLOW_FAILED` and rolls back, instead of silently never firing for entities that short-circuit past it |
| `delete-not-converged.md` | Batched delete (`transactionSize`, no `pointInTime`) is capped at a fixed number of selection cycles and fails `409 DELETE_NOT_CONVERGED` (retryable) when matching entities keep being created; committed batches stay deleted; distinct from `CONFLICT` |
| `transaction-control-params.md` | `transactionTimeoutMillis`/`transactionSize`/search `timeoutMillis` now honored (opt-in); fictional spec defaults removed; new `408 TRANSACTION_TIMEOUT`/`SEARCH_TIMEOUT`; joined-request `400` rejection; batched-delete partial-commit + version-guard semantics; `EntityDeleteAllRequest.TransactionSize` becomes `*int` |
| `model-field-name-grammar.md` | A model field name must be spellable as a single wire-`jsonPath` segment (`A-Za-z0-9_-`, ASCII, non-empty, no `.`); enforced at the one walker both field-set-establishing paths share — sample-data import and ChangeLevel-driven schema extension on a write — with `400 VALIDATION_FAILED`. Strict validation and PATCH are unaffected; pre-existing non-conforming models are not migrated |
| `model-kind-enforcement.md` | A write whose value kind is outside the field's declared kind set is `400` on every ingress and at every depth when the model is fixed; with a `changeLevel` set it is a `STRUCTURAL` schema change (`null` exempt; polymorphic fields keep every declared kind); a sample-data array body is a collection of documents, and any other non-document body is `400 VALIDATION_FAILED`; `SIMPLE_VIEW`/`JSON_SCHEMA` name every declared branch, one `[*]` hop per array level |
| `validation-failure-code.md` | A payload that parses and then fails against the registered model answers `400 VALIDATION_FAILED`, not `BAD_REQUEST`, on every entity ingress; `BAD_REQUEST` keeps what the server cannot parse or whose parameters are wrong; `INCOMPATIBLE_TYPE` is unchanged |
| `like-glob-grammar.md` | `LIKE` is matched directly as a glob, not translated into a regex: no regex metacharacter is meaningful, `%`/`_` reach newlines, `\` escapes ANY following character, a trailing unpaired `\` is invalid, and literals compare bytewise. Also carries the **boundary contract**: a malformed `LIKE` or `MATCHES_PATTERN` operand is rejected up front (`400`), while the evaluator below it still treats one as a leaf that never matches — both halves are required. Pinned by `spitest`'s `Searcher/Pattern/LikeGrammar` + `MalformedLike`; a backend carrying its own LIKE→regex fork must adopt the kernel |
| `2026-08-22-async-ordering-and-list-order.md` | Async search results must come back in the requested `OrderBy` order (Cloud ignores it today — a tracked Cloud-side bug, not an accepted difference), and paged entity listing orders by the engine's own canonical entity-ID order, which is deliberately per-engine rather than cross-backend identical |
| `search-has-one-path.md` | A synchronous search reaches storage through exactly one call, `EntityStore.Search`; the whole-model read and the in-memory fallback are gone. A condition that cannot be translated is `400 INVALID_CONDITION` (`INVALID_FIELD_PATH` when path-shaped) rather than a request served by a different mechanism — on direct search, async submit, conditional delete and grouped statistics alike, which each kept a fallback of their own. A nil condition is refused rather than read as match-all, and async submit refuses at submission what its executor could not run |
| `grouped-stats-iterate-required.md` | Grouped statistics never answer `501 NOT_IMPLEMENTED_BY_BACKEND`; the code is retired. `EntityStore.Iterate` is required of every backend, so the endpoint always has an execution path — `GroupedAggregator` pushdown when offered and accepted, otherwise a streamed tally |
