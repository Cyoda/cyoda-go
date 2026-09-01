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
2. **An unsatisfiable comparison answers correctly** (§ 4.2) — today
   `$.n NOT_EQUAL 12.5` on an integer field returns no rows where it should
   return every entity holding a number. A wrong answer in its own right, and one
   `NOT` would compound.
3. **Criterion validation at evaluation** (§ 5) — a defect in its own right:
   today a misspelled field in a criterion silently never fires the transition.
4. **Filter-path validation through a `NOT` node** in all three storage plugins
   (§ 6) — a prerequisite: without it a malformed path inside a `NOT` reaches a
   backend unchecked.
5. Strict rejection of unknown group operators in the shared translator (§ 7).

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

`NOT(c)` is **true exactly when `c` is false**. Two-valued. No third truth value.

Two separate things have to be true for that to be sound, and neither is true
today:

- Every leaf that reaches evaluation must give a **correct** answer, not merely
  an answer. § 4.2 fixes a class where the answer is wrong.
- A leaf that cannot be evaluated at all must never reach evaluation. § 4.3 lists
  those and where each is refused.

### 4.2 An unsatisfiable comparison is an answer, and it is currently wrong

When an operand cannot be satisfied by the type of the value actually stored,
the comparison is **unsatisfiable for that value** — *no value of this type can
satisfy this comparison*. That is a determinate answer about the entity, not a
failure to evaluate.

`EvalLeaf` answers **false** in that situation whatever the operator. That is
right for a positive operator and wrong for a negative one. Measured, field
declared `INTEGER`, entity `{"n":5}`:

| Leaf | Answers | Correct |
|---|---|---|
| `$.n EQUALS 12.5` | false | false ✓ |
| `$.n NOT_EQUAL 12.5` | **false** | **true** ✗ |

`$.n NOT_EQUAL 12.5` returns no rows. It should return every entity holding a
number there, because 5 is not 12.5. PostgreSQL agrees: `select 5::int <> 12.5`
is `t`. A live wrong answer with no `NOT` involved.

**The fix is per stored-value type family, not per expansion.** `Expansion.void`
is the wrong granularity: it is set only when **every** declared type dropped the
operand (`eval_leaf.go:208`). A field declared
`[INTEGER, String]` is not void for the operand `12.5`, because the string branch
accepts it — yet the numeric branch still dropped, and the same wrong answer
survives. Measured:

| Declared | `$.n EQUALS 12.5` | `$.n NOT_EQUAL 12.5` | void? |
|---|---|---|---|
| `[INTEGER]` | false | **false** ✗ | yes |
| `[INTEGER, String]` | false | **false** ✗ | **no** |

Polymorphic declared sets are ordinary here — the model grows a type set from
written data, the sqlite planner has a rule keyed on `len(f.Declared) > 1`, and
the equivalence corpus already generates `{Integer, String}`.

So the rule is: **at evaluation, when the stored value's own type family has no
surviving sub-condition, answer by operator polarity** — false for a positive
operator, true for a negative one. That subsumes the whole-expansion case, since
a void expansion is one where every family dropped.

Unchanged and load-bearing: `operator-semantics.md` § 2 still applies first — a
null or absent value never matches any binary operator, negatives included. The
two presence tests carry no operand and are unaffected.

**Which operators this reaches is settled by test, not asserted here.** Only the
six comparison operators can currently produce a whole-expansion `void`
(`expandCompare` is the sole writer; the string and pattern operators take a
different path and `expandBetween` errors rather than voiding). At per-family
granularity the negative string operators come into scope as well. The
implementation determines the reachable set operator by operator, with a test per
operator, rather than working from a list written in advance.

**This is not a rejection.** `$.n EQUALS 12.5` is a well-formed question whose
answer is "no", and PostgreSQL answers `f` rather than erroring. Nor does it
touch `EQUALS "2024"` on a date-declared field: that operand floors to
`2024-01-01` and the comparison runs. The imprecise-`EQUALS` drop applies in the
other direction only — a precise operand against a coarser declared field, such
as `Year EQUALS "2024-09-09"`.

### 4.3 What genuinely cannot be evaluated, and where each is refused

Three things leave a leaf unable to answer at all. Each is decided when the query
is prepared, from the condition alone, before any entity is read.

| Cause | Refused where |
|---|---|
| the operand fits no declared type of the field | `400 CONDITION_TYPE_MISMATCH` at the condition boundary — already the case for search; § 5 adds it for criteria |
| the path is not declared by the model | `400 INVALID_FIELD_PATH` at the condition boundary — already the case for search; § 5 adds it for criteria |
| a pattern operand will not compile | `400 INVALID_CONDITION` — already the case for search; § 5 adds it for criteria |

So the whole gap is the **criterion** surface, and § 5 closes it. The search
surfaces already refuse all three.

**One conformance rule is wrong and must be inverted.** `spitest`'s
`Searcher/Pattern/MalformedLike` requires a backend's `Search` to return **no
error and no rows** for a malformed `LIKE` operand, justified as *"rejecting it
with a 400 is the request boundary's job, above the Searcher."*

That reasoning does not hold. "The boundary already refuses this" is a reason the
case should be unreachable. It is not a reason to require the wrong answer
underneath, which is what pinning an empty page does. Leniency belongs at
workflow **import**, where a criterion is stored before it is checked against a
model. Nothing about query execution is lenient. `Search` must return an error,
and § 14 carries it as a backend obligation.

### 4.4 What callers see change

| Request | Today | After |
|---|---|---|
| `$.n NOT_EQUAL 12.5`, `n` declared `INTEGER` | no rows | every entity holding a non-null value at `n` whose type family cannot satisfy the operand |
| `$.n EQUALS 12.5`, `n` declared `INTEGER` | no rows | unchanged |
| `$.d EQUALS "2024"`, `d` declared `LocalDate` | matches `2024-01-01` | unchanged |
| criterion naming a field the model does not declare | transition silently does not fire | save aborted and rolled back (§ 5) |
| malformed `LIKE` reaching a backend `Search` | empty page | error |

Declared `### Breaking`: the first row returns rows where it returned none, and
the fourth stops a save that succeeds today.

### 4.5 Truth table

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

### 4.6 Over a list, `NOT` is a universal quantifier

This is the capability the feature adds, and the one thing a caller is most
likely to get backwards:

```
NOT( $.tags[*] EQUALS "red" )     =  NO element equals "red"
    $.tags[*] NOT_EQUAL "red"     =  SOME element differs from "red"
```

For `{"tags":["red","blue"]}` the first is **false** and the second is **true**.
They are different questions, both useful, and neither is a spelling of the
other.

**`ALL(P)` has no correct spelling, and this document does not offer one.** The
obvious construction — `NOT(some element satisfies ¬P)` — is unsound whenever the
list may hold a null. Measured, for `NOT($.tags[*] NOT_EQUAL "red")`:

| Stored | Reports "every tag is red" | Correct |
|---|---|---|
| `["red","blue"]` | false | ✓ |
| `["red"]` | true | ✓ |
| `["red",null]` | **true** | ✗ |
| `[null]` | **true** | ✗ |

The cause is § 4.7's asymmetry seen from the inside: a negative operator does not
match a null element either, so the null is invisible to the inner test and the
outer `NOT` reads its absence as success. `NOT` sits above the existential and
cannot negate the element predicate, so no rearrangement fixes it. See § 12.

### 4.7 The absent-field asymmetry

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

### 4.8 `NOT` and the presence tests

On a wildcard path `IS_NULL` and `NOT_NULL` are not complements
(`path-grammar.md` § 5) — an empty list, an explicit null and an absent field
all present no elements, so both answer false. Therefore `NOT($.a[*] IS_NULL)`
is **not** `$.a[*] NOT_NULL`: over `{"a":[]}` the first is true and the second
false. Ask about the list itself with the bare path `$.a`, which separates the
three states.

### 4.9 No De Morgan rewriting

`NOT(EQUALS)` must never be normalised into `NOT_EQUAL`, nor a negated group
distributed over its children. §§ 4.3 and 4.4 are the two places the rewrite
changes answers. This is stated because the rewrite looks like an obvious
simplification.

### 4.10 A `FUNCTION` clause under `NOT`

Unchanged and consistent with today: the workflow engine dispatches a `FUNCTION`
clause only at a criterion's top level, so one nested inside any group — `NOT`
included — fails the evaluation, as `function-condition-search-rejection.md`
already states. Search rejects `FUNCTION` outright on every surface.

### 4.11 Depth

A `NOT` is one level, counting against `MaxConditionDepth` and the parser's own
depth cap like any group.

## 5. A criterion is checked against the model before it is evaluated

**Ruled by @pschleger.** A query never executes against a field the model does
not declare. There is no such thing as an undeclared field to query. A criterion
is imported without asserting model compliance, because a model may legitimately
be declared later; but when that criterion is **evaluated** and the model does
not declare its field, the workflow transaction is **aborted and rolled back**.

This applies to **all 26 operators**, not only the eight that need a declared
type. The eight are merely where the current behaviour is visibly wrong; the rule
is about what may execute, not about which operators happen to misbehave.

### The defect today

A criterion's paths are never checked for membership in the model, at either end.

- At **import**, `walkCriterion` checks path grammar, operator names, lifecycle
  type-soundness and regex operands. Not membership. This part is correct and
  stays: a criterion may name a field the model has not been given yet.
- At **evaluation**, `evaluateCriterion` loads the model, and a load failure
  already fails the save closed. But the model is consulted only for *typing*:
  `if fd, ok := fields[p]; ok { return fd.Types }; return nil`. An undeclared path
  silently yields no declared types and the criterion answers non-match.

Measured, with no `NOT` involved:

| Model | `$.amount GREATER_THAN 3` over `{"amount":5}` | Truth |
|---|---|---|
| declares `amount` | true | true |
| does not declare `amount` | **false** | true |

So a misspelled field name in a criterion means the transition silently never
fires, and the save reports success. This is the defect class
`unevaluable-criterion-fails-save.md` closed for an unrecognised *operator* and
left open for everything else.

### The fix

Criterion evaluation applies the checks the other condition surfaces already
apply, and aborts the transaction when any of them refuses:

- `search.ValidateKnownPaths` — every data path is declared by the model. It
  carries the single bounded schema refresh `path-grammar.md` § 6 requires, so a
  field a peer node has just added is not falsely refused by a node holding an
  older cached schema (`.claude/rules/multi-node-primary.md`).
- `search.ValidateConditionValueTypes` — the operand fits a declared type, and a
  container path is not compared to a scalar.
- `search.ValidatePatterns` — pattern operands compile. Import-time validation
  does not reach a workflow stored before that validation existed, and
  `ExpandLeaf` swallows a compile failure.

Failure is `400 WORKFLOW_FAILED`, rolled back — no entity write, no state
transition, no partial effect — matching the existing unevaluable-criterion
contract.

### Consequences, stated rather than discovered

**A field that no entity has written must be declared in the model.** The model
grows automatically from written data, so an optional field that nothing has ever
carried is not declared by that route. It is declared explicitly through
`POST /model/import/...`, which sets the schema directly. A criterion on such a
field is supported; declaring the field is the modelling step that makes it so.

**This reverses `path-grammar.md` § 7.** That section records that a criterion is
checked at import and not at evaluation, arguing that an evaluation-time
rejection "would fail a save, repeatedly, for every entity … and report the fault
to a caller who cannot fix it". The same rationale sits in
`internal/domain/workflow/validate.go`. It is right that leniency belongs at
import and wrong that it extends to evaluation: the alternative it chose is not
"no rejection" but "silently answer the wrong thing". § 7's table row and its
subsection are rewritten, not contradicted; § 13 carries the edit.

**Cost.** `ValidateConditionValueTypes` takes a `*schema.ModelNode`, not the
fields map `evaluateCriterion` already builds, so the criterion path needs the
same second model read conditional delete performs. `ValidateKnownPaths` issues
one bounded refresh on a miss, inside the write transaction. Two rules follow:

- **Gate the model READ, never the validation call.** A lifecycle-only criterion
  stays answerable without the schema (`path-grammar.md` § 6), so
  `ConditionFieldPaths` returning empty skips the model load. It must **not** skip
  `ValidateConditionValueTypes`, which is called with a nil model in that case —
  the pattern grouped stats already uses. Skipping the call outright would leave
  `validateLifecycleType` unreached, and that is the one check that refuses a
  text operator on a temporal meta field. `NOT( creationDate CONTAINS "2024" )`
  carries no data path, so a gate on the whole block would let it through to the
  § 11 guard's never-match and let the `NOT` invert it into matches-everything —
  reintroducing precisely the fail-open this section closes.
- **Preserve the existing precedence.** `evaluateCriterion` documents that an
  infra failure wins over a structural fault. The new reads must not reorder
  that; an infra failure on either read stays an infra failure.

**It closes a fail-open that § 11's permanent guard would otherwise create.**
`internal/match` deliberately answers never-match for a text or pattern operator
on a temporal meta field. That is a semantic non-match, not a failure — and a
`NOT` above it would invert it into matches-everything. The predicate is refused
by `validateLifecycleType`, which every *validated* entry point already crosses
and which a stored criterion did not. Once criterion evaluation runs these
checks, the guard becomes unreachable rather than invertible, and § 11's
divergence stays pinned without becoming a hole.

**Behaviour change to declare.** A stored criterion naming a field the model does
not declare evaluates to "not satisfied" today and the save succeeds. It will
abort the save. `### Breaking`.

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

`path-grammar.md` § 9 requires a backend to reject a filter path outside the
grammar rather than answer an empty result set. It does **not** state that
validation must walk the whole `Filter` tree — no document does. The nearest
written rule is § 8's clause table, which governs the wire boundary, not a
plugin's `spi.Filter` tree.

**§ 9 must therefore add the whole-tree rule**, not merely note that it covers
`NOT`. § 13 carries it as an addition. The consequence is worse under `NOT` than
under `AND`: an unvalidated malformed path becomes a never-match leaf, and `NOT`
inverts that into matches-everything — a superset on search, and on
`DELETE /entity/...` a destructive one.

The conformance suite does not catch it: `spitest`'s nested malformed-path case
is built as a `FilterAnd` only, so no backend — the commercial one included —
would fail on this at its next pin bump.

**Fix.** All three validators recurse through `FilterNot`, and `spitest` grows a
malformed path nested under a `NOT`.

Recurse on **any node carrying children**, rather than adding `FilterNot` to the
`case` list. Naming each branch operator reproduces this defect for the next one
added; the property that matters is "this node has a subtree", not which operator
it is.

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

**The cost is real and is named rather than left implicit.** A residual filter is
also what gates the SQL `LIMIT` fast path — it is pushed only when the plan has no
post-filter. So a query containing a `NOT` streams the model through the kernel
and stops only once enough *matches* accumulate, rather than bounding in SQL. It
stays bounded by the result-limit sentinel, not unbounded, but given this
milestone's search-bounding history it is stated here and in the help topic
rather than discovered. It matters most on `DELETE /entity/...`.

A planner test pins that `NOT` stays residual, so a later change to `isPushable`
cannot silently make it pushable without the soundness rule being written first.

## 9. Representation

`spi.Filter` gains `FilterNot`, a branch node with exactly one child and an
empty `Path`.

**`spi.Prepare` gains an error return**, and the arms that currently swallow a
failure into a never-match leaf return it instead:

- `prepared_filter.go`'s `!n.expanded` arm — reached from an `ExpandLeaf` error,
  a `ParseFilterPath` failure, and an empty `SourceData` leaf path.
- a pattern operand that will not compile. The swallow for this one is inside
  `ExpandLeaf`, not `Prepare` (`eval_leaf.go:141-146`, which returns a nil error
  and a nil matcher), so `Prepare` has nothing to propagate today. `Prepare`
  calls `ValidateLeafPattern` itself rather than changing `ExpandLeaf`'s
  contract, which has other callers.

An unsatisfiable comparison is **not** in this list. It is a determinate answer,
not a failure, and § 4.2 fixes how it is evaluated rather than rejecting it.

**`internal/match` carries the same swallows and gets the same treatment.**
Hardening only the SPI kernel would leave the evaluator that serves every
workflow criterion and every residual, which is where a `NOT` arm is also being
added. It has four:

| Site | Swallow |
|---|---|
| `leafNode` | operand parses into no declared type, or a malformed range arity |
| `prepareSimple` | empty path |
| `prepareSimple` | path outside the grammar |
| `prepareLifecycle` | the temporal-meta guard (§ 11) |

The first three become errors, exactly as their SPI counterparts do. The fourth
is different in kind — it is a deliberate semantic non-match, not a failure — and
§ 5 is what stops a `NOT` inverting it, by refusing the predicate before
evaluation.

The third site's own comment justifies the swallow as *"the path is validated at
the boundary before a condition ever reaches this evaluator"*. That is the same
reasoning § 4.3 rejects for `spitest`: the boundary already refusing something is
a reason the case should be unreachable, not a reason to require the wrong answer
underneath it. The asymmetry is removed rather than explained.

This is a breaking SPI signature change, and the ripple is wider than the
signature: `spi.Prepare` has **seven** non-test call sites across the three
plugins — `memory/searcher.go`, `memory/grouped_stats.go` (×2),
`sqlite/searcher.go`, `sqlite/query_planner.go`, `sqlite/grouped_stats.go`,
`postgres/query_planner.go` — all inside functions that return no error today. `planFor`, `planQuery` and the
grouped-aggregator constructors each grow an error return, and their callers with
them. **No SPI tag is cut for it.** `MAINTAINING.md` tags at end-of-milestone
only; while `v0.8.4` is in flight cyoda-go pins a pseudo-version against SPI
`main`, so each SPI change merges and the four `go.mod` pins advance. The change
is recorded in the SPI `CHANGELOG.md`'s `[Unreleased]` section and narrated in
`COMPATIBILITY.md`'s `v0.8.4` row when the pin moves. #516 row 14 cuts the single
tag once every `v0.8.4` SPI change has merged.

**A malformed `FilterNot` is an error, not a false.** `Filter` is a public struct
any backend may build. A `FilterNot` whose `Children` length is not exactly 1, or
whose child is a zero-`Op` leaf, fails `Prepare` — never an unguarded
`Children[0]`, never "invert the AND of the children", and never a plain false
that an enclosing `NOT` could invert into a match.

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

**No new error code**, so no `errors/<CODE>.md` topic is owed. Four existing error
topics change text — see § 13.

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

**The gap this leaves is stated at full width.**

`ALL(P)` is unavailable **for all 26 operators** whenever a list may contain a
null element, because the only construction available — `NOT(some element
satisfies ¬P)` — reports success for a list of nulls (§ 4.6, measured). `NOT`
sits above the existential quantifier and cannot reach inside it to negate the
element predicate.

A quantifier node carrying its own child predicate — the thing `#32` proposed —
would close this, because a `NOT` could then sit *inside* the quantifier, below
the element iteration. Dropping `#32` therefore accepts a real gap, not a
theoretical one.

Accepted on that basis: `ALL` over a list is not offered at all, rather than
offered with a construction that is wrong on null elements. § 4.6 documents its
absence rather than publishing the broken recipe. If `ALL` is wanted later, it
needs the quantifier node, and this is the argument to reopen.

## 13. Documentation

**Contract — `docs/cloud-parity/`**

| File | Change |
|---|---|
| `negation.md` (new) | what `NOT` is and how it evaluates: truth table, the one-condition rule, the ∀ reading over a list, the two asymmetries, the errors |
| `operator-semantics.md` | the `NOT` section; § 4.2's per-family rule for an unsatisfiable comparison; § 4 qualified where it says the eight answer non-match |
| `path-grammar.md` | § 7's criterion row moves from "no model check" to checked at evaluation, and its subsection is rewritten (§ 5); § 8's clause table gains `NOT`; § 9 **adds** the whole-tree filter-path rule (§ 6); § 11 gains the criterion-at-evaluation row |
| `unevaluable-criterion-fails-save.md` | § 5 extends its invariant from an unrecognised operator to an undeclared path |
| `README.md` | its contents table indexes every file in the folder |

**CLI help — `cmd/cyoda/help/content/`**

| File | Change |
|---|---|
| `search.md` | `:144` calls `AND`/`OR` "the only supported values" and `:147` says *"`NOT` is not supported"*. Both replaced: the operator set, the one-condition rule, the ∀ reading, the two asymmetries. Plus § 8's cost — a condition containing a `NOT` is not bounded by a pushed SQL `LIMIT` |
| `predicates.md` | applies to search and criteria alike; carries the negative-operator null rule § 4.7 qualifies and the operator table § 4.2 changes |
| `crud.md:474` | names the grouped-stats clause set as "`GroupCondition` with `AND`/`OR`" |
| `analytics.md:228` | describes `expressionCondition` as "AND/OR groups" |
| `workflows.md` | § 5's behaviour change: a criterion naming an undeclared path aborts and rolls back the save |
| `workflows/schema-version.md` | the MINOR bump for `NOT` in a criterion |
| `errors/INVALID_CONDITION.md` | `:22`'s cause list gains `NOT` with `conditions` ≠ 1 |
| `errors/VALIDATION_FAILED.md` | a criterion `NOT` with `conditions` ≠ 1, refused at import |
| `errors/WORKFLOW_FAILED.md` | its cause list gains a criterion naming an undeclared path |
| `errors/CONDITION_TYPE_MISMATCH.md` | § 4.2 changes when a comparison answers; confirm the text still holds |

No new error code, so no `errors/<CODE>.md` is added and `TestErrCode_Parity`
needs nothing.

**Everything else**

| File | Change |
|---|---|
| `api/openapi.yaml` | `GroupConditionDto` — describe `NOT` and its one-condition rule |
| `docs/workflow-schema-versioning.md` | **Gate 4, two separate answers.** `NOT` in a criterion widens the `WorkflowConfigurationDto` accepted-input set — the doc names "new condition operator" as a MINOR example, so it is a bump. § 5 is evaluation-time only and changes no import validation, acceptance rule or export shape, so it is not. Record both; do not merge them |
| `docs/cyoda/cloud-divergences.md` | it has no `GroupConditionDto` / `NOT` row today although one was owed; add it and strike it in the same change |
| `internal/domain/workflow/validate.go` | its doc comment carries the "import is the only boundary a criterion crosses" rationale § 5 reverses |
| `COMPATIBILITY.md` | the `v0.8.4` row: the obligations of § 14, narrated when the pin moves |
| `CHANGELOG.md` | the feature, and `### Breaking` for § 4.4 and § 5 |
| `cyoda-go-spi/CHANGELOG.md` | `[Unreleased]`: `FilterNot`, the breaking `Prepare` error return, `groupToFilter` strictness, the inverted `MalformedLike` case |

`docs/cloud-parity/` documents state the contract and nothing else: what the rule
is, what it accepts, what it rejects, and the tables a reader needs. No
comparison against Cloud, no alignment task, no account of how a rule was arrived
at, no record of what a document previously said. The same applies to the help
topics.

## 14. Commercial-backend obligations

Tracked as an issue in the commercial backend's own repository, per the
cross-repo rule. `spitest` is what enforces them: each fails conformance at that
backend's next pin bump.

1. **Validate filter paths through a `NOT` node** (§ 6). A backend walking only
   `AND`/`OR` skips a `NOT`'s subtree.
2. **Evaluate `FilterNot`** if it self-executes searches: exactly one child,
   inverting a two-valued result, failing rather than matching for a malformed
   node.
3. **Do not push a `NOT` into a query** unless every leaf beneath it translates
   exactly. Leaving it residual is always correct.
4. **`Search` fails on an unevaluable operand** rather than returning an empty
   page (§ 4.3), and the refusal carries a sentinel
   `search.ClassifyStoreQueryError` maps — otherwise it surfaces as `500` and
   contradicts § 10.
5. **Answer an unsatisfiable comparison by operator polarity** (§ 4.2), per
   stored-value type family, rather than false for every operator.

`COMPATIBILITY.md`'s `v0.8.4` row carries the same list.

## 15. Test coverage

Scenario × layer. "parity" is `e2e/parity`, run on memory, sqlite and postgres —
memory included deliberately, since it pushes nothing and the SQL backends push
some leaves, which is where they can differ.

| Scenario | unit | e2e (postgres) | parity | gRPC |
|---|---|---|---|---|
| `NOT` over a simple condition | ✓ | ✓ | ✓ | ✓ |
| `NOT` over an `AND` group | ✓ | — | ✓ | — |
| `NOT` over an `OR` group | ✓ | — | ✓ | — |
| `NOT(NOT(x))` | ✓ | — | — | — |
| `NOT` over a wildcard path — the ∀ reading | ✓ | — | ✓ | — |
| `NOT` vs the negative twin differ (§ 4.6) | ✓ | — | ✓ | — |
| `NOT` over an absent field (§ 4.7) | ✓ | — | ✓ | — |
| `NOT` over empty / null / absent list (§ 4.8) | ✓ | — | ✓ | — |
| `NOT(IS_NULL)` ≠ `NOT_NULL` on a wildcard path (§ 4.8) | ✓ | — | ✓ | — |
| `NOT(AND[])`, `NOT(OR[])` | ✓ | — | — | — |
| a `FUNCTION` clause nested inside a `NOT` is rejected (§ 4.10) | ✓ | ✓ | — | — |
| a `NOT` counts one level against both depth caps (§ 4.11) | ✓ | ✓ | — | — |
| **unsatisfiable comparison, negative operator**: `$.n NOT_EQUAL 12.5` on `INTEGER` returns rows (§ 4.2) | ✓ | — | ✓ | — |
| — the positive twin `$.n EQUALS 12.5` still returns none (§ 4.2) | ✓ | — | ✓ | — |
| — null and absent still match neither (§ 4.2, `operator-semantics.md` § 2) | ✓ | — | ✓ | — |
| — a **polymorphic** declared set, e.g. `[INTEGER, String]`, gets the same fix (§ 4.2) | ✓ | — | ✓ | — |
| — the reachable operator set, one test per operator (§ 4.2) | ✓ | — | — | — |
| — `EQUALS "2024"` on a `LocalDate` field is unchanged, not rejected (§ 4.4) | ✓ | ✓ | — | — |
| `ALL(P)` via the negative twin is wrong on a null element (§ 4.6) — pinned so the recipe is not published later | ✓ | — | — | — |
| operand fits no declared type in a criterion → save aborted (§ 5) | ✓ | ✓ | — | — |
| uncompilable pattern in a stored criterion → save aborted (§ 5) | ✓ | ✓ | — | — |
| `Search` **errors** on an unevaluable operand — `spitest`, inverted (§ 4.3) | ✓ | — | ✓ | — |
| — and the error classifies as `400`, not `500` (§ 14.4) | ✓ | ✓ | — | ✓ |
| `spi.Prepare` returns an error rather than a never-match leaf (§ 9) | ✓ | — | — | — |
| — all seven plugin call sites propagate it (§ 9) | ✓ | — | — | — |
| `internal/match`'s three failure swallows become errors (§ 9) | ✓ | ✓ | — | — |
| `conditions` = 0 → `400 INVALID_CONDITION`, **each of the four condition surfaces** | ✓ | ✓ | — | ✓ |
| `conditions` ≥ 2 → `400 INVALID_CONDITION`, **each of the four condition surfaces** | ✓ | ✓ | — | ✓ |
| unrecognised group operator → `400 INVALID_CONDITION` | ✓ | ✓ | — | ✓ |
| `groupToFilter` errors on an unknown group operator (§ 7) | ✓ | — | — | — |
| bad path inside `NOT` → `400 INVALID_FIELD_PATH` | ✓ | — | ✓ | — |
| bad operand type inside `NOT` → `400 CONDITION_TYPE_MISMATCH` | ✓ | ✓ | — | ✓ |
| `NOT` on delete-by-condition | ✓ | ✓ | ✓ | — |
| `NOT` in grouped stats | ✓ | ✓ | — | n/a |
| `NOT` in a workflow criterion, evaluated on save | ✓ | ✓ | — | — |
| `NOT` criterion, `conditions` ≠ 1 → import `400 VALIDATION_FAILED` | ✓ | ✓ | — | — |
| criterion on an undeclared path aborts and rolls back the save (§ 5) | ✓ | ✓ | — | — |
| — for a Group B operator too, e.g. `IS_NULL` (§ 5, all 26 operators) | ✓ | ✓ | — | — |
| — a lifecycle-only criterion still evaluates with no model read (§ 5) | ✓ | ✓ | — | — |
| — a temporal meta field under a text operator is refused, not inverted (§ 5) | ✓ | ✓ | — | — |
| criterion comparing a container path to a scalar fails the save (§ 5) | ✓ | ✓ | — | — |
| criterion on a path a peer just added is **not** rejected (§ 5 refresh) | ✓ | ✓ | — | — |
| malformed `FilterNot` from a backend fails `Prepare` (§ 9) | ✓ | — | — | — |
| `NOT` stays residual on both planners, and `LIMIT` is not pushed (§ 8) | ✓ | — | — | — |
| malformed path under `NOT` rejected — `spitest` | ✓ | — | ✓ | — |
| `FilterNot` **evaluated** through every search entry point — `spitest` (§ 14.2) | ✓ | — | ✓ | — |
| equivalence corpus emits `NOT` groups, and its temporal-meta skip propagates through the new node (§ 11) | ✓ | — | — | — |

Delete-by-condition carries the worst blast radius — a wrongly-true `NOT`
deletes rows — and is covered on every layer it reaches.

**One waiver, per `.claude/rules/test-coverage.md`.** Grouped stats has no gRPC
cell because it has no gRPC entry point: the service is reached only from its
HTTP handler, and the proto surface carries no grouped-stats method.

Related, and worth stating because `NOT` raises its cost: grouped stats performs
its **model-dependent** validation in the HTTP handler rather than the service,
so a direct caller of the service gets only the model-independent half. Under
`NOT` that asymmetry turns an empty page into a superset. Not fixed here; named
so it is not mistaken for coverage.

A transport test is not coverage until the production change has been reverted
under it and the test observed to fail. That check is required for each row
above, per the rollout's own standing rule.

## 16. Sequence

1. **Fix how an unsatisfiable comparison answers** (§ 4.2), and **make an
   unevaluable one an error** (§ 4.3, § 9): `spi.Prepare` gains an error, its
   eight plugin call sites propagate it, the classifier gains the sentinels, and
   `spitest`'s `MalformedLike` case is inverted. Lands and is verified with no
   `NOT` in the language yet — each is a defect fix on its own terms.
2. § 5 — criterion validation at evaluation.
3. § 6 — plugin path validation through `NOT`, plus the `spitest` path case.
4. § 7 and § 9 — the SPI: `FilterNot`, its prepared evaluator, `groupToFilter`
   strictness, the wire translation, the `spitest` evaluation cases (§ 14.2).
5. Engine: `internal/match` `NOT` arm; the search validator and the criterion
   validator.
6. § 8 planner pin, equivalence corpus extended to emit `NOT`, parity scenarios
   registered in `e2e/parity/registry.go`.
7. § 13 documentation.

**Where the arity check lives is load-bearing.** It must go in
`search.ValidateCondition` / `ValidateCriterionCondition`, beside the existing
`AND`/`OR` rejections — **not** in `predicate.ParseCondition`. `validateCriterion`
returns nil when parsing fails, so an arity check in the parser would make a
malformed `NOT` criterion import with `200` and then fail every subsequent save
on that transition, permanently, instead of being refused at import as § 10
states.

**Two SPI merges, no tags.** Steps 1–2 and step 5 each merge to SPI `main` and
each advance cyoda-go's pseudo-pin; neither cuts a tag. Ordering still matters
for a consumer that re-pins between them: the earlier pin carries the `Prepare`
error and the inverted conformance case but no `FilterNot`, so nothing can yet
ask it to negate anything. Step 5's pin adds `FilterNot` together with the § 14
conformance cases, so the node and the cases that fail an unimplemented backend
arrive in the same pin.

The guarantee is not that no window exists, but that the window contains no
negation.
