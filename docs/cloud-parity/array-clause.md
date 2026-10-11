# The array clause

cyoda-go defines the array clause's wire shape; Cyoda Cloud aligns to it.

## The contract

```json
{"type":"array", "jsonPath":"$.scores[*]", "operatorType":"GREATER_THAN", "values":[50, null, 70]}
```

- `operatorType` is required and is applied at every tested position. The
  accepted names are those of a `simple` clause. `operator` and `operation`
  are accepted as aliases.
- `values` holds one entry per position from index 0. `null` does not test
  that position. An entry is a scalar, or a `[low, high]` pair for `BETWEEN`
  and `BETWEEN_INCLUSIVE`. For `IS_NULL`/`NOT_NULL` a non-null entry only
  selects the position.
- The clause means an AND of one `simple` clause per tested position, and
  every `simple`-clause rule applies to those clauses.
- Refused: no operator or an unknown one, a clause that tests no position
  (missing, empty or all-null `values`), an object entry — `400
  INVALID_CONDITION`; a path that does not end in `[*]` or has an earlier
  `[*]` — `400 INVALID_FIELD_PATH`. On workflow import every refusal is `400
  VALIDATION_FAILED`.
- Accepted on every surface that takes a condition: direct and async search,
  conditional delete, grouped-stats `condition`, gRPC search, workflow and
  transition criteria.

## What Cloud changes

Cloud's `ArrayCondition` has `operation` and `value: List<String>` and
applies the operator at every listed position, on REST search only. To align:

- Rename `value` to `values`. Keep `operation` and its `operatorType` alias.
- Accept `null` entries as "do not test this position".
- Accept non-string scalars and `[low, high]` pairs; apply range operators
  per position (Cloud refuses them today).
- Refuse a missing operator, a clause that tests no position, and a path with
  a wildcard before its trailing `[*]`.
- Accept the clause in workflow criteria and in gRPC search.
