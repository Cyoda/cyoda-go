# Function conditions in criteria, and the function pre-check

Issue #628. Tracker #651, row 7. Base: `release/v0.9.0`.

## 1. Terms

- **Criterion** — the `criterion` member of a workflow or of a transition. The engine evaluates it to select a
  workflow, or to allow a transition.
- **Function condition** — `{"type":"function","function":{"name":…,"config":{…},"criterion":…}}`. Evaluating it
  makes a callout to a compute member (`EntityCriteriaCalculationRequest`); the member's answer is its value.
- **Pre-check** — the `function.criterion` member of a function condition.
- **Local condition** — a `simple`, `lifecycle` or `array` condition, or a `group` whose members are all local
  conditions. The engine evaluates a local condition itself, without a callout.

## 2. Rules

**Rule 1 — the pre-check.**

- A pre-check contains only local conditions. A function condition anywhere in a pre-check, at any depth, is
  refused at import.
- The engine evaluates the pre-check before the callout:
  - the pre-check does not match → the function condition is false, and no callout is made;
  - the pre-check matches → the callout is made, and the member's answer is the function condition's value;
  - no pre-check (member absent or `null`) → the callout is made.
- Every local condition type is accepted in a pre-check.

**Rule 2 — the criterion.**

- A criterion may contain function conditions at any depth: as the whole criterion, or as members of `AND`, `OR`
  and `NOT` groups, nested to any depth.
- Every function condition in a criterion may carry a pre-check. Rule 1 applies to it.

**Rule 3 — evaluation of a criterion.**

1. Before the first callout, the whole criterion is checked and compiled: every local condition in it and in every
   pre-check, and every group that contains a function condition. A fault anywhere fails the operation, also in a
   member that evaluation would not reach.
2. Group members are evaluated in declaration order. Evaluation of a group stops as soon as its value is known:
   `AND` at the first false member, `OR` at the first true member. `NOT` has exactly one member and inverts it.
3. A function condition that is reached is evaluated by Rule 1.
4. A callout that fails fails the operation. The first failure in evaluation order decides; no further member is
   evaluated to find a value around it.

Criteria and functions must have no effects (`cyoda help workflows`), so the order and the stop rule change which
callouts are made, never the value of the criterion.

**Rule 4 — search is unchanged.** A function condition anywhere in a search condition is `400 INVALID_CONDITION`.

## 3. Wire contract (`api/openapi.yaml`)

- **`CriterionGroupConditionDto`** (new). The same shape and description as `GroupConditionDto`; its
  `conditions` items are `oneOf` Array, CriterionGroup, Function, Lifecycle, Simple. Discriminator value `group`.
- **Workflow and transition `criterion`**: `oneOf` Array, CriterionGroup, Function, Lifecycle, Simple.
- **`GroupConditionDto`** is unchanged: its members are local only. It stays the group of the three search request
  bodies and of the pre-check.
- **`ExternalizedFunctionDto.criterion`** (the pre-check): `oneOf` `null`, Array, Group, Lifecycle, Simple; `null`
  is no pre-check. The schema therefore expresses Rule 1 at every depth. The description states Rule 1 and the guard semantics.
- **`QueryConditionDto`** stays the base of `FunctionConditionDto`; nothing else refers to it.
- **Audit `data`** (the `StateMachineAuditEvent.data` description) documents the `functions` field (section 8).
- `go generate ./api` after the edit. No Go code uses the generated condition types.

## 4. SPI (`cyoda-go-spi`, PR into `main`, pseudo-pinned)

`predicate.FunctionCondition` changes from `struct{}` to:

```go
type FunctionCondition struct {
    // Raw is the function condition's JSON as the parser received it. The
    // engine dispatches a function condition with its own JSON.
    Raw json.RawMessage
    // PreCheck is the parsed `function.criterion` member; nil when it is
    // absent or null.
    PreCheck Condition
}
```

- `ParseCondition` fills both. `Raw` is a copy of the input bytes, never the caller's slice. The pre-check is
  parsed by the same parser, at the depth of the function condition plus one, so the parser's depth limit covers
  the whole criterion.
- `MarshalJSON` writes `Raw` when it is set, and only the discriminator otherwise.
- The parser does not enforce Rule 1. It parses whatever the pre-check holds, so that cyoda-go can refuse it with a
  message that names its location.
- `ConditionToFilter` and the other search walkers keep refusing a function condition; they do not enter a
  pre-check.
- SPI `CHANGELOG.md` `[Unreleased]`: additive change. Parser unit tests: raw copy, pre-check parsed, depth counted
  through a pre-check, `null` pre-check, marshal round trip.
- cyoda-go `go.mod` (root and the three plugins) take the new pseudo-version in one commit; `COMPATIBILITY.md`
  v0.9.0 row names it.
- cassandra: it pins SPI `v0.8.3` and keeps its own predicate copy for search only, so nothing changes until its
  v0.9.0 bump, which only compiles against the additive field. No cassandra issue.

## 5. Parsing in cyoda-go

- `contract.ParseCriterionFunction(raw)` stays the one typed view of a function condition's `function` member,
  used by import, by the engine and by callout construction. It additionally reports whether the `function`
  member is present and not `null`. It also carries the pre-check as written (`Criterion json.RawMessage`), and the
  audit's `preCheck` field is decoded from it.
- Each function condition is dispatched with its own `FunctionCondition.Raw`. A `FunctionCondition` whose `Raw` is
  empty fails the evaluation (it cannot come from the parser).

## 6. Import (`internal/domain/workflow/validate.go`)

Every rule answers `400 VALIDATION_FAILED`, with a message that names the workflow, state and transition, and the
position of the condition (JSON Pointer into the criterion, e.g. `/conditions/1/function/criterion`).

| # | Rule | Where |
|---|---|---|
| I1 | The criterion parses as a condition (existing). | criterion |
| I2 | A function condition has a `function` member that is an object, and a non-empty `name`. | every function condition, any depth |
| I3 | Its `config` parses; `retryPolicy` is `NONE`, `FIXED` or empty (existing, now at every depth). | every function condition |
| I4 | Its `responseTimeoutMs` is not negative and not above `CYODA_CALLOUT_RESPONSE_TIMEOUT_MAX_MS` (existing, now at every depth — `validateCalloutLimits` walks the whole criterion). | every function condition |
| I5 | A pre-check contains no function condition, at any depth (Rule 1). | every pre-check |
| I6 | Local checks: path grammar, array-clause path shape, lifecycle type soundness, operator and operand shape, `BETWEEN` arity and bounds, group operator and `NOT` arity, patterns. | every local condition, pre-checks included |
| I7 | Depth: the whole criterion, pre-checks included, is within the parser's depth limit (50). The SPI parser parses a pre-check one level below its function condition. | criterion |
| I8 | Entries: the whole criterion, pre-checks included, tests at most `MaxConditionEntries` (10,000). A function condition counts 1 plus its pre-check's entries. | criterion |

The local checks (I6) run once per **local unit**: each maximal function-free subtree of the criterion, and each
pre-check. The message names the unit's position. Import and the engine's compile phase (section 7) split the
criterion into the same units with one function. The search walkers therefore never meet a function condition from a
criterion, and they do not enter a pre-check. The one exception is `conditionEntries`: it counts a function condition
as 1 plus its pre-check's entries, so that I8 counts the whole criterion. The search entry points refuse a function
condition before any other walker reaches it.

`docs/workflow-schema-versioning.md`, 1.5 entry: no version bump (1.5 is unreleased).

- Tightening: a pre-check is now evaluated; a pre-check that breaks I5–I8 is refused; a function condition without
  a `function` member or `name` is refused (it failed every dispatch).
- Additive: a function condition inside a group is accepted (it failed every evaluation before).
- The heading's field count, the tightening count and the dual-shape paragraph are updated to match.

## 7. Evaluation (`internal/domain/workflow/engine.go`)

`evaluateCriterion` keeps its callers: workflow selection (`selectWorkflow`, target
`WORKFLOW`, also used by the transitions read through `resolveWorkflowForQuery`), the manual transition, the
automated cascade, and scheduled firing (target `TRANSITION`). It returns the value, the evaluation record
(section 8) and an error.

**Compile phase** (no callout, no entity write):

1. Parse the criterion.
2. A criterion with no function condition takes today's local path unchanged.
3. Otherwise, build an evaluation tree:
   - a maximal function-free subtree → one local node, prepared by `match.Prepare` as today;
   - a group that contains a function condition → an engine group node; its operator must be `AND`, `OR` or `NOT`,
     and a `NOT` has exactly one member;
   - a function condition → a function node holding its parsed `function` member and, when present, its pre-check
     prepared as a local node.
4. Model checks run once, over the paths of every local node and every pre-check, with today's order and
   classification: model-store failure → `ErrCriterionTypingInfra`; undeclared path (after the one bounded refresh)
   → unevaluable; pattern and value-type checks → unevaluable.
5. A criterion with a function condition on an engine with no callout dispatcher fails here.

**Run phase**:

- Local node → `Prepared.Match` on the entity.
- Engine group node → members in order, with the stop rule of Rule 3.
- Function node → if it has a pre-check and the pre-check does not match: value false, no callout. Otherwise one
  callout through `DispatchCriteria` with the node's `Raw`, wrapped exactly as today's single callout:
  `txgate.Suspend`, the call context, `resume`, `fence.Check`. The compute member's reason is capped as today.
- A callout error or a fence refusal ends the evaluation with that error.

All function nodes see the same `*spi.Entity` value, the one the criterion is evaluated on.

**Multi-node.** The pre-checks run on the node that runs the evaluation. A callout handed over to a peer carries
the function condition's own JSON (`Source.Criterion` = `Raw`); the peer builds the callout from it
(`internal/cluster/dispatch/handover.go`) and never evaluates a pre-check.

**Scheduled firing.** Criterion false → Declined (as today). A callout failure → the failure path that today's
single function criterion takes (recorded, retried).

**Transitions read** (`GET /entity/{entityId}/transitions`). Workflow selection makes the callouts the workflow
criteria need, records no audit event (as today), and fails the request when a criterion fails.

## 8. Audit and the refusal message

**Evaluation record.** For a criterion that contains at least one function condition, the engine keeps one entry
per function condition the evaluation reached, in evaluation order:

| Field | Present | Value |
|---|---|---|
| `path` | always | JSON Pointer of the function condition in the criterion; `""` for a whole-criterion function |
| `name` | always | `function.name` |
| `preCheck` | when it has one | the pre-check, decoded to plain JSON values |
| `preCheckMatched` | when it has one | boolean |
| `called` | always | boolean |
| `matches` | when called | the member's answer |
| `reason` | when called and the member gave one | the member's reason, capped |

A function condition the stop rule skipped has no entry.

**Events.** `functions` (the list above) is added to the `data` of:

- `WORKFLOW_FOUND` (criterion matched) and `WORKFLOW_SKIP` (criterion did not match);
- `TRANSITION_NOT_MATCH_CRITERION`;
- `TRANSITION_MAKE`, on every path (manual, cascade, scheduled), when the transition's criterion contains a
  function condition.

`WORKFLOW_FOUND` and `TRANSITION_MAKE` gain a `data` object only in that case, and that object holds only
`functions`. The other `data` members of all four events keep their current meaning.

**`reason`** (on `WORKFLOW_SKIP` and `TRANSITION_NOT_MATCH_CRITERION`):

- a criterion without a function condition: unchanged (`criterion did not match`);
- a whole-criterion function that was called: unchanged (the member's reason, else `criterion did not match`);
- otherwise: one clause per entry, in order, joined with `; `:
  - not called: `<name> at <path>: pre-check did not match, not called`;
  - called: `<name> at <path>: answered <true|false>` plus `: <reason>` when the member gave one;
  - for a whole-criterion function the ` at <path>` part is left out.

Every callout made is listed with its answer. No attempt is made to say which answer "caused" the result: under
`NOT` a true answer can refuse the transition, and the list with the criterion is what lets a reader see that.

**Manual transition refused by its criterion** (`400 WORKFLOW_FAILED`): `transition "<name>" criterion not
matched`, followed by `: <reason>` (the reason built above) when that reason is not the default
`criterion did not match`, and bare otherwise. So a criterion without a function condition, and a whole-criterion
function whose member gave no reason, keep today's message. The manual path rolls back, so its
audit event is not durable; the message is the guaranteed surface (`docs/cloud-parity/criterion-stoppage-reason.md`).

Row 8 (#625) types the state-machine event `data`; its subtypes include `functions`.

## 9. Errors per entry point

Unchanged codes keep their existing meaning; this table lists what a criterion with function conditions can now
produce.

**`POST /model/{entityName}/{modelVersion}/workflow/import`**

| Status | Code | When |
|---|---|---|
| 200 | — | criterion with function conditions in groups; pre-check of each local type |
| 400 | `VALIDATION_FAILED` | I1–I8 (section 6), each with its position |

**Entity create `POST /entity/{format}/{entityName}/{modelVersion}`, update `PUT /entity/{format}/{entityId}` and
`PUT /entity/{format}`, manual transition `PUT /entity/{format}/{entityId}/{transition}`, transitions read
`GET /entity/{entityId}/transitions`; gRPC entity create, update and transition**

| Status / gRPC envelope | Code | When |
|---|---|---|
| 2xx / `success` | — | criterion evaluated; automated: the transition fires or the cascade moves on; selection picks a workflow |
| 400 | `WORKFLOW_FAILED` | manual transition whose criterion is false (message, section 8) |
| 400 | `WORKFLOW_FAILED` | unevaluable local condition anywhere in the criterion or a pre-check; a callout answered `success=false` |
| 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | a reached function condition has no member for its tags |
| 503 | `DISPATCH_TIMEOUT` | a reached callout got no answer in time |
| 503 | `COMPUTE_MEMBER_DISCONNECTED` | a reached callout's member dropped |
| 503 | `DISPATCH_FORWARD_FAILED` | a reached callout handed over to a peer node got no readable answer from it |
| 503 | `CALLOUT_FAILED` | more than one try of a reached callout failed; the message lists the tries |
| 500 | — (ticket) | model store failure during the compile phase |

gRPC answers `success=false`. The envelope's `Error.Code` is `CLIENT_ERROR` for the 400 and the 503 rows (the 503s
are operational errors with a domain code) and `SERVER_ERROR` for the 500. `Error.Message` carries the domain code
(`internal/grpc/workflow_selection_test.go`, `assertClientErrorEnvelope` in
`internal/grpc/scheduled_function_rpc_test.go`).

**Scheduled firing** (no endpoint): false → Declined (`SCHEDULED_TRANSITION_*` as today); callout or criterion
failure → retried by the scheduler's failure path.

## 10. Coverage matrix

Layers: **U** unit (`internal/domain/workflow`, `internal/contract`, `internal/domain/search`, SPI `predicate`),
**E** e2e HTTP on PostgreSQL (`internal/e2e`), **G** gRPC (`internal/grpc`), **P** cross-backend parity
(`e2e/parity`, registered in `registry.go`), **M** multi-node.

| Scenario | U | E | G | P | M |
|---|---|---|---|---|---|
| Import: pre-check of each local type (simple, lifecycle, array, group) | ✓ | ✓ | | | |
| Import: function in a pre-check, top level and nested → 400 | ✓ | ✓ | | | |
| Import: pre-check not a condition / bad path / bad operator / bad pattern → 400 | ✓ | ✓ | | | |
| Import: function in `AND`, `OR`, `NOT`, nested group → 200 | ✓ | ✓ | | | |
| Import: function at depth with bad `retryPolicy` / `responseTimeoutMs` / missing `name` / missing `function` → 400 | ✓ | ✓ | | | |
| Import: depth and entry limits counted through pre-checks → 400 | ✓ | ✓ | | | |
| Selection: pre-check false → workflow skipped, 0 callouts, audit `functions` | ✓ | ✓ | ✓ | ✓ | |
| Selection: function in a group selects / skips, audit `functions` | ✓ | ✓ | | ✓ | |
| Cascade: pre-check false → 0 callouts, entity stays (09/12 rewritten: 0 then 1 criterion request) | ✓ | ✓ | ✓ | ✓ | |
| Cascade: pre-check true → 1 callout, transition fires, `TRANSITION_MAKE.functions` | ✓ | ✓ | ✓ | ✓ | |
| Cascade: `AND`/`OR`/`NOT` with functions — value, order and stop rule (callout counts per member) | ✓ | ✓ | ✓ | ✓ | |
| Cascade: false group criterion → the next automated transition fires | ✓ | ✓ | | | |
| Manual: refused → 400 `WORKFLOW_FAILED`, reason lists callouts | ✓ | ✓ | ✓ | ✓ | |
| Manual: allowed → `TRANSITION_MAKE.functions` | ✓ | ✓ | | | |
| Scheduled: pre-check false → Declined, 0 callouts | ✓ | ✓ | | | |
| Scheduled: callout failure in a group → retried, not Declined | ✓ | ✓ | | | |
| Transitions read: function in a group in a workflow criterion | ✓ | ✓ | | | |
| Unevaluable local condition in a member the stop rule skips / in a pre-check → 400, 0 callouts | ✓ | ✓ | ✓ | | |
| Callout failure in a group: no member → 503; `success=false` → 400 | ✓ | ✓ | ✓ | | |
| Model store failure in the compile phase → 500, 0 callouts | ✓ | | | | |
| No dispatcher configured, function anywhere → error before any callout | ✓ | | | | |
| Hand-over: a peer receives the function condition's own JSON | ✓ | | | | ✓ |
| Search: function in a group still `400 INVALID_CONDITION` | ✓ (existing) | ✓ (existing) | ✓ (existing) | | |
| Search: function whose pre-check is malformed → HTTP `400 INVALID_CONDITION`; gRPC status `InvalidArgument` (the parser now reads the pre-check) | | ✓ | ✓ | | |
| SPI parser: raw copy, pre-check parsed, depth through pre-check, marshal | ✓ | | | | |

The concurrency rule does not apply: nothing here is a race.

## 11. Documentation

- `cmd/cyoda/help/content/workflows.md` — CRITERIA: Rules 1–3, the evaluation order, the audit `functions`
  field, the import rules.
- `cmd/cyoda/help/content/search.md` — the function-condition paragraph: valid in criteria at any depth, never in
  search.
- `cmd/cyoda/help/content/grpc.md` — one `EntityCriteriaCalculationRequest` per reached function condition, from
  workflow selection and transitions.
- `cmd/cyoda/help/content/crud.md` — transitions read: callouts for workflow criteria.
- `docs/cloud-parity/function-conditions-in-criteria.md` (new) and `README.md` index; updates to
  `function-condition-search-rejection.md`, `criterion-stoppage-reason.md` (reason and message rules) and
  `unevaluable-criterion-fails-save.md` (faults in pre-checks and skipped members).
- `docs/workflow-schema-versioning.md` — section 6.
- `CHANGELOG.md` — `### Breaking`: pre-checks are evaluated; a pre-check with a function condition, and a function
  condition without `function`/`name`, are refused at import; audit `data` gains `functions`. `### Added`: function
  conditions in groups.
- `COMPATIBILITY.md` — the SPI pin.

## 12. Cloud (Gate 7)

CaaS ticket, `[CaaS]` prefix, component CaaS. Cloud differs today:

- its spec types the pre-check as the full condition union (functions included); its import accepts only
  `simple` and `group` in a pre-check;
- it allows a function condition only in a top-level `AND`, unwrapped into a set without a defined order; it
  refuses a top-level `array` or `lifecycle` criterion;
- the error behaviour of a pre-check (fail open or closed) — to be confirmed in Cloud's code before the ticket is
  filed;
- the audit carries the pre-check's outcome only in a reason text.

The ticket asks Cloud to adopt sections 2, 3 and 8.
