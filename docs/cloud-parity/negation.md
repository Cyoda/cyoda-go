# Negation — Cloud twin-alignment spec

cyoda-go leads this contract. Cloud aligns to the same evaluation rules.

`NOT` is a group operator, alongside `AND` and `OR`. This document defines its
wire form, what it means, and where it diverges from a construction a caller
might reach for instead. `operator-semantics.md` and `path-grammar.md` define
the operator and the path underneath it; this document is what sits above
both.

## 1. Wire form

```json
{"type": "group", "operator": "NOT", "conditions": [ <exactly one condition> ]}
```

`conditions` must hold **exactly one** entry. The single entry may be any
condition, including a group, so `NOT(A AND B)` is written by nesting the
group inside the `NOT`. Nesting is unrestricted: `NOT(NOT(x))` is legal and
restores the inner condition's own answer.

Zero entries, or two or more, are rejected `400 INVALID_CONDITION` (`400
VALIDATION_FAILED` at workflow import). A bare list under `NOT` has two
defensible readings that disagree on the same data — "not both"
(`NOT(A AND B)`) and "neither" (`NOT(A) AND NOT(B)`) — and nothing in the
request says which was meant, so the request is refused rather than guessed.

`operator` is case-sensitive and exact, as `AND` and `OR` already are. `NOT`
counts as one level against `MaxConditionDepth` and the parser's own depth
cap, like any group.

## 2. Semantics

`NOT(c)` is true exactly when `c` is false. Two-valued; there is no third
truth value. This requires two things of `c`:

- Every leaf that reaches evaluation gives a correct answer for the stored
  value's type family, including when the operand cannot be satisfied by that
  family (`operator-semantics.md`).
- A leaf that cannot be evaluated at all — an undeclared path, an operand
  fitting no declared type, an uncompilable pattern — never reaches
  evaluation; it is refused before any entity is read (§ 7).

## 3. Truth table

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

**`NOT` over an empty list, an explicit null, or an absent field is true**,
because the inner leaf is false in every one of those three states
(`path-grammar.md` § 5).

## 4. Over a list, `NOT` is a universal quantifier

This is the one thing most likely to be read backwards:

```
NOT( $.tags[*] EQUALS "red" )     =  NO element equals "red"
    $.tags[*] NOT_EQUAL "red"     =  SOME element differs from "red"
```

`$.tags[*]` addresses every element, and a leaf on it holds when **some**
addressed value satisfies it (`path-grammar.md` § 3). `NOT` sits above that
existential and inverts the whole answer, which makes it universal: `NOT(c)`
holds only when *no* element satisfies `c`. `$.tags[*] NOT_EQUAL "red"` asks a
different question — it is still existential, just over the negative leaf.

For `{"tags":["red","blue"]}` the first is **false** and the second is
**true**. Both are useful questions; neither is a spelling of the other.

**`NOT(EQUALS)` is not `NOT_EQUAL`, and `NOT(IS_NULL)` is not `NOT_NULL`.**
`NOT` is never rewritten by De Morgan into a leaf's negative twin, and a
negated group is never distributed over its children — the rewrite changes
the answer, over a list (this section) and over an absent field (§ 5).

**`ALL(P)` — "every element satisfies P" — is not offered.** The only
candidate spelling, `NOT(some element satisfies ¬P)`, is unsound whenever the
list may hold a null element:

| Stored | `NOT($.tags[*] NOT_EQUAL "red")` reports "every tag is red" | Correct |
|---|---|---|
| `["red","blue"]` | false | correct |
| `["red"]` | true | correct |
| `["red",null]` | **true** | **wrong** |
| `[null]` | **true** | **wrong** |

The cause is § 5's absent-field asymmetry seen from inside the quantifier: a
negative operator does not match a null element, so the null is invisible to
the inner leaf and the outer `NOT` reads its absence as success. `NOT` sits
above the existential quantifier and cannot reach inside it to negate the
element predicate, so no rearrangement of the condition language fixes this.
Do not construct `ALL(P)` this way over a list that may contain a null.

## 5. Two asymmetries

**The absent-field asymmetry.** A missing or null value never matches any
binary operator, including a negative one (`operator-semantics.md` § 2). `NOT`
sits outside the operator and inverts its result, so on an entity lacking the
field:

| Condition | Result |
|---|---|
| `$.x EQUALS "A"` | no match |
| `$.x NOT_EQUAL "A"` | no match |
| `NOT($.x EQUALS "A")` | **match** |

This is deliberate, matching SQL/JSON's `!(@.x == "A")` versus `@.x != "A"`.

**The presence-test asymmetry.** On a wildcard path `IS_NULL` and `NOT_NULL`
are not complements (`path-grammar.md` § 5): an empty list, an explicit null,
and an absent field all present no elements, so both presence tests answer
false on all three. Therefore `NOT($.a[*] IS_NULL)` is **not** `$.a[*]
NOT_NULL`: over `{"a":[]}` the first is true and the second false. Ask about
the list itself with the bare path `$.a`, which separates the three states.

## 6. `FUNCTION` under `NOT`

Unchanged: the workflow engine dispatches a `function` clause only at a
criterion's top level, so one nested inside any group — `NOT` included —
fails the evaluation (`function-condition-search-rejection.md`). Search
rejects `function` outright on every surface, at any depth.

## 7. Errors

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
| criterion naming an undeclared path, at evaluation | `400 WORKFLOW_FAILED`, rolled back — see `unevaluable-criterion-fails-save.md` |

No new error code. Over gRPC a rejection is an envelope error with
`success=false` and `error.code = CLIENT_ERROR`, the message carrying the code
above — never an empty stream, which a client reads as "no matches".

## 8. Push-down cost

`NOT` is evaluated in memory on every backend; no query planner pushes it
down, on either SQL backend. A residual filter is also what gates a backend's
pushed-`LIMIT` fast path, so a condition containing a `NOT` is not bounded by
a pushed SQL `LIMIT` — it streams through the in-process kernel and stops once
the result-limit sentinel is reached, not once a backend's `LIMIT` clause is.
`AND[leaf, NOT(x)]` still pushes `leaf`; `OR[leaf, NOT(x)]` goes wholly
residual.

## 9. Filter-path validation reaches through `NOT`

A backend's own filter-path validator must recurse into `NOT`'s subtree, not
only `AND`/`OR`'s. A validator that only recognises the two conjunction/
disjunction branch operators leaves a `NOT`'s child unvalidated, and the
consequence is worse than under `AND`: an unvalidated malformed path becomes a
never-match leaf, and `NOT` inverts that into matches-everything — a superset
on search, and a destructive one on `DELETE /entity/...`. `path-grammar.md`
§ 9 states the resulting rule: a backend recurses on any node carrying
children, not on a fixed list of recognised branch operators.

## Cloud obligations

Cloud implements every section above. Two points need implementing rather
than inheriting, because cyoda-go's in-tree backends satisfy them through
shared code a self-executing backend does not run:

- **Evaluate `FilterNot`** if Cloud self-executes a search: exactly one
  child, inverting a two-valued result, failing rather than matching for a
  malformed node (`Children` of length 0 or ≥ 2, or a zero-`Op` child).
- **Do not push a `NOT` into a query** unless every leaf beneath it
  translates exactly. Leaving it residual is always correct; § 8 explains
  why almost nothing qualifies.

The cross-backend parity suite runs against every backend wired into it, so a
backend that has not implemented these fails on its next dependency update
rather than diverging silently.

## Test surface

- The wire form and its arity rule: `internal/domain/search/operators_test.go`
  (search) and `internal/domain/workflow/criterion_operator_test.go`
  (criterion, at import).
- The kernel: `FilterNot`, its arity guard and its universal-quantifier
  semantics, in `cyoda-go-spi`'s `prepared_filter_test.go` and
  `condition_filter_test.go`; the equivalence corpus extended to emit `NOT`
  groups in `prepared_filter_equivalence_test.go`.
- The engine's own evaluator, kept aligned to the SPI kernel:
  `internal/match/prepared_test.go` and
  `internal/match/prepared_equivalence_test.go`.
- Filter-path validation recursing through `NOT`:
  `plugins/memory/path_validation_test.go`,
  `plugins/sqlite/path_validation_test.go`,
  `plugins/postgres/path_validation_test.go`.
- Push-down stays residual: `plugins/sqlite/query_planner_test.go`,
  `plugins/postgres/query_planner_test.go`.
- Cross-backend parity, run through the full HTTP stack on every wired
  backend: `e2e/parity/negation.go`, registered in `e2e/parity/registry.go`.
- End to end, on a running backend and over both entry points:
  `internal/e2e/search_group_operator_not_test.go`,
  `internal/e2e/workflow_criterion_not_arity_test.go`,
  `internal/e2e/workflow_criterion_undeclared_field_test.go`,
  `internal/grpc/search_group_operator_not_test.go`.
