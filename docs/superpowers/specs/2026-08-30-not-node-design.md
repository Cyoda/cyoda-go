# Negation in search conditions — design

Adds `NOT` to the condition language. Closes `cyoda-go-spi#46`.

`cyoda-go-spi#32` (an array-wildcard quantifier) is **dropped** — see § 12.

Ground truth for everything asserted here about current behaviour:
`cyoda-go-spi/docs/superpowers/research/2026-08-30-not-node-current-behaviour.md`.
Every claim in it was produced by running the code, not by reading it.

## 1. Why

A complement predicate cannot be written at all today. The only branch nodes
are `AND` and `OR`, so:

- **"No element of a list satisfies P" is unaskable.** A leaf on an
  array-wildcard path holds when *some* addressed value satisfies it
  (`path-grammar.md` § 3), so `$.items[*].sku NOT_EQUAL "A"` means *some element
  differs from A* — true for `["A","B"]`, an entity that does contain an `"A"`.
  `EQUALS "A"` is also true for that entity. Two contradictory-looking
  predicates both hold, and the question the caller meant cannot be put.
- **"Every element satisfies P" is unaskable**, for the same reason.
- **`NOT` over a group has no spelling.** De Morgan only rewrites a group whose
  every leaf has a negative twin, and four operators have none: `LIKE`,
  `MATCHES_PATTERN`, `BETWEEN`, `BETWEEN_INCLUSIVE`. *"name does not match this
  pattern"* is inexpressible on a plain scalar field, with no list involved.

Every comparable system can express both readings — SQL/JSON, XPath, MongoDB,
N1QL, Cypher, and both SQL engines over `json_each` / `jsonb_array_elements`.

The contract also already promises it. `api/openapi.yaml`'s
`GroupConditionDto.operator` enum has carried `NOT` since the initial import
(`d1f6875`), while `cmd/cyoda/help/content/search.md:147` states *"`NOT` is not
supported"* and the server answers `400`. Three sources, three answers.

## 2. Scope

In scope:

1. `NOT` in the condition language, end to end.
2. **Criterion path validation** (§ 5) — a prerequisite defect, not a related
   cleanup: without it `NOT` over an undeclared path matches every entity.
3. **Filter-path validation through a `NOT` node** in all three storage plugins
   (§ 6) — a prerequisite defect for the same reason.
4. Strict rejection of unknown group operators in the shared translator (§ 7).

Out of scope, stated so the boundary is not re-litigated:

- **Element correlation.** `AND[$.items[*].sku EQ "A", $.items[*].qty EQ 9]`
  matches when *different* elements satisfy the two leaves. That is what
  `path-grammar.md` § 3 defines and what SQL/JSON answers for two separate path
  expressions. Asking the correlated question (MongoDB's `$elemMatch`) is a
  capability this system does not have and does not gain here.
- **Merging the two tree-walking evaluators.** See § 11.
- **Query-planner changes.** `NOT` is residual-only by construction; see § 8.

## 3. The wire form

```json
{"type": "group", "operator": "NOT", "conditions": [ <exactly one condition> ]}
```

The single entry may be any condition, including a group, so `NOT(A AND B)` is
written by nesting the group inside the `NOT`. Nesting is unrestricted:
`NOT(NOT(x))` is legal.

**`conditions` must hold exactly one entry.** Zero, or two or more, is rejected.

A bare list under `NOT` has two defensible readings that disagree on the same
data. For `{"status":"active","amount":50}` with children *status equals active*
and *amount greater than 100*:

| Reading | Meaning | Result |
|---|---|---|
| "not both" | `NOT(A AND B)` | matches |
| "neither" | `NOT(A) AND NOT(B)` | does not match |

Nothing in the request says which was meant, and guessing returns a plausible
result set rather than an error, so the caller never learns of the mistake. The
requester writes the group they meant.

`operator` stays case-sensitive and exact, as `AND` and `OR` already are.

## 4. Semantics

### 4.1 The rule

`NOT(c)` is **true exactly when `c` is false**. Two-valued, no third state.

This is sound only because nothing unevaluable reaches evaluation. That
invariant is what §§ 5–7 exist to establish, and it is stated here as a
requirement in its own right:

> **No leaf reaches evaluation unless it can be evaluated.** A leaf carrying an
> operator that needs a declared type reaches evaluation only with one; a leaf
> naming a path the model does not declare, or carrying an unrecognised
> operator, is rejected before any entity is examined.

Without it, "false" would mean either *evaluated and did not match* or *could
not be evaluated*, and `NOT` would invert the second into "matches everything" —
fail-open, against `.claude/rules/correctness-over-availability.md`.

### 4.2 Truth table

| Condition | `c` | `NOT(c)` |
|---|---|---|
| `$.status EQUALS "active"` on `{"status":"active"}` | true | false |
| `$.status EQUALS "active"` on `{"status":"draft"}` | false | true |
| `$.status EQUALS "active"` on `{}` (absent) | false | **true** |
| `$.tags[*] EQUALS "red"` on `{"tags":["red","blue"]}` | true | false |
| `$.tags[*] EQUALS "red"` on `{"tags":["blue"]}` | false | true |
| `$.tags[*] EQUALS "red"` on `{"tags":[]}` | false | **true** |
| `$.tags[*] EQUALS "red"` on `{"tags":null}` | false | **true** |
| `$.tags[*] EQUALS "red"` on `{}` (absent) | false | **true** |
| `AND[]` (empty group) | true | false |
| `OR[]` (empty group) | false | true |

### 4.3 Over a list, `NOT` is a universal quantifier

This is the capability the feature adds, and the one thing a caller is most
likely to get backwards:

```
NOT( $.tags[*] EQUALS "red" )     =  NO element equals "red"
    $.tags[*] NOT_EQUAL "red"     =  SOME element differs from "red"
```

For `{"tags":["red","blue"]}` the first is **false** and the second is **true**.
They are different questions, both useful, and neither is a spelling of the
other. `ALL(P)` is written `NOT(some element satisfies ¬P)`, which is available
for the 22 operators that have a negative twin.

### 4.4 The absent-field asymmetry

`operator-semantics.md` § 2 holds that a missing or null value never matches any
binary operator, negatives included. `NOT` sits outside the operator and
inverts that result, so on an entity lacking the field:

| Condition | Result |
|---|---|
| `$.x EQUALS "A"` | no match |
| `$.x NOT_EQUAL "A"` | no match |
| `NOT($.x EQUALS "A")` | **match** |

This matches SQL/JSON, where `!(@.x == "A")` and `@.x != "A"` differ in exactly
this way. It is deliberate and must be documented rather than discovered.

### 4.5 `NOT` and the presence tests

On a wildcard path `IS_NULL` and `NOT_NULL` are not complements
(`path-grammar.md` § 5) — an empty list, an explicit null and an absent field
all present no elements, so both answer false. Therefore `NOT($.a[*] IS_NULL)`
is **not** `$.a[*] NOT_NULL`: over `{"a":[]}` the first is true and the second
false. Ask about the list itself with the bare path `$.a`, which separates the
three states.

### 4.6 No De Morgan rewriting

`NOT(EQUALS)` must never be normalised into `NOT_EQUAL`, nor a negated group
distributed over its children. §§ 4.3 and 4.4 are the two places the rewrite
changes answers. This is stated because the rewrite looks like an obvious
simplification.

### 4.7 A `FUNCTION` clause under `NOT`

Unchanged and consistent with today: the workflow engine dispatches a `FUNCTION`
clause only at a criterion's top level, so one nested inside any group — `NOT`
included — fails the evaluation, as `function-condition-search-rejection.md`
already states. Search rejects `FUNCTION` outright on every surface.

### 4.8 Depth

A `NOT` is one level, counting against `MaxConditionDepth` and the parser's own
depth cap like any group.

## 5. Prerequisite: a criterion must check its paths against the model

**Defect, pre-existing, independent of `NOT`.** A workflow criterion's paths are
never checked for membership in the model — at either end.

- At **import**, `walkCriterion` checks path *grammar*, operator names,
  lifecycle type-soundness and regex operands. Not membership.
- At **execution**, `evaluateCriterion` **does** load the model, and a load
  failure fails the save closed. But the model is consulted only for *typing*:
  `if fd, ok := fields[p]; ok { return fd.Types }; return nil`. An undeclared
  path silently yields no declared types.

`operator-semantics.md` § 4 then has the eight comparison and ordering operators
answer non-match with no declared type. Measured, with no `NOT` involved:

| Model | `$.amount GREATER_THAN 3` over `{"amount":5}` | Truth |
|---|---|---|
| declares `amount` | true | true |
| does not declare `amount` | **false** | true |

So a misspelled field in a criterion means the transition silently never fires.
This is the defect class `unevaluable-criterion-fails-save.md` was written to
close for a bad *operator* — *"a criterion that cannot be evaluated must never be
read as 'condition not met'"* — left open for a bad *path*. Under `NOT` it
inverts from silently-never to silently-always.

**Fix.** Criterion evaluation applies the **same pair of checks every other
condition surface applies**, against the model it already loads, and fails the
save when either refuses — `400 WORKFLOW_FAILED`, rolled back, matching the
existing unevaluable-criterion contract:

- `search.ValidateKnownPaths` — the path is declared by the model. It carries
  the single bounded schema refresh `path-grammar.md` § 6 requires, so a field a
  peer node has just added is not falsely rejected by a node holding an older
  cached schema (`.claude/rules/multi-node-primary.md`).
- `search.ValidateConditionValueTypes` — the operand fits a declared type, and a
  **container path is not compared to a scalar** (`path-grammar.md` § 6). This
  second check is load-bearing for § 4.1's invariant and not merely symmetry: a
  declared container path carries no scalar type either, so without it a
  comparison on one still reaches evaluation with an empty declared-type set and
  answers non-match, which `NOT` would invert into matches-everything. Membership
  alone does not close the hole.

Conditional delete already calls exactly this pair
(`internal/domain/entity/service.go:1048`, `:1063`); the criterion path is the
one condition surface that calls neither.

Import keeps checking grammar only. A criterion may legitimately be imported
before the model declares the field, since a model extends on write; at
execution the field either exists or it does not.

**Behaviour change to declare.** A stored criterion naming an undeclared field
currently evaluates to "not satisfied" and the save succeeds. It will fail the
save. That is the correct direction — loud over silent — and it is the same
change of shape the operator fix already made, but it changes the outcome of
saves that succeed today and belongs in `### Breaking`.

## 6. Prerequisite: plugins must validate paths through a `NOT`

**Defect this change would otherwise introduce.** All three plugins walk only
`AND` and `OR`:

```go
switch f.Op {
case spi.FilterAnd, spi.FilterOr:
    // recurse into Children
}
if f.Path == "" { return nil }
return validateJSONPath(f.Path)
```

`plugins/postgres/path_validation.go`, `plugins/sqlite/path_validation.go`,
`plugins/memory/path_validation.go`. A `FilterNot` carries an empty `Path`, so
it returns `nil` and **its child is never validated**.

`path-grammar.md` § 9 makes whole-tree rejection mandatory: *"a malformed path
nested under an and/or branch is still malformed"*, and a backend must reject
rather than answer an empty page. The consequence is worse under `NOT` than
under `AND`: an unvalidated malformed path becomes a never-match leaf, and `NOT`
inverts that into matches-everything — a superset on search, and on
`DELETE /entity/...` a destructive one.

The conformance suite does not catch it: `spitest`'s nested malformed-path case
is built as a `FilterAnd` only, so no backend — the commercial one included —
would fail on this at its next pin bump.

**Fix.** `FilterNot` recurses in all three validators, and `spitest` grows a
malformed path nested under a `NOT`.

## 7. The shared translator must reject unknown group operators

`groupToFilter` maps any operator that is not `"OR"` (case-insensitively) to
`FilterAnd`. Measured: `"NOT"`, `"xor"` and `""` all produce `Filter{Op:"and"}`.

Today the engine's own boundary makes that unreachable over HTTP. Its value is
for a **self-executing backend** calling `ConditionToFilter` directly, which is
the reason that function lives in the SPI at all — and once `NOT` is a real
operator, a near-miss silently becoming a conjunction is a wrong answer rather
than a rejected request.

`groupToFilter` returns an error wrapping `ErrUnknownOperator` for any operator
outside the recognised set.

This is **not** the source of the client-facing `400`; that is
`internal/domain/search/operators.go`, which stays. Recorded so the boundary
check is not later removed as redundant.

## 8. Push-down: residual by construction

`FilterNot` is not in either planner's `isPushable` set, and `dissect`'s default
arm routes an unrecognised op to the residual, where the kernel is
authoritative. So `NOT` is evaluated in memory on both SQL backends **with no
planner change**. `AND[leaf, NOT(x)]` still pushes `leaf` — narrowing one
conjunct of a conjunction is sound; `OR[leaf, NOT(x)]` goes wholly residual.

No push-down rule is written for `NOT`, deliberately. The rule one would write —
push only over an exactly-translatable child, since negating an approximate
child turns a superset into a subset and drops rows a narrowing `WHERE` never
returns for re-checking — would authorise almost nothing, because `leafExact` is
`IS_NULL` and `NOT_NULL` alone on both backends.

A planner test pins that `NOT` stays residual, so a later change to `isPushable`
cannot silently make it pushable without the soundness rule being written first.

## 9. Representation

`spi.Filter` gains `FilterNot`, a branch node with exactly one child and an
empty `Path`.

**A malformed `FilterNot` fails closed.** `Filter` is a public struct any
backend may build, and `Prepare` returns no error by contract. A `FilterNot`
whose `Children` length is not exactly 1 prepares to a node that never matches —
never an unguarded `Children[0]`, and never "invert the AND of the children".

A `NOT` over a zero-`Op` child inverts a never-match leaf into matches-everything
if written naively; the prepared `NOT` node must therefore treat a child that
did not prepare as a child that cannot be inverted, and never match.

## 10. Surfaces and errors

`NOT` is accepted wherever a `group` clause is accepted.

| Surface | Valid `NOT` | `conditions` ≠ 1 entry |
|---|---|---|
| `POST /search/direct/{entityName}/{modelVersion}` | executes | `400 INVALID_CONDITION` |
| `POST /search/async/{entityName}/{modelVersion}` | executes; no job issued on rejection | `400 INVALID_CONDITION` |
| `DELETE /entity/{entityName}/{modelVersion}` | executes | `400 INVALID_CONDITION` |
| `POST /entity/stats/{entityName}/{modelVersion}/query` | executes | `400 INVALID_CONDITION` |
| workflow / transition `criterion`, at workflow import | stored | `400 VALIDATION_FAILED` |

Unchanged and inherited, listed so the table is a complete checklist:

| Case | Code |
|---|---|
| path inside a `NOT` outside the grammar, or not declared by the model | `400 INVALID_FIELD_PATH` |
| operand inside a `NOT` not fitting the field's declared type | `400 CONDITION_TYPE_MISMATCH` |
| unrecognised group operator | `400 INVALID_CONDITION` |
| criterion naming an undeclared path, at evaluation (§ 5) | `400 WORKFLOW_FAILED`, rolled back |

**No new error code**, so no `errors/<CODE>.md` topic is owed.

Over gRPC a rejection is an envelope error with `success=false` and
`error.code = CLIENT_ERROR`, the message carrying the code above — never an
empty stream, which a client reads as "no matches".

## 11. Two evaluators stay two

There are two tree walks — `spi.Prepare`/`PreparedFilter.Match` over
`spi.Filter`, and `internal/match` over `predicate.Condition`. `NOT` is
implemented in both.

Merging them was considered and **rejected**. Since `de71289` the engine
evaluator already delegates path parsing, path resolution, operand expansion and
leaf evaluation to the SPI; what remains duplicated is a tree walk. Against that
small saving, `internal/match/prepared.go` guards a temporal meta field
(`creationDate` / `lastUpdateTime`) to never-match under a text or pattern
operator, where the kernel bridges it to RFC3339 and compares lexically. That
divergence is pinned as deliberate and permanent in
`internal/match/prepared_equivalence_test.go`, on the grounds that a workflow
criterion is validated once at import and then stored verbatim, so relaxing the
engine's guard *"would silently reactivate a pre-existing stored criterion's
dormant transition on a binary upgrade alone"*. Merging deletes that guard.

The alignment gate is the existing 200,000-case equivalence corpus, extended to
emit `NOT` groups.

## 12. `cyoda-go-spi#32` is dropped

Its two stated premises are false as of `de71289`, both re-checked by running the
code:

- *"Array-wildcard predicates cannot be expressed in `spi.Filter`"* — they can.
  A wildcard leaf is already existential.
- *"Every one of them takes the unbounded fallback"* — they do not. Both
  planners route them to the residual.

What remained of it was element correlation, which § 2 places out of scope: it
is a capability the system lacks, not a wrong answer it gives.

## 13. Documentation

| File | Change |
|---|---|
| `docs/cloud-parity/operator-semantics.md` | the `NOT` section: truth table, the universal-quantifier reading, the absent-field asymmetry, no-De-Morgan |
| `docs/cloud-parity/path-grammar.md` | § 8 clause table gains `NOT`; § 9 states that whole-tree path validation includes it |
| `docs/cloud-parity/` (new) | Cloud twin-alignment document for negation |
| `cmd/cyoda/help/content/search.md:147` | replace *"`NOT` is not supported"* |
| `api/openapi.yaml` | `GroupConditionDto` — describe `NOT` and its one-condition rule |
| `COMPATIBILITY.md` | commercial-backend obligations (§ 14) |
| `CHANGELOG.md` | the feature, and `### Breaking` for § 5 |

The specification documents are written to stand on their own. Cloud publishes
`NOT` with an unbounded `conditions` array in its own spec copy
(`docs/cyoda/openapi.yml`); cyoda-go leads this contract, so the alignment
document states the one-condition rule as the contract and names it as a
narrowing Cloud must adopt.

## 14. Commercial-backend obligations

1. **Validate filter paths through a `NOT` node.** A backend walking only
   `AND`/`OR` skips a `NOT`'s subtree. Enforced by the new `spitest` case, so it
   fails conformance at the next pin bump.
2. **Evaluate `FilterNot`** if it self-executes searches: exactly one child,
   inverting a two-valued result, and never matching for a malformed node.
3. **Do not push a `NOT` into a query** unless every leaf beneath it translates
   exactly. Leaving it residual is always correct.

## 15. Test coverage

Scenario × layer. "parity" is `e2e/parity`, run on memory, sqlite and postgres —
memory included deliberately, since it pushes nothing and the SQL backends push
some leaves, which is where they can differ.

| Scenario | unit | e2e (postgres) | parity | gRPC |
|---|---|---|---|---|
| `NOT` over a simple condition | ✓ | ✓ | ✓ | ✓ |
| `NOT` over an `AND` group | ✓ | ✓ | ✓ | — |
| `NOT` over an `OR` group | ✓ | ✓ | ✓ | — |
| `NOT(NOT(x))` | ✓ | ✓ | — | — |
| `NOT` over a wildcard path — the ∀ reading | ✓ | ✓ | ✓ | — |
| `NOT` vs the negative twin differ (§ 4.3) | ✓ | ✓ | ✓ | — |
| `NOT` over an absent field (§ 4.4) | ✓ | ✓ | ✓ | — |
| `NOT` over empty / null / absent list (§ 4.2) | ✓ | — | ✓ | — |
| `NOT(IS_NULL)` ≠ `NOT_NULL` on a wildcard path (§ 4.5) | ✓ | — | ✓ | — |
| `NOT(AND[])`, `NOT(OR[])` | ✓ | — | — | — |
| `conditions` = 0 → `400 INVALID_CONDITION` | ✓ | ✓ | — | ✓ |
| `conditions` ≥ 2 → `400 INVALID_CONDITION` | ✓ | ✓ | — | ✓ |
| bad path inside `NOT` → `400 INVALID_FIELD_PATH` | ✓ | ✓ | ✓ | ✓ |
| bad operand type inside `NOT` → `400 CONDITION_TYPE_MISMATCH` | ✓ | ✓ | — | — |
| `NOT` on delete-by-condition | ✓ | ✓ | ✓ | — |
| `NOT` in grouped stats | ✓ | ✓ | — | ✓ |
| `NOT` in a workflow criterion, evaluated on save | ✓ | ✓ | — | — |
| `NOT` criterion, `conditions` ≠ 1 → import `400 VALIDATION_FAILED` | ✓ | ✓ | — | — |
| criterion on an undeclared path fails the save (§ 5) | ✓ | ✓ | — | — |
| criterion comparing a container path to a scalar fails the save (§ 5) | ✓ | ✓ | — | — |
| criterion on a path a peer just added is **not** rejected (§ 5 refresh) | ✓ | ✓ | — | — |
| malformed `FilterNot` from a backend never matches (§ 9) | ✓ | — | — | — |
| `NOT` stays residual on both planners (§ 8) | ✓ | — | — | — |
| malformed path under `NOT` rejected — `spitest` | ✓ | — | ✓ | — |
| equivalence corpus emits `NOT` groups | ✓ | — | — | — |

Delete-by-condition carries the worst blast radius — a wrongly-true `NOT`
deletes rows — and is covered on every layer it reaches.

A transport test is not coverage until the production change has been reverted
under it and the test observed to fail. That check is required for each row
above, per the rollout's own standing rule.

## 16. Sequence

1. § 5 — criterion path membership. Prerequisite.
2. § 6 — plugin path validation through `NOT`, plus the `spitest` case.
   Prerequisite.
3. § 7 and § 9 — the SPI: `FilterNot`, its prepared evaluator, `groupToFilter`
   strictness, the wire translation.
4. Engine: `internal/match` `NOT` arm; search and workflow-import validators.
5. § 8 planner pin, equivalence corpus, parity scenarios.
6. § 13 documentation.

1 and 2 land before 3, so no window exists in which `NOT` is expressible while a
path inside it is unchecked.
