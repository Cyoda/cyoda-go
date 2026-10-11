# The order in which a condition is checked, and the code each step answers

Status: implemented in cyoda-go (v0.9.0). Cloud must align.

## The rule

Every surface that takes a search condition checks it in one fixed order:

- direct (sync) search,
- async search submit,
- conditional delete (`DELETE /entity/{entityName}/{modelVersion}` with a
  condition body),
- grouped statistics (the request's `condition`).

The first step that fails gives the answer. A later step does not run.

| Step | What is checked | Code when it fails |
|---|---|---|
| 1. Parse | The body is a condition: valid JSON, a known `type`, members of the correct JSON type | `400 INVALID_CONDITION` |
| 2. Model | The model named in the path exists for the tenant | `404 MODEL_NOT_FOUND` |
| 3. Structure | First the size: at most 10,000 tested entries, counted before anything is expanded (an `array` clause counts one per entry in `values`, `null` entries included; every other clause counts one). Then operators, operand shapes, `BETWEEN` pairs, group operators, the `array` clause's shape, `function` clauses (refused in search), path grammar | `400 INVALID_CONDITION`; `400 INVALID_FIELD_PATH` for a path that is not valid path grammar |
| 4. Patterns | Every `LIKE` and `MATCHES_PATTERN` operand compiles | `400 INVALID_CONDITION` |
| 5. Model paths | Every path the condition names is declared by the model | `400 INVALID_FIELD_PATH` |
| 6. Types | Every operator applies to the declared type of its field, and every operand fits that type | `400 CONDITION_TYPE_MISMATCH` |

Steps 3 and 4 need no model. Steps 5 and 6 need the model's schema. Because
the checks that need no model come first, a structural or pattern fault gets
the same code whatever the model declares.

Step 2 comes before every check of a condition that parses. An unknown model
is `404` even when the condition also has a structural fault. A body that
does not parse is refused at step 1, before the model is looked up, so a body
that does not parse is `400 INVALID_CONDITION` even for an unknown model.

Grouped statistics also checks its own request members (`groupBy`,
aggregations) before step 2, and checks their paths together with the
condition's paths in step 5.

Over gRPC, a condition that does not parse is the transport status
`InvalidArgument`. Steps 2 to 6 answer in the response's error envelope
(`CLIENT_ERROR`), and its message names the same code as over HTTP.

## Examples

```
POST /api/search/direct/{model}/1

{"type":"bogus"}                                                        400 INVALID_CONDITION   (step 1)
{"type":"simple","jsonPath":"$.x"}            on an unknown model       404 MODEL_NOT_FOUND     (step 2)
{"type":"simple","jsonPath":"$.nope","value":1}
                                              no operatorType           400 INVALID_CONDITION   (step 3)
{"type":"simple","jsonPath":"$.nope","operatorType":"MATCHES_PATTERN","value":"("}
                                              $.nope not declared       400 INVALID_CONDITION   (step 4)
{"type":"simple","jsonPath":"$.nope","operatorType":"EQUALS","value":1}
                                              $.nope not declared       400 INVALID_FIELD_PATH  (step 5)
{"type":"simple","jsonPath":"$.count","operatorType":"CONTAINS","value":"9"}
                                              $.count is a number       400 CONDITION_TYPE_MISMATCH (step 6)
```

## What Cloud must mirror

- Run the same six steps in the same order on the same four surfaces, so one
  malformed condition gets one code on every surface.
- Answer a body that does not parse with `400 INVALID_CONDITION`, not
  `400 BAD_REQUEST`, on search as on the other surfaces.
- Answer an unknown model with `404 MODEL_NOT_FOUND` before any check of a
  condition that parses.
- Check patterns before model paths. A bad pattern on an undeclared path is
  `400 INVALID_CONDITION`, not `400 INVALID_FIELD_PATH`.
- Refuse a condition that tests more than 10,000 entries with
  `400 INVALID_CONDITION`, counted by the rule in step 3, before any other
  structural check and before the condition is expanded. Workflow import
  refuses such a criterion with `400 VALIDATION_FAILED`.

## Where it is enforced

The one sequence is `search.ValidateConditionForModel`; each surface resolves
the model first and then runs it. `internal/e2e` pins the order on all four
surfaces (`TestConditionValidationOrder_SameCodeOnEverySurface`,
`TestConditionEndpoints_UnknownModel_404BeforeConditionChecks`,
`TestSearch_UnparseableCondition_IsInvalidCondition`).
