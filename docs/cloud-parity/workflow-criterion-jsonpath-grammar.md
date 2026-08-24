# Workflow criterion `jsonPath` grammar — Cloud twin-alignment spec

cyoda-go leads this contract. A workflow or transition `criterion` addresses
entity data by `jsonPath`, and that `jsonPath` is JSON Path nomenclature — the
same grammar, and the same implementation of it, a search condition obeys. A
path outside the grammar is rejected at **workflow import**, `400
VALIDATION_FAILED`. Cloud aligns to the same accept/reject set, at the same
boundary, with the same code.

This is a **wire-contract tightening**: a workflow that imports today stops
importing.

Companion to `condition-jsonpath-grammar.md`, which tightened the search
condition surface and explicitly deferred this one. It is no longer deferred.

## The grammar

Unchanged from the search condition surface — see
`condition-jsonpath-grammar.md` for the production and the full reject list:

```
jsonPath  = "$." segment ( "." segment )*
segment   = name subscript*
name      = 1*( ALPHA / DIGIT / "_" / "-" )   ; ASCII only
subscript = "[" ( "*" / 1*DIGIT ) "]"
```

The **condition** variant applies, not the scalar one: a **well-formed** array
subscript — the wildcard `[*]` or a non-negative index — is ACCEPTED
(`$.tags[*].name`, `$.arr[0]`). A criterion is only ever served by the
in-process predicate evaluator, which resolves them.

A **malformed** bracket spelling (`$.tags[-1]`, `$.tags[0:2]`, `$.a[?(@.x)]`,
`$.a[`, `$.a[0]b`) is rejected at import like any other syntax error. This
surface is where that matters most: import validation is grammar-only — there
is no schema check behind it to catch a bad path later — so a malformed
subscript used to import cleanly and then evaluate to false for every entity,
and the transition it guarded **silently never fired**. There was no error
anywhere to notice.

Which clauses carry a path:

| Clause | Path checked | Why |
|---|---|---|
| `simple` | `jsonPath` | addresses a data field |
| `array` | `jsonPath` | addresses a data field (its container) |
| `group` | — | recursed into; every leaf beneath it is checked |
| `lifecycle` | — | `field` names a member of the closed meta vocabulary, not a path |
| `function` | — | carries no path; dispatched to a compute member |

## Why the search tightening did not already cover it

`condition-jsonpath-grammar.md` validates at `search.ValidateCondition`, the
boundary every *search-shaped* entry point funnels through. A criterion is not
search-shaped: it never reaches `spi.ConditionToFilter` and never reaches that
boundary. It is parsed and evaluated by `match.Prepare`, which resolves a bare
`amount` happily.

So a bare criterion path did not merely go unrejected — it **worked**, and fired
transitions. One model syntax, two spellings of what a path is, disagreeing on
which paths exist depending on where the condition was written.

Project ruling: *"`variantId` isn't a JSON path and therefore simply should
error. We have a model syntax that is based on JSON Path nomenclature. Invalid
paths are invalid paths."*

## Why import, and not evaluation

Import is the only boundary a criterion crosses. A criterion is stored verbatim
and read back on every entity write that touches its transition, so rejecting at
evaluation time would fail a *save* — repeatedly, for every entity, long after
the workflow was accepted — and the operator who has to fix it is not the caller
who sees the error.

This is the same placement, and the same rationale, as the two criterion checks
that already run at import: the `MATCHES_PATTERN` regex compile, and
lifecycle/meta type-soundness. The path check joins them in `walkCriterion`.

## Error surface

Import-time criterion validation is `400 VALIDATION_FAILED`, with the offending
workflow / state / transition named in `detail` — the shape every other
import-time validation failure already uses. No new error code, and
deliberately not `INVALID_FIELD_PATH`: the workflow import endpoint reports one
validation code and locates the fault by name, and a caller distinguishing
"which rule failed" reads `detail`.

There is no gRPC twin to cover: workflow import is HTTP-only
(`POST /api/model/{entityName}/{modelVersion}/workflow/import`). gRPC's
CloudEvents surface carries entity-model import, not workflow import.

## Consequence for existing workflows

A stored workflow carrying a bare criterion path keeps evaluating — validation
runs on the **incoming** import request only, matching the existing
non-retroactive policy for structural rules. It fails on its next re-import, and
that is intended: the export→edit→import round-trip is where the fix gets made.

## Caller migration

Add the leader. Replace bracket-quoted access with dotted access.

```diff
  "criterion": {
-   "type": "simple", "jsonPath": "amount",   "operatorType": "GREATER_THAN", "value": 50
+   "type": "simple", "jsonPath": "$.amount", "operatorType": "GREATER_THAN", "value": 50
  }
```

## Test surface

- `internal/domain/workflow/criterion_path_test.go` — reject table and accept
  table against `validateImportRequest`, for a transition criterion, a
  workflow-level criterion, a `simple` leaf, an `array` leaf, and a leaf nested
  under a group; plus the lifecycle-is-not-a-path and function-is-exempt
  controls.
- `internal/e2e/workflow_criterion_path_key_test.go` — status + error code over
  real HTTP on postgres, sharing the search suite's `nonJSONPathSpellings`
  table so the two surfaces cannot drift on what they reject; plus the
  array-subscript accept control and the fires/does-not-fire pair proving a
  valid path still works.
- `e2e/parity/criterion_path.go` —
  `RunWorkflowCriterionPathRequiresJSONPathLeader` carries the malformed-bracket
  cases (`$.tags[-1]`, `$.tags[0:2]`) on every backend, and
  `RunPositionalSubscriptPathResolves` is the accept-side control that a
  well-formed positional subscript still guards a transition.
