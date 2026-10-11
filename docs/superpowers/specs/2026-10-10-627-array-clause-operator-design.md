# #627 — the array clause carries an operator

## Problem

An array clause tests the elements of one array by position. Three definitions
of it disagree:

- **The spec** (`api/openapi.yaml`, `ArrayConditionDto`) declares `operatorType`
  and `value: [string]`.
- **The server** (cyoda-go-spi `predicate/parse.go` `parseArray`) reads only
  `values`, where `null` skips a position, and always compares with `EQUALS`
  (`condition_filter.go` `desugarArrayCondition`).
- **Cyoda Cloud** reads `operation` + `value: List<String>` and applies the one
  operator at every position (`TreeNodeConditionUtils.kt:433-453`). Cloud has no
  skip. It accepts the clause on REST search only.

The server ignores the fields the spec declares. A body in the spec's shape
therefore has no `values`. A clause with no `values` tests no position, and a
clause that tests nothing matches every entity. A conditional delete in the
spec's shape deletes every entity of the model.

Cloud's per-position operator is real API function. Removing it to align on
cyoda-go's shape would break the rule that a contract gap is never closed by
removing function. The clause therefore keeps both sides' function.

## The contract

```json
{"type":"array", "jsonPath":"$.scores[*]", "operatorType":"GREATER_THAN", "values":[50, null, 70]}
```

- **`jsonPath`** names the array's elements. It ends in `[*]`, and it contains no
  other `[*]` (see *Paths* below).
- **`operatorType`** is required. It is applied at every position that `values`
  tests. The accepted names are the 26 that a simple clause accepts. The parser
  also accepts the `operator` and `operation` aliases, exactly as it does for a
  simple clause.
- **`values`** has one entry per position, from index 0. An entry is a scalar, or
  a two-element array `[low, high]` for `BETWEEN`/`BETWEEN_INCLUSIVE`. `null` means
  "do not test this position".
- **Meaning:** an AND of one simple clause per tested position. The example
  means `$.scores[0] > 50` AND `$.scores[2] > 70`.
- **Operators with no operand** (`IS_NULL`, `NOT_NULL`): a non-null entry selects
  the position, and its value is not used. `[true, null, true]` with `IS_NULL`
  means "element 0 is null and element 2 is null".
- **At least one position must be tested.** A missing `values`, an empty one, and
  an all-`null` one are refused. A client that wants "match everything" sends an
  empty `AND` group, which stays valid.

### Paths

The trailing `[*]` is replaced by the position index: `$.scores[*]` gives
`$.scores[0]`, `$.scores[2]`. A path with an earlier wildcard, such as
`$.orders[*].lines[*]`, is refused. Each positional clause of such a path would
match in *any* order, so positions 0 and 2 could match in two different orders.
The clause would then return entities that no single `lines` array satisfies.
A client that wants one order's lines writes the outer index
(`$.orders[0].lines[*]`). A client that wants the uncorrelated meaning writes a
group of simple clauses.

## Where it applies

The clause is accepted wherever a condition is: direct search, async search,
conditional delete, the grouped-stats `condition`, the gRPC search and snapshot
search requests, and workflow and transition criteria. All of them share one
parser and one set of validators.

## Design

The array clause stays *sugar*: one rewrite turns it into simple clauses, and no
other code defines what it means.

1. **SPI `predicate.ArrayCondition`** gains `OperatorType string`. `parseArray`
   reads it with the same alias rule as `parseSimple` (one shared helper).
   `MarshalJSON` writes it as `operatorType`. Async search persists a condition
   as JSON and re-parses it on whichever node runs the job, so the round trip must
   be lossless.
2. **SPI `DesugarCondition`** uses the clause's operator for each positional
   simple clause. Its result for a clause with no tested position is no longer
   "match everything": `ConditionToFilter` and `Prepare` refuse such a clause, as
   `Prepare` already refuses a malformed `BETWEEN`. This is the backstop for any
   caller that skips validation.
3. **SPI validators** (`ValidateConditionOperators`, `ValidateConditionPatterns`)
   check the array clause through `DesugarCondition`, not through a case of their
   own that checks nothing.
4. **cyoda-go validators.** Each array-clause branch first checks the clause as a
   whole: the path (trailing `[*]`, no earlier wildcard) and that at least one
   position is tested. It then validates the rewritten simple clauses with the
   simple-clause rules. The branches are `search.validateConditionAtDepth`,
   `search.validateCriterionConditionAtDepth`, and `workflow.walkCriterion`. The
   type check (`condition_type_validate.go`) already goes through the rewrite, so
   `CONDITION_TYPE_MISMATCH` applies with no change.
5. **Workflow import refuses a criterion that does not parse.**
   `validateCriterion` returns `nil` when `ParseCondition` fails, so
   `{"type":"bogus"}`, `[1,2]` and `{"type":"simple","jsonPath":5}` import cleanly
   and then fail every save that reaches them (`engine.go` `evaluateCriterion`).
   Import now refuses them with `VALIDATION_FAILED`. Without this, an array
   criterion whose `values` is not an array would import cleanly.
   `TestValidateWorkflowStructure_UnparseableCriterionNotNewlyRejected` pinned the
   old behaviour and is inverted.

## The spec (`api/openapi.yaml`)

- `ArrayConditionDto`: properties `jsonPath`, `operatorType`, `values`;
  `required: [type, jsonPath, operatorType, values]`; `values.items` is a scalar
  or a two-element array, nullable; `minItems: 1`. `value` is removed. The
  `jsonPath` example becomes `$.scores[*]`, and the schema gets a whole-clause
  example.
- **The operator enum** on `ArrayConditionDto`, `SimpleConditionDto` and
  `LifecycleConditionDto` lists 62 names. The server accepts 26
  (`spi.OperatorNames`). The other 36 have always been refused with
  `INVALID_CONDITION`. `IS_CHANGED`/`IS_UNCHANGED` are documented as unsupported
  (`cyoda help predicates`). The three enums are cut to the 26. This is a
  spec-to-reality reconciliation under ADR 0003 Decision 7: the oasdiff gate
  gets exact `--err-ignore` entries, because no working client sends a refused
  operator.
- `go generate ./api` is re-run and the generated code committed.

## Errors

| Surface | Condition | Status / code |
|---|---|---|
| `POST /search/direct/{entityName}/{modelVersion}` | missing or unknown `operatorType` | 400 `INVALID_CONDITION` |
| same | `values` missing, empty or all `null` | 400 `INVALID_CONDITION` |
| same | an entry is an object, or a `BETWEEN` entry is not a two-element array | 400 `INVALID_CONDITION` |
| same | `values` is not an array (body does not parse) | 400 `BAD_REQUEST` |
| same | path without trailing `[*]`, or with an earlier `[*]` | 400 `INVALID_FIELD_PATH` |
| same | operator does not apply to the element's declared type | 400 `CONDITION_TYPE_MISMATCH` |
| `POST /search/async/{entityName}/{modelVersion}` | each row above | the same codes |
| `DELETE /entity/{entityName}/{modelVersion}` (with a condition) | each row above | the same codes |
| `POST /entity/stats/{entityName}/{modelVersion}/query`, `condition` | each row above | the same codes |
| gRPC `EntitySearchRequest` / `EntitySnapshotSearchRequest` | each validation row above | envelope `Success=false`, `Error.Code` `CLIENT_ERROR`, the code above in `Error.Message` |
| same | the condition does not parse (`values` not an array) | transport status `InvalidArgument` (as for any condition that does not parse) |
| `POST /model/{entityName}/{modelVersion}/workflow/import`, array criterion | any of the above | 400 `VALIDATION_FAILED` |
| same | criterion that does not parse at all | 400 `VALIDATION_FAILED` |

Happy path on every surface: 200, and only the entities whose tested positions
satisfy the operator.

## Test coverage

| Scenario | SPI unit | cyoda-go unit | e2e (postgres) | parity (all backends) | gRPC |
|---|---|---|---|---|---|
| parse reads operator + aliases; marshal → parse round trip | ✓ | | | | |
| rewrite uses the operator; null skips; unary operators | ✓ | ✓ (`internal/match`) | | | |
| no tested position refused by `ConditionToFilter`/`Prepare` | ✓ | | | | |
| operator / pattern validators reach the array clause | ✓ | ✓ | | | |
| non-EQUALS operator per position (GREATER_THAN, BETWEEN, IS_NULL) | | | ✓ direct search | ✓ pushed-down form and a form that is not pushed down (inside `NOT`) | ✓ |
| async search with an operator | | | ✓ | ✓ | ✓ snapshot search |
| conditional delete with an operator | | | ✓ | | |
| grouped stats with an array `condition` | | | ✓ | | |
| workflow criterion with an operator (fires / does not fire) | | ✓ | ✓ | | |
| each error row × each surface | | ✓ validators | ✓ one test per endpoint × code | | ✓ one per error class |
| unparseable criterion refused at import | | ✓ | ✓ | | |
| spec's own `ArrayConditionDto` example parses and validates | | ✓ (`api` package) | | | |

The existing array-clause tests and fixtures (`ArrayCondition{}` literals,
`e2e/parity/path_addressing.go`, `internal/e2e/workflow_criterion_array_clause_test.go`)
gain an operator.

## Documentation

- `cmd/cyoda/help/content/search.md` (ArrayCondition section and error causes),
  `predicates.md`, `workflows.md` (criteria).
- `docs/cloud-parity/path-grammar.md` §8 and `operator-semantics.md`; a new
  `docs/cloud-parity/array-clause.md` for the wire shape and what Cloud changes.
- `docs/workflow-schema-versioning.md`: within the unreleased 1.5 contract, two
  more tightenings. (4) An array criterion must carry `operatorType` and test at
  least one position, and must not have an earlier wildcard in its path. A
  criterion without an operator imported and worked as EQUALS, so this is an
  optional-to-required change. It stays in 1.5 and is not a MAJOR change, because
  no deployment of either tier exists. (5) A criterion that does not parse is
  refused at import. It never worked, because every evaluation failed. Both apply
  under every schema version. The entry's tightening count is corrected.
- `CHANGELOG.md` under `### Breaking`.
- `docs/cyoda/` is a byte-identical mirror of Cloud's spec and is not edited.

## Outside this repository

- **cyoda-go-spi:** PR into `main`, pseudo-pinned by cyoda-go; no tag before the
  release cut.
- **cyoda-go-cassandra (issue, must land before its v0.9.0 bump).** Its async
  search re-parses the stored condition with its own copy of the parser and the
  matcher (`search/executor.go`, `search/predicate/match.go` `matchArray`). That
  copy ignores `operatorType` and compares with EQUALS. Under v0.9.0 a
  `GREATER_THAN` array clause would return wrong rows with a 200. The plugin must
  use the SPI's parser and `DesugarCondition`. This is added to release-cut step 3
  in #651.
- **Cyoda Cloud (CaaS ticket).** Rename `value` to `values`. Accept `null` (skip),
  non-string scalars, and `[low, high]` pairs for `BETWEEN` (Cloud refuses range
  operators per position today). Refuse a missing operator and a clause that tests
  no position. Refuse an earlier wildcard in the path. Accept the clause in
  workflow criteria and gRPC search.
