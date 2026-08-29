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
  refused. Every layer has to hold the union for this to be true: a node records
  the SET of kinds the field has been observed as, the persisted schema carries
  every one of them back, validation dispatches on the branch the value's kind
  selects, and both exports name each. There is no dominant kind and no single
  label — a label can only name one of three independent observations, so any
  layer that reads one silently drops the others.
- **The two write doors ask different questions.** With no `changeLevel` (and
  on PATCH) the model is fixed: the only question is whether the value fits, and
  a kind outside the declared set is `VALIDATION_FAILED`. With a `changeLevel`
  set the write additionally proposes a schema change, so the question is
  whether the level permits it. Giving a path a kind it does not declare is a
  `STRUCTURAL` change — a new kind is strictly more fundamental than a new
  field — refused below that level as an ordinary change-level violation and
  accepted at it. Raising the level therefore resolves it, which is why there is
  no separate error code for the case.
- **A write can establish a second kind, and every declared kind is writable.**
  Both follow from the node holding a set. A field that declares a scalar and an
  array admits either at any `changeLevel`; a third kind is admitted at
  `STRUCTURAL` and refused below it. The one asymmetry worth stating: a path that
  declares NO kind — observed only as `null`, or an array observed with no
  content — has nothing to conflict with, so establishing its first kinds keeps
  the level that promotion has always had (`TYPE`, or `ARRAY_ELEMENTS` on an
  array's element) rather than requiring `STRUCTURAL`.

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

The same completeness applies to the field walk that resolves a path's declared
types for search, and to the persisted schema itself: every branch a write may
take must survive storage and be reachable in the field walk, or a predicate on
the missing branch silently evaluates as though the field held nothing — and the
model quietly stops admitting a document it was derived from.

That walk has exactly one implementation, in `cyoda-go-spi`, and the engine uses
it. A backend that executes a search itself reads the same bytes through the
same code, so it cannot resolve a path's declared types differently. This is
load-bearing rather than tidiness: the failure mode of a second implementation
is not an error but a narrower answer — the path misses in the fields map, the
leaf comparison matches nothing, and the query returns fewer rows with nothing
reported.

## Coverage

- Unit: `internal/domain/model/schema` (validation, field walk),
  `internal/domain/model/importer` (collection import),
  `internal/domain/model/exporter` (both renderings).
- HTTP e2e: `internal/e2e/model_kind_enforcement_test.go` — write and PATCH
  rejections, no widening under a rejected write, collection import, rejected
  bodies, export branches.
  `internal/e2e/model_kind_branch_extension_test.go` — the `changeLevel` door:
  every declared kind accepted at every level, a new kind refused below
  `STRUCTURAL` and accepted at it, and the export naming both branches after.
- gRPC: `internal/grpc/model_kind_enforcement_test.go`,
  `internal/grpc/model_kind_branch_test.go` — a separate entry point, so the
  same rules are asserted on the envelope.
- Cross-backend parity: `ModelKindEnforcementRejected`,
  `ModelSampleDataCollectionImport`, `ModelKindBranchExtension` in
  `e2e/parity/`. The rules run above the SPI, so no backend may answer
  differently — and the last one additionally checks that the delta survives the
  plugin's own extension log and is folded back on read.
