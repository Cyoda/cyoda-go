# Function conditions in criteria, and the function pre-check

cyoda-go defines how a workflow or transition criterion evaluates function
conditions; Cyoda Cloud aligns to it.

## 1. Terms

- **Criterion** — the `criterion` member of a workflow or of a transition. The
  engine evaluates it to select a workflow, or to allow a transition.
- **Function condition** —
  `{"type":"function","function":{"name":…,"config":{…},"criterion":…}}`.
  Evaluating it makes a callout to a compute member
  (`EntityCriteriaCalculationRequest`); the member's `matches` answer is its
  value.
- **Pre-check** — the `function.criterion` member of a function condition.
- **Local condition** — a `simple`, `lifecycle` or `array` condition, or a
  `group` whose members are all local conditions. The engine evaluates a local
  condition itself, without a callout.

## 2. Rules

**Rule 1 — the pre-check.**

- A pre-check contains only local conditions, of every local type. A function
  condition anywhere in a pre-check, at any depth, is refused at import.
- The engine evaluates the pre-check before the callout:
  - the pre-check does not match → the function condition is false, and no
    callout is made;
  - the pre-check matches → the callout is made, and the member's answer is
    the function condition's value;
  - no pre-check (member absent or `null`) → the callout is made.

**Rule 2 — the criterion.** A criterion may contain function conditions at any
depth: as the whole criterion, or as members of `AND`, `OR` and `NOT` groups,
nested to any depth. Every function condition in a criterion may carry a
pre-check, and Rule 1 applies to it.

**Rule 3 — evaluation of a criterion.**

1. Before the first callout, the whole criterion is checked against the
   entity's model and compiled: every local condition in it and in every
   pre-check, and every group that contains a function condition. A fault
   anywhere fails the operation, also in a member that evaluation would not
   reach.
2. Group members are evaluated in declaration order. Evaluation of a group
   stops as soon as its value is known: `AND` at the first false member, `OR`
   at the first true member. `NOT` has exactly one member and inverts it.
3. A function condition that is reached is evaluated by Rule 1.
4. A callout that fails fails the operation. The first failure in evaluation
   order decides; no further member is evaluated to find a value around it.

Criteria and functions must have no effects, so the order and the stop rule
change which callouts are made, never the value of the criterion.

**Rule 4 — search is unchanged.** A function condition anywhere in a search
condition is `400 INVALID_CONDITION` (`function-condition-search-rejection.md`).
A function condition whose pre-check does not parse fails the parse of the
search condition: `400 INVALID_CONDITION` on HTTP, the transport status
`InvalidArgument` on gRPC, as for any condition that does not parse.

**Multi-node.** The node that runs the evaluation runs the pre-checks. A
callout handed over to a peer carries the function condition's own JSON, and
the peer builds the callout from it; a peer never evaluates a pre-check.

## 3. Wire contract (OpenAPI)

- **`CriterionGroupConditionDto`** is the group of a criterion. It has the
  shape of `GroupConditionDto`, except that its `conditions` items are `oneOf`
  Array, CriterionGroup, Function, Lifecycle, Simple. Its description is that
  of `GroupConditionDto` plus one paragraph: a criterion's group may hold
  function conditions at any depth, evaluated in declaration order with the
  stop rule, while a search condition's group holds local conditions only.
  Discriminator value `group`.
- **Workflow and transition `criterion`**: `oneOf` Array, CriterionGroup,
  Function, Lifecycle, Simple.
- **`GroupConditionDto`** holds local conditions only. It is the group of the
  three search request bodies and of the pre-check.
- **`ExternalizedFunctionDto.criterion`** (the pre-check): `oneOf` `null`, Array,
  Group, Lifecycle, Simple; `null` is no pre-check. The schema expresses Rule 1
  at every depth, and the description states Rule 1 and its guard semantics.
- **Discriminator.** Every condition `oneOf` (the criterion unions, both
  groups' `conditions` items, the pre-check, the search request bodies and the
  grouped-stats `condition`) maps `type` to its member: `array`, `group`,
  `lifecycle`, `simple`, and `function` in the criterion unions. The
  pre-check's `null` branch has no mapping.
- **Condition `value`.** `SimpleConditionDto.value` and
  `LifecycleConditionDto.value` admit what the server accepts: a string,
  number or boolean, a `[low, high]` pair for `BETWEEN` and
  `BETWEEN_INCLUSIVE`, or no value (absent or `null`) only for `IS_NULL` and
  `NOT_NULL`. `value` is not required in the schema; the server refuses a
  missing or `null` value under any other operator (400 `INVALID_CONDITION`,
  400 `VALIDATION_FAILED` at import). An object is refused.

The `EntityCriteriaCalculationRequest` and `EntityCriteriaCalculationResponse`
shapes are unchanged. One request is sent per function condition reached, with
`target` `WORKFLOW` in workflow selection and `TRANSITION` in a transition.

## 4. Import

Each rule answers `400 VALIDATION_FAILED`. The message names the workflow,
state and transition, and the position of the condition as a JSON Pointer into
the criterion: a group member is `/conditions/<i>`, a pre-check is
`<function position>/function/criterion`, and the whole criterion is `""` (the
message then names no position).

| # | Rule | Where |
|---|---|---|
| I1 | The criterion parses as a condition. | criterion |
| I2 | A function condition has a `function` member that is an object, and a non-empty `name`. | every function condition, any depth |
| I3 | Its `config` parses; `retryPolicy` is `NONE`, `FIXED` or empty. | every function condition |
| I4 | Its `responseTimeoutMs` is not negative and not above the server's upper bound. | every function condition |
| I5 | A pre-check contains no function condition, at any depth (Rule 1). | every pre-check |
| I6 | Local checks: path grammar, array-clause path shape, lifecycle type soundness, operator and operand shape, `BETWEEN` arity and bounds, group operator and `NOT` arity, patterns. | every local condition, pre-checks included |
| I7 | Depth: the whole criterion, pre-checks included, is within the parser's depth limit (50). A pre-check is one level below its function condition. | criterion |
| I8 | Entries: the whole criterion, pre-checks included, tests at most 10,000 entries. A function condition counts 1 plus its pre-check's entries. | criterion |

A function condition inside a group is accepted.

## 5. Audit

For a criterion that contains at least one function condition, the engine keeps
one entry per function condition the evaluation reached, in evaluation order. A
function condition that evaluation did not reach has no entry.

| Field | Present | Value |
|---|---|---|
| `path` | always | JSON Pointer of the function condition in the criterion; `""` for a whole-criterion function |
| `name` | always | `function.name` |
| `preCheck` | when it has one | the pre-check as written, as plain JSON |
| `preCheckMatched` | when it has one | boolean |
| `called` | always | boolean |
| `matches` | when called | the member's answer |
| `reason` | when called and the member gave one | the member's reason, capped |

The list is the `functions` member of the state-machine event's `data`:

- `WORKFLOW_SKIP` and `TRANSITION_NOT_MATCH_CRITERION` add `functions` to the
  `data` they already carry;
- `WORKFLOW_FOUND` and `TRANSITION_MAKE` (manual, cascade and scheduled) carry
  `data: {"functions": [...]}` only when the criterion holds a function
  condition, and that object holds only `functions`.

The other `data` members of the four events keep their meaning.

**The reason** (`data.reason` on `WORKFLOW_SKIP` and
`TRANSITION_NOT_MATCH_CRITERION`):

- a criterion without a function condition: `criterion did not match`;
- a whole-criterion function that was called: the member's reason, else
  `criterion did not match`;
- otherwise one clause per entry, in order, joined with `; `
  (`criterion did not match` when there is no entry):
  - not called: `<name> at <path>: pre-check did not match, not called`;
  - called: `<name> at <path>: answered <true|false>`, plus `: <reason>` when
    the member gave one;
  - for a whole-criterion function the ` at <path>` part is left out.

Every callout made is listed with its answer. No answer is singled out as the
cause: under `NOT` a true answer can refuse the transition, and the list with
the criterion lets a reader see that.

**Manual transition refused by its criterion** (`400 WORKFLOW_FAILED`):
`transition "<name>" criterion not matched`, followed by `: <reason>` when the
reason is not `criterion did not match`, and bare otherwise. A criterion
without a function condition, and a whole-criterion function whose member gave
no reason, therefore keep the bare message. The manual path rolls back, so its
audit event is not durable; the message is the guaranteed surface
(`criterion-stoppage-reason.md`).

## 6. Errors per entry point

**Workflow import** — `200` for a criterion with function conditions in groups
and a pre-check of each local type; `400 VALIDATION_FAILED` for I1–I8.

**Entity create and update, manual transition, the transitions read, and the
gRPC entity create, update and transition:**

| Status | Code | When |
|---|---|---|
| 2xx | — | criterion evaluated |
| 400 | `WORKFLOW_FAILED` | manual transition whose criterion is false (message, section 5) |
| 400 | `WORKFLOW_FAILED` | unevaluable local condition anywhere in the criterion or a pre-check; a callout answered `success: false` |
| 503 | `NO_COMPUTE_MEMBER_FOR_TAG` | a reached function condition has no member for its tags |
| 503 | `DISPATCH_TIMEOUT` | a reached callout got no answer in time |
| 503 | `COMPUTE_MEMBER_DISCONNECTED` | a reached callout's member dropped |
| 503 | `DISPATCH_FORWARD_FAILED` | a reached callout handed over to a peer node got no readable answer from it |
| 503 | `CALLOUT_FAILED` | more than one try of a reached callout failed; the message lists the tries |
| 500 | — (ticket) | the model store failed while the criterion was being checked |

On gRPC the envelope has `success: false`; `Error.Code` is `CLIENT_ERROR` for
the `400` and `503` rows and `SERVER_ERROR` for the `500`, and
`Error.Message` carries the domain code.

**Transitions read** (`GET /entity/{entityId}/transitions`): workflow
selection makes the callouts the workflow criteria need, records no audit
event, and fails the request when a criterion fails.

**Scheduled firing**: a false criterion declines the transition; a callout or
criterion failure is retried by the scheduler's failure path.

## 7. What Cloud changes

Cloud's handling of function conditions in criteria differs in these ways:

- its spec types the pre-check as the full condition union, function
  conditions included, while its import converts only `simple` and `group`
  conditions in a pre-check;
- it accepts a function condition only as the whole criterion or as a member
  of a top-level `AND`, which its import unwraps into separate criteria;
- its import refuses an `array` or `lifecycle` condition at any depth of a
  criterion, as the whole criterion and as a group member.

To align, Cloud adopts sections 2, 3 and 5:

- type the pre-check as local conditions only, accept every local type in it,
  and refuse a function condition in it at any depth;
- accept function conditions at any depth in `AND`, `OR` and `NOT` groups, and
  `array` and `lifecycle` conditions at any depth of a criterion;
- evaluate group members in declaration order with the stop rule, and check the
  whole criterion, pre-checks included, before the first callout;
- fail the operation when a pre-check cannot be evaluated or a callout fails —
  never read either as a match or as no match;
- record the `functions` list and build the reason and the manual refusal
  message as in section 5.
