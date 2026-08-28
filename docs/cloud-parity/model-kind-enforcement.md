# Model kind enforcement, sample-document collections, and a faithful export

Status: implemented in cyoda-go (v0.8.4). Cloud must align.

Three rules about the same thing — the model is a description of what is
stored, and every surface must agree with it.

## 1. A value's kind must be one the field declares

A field declares a **kind set**. A field derived from a scalar declares
`{scalar}`; from an array, `{array}`; from an object, `{object}`. A field
observed in more than one kind (repeated sample-data imports while the model is
`UNLOCKED`) declares each of them.

A write whose value has a kind outside that set is rejected with `400`, before
any type comparison. This holds on every ingress that validates a payload —
entity create, update, PATCH, and processor output — and at every depth,
including array elements.

```
model: { ".s": "STRING", ".a[*]": "STRING", ".o": OBJECT }

{"s":["A"]}       -> 400  expected scalar, got array
{"s":{"k":"v"}}   -> 400  expected scalar, got object
{"a":[{"k":"v"}]} -> 400  expected scalar, got object   (at a[0])
{"a":"x"}         -> 400  expected array, got string
{"o":["x"]}       -> 400  expected object, got array
{"s":1}           -> 400  INCOMPATIBLE_TYPE             (kind ok, type wrong)
{"s":null}        -> 200                                (null is not a kind)
```

Notes:

- **`null` follows the declaration.** A scalar declaration always admits it. A
  container declaration admits it only where the model observed one — a
  `NULL`-only declaration is the nullable marker, not an observation of a
  scalar. Null never widens the model and is not a kind of its own.
- **A kind mismatch is not a type incompatibility.** There is no `DataType` to
  report for an array or an object, so it does not carry
  `INCOMPATIBLE_TYPE`/`expectedType`/`actualType`. cyoda-go answers
  `400 VALIDATION_FAILED` with the offending kind named in the wire vocabulary
  (`expected scalar, got array`), and every kind the field does declare. The
  code is the dictionary's own definition — the payload parsed and then failed
  against the registered model — and it is the answer every generic validation
  failure on a write now carries, the container-vs-scalar direction included.
  `BAD_REQUEST` stays for what the server cannot parse: a malformed body, a bad
  parameter, unstorable bytes.
- **This is not a ban on polymorphic fields.** A field that declares both a
  scalar and an array admits both. Only a kind outside the declared set is
  refused. Every layer has to hold the union for this to be true — Merge folds
  the branches onto one node, the persisted schema must carry all of them back,
  validation dispatches on the branch the value's kind selects, and the export
  names each. In cyoda-go the object-and-array union is the one Merge folds onto
  a node whose *dominant* kind is OBJECT, so any layer that dispatches on the
  dominant kind alone silently drops the array branch.
- **Both write doors reject; the codes differ deliberately.** With no
  `changeLevel` (and on PATCH) the model is fixed and the answer is
  `VALIDATION_FAILED` — the value simply does not fit. With a `changeLevel` set the
  write additionally proposes a schema change, and the extension path refuses
  the kind change at every level including `STRUCTURAL`, with
  `POLYMORPHIC_SLOT`.
- **Known limitation of the `changeLevel` door in cyoda-go.** That path compares
  one kind per path, so on a model with a multi-kind field it also refuses a
  write matching a *declared* but non-dominant branch. The two doors therefore
  disagree about a conforming value: strict validation admits it, the extension
  path does not. This is the same gap as "an entity write cannot create a
  multi-kind declaration, while the sample-data import merge can" — a separate,
  open question, not settled here. Cloud, which implements polymorphic slots
  natively, is not expected to reproduce the limitation.

Why it matters beyond the write: search comparison is type-directed off these
declarations. A value admitted against a declaration that forbids its kind is a
value no predicate on that field can address consistently. Enforcing the
declaration removes that class rather than requiring search to define semantics
for data the model says cannot exist.

## 2. Sample data is a document, or a collection of documents

`POST /api/model/import/{format}/SAMPLE_DATA/{name}/{version}`:

| body | reading |
|---|---|
| JSON object | one sample document |
| JSON array | several sample documents; the derived model is their merge — identical to importing them one after another while `UNLOCKED` |
| anything else (scalar root, array holding a non-object) | `400 VALIDATION_FAILED`, naming the offending element; no model is registered |

An array body must **not** be read as "the entity is an array". The entity
ingress already reads an array body as a collection of entities of the same
type, and a model rooted at an array describes nothing the entity ingress can
accept — it renders as an empty model and refuses the documents it was derived
from.

An empty array carries no observations and derives the same empty model an
empty document does.

## 3. The export names every branch a field declares

`SIMPLE_VIEW`:

- One `[*]` hop per array level. An array of arrays of scalars is
  `".m[*][*]": "STRING"`; an array of arrays of objects is `"#.m": "OBJECT"`
  with the element bucket at `"$.m[*][*]"`. This is the same `jsonPath` spelling
  search uses to address those elements.
- A field declaring more than one kind names each branch: `".poly": "STRING"`
  alongside `".poly[*]": "STRING"`, `".o": "STRING"` alongside
  `"#.o": "OBJECT"`, or `"#.f": "OBJECT"` alongside `".f[*]": "INTEGER"`.
  Rendering only one branch makes two models that enforce differently render
  identically. The rule applies to array elements too, and at every level.
- An array whose elements were never observed is still a declared array and is
  named — `".a[*]": "NULL"` — rather than omitted.

`JSON_SCHEMA`: a kind union renders as an `anyOf` over its branches — *not*
`oneOf`, which requires exactly one branch to match and would reject a value the
model admits whenever two branches render the same JSON Schema shape (`Integer`
and `Long` both render `{"type":"integer"}`).

The same completeness applies to the internal field walk that resolves a path's
declared types for search, and to the persisted schema itself: every branch a
write may take must survive storage and be reachable in the field walk, or a
predicate on the missing branch silently evaluates as though the field held
nothing — and the model quietly stops admitting a document it was derived from.

## Coverage

- Unit: `internal/domain/model/schema` (validation, field walk),
  `internal/domain/model/importer` (collection import),
  `internal/domain/model/exporter` (both renderings).
- HTTP e2e: `internal/e2e/model_kind_enforcement_test.go` — write and PATCH
  rejections, no widening under a rejected write, collection import, rejected
  bodies, export branches.
- Cross-backend parity: `ModelKindEnforcementRejected`,
  `ModelSampleDataCollectionImport` in `e2e/parity/`. Both rules run above the
  SPI, so no backend may answer differently.
