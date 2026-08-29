# A schema failure on a write is VALIDATION_FAILED, not BAD_REQUEST

Status: implemented in cyoda-go (v0.8.4). Cloud must align.

## The rule

Two codes, split by whether the request was understood:

| condition | code |
|---|---|
| the server cannot parse or structurally process the request — malformed JSON/XML, unsupported format, a parameter outside its range, mutually exclusive parameters, unstorable bytes (U+0000, invalid UTF-8) | `400 BAD_REQUEST` |
| the payload parsed, and then failed against the registered model | `400 VALIDATION_FAILED` |
| …except a leaf value whose DataType is not assignable to the declared one | `400 INCOMPATIBLE_TYPE` (+ `fieldPath`, `expectedType`, `actualType`) |

`VALIDATION_FAILED` therefore covers, on every entity ingress (create, update,
PATCH, collection writes, processor output):

- a field the model does not declare,
- a value whose JSON kind the field does not declare,
- a change the configured `changeLevel` does not permit,
- a field name the wire `jsonPath` grammar cannot address,
- a payload the walker cannot read at all.

## Why it changed

The error dictionary already defined the split this way:
`errors/VALIDATION_FAILED.md` said "the payload parses but violates the
registered model schema", and `errors/BAD_REQUEST.md` said "the server cannot
parse the request". The implementation did not follow it — the catch-all in the
entity handler's `classifyValidateOrExtendErr` answered `BAD_REQUEST` for every
generic validation failure, so the documented meaning of both codes was wrong,
and an SDK could not branch on the documentation.

Only the catch-all moved. The three specific codes above were already correct and
are unchanged, and every `BAD_REQUEST` raised before validation (body read,
parameter checks, `RejectUnstorable`) is unchanged.

## What a client sees

```
POST /api/entity/JSON/{model}/1     model declares { ".s": "STRING" }

{"s":["A"]}       400 VALIDATION_FAILED  validation failed: s: expected scalar, got array
{"unknown":1}     400 VALIDATION_FAILED  validation failed: unknown: unexpected field not present in model
{"s":1}           400 INCOMPATIBLE_TYPE  s: value of type INTEGER is not compatible with [STRING]
<not JSON>        400 BAD_REQUEST        invalid JSON
?transactionWindow=0
                  400 BAD_REQUEST        transactionWindow out of range
```

## Notes for Cloud

- The status stays `400` in every case. Only the code in the problem document
  changes, so a client that branches on status alone is unaffected.
- A client that branches on `BAD_REQUEST` to detect a schema violation must move
  to `VALIDATION_FAILED`. cyoda-go is pre-1.0 and this shipped in a PATCH
  release; it is listed under Breaking in the CHANGELOG.
- The per-endpoint error tables in `api/openapi.yaml` and the entity error-code
  matrix in `internal/e2e/zzz_errorcode_matrix_test.go` are the enforced
  statement of which codes each operation may return.
