# Positional array subscripts resolve — Cloud twin-alignment spec

cyoda-go leads this contract. A path addressing **one array element by
position** — `$.arr[0]`, `$.items[2].sku` — must resolve to the value that
element holds. Answering an empty page for a field that holds the value is a
defect, not a permitted "not supported" outcome.

## Behaviour

`$.arr[0]` is valid JSON Path and is **accepted** at the API boundary (the
condition grammar deliberately tolerates subscripts —
`condition-jsonpath-grammar.md`). It must then be evaluated, on every surface
that takes a condition:

- `POST /search/direct/…` and `POST /search/async/…`
- conditional `DELETE /entity/{entityName}/{modelVersion}`
- the `condition` field of `POST /entity/stats/…/query`
- a workflow or transition `criterion`

`{"jsonPath":"$.arr[0]","operatorType":"EQUALS","value":1}` selects exactly the
entities whose element 0 is `1` — not every entity, not none.

**Type-checking follows the same rule.** A positional path resolves its declared
type the way its wildcard twin does: the schema records an array's element type
once, under `$.arr[*]`, and `$.arr[0]` must consult that entry. The visible
consequence is a new rejection — `{"jsonPath":"$.arr[0]","operatorType":"EQUALS","value":"abc"}`
against an integer array is **400 `CONDITION_TYPE_MISMATCH`**, where it
previously returned an empty page.

Also a consequence: a positional path must **not** be rejected by the
field-existence check as naming a field the model does not declare. The model
declares `$.arr[*]`; `$.arr[0]` is that field.

**Rejected, not resolved:** negative indices, slices, unions and filter
expressions (`$.arr[-1]`, `$.arr[0:2]`, `$.arr[0,1]`, `$.arr[?(@.x)]`). No
evaluator resolves them, so they are refused at the boundary with
**400 `INVALID_FIELD_PATH`** — see `condition-jsonpath-grammar.md`. Only the
wildcard `[*]` and a non-negative index are resolvable subscripts, and this
document covers the index form.

**A trailing wildcard is a different operation.** `$.arr[*]` with no following
segment resolves to the array's *count*, not its elements, so a comparison on
it compares against the length. That is the defined meaning of the spelling,
not a positional subscript, and this document does not change it.

## Why this is a Cloud obligation, not a shared-code guarantee

In cyoda-go every backend reaches the same in-process evaluator for a
subscripted path, because no in-tree backend can push one into its query — so
one fix covers all three. **That reasoning does not extend to the commercial
backend**, which self-executes search with an evaluator of its own
(`COMPATIBILITY.md`, `v0.8.4` row). It must implement positional-subscript
resolution — path resolution, declared-type lookup, and the field-existence
check — itself.

The parity scenario below runs against any backend wired into the suite and
will fail on a backend that has not, so the obligation surfaces on the next
dependency update rather than as a silent divergence.

## Why it was broken

Three lookups missed, each independently enough to make the leaf false for
every entity:

1. the evaluator handed gjson a path it has no syntax for (`arr[0]`, where
   gjson wants `arr.0`);
2. the declared-type lookup probed a schema key that cannot exist (`$.arr[0]`);
   a comparison with no declared type expands into nothing;
3. search's field-existence check rejected the path **400** as naming an
   undeclared field.

The wildcard spelling of the same path worked throughout, so two spellings of
one path disagreed.

## Test surface

- `e2e/parity/criterion_path.go` — `RunPositionalSubscriptPathResolves`,
  registered in `e2e/parity/registry.go`. Two entities hold the same two values
  in opposite order, so a path that ignored its subscript, or read the wrong
  element, inverts the expected set rather than merely weakening the assertion.
  It covers both arms: the search surface (where the plan is per-backend) and
  the workflow-criterion surface (which has no fallback at all — a criterion is
  only ever served by the in-process evaluator).
- `internal/match/jsonpath_subscript_test.go` — leaf-level resolution and the
  `ErrInvalidFilterPath` classification that keeps a subscripted path on the
  fall-back-to-in-memory path rather than turning it into a 400.
