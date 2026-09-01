# An unevaluable workflow criterion fails the save — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's workflow-criterion evaluation behaviour. cyoda-go is the
authoritative implementation.

## Behaviour

Workflow import validates a `MATCHES_PATTERN` regex, but it does not validate
operator names — a criterion such as

```
AND[state == "SHIPPED", $.amount FROBNICATE 1]
```

stores cleanly even though `FROBNICATE` is not a real operator. The criterion
evaluator now inspects the whole condition tree before evaluating any leaf
against data. If any operator in the tree cannot be evaluated, the save that
triggered evaluation fails with **400 `WORKFLOW_FAILED`** and the transaction
is rolled back — no entity write, no state transition, no partial effect.

This replaces a lazy, short-circuiting walk: for an entity outside
`SHIPPED`, the old evaluator never reached the `FROBNICATE` leaf, the
condition reported "not satisfied", the transition silently did not fire, and
the save returned `2xx`. The bad operator was only ever surfaced for an
entity that happened to reach it, and even then it degraded to "not
satisfied" rather than an error.

**The same invariant covers a field the model does not declare.** A query
never executes against a field the model does not declare — there is no such
thing as an undeclared field to query. A criterion is imported without
asserting model compliance, because a model may legitimately be declared
later; but when that criterion is evaluated and the model still does not
declare its field, the transition attempt fails and rolls back the same way
an unrecognised operator does. Measured, with no unrecognised operator
involved:

| Model | `$.amount GREATER_THAN 3` over `{"amount":5}` | Correct answer |
|---|---|---|
| declares `amount` | true | true |
| does not declare `amount` | **false** (before this fix) | true |

A misspelled field name in a criterion used to mean the transition silently
never fired, and the save reported success — the same failure mode as the
unrecognised-operator case, reached through a different check. This applies
to **all 26 operators**, not only the ones that need a declared type: what
may execute is what is declared, regardless of which operator names the
field.

A field that no entity has ever written must be declared explicitly through
`POST /model/import/...` — the model does not grow from a criterion alone. A
criterion on such a field is supported once the field is declared.

## Invariant Cloud must mirror

A criterion that cannot be evaluated must never be read as "condition not
met". If any operator anywhere in the condition tree is unrecognised, names a
field the model does not declare, or is otherwise structurally unevaluable
(an operand fitting no declared type, an uncompilable pattern), the
transition attempt must fail loudly and roll back, regardless of whether the
entity's own data would have short-circuited past that leaf under a lazy
walk. Evaluability is a property of the criterion's shape checked against the
current model, not of the specific entity being tested against it — so the
same eager, whole-tree check applies uniformly, not only to the leaf a given
entity happens to reach.

The model-membership check applies one bounded schema refresh before it
refuses, so a field a peer node has just declared is not falsely refused by a
node holding an older cached schema (`path-grammar.md` § 6).

## Wire impact

- Status: **400**.
- Error code: **`WORKFLOW_FAILED`** (existing code, no new one).
- Transaction: rolled back — the entity is left exactly as it was before the
  attempted transition, with no schema change and no partial write.
- Scope: this is an evaluation-time check, not an import-time one. Import
  still only validates regex syntax, path grammar, operator names and
  lifecycle type-soundness — not model membership; an unrecognised operator
  name or a field the model does not declare is accepted at import and only
  surfaced the first time a transition guarded by it is attempted. See
  `docs/workflow-schema-versioning.md` for why this is not a schema-version
  bump.

## What did not change

- No new error code — `WORKFLOW_FAILED` already covers criterion evaluation
  failures.
- No status code moves for any other criterion outcome. A criterion that
  evaluates cleanly to `false` still reports "not satisfied", not an error.
- No change to `WorkflowConfigurationDto` import validation, acceptance
  rules, or export shape — a workflow carrying this criterion imports exactly
  as before.
- No API shape change on any endpoint.
