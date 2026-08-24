# A trailing array wildcard addresses the elements — Cloud twin-alignment spec

cyoda-go leads this contract. A path whose **last hop is an array wildcard** —
`$.tags[*]`, `$.matrix[*][*]` — addresses the array's **elements**. A leaf on
such a path holds when **some** element satisfies it. Resolving it to the
array's length, and comparing the operand against that, is a defect.

## Behaviour

`$.tags[*]` is valid JSON Path and is **accepted** at the API boundary (the
condition grammar deliberately tolerates subscripts —
`condition-jsonpath-grammar.md`). It must then be evaluated element-wise, on
every surface that takes a condition:

- `POST /search/direct/…` and `POST /search/async/…`
- conditional `DELETE /entity/{entityName}/{modelVersion}`
- the `condition` field of `POST /entity/stats/…/query`
- a workflow or transition `criterion`

For `{"tags": ["red", "blue"]}`:

| Condition on `$.tags[*]` | Result |
| --- | --- |
| `EQUALS "red"` | matches |
| `EQUALS "blue"` | matches |
| `EQUALS "purple"` | no match |
| `EQUALS 2` | **no match** — 2 is the length, not an element |
| `CONTAINS "lu"` | matches |
| `NOT_NULL` | matches |

**Existential, and vacuously false on an empty array.** `NOT_NULL` on
`{"tags": []}` does **not** match: there is no element to be non-null. Neither
does `IS_NULL` — the same question, asked of no elements. This is the visible
consequence of the change for presence tests, because a length of `0` is a
present number.

**Every array hop counts, wherever it sits.** The rule is about the values the
path addresses, not about where the brackets are:

| Path | Addresses |
| --- | --- |
| `$.tags[*]` | each element of `tags` |
| `$.matrix[*][*]` | each element of each inner array |
| `$.a[*].b[*]` | each element of each `b` |
| `$.orders[*].lines[*].sku` | each `sku`, across both hops |

The last row has **no** trailing wildcard and was broken too: with two array
hops the result nests once per hop, so a per-element comparison compared a
scalar against an array.

**Validity is a question about the model, not the grammar.** A field can be
polymorphic, and a trailing wildcard only means something if the element can be
compared to a scalar. The schema answers it, at the exact `[*]` key:

| Model shape | `$.x[*]` with a scalar operand |
| --- | --- |
| array of scalars (`["red"]`) | **valid** — the element is a scalar |
| array of objects that were also observed as a bare scalar | **valid** — the scalar occurrences are comparable |
| array of pure objects (`[{"sku": "A1"}]`) | **400 `INVALID_FIELD_PATH`** — the element has substructure and no scalar form, so the comparison could only ever be false |

That is not a new rule. It is the leaf-vs-container split every path already
obeys: the schema records a descriptor at the `[*]` key exactly for the first two
shapes, and records only children (`$.items[*].sku`) for the third. Navigate to
the leaf sub-path instead.

The unary presence tests carry no scalar operand, so `IS_NULL` / `NOT_NULL` on
an array of pure objects stays **valid** — "does some element exist and is it
non-null" is a meaningful question about objects.

**With no schema bound the check is skipped and the path is accepted**, exactly
as every other schema-dependent validation in the stack degrades. With no schema
there is no authority saying the element is an object, the model may legitimately
hold an array of scalars, and a rejection would be a false 400 on a valid query.
Element-wise resolution is correct either way; only the declared types are
missing, and an untyped comparison leaf degrades to non-match, never to a wrong
match.

## Why this is a Cloud obligation, not a shared-code guarantee

In cyoda-go every backend reaches the same in-process evaluator for a
subscripted path, because no in-tree backend can push one into its query — so
one fix covers all three. **That reasoning does not extend to the commercial
backend**, which self-executes search with an evaluator of its own
(`COMPATIBILITY.md`, `v0.8.4` row). It must resolve a trailing wildcard
element-wise itself.

The parity scenario below runs against any backend wired into the suite and will
fail on a backend that has not, so the obligation surfaces on the next
dependency update rather than as a silent divergence.

## Why it was broken

The in-process evaluator rewrites JSON Path into gjson syntax, and gjson spells
an array projection `#`. The rewrite mapped **every** `[*]` to `#`, which is
right mid-path and wrong at the end: a `#` in final position is the array's
**length**. `$.tags[*] EQUALS "red"` therefore compared `"red"` against `2` and
never matched.

The second half of the same arithmetic: each `#` projection wraps its sub-result
in one more array level, and the evaluator iterates a result exactly once. Two
hops produced `[["S1","S2"],["S3"]]`, so the comparison saw arrays where it
expected scalars.

Both are wrong-but-available answers: an empty page on search, and a workflow
criterion that silently never fires — the failure mode with no result page to
look wrong.

## Test surface

- `e2e/parity/trailing_wildcard_path.go` —
  `RunSearchTrailingWildcardPathResolves`, registered in
  `e2e/parity/registry.go`. Both arms: the search surface (where the plan is
  per-backend) and the workflow-criterion surface (which has no fallback at all).
  Both entities' arrays are two elements wide, so a length-valued leaf separates
  them on neither the tag values nor the numeric comparison.
- `internal/match/trailing_wildcard_test.go` — the rewrite and the element-wise
  evaluation, including the empty-array and absent-field cases where existential
  and length semantics genuinely diverge.
- `internal/domain/search/trailing_wildcard_path_test.go` — the model-dependent
  accept/reject rule, and the schema flattening contract it is derived from.
- `internal/e2e/search_trailing_wildcard_test.go`,
  `internal/grpc/search_trailing_wildcard_test.go` — HTTP and gRPC, both entry
  points, including the `400 INVALID_FIELD_PATH` on an array of pure objects.
