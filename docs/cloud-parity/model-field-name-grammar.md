# Model field-name grammar — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

This is a **wire-contract tightening**: ingestion that works today stops
working.

## Rule

A model field name must be spellable as a single wire-`jsonPath` segment: one
or more of `A-Za-z0-9_-`, ASCII only. The empty name is rejected, and so is any
name containing `.` — it would spell two segments and name a field no lookup
could distinguish from a nested one.

The check delegates to `schema.IsSegmentName` — the single definition of the
segment character class, which the query-side path grammar is built from too, so
the model side and the query side cannot drift on which names are addressable.
A whole-segment check is exactly the rule a bare field name needs: it admits no
subscript (a field name must denote one node) and no `.`. See
`path-grammar.md` for the path grammar that consumes it.

## Where it is enforced

The two paths that establish a model's field set, and only those:

| Path | Surface |
| --- | --- |
| Sample-data model import | `POST /api/model/import/{dataFormat}/SAMPLE_DATA/{name}/{version}`; CloudEvent `EntityModelImportRequest` |
| ChangeLevel-driven schema extension on a write | `POST /api/entity/...` and the update/collection/transition variants; CloudEvent `EntityCreateRequest` and siblings; processor-returned data |

Both funnel through one walker, so the two doors cannot diverge. That is the
load-bearing design point for an implementer: the rule belongs at the single
point where a field set is *established*, not replicated at each entry point.

Strict validation — a model with no ChangeLevel, and PATCH — does not establish
fields, so the rule does not apply there: it validates against the stored
schema, which rejects unknown fields outright.

## Response

`400` with `properties.errorCode = VALIDATION_FAILED`. The detail names the
offending key and the canonical path of the object that declares it, and states
the allowed character set:

```
invalid field name: "first name" in object at "$.customer" — a field name must be
addressable as a jsonPath segment: ASCII letters, digits, "_" and "-" only, and
not empty; rename the field
```

Over gRPC the same rejection is an envelope error with `success=false` and
`error.code = CLIENT_ERROR`, the message carrying the `VALIDATION_FAILED:`
prefix and the same detail.

`VALIDATION_FAILED` is deliberately the same code the workflow importer answers
for its own content rules: the document is well-formed, so this is a
content-level contract violation with a concrete remedy, not a parse failure.

## Rationale

The query surface addresses a field by a `jsonPath` whose segments are that
charset, and offers no escape hatch — bracket-quoted access is rejected, and no
evaluator in the stack resolves it. Recording a field outside the charset would
guarantee data that can be written and never queried. cyoda-go fails closed: the
field is refused at the door rather than accepted into a model that has already
guaranteed it cannot serve it.

The charset's exclusions are deliberate, not incidental to writing it as an
allowlist. Two spellings collapse a field name onto a *nested path*: `.`, and —
in the evaluator cyoda-go uses in memory — `|`, which is read as an alternative
segment separator, so `a|b` is answered by a nested `a`→`b` where the document
has one. The others are read as instructions rather than names: `*` and `?`
(key wildcards, matching a different key than the one written), `@` (modifier),
`!` (literal), `#` (array count/projection, so the same name means "this key"
over an object and "how many elements" over an array), and `\` (escape).

None of these merely fails to find the field: `|` and `\` resolve to a
different node outright, `*` and `?` do whenever a sibling key matches the
glob, `!` ignores the document, and `#` means one thing over an object and
another over an array. A silent wrong answer is possible in every case, which
is why the name is refused at the door instead of being escaped at each
path-building site. An
implementer whose evaluator has a different metacharacter set still holds to
this charset: it is the contract, not a local convenience — and its own
metacharacters must be a subset of what the charset already excludes, or it has
the same defect under a different spelling.

## Pre-existing data

A model carrying a non-conforming field is not migrated, and no compatibility
path is provided. The field was never addressable by search — that is the
defect this closes, not a capability being withdrawn. Rename the key in the
source data and re-establish the model.

## Test surface

- `internal/domain/model/importer/fieldname_test.go` and
  `internal/domain/model/ingest/fieldname_test.go` — the validator and each
  ingress's classification of the sentinel.
- `internal/domain/model/fieldname_test.go` — the import handler's
  `400 VALIDATION_FAILED`, plus `TestImportModel_LegacyModelReimportRejected`,
  which pins the upgrade path: a rejected re-import must not overwrite the
  stored descriptor.
- `internal/e2e/model_field_name_test.go` — status and error code over real
  HTTP.
- `internal/grpc/model_field_name_test.go` — envelope shape on both the model
  import and the entity-create doors, plus the accept-side control.
- `e2e/parity/model_field_name.go` — `ModelFieldNameRejected`, registered in
  `e2e/parity/registry.go`, so every backend refuses the same field name.
