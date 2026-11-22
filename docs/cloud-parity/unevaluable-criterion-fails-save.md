# An unevaluable workflow criterion fails the save — Cloud twin-alignment spec

This document is the contract Cyoda Cloud implements to stay aligned with
cyoda-go's workflow-criterion evaluation behaviour. cyoda-go is the
authoritative implementation.

## Behaviour

Workflow import checks every criterion on its own: operator names, operand
shape, path grammar, lifecycle type-soundness, and LIKE and MATCHES_PATTERN
patterns. It does not check the criterion against the model, because a model
may legitimately be declared after the workflow that references it. So a
criterion such as

```
AND[state == "SHIPPED", $.amonut GREATER_THAN 1]
```

imports cleanly even though the model declares `amount`, not `amonut`.

The criterion evaluator checks the whole condition tree against the current
model before it evaluates any leaf against data. If any leaf names a field the
model does not declare, compares a scalar with a path the model declares only
as a container, applies an operator that none of the field's declared types
supports (such as `CONTAINS` on a number), or carries a comparison or range
operand that fits none of the declared types the operator applies to, the save
that triggered evaluation fails with **400 `WORKFLOW_FAILED`** and the
transaction is rolled back — no entity write, no state transition, no partial
effect. This holds for an entity
outside `SHIPPED` too, which a lazy, short-circuiting walk would never take
to the `$.amonut` leaf: such a walk would report "not satisfied", the
transition would silently not fire, and the save would return `2xx`.

A query never executes against a field the model does not declare — there is
no such thing as an undeclared field to query. Reading such a field as
absent gives a wrong answer:

| Model | `$.amount GREATER_THAN 3` over `{"amount":5}` | Correct answer |
|---|---|---|
| declares `amount` | true | true |
| does not declare `amount`, field read as absent | **false** | true |

So a misspelled field name never means a transition that silently does not
fire while the save reports success. This applies to **all 26 operators**,
not only the ones that need a declared type: what may execute is what is
declared, regardless of which operator names the field.

A field that no entity has ever written must be declared explicitly through
`POST /model/import/...` — the model does not grow from a criterion alone. A
criterion on such a field is supported once the field is declared.

## Invariant Cloud must mirror

A criterion that cannot be evaluated must never be read as "condition not
met". If any operator anywhere in the condition tree is unrecognised, names a
field the model does not declare, or is otherwise structurally unevaluable
(a comparison or range operand fitting none of the declared types its
operator applies to, an uncompilable pattern), the transition attempt must
fail loudly and roll back, regardless of whether the
entity's own data would have short-circuited past that leaf under a lazy
walk. Evaluability is a property of the criterion's shape checked against the
current model, not of the specific entity being tested against it — so the
same eager, whole-tree check applies uniformly, not only to the leaf a given
entity happens to reach.

**Function conditions.** The whole tree includes the pre-check of every function
condition in the criterion, and every member that the stop rule would skip
(`function-conditions-in-criteria.md`). The check runs before the first callout:
a criterion with an unevaluable leaf anywhere — in a pre-check, or in a member
after a function condition — fails with `400 WORKFLOW_FAILED` and makes no
callout. A pre-check that cannot be evaluated is never read as "pre-check did
not match".

The model-membership check applies one bounded schema refresh before it
refuses, so a field a peer node has just declared is not falsely refused by a
node holding an older cached schema (`path-grammar.md` § 6).

## Wire impact

- Status: **400**.
- Error code: **`WORKFLOW_FAILED`** (existing code, no new one).
- Transaction: rolled back — the entity is left exactly as it was before the
  attempted transition, with no schema change and no partial write.
- Scope: this is an evaluation-time check, not an import-time one. Import
  refuses an unrecognised operator, a malformed operand, a malformed path,
  an unsound lifecycle condition and an uncompilable pattern, but it does not
  check the model; a field the model does not declare is accepted at import
  and surfaces the first time a transition guarded by it is attempted. See
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
