---
topic: models
title: "models — entity model schema system"
stability: stable
see_also:
  - crud
  - workflows
  - search
  - errors.MODEL_NOT_FOUND
  - errors.MODEL_ALREADY_LOCKED
  - errors.MODEL_ALREADY_UNLOCKED
  - errors.MODEL_HAS_ENTITIES
  - errors.MODEL_ADMIN_IN_JOINED_TRANSACTION
  - errors.INVALID_CHANGE_LEVEL
  - errors.VALIDATION_FAILED
  - errors.UNIQUE_VIOLATION
  - errors.INVALID_UNIQUE_KEY
  - errors.COMPOSITE_KEY_UNSUPPORTED
  - errors.INVALID_UNIQUE_KEY_DEFINITION
  - openapi
---

# models

## NAME

models — entity model schema system: registration, lifecycle, import, export, and validation.

## SYNOPSIS

```
GET    /api/model/
GET    /api/model/export/{converter}/{entityName}/{modelVersion}
POST   /api/model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}
POST   /api/model/validate/{entityName}/{modelVersion}
DELETE /api/model/{entityName}/{modelVersion}
POST   /api/model/{entityName}/{modelVersion}/changeLevel/{changeLevel}
PUT    /api/model/{entityName}/{modelVersion}/lock
PUT    /api/model/{entityName}/{modelVersion}/unlock
PUT    /api/model/{entityName}/{modelVersion}/unique-keys
GET    /api/model/{entityName}/{modelVersion}/workflow/export
POST   /api/model/{entityName}/{modelVersion}/workflow/import
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`).

## DESCRIPTION

A model is a named, versioned schema registered per tenant. Every entity in the system is an instance of exactly one model. Models are identified by `(entityName, modelVersion)`. The model ID is a deterministic UUID v5 derived from that key: `UUID.newSHA1(NameSpaceURL, "{entityName}.{modelVersion}")`.

Models have two lifecycle states: `UNLOCKED` and `LOCKED`. A `LOCKED` model blocks further imports. An `UNLOCKED` model accepts re-import and schema merging. Entities can only be created against a `LOCKED` model. Deletion is blocked while any entities reference the model.

Schema inference is additive: importing sample data against an existing model merges the incoming schema with the stored one. The model's `changeLevel` field controls which structural changes are allowed during entity ingestion on a locked model.

**Sample data is a document, or a collection of documents.** A JSON object is one sample document. A JSON array is several — each element must itself be an object, and the derived model is their merge, the same result successive imports produce. Any other body (a scalar, an array holding a non-object) is rejected with `400 VALIDATION_FAILED`, naming the offending element.

**A value's kind must be one the field declares.** A field declared as a scalar accepts a scalar; declared as an array, an array; declared as an object, an object. A value of any other kind is rejected with `400`, at every depth including array elements. On a model with no `changeLevel` (and on PATCH) that is `VALIDATION_FAILED` — `expected scalar, got array`, naming every kind the field does declare. With a `changeLevel` set the write also proposes a schema change, so the answer turns on the level: giving a path a kind it does not declare is a `STRUCTURAL` change, refused below that level and accepted at it.

A field may declare more than one kind — observed in each while the model is `UNLOCKED` (successive imports, or one import of several sample documents), or added by a write at `STRUCTURAL`. Every declared kind is admissible at every `changeLevel`, and the export names each branch.

A path that declares no kind at all is the exception worth knowing: a field observed only as `null`, or an array observed with no content, has nothing to conflict with, so it learns its first kind at `TYPE` (or `ARRAY_ELEMENTS` for an array's element) rather than at `STRUCTURAL`.

**A value costs a `TYPE`-level change only if the field does not already admit it.** A field admits a value when the value's JSON kind matches one of the field's declared types and that type's own admission test accepts the value — the model changes only when it does not. This is not "the value's classified type widens into what's declared"; it is a direct, per-value test against each declared type.

For a number the test is per numeric family, not "inside the declared type's range": `INTEGER`/`LONG`/`BIG_INTEGER` admit a whole number within the type's own bounds; `UNBOUND_INTEGER` admits any whole number; `DOUBLE` admits a value within its range **and** representable in at most 15 significant digits with a scale of at most 292; `BIG_DECIMAL` admits by magnitude alone; `UNBOUND_DECIMAL` admits everything. Writing `1000`, `1000.0`, `1e3` — or `2147483648`, ten digits and still exactly representable — to a field declared `DOUBLE` proposes no schema change and is accepted at every `changeLevel`, `ARRAY_LENGTH` included, because each value's own precision and scale fit `DOUBLE`'s admission test.

A larger whole number can still force a change: `9007199254740993` needs sixteen significant digits, past what `DOUBLE`'s 53-bit mantissa can hold exactly, so writing it to a `DOUBLE` field is a real type change — refused below `TYPE`, and at `TYPE` it widens the field to `UNBOUND_DECIMAL`, the narrowest type that holds both. A decimal into a field declared `INTEGER` is a type change for the same reason: the integer family never admits a fractional value. See `docs/numeric-classification.md` for the full per-family predicate and why the range alone is not the test.

**A `STRING` field admits any string, including one shaped like a date or timestamp, without changing the model.** Writing `"2026-03-01"` to a field declared `STRING` is not a type change — `STRING` admits every string — and the field's declared types are unaffected. A field that is *also* declared as a temporal type (`LOCAL_DATE`, `ZONED_DATE_TIME`, and so on) holds a value under that type only when the string's own classification is exactly that type: a `ZONED_DATE_TIME`-declared field does not hold a bare `"2026"` (a year, not a timestamp), and a `LOCAL_DATE_TIME`-declared field does not hold a string carrying a UTC offset (that string denotes a different instant than the one truncating the offset would give). An entity write to a text-declared field no longer learns a temporal subtype the way a write once did — **registration is how a field acquires a temporal type**: importing sample data that shows both a plain string and a date-shaped one yields a field declared both `STRING` and, say, `LOCAL_DATE`; an ordinary write to an already-`STRING` field does not grow that declaration on its own.

`null` follows the declaration like any other value: a scalar field always accepts it, and a container field accepts it only where the model observed one (the sample data had `null` there). It is not a kind of its own, so it never widens the model.

**Field names must be searchable.** A field name is accepted only if it is a valid `jsonPath` segment: one or more ASCII letters, digits, `_` or `-`. Spaces, dots, quotes, brackets, `$`, `@`, `:`, the evaluator's own metacharacters (`*`, `?`, `#`, `|`, `!`, `\`), any non-ASCII character, and the empty name are rejected with `400 VALIDATION_FAILED`, naming the offending key and the object that declares it. Search addresses a field by a `jsonPath` built from exactly this charset and offers no escape hatch, so a field outside it could be written and never queried — it is refused at the door instead.

This applies to both paths that establish a model's field set: sample-data import, and the `changeLevel`-driven schema extension an entity write performs. Strict validation (a model with no `changeLevel`, and PATCH) does not establish fields, so the rule does not apply there. A model that already carries a non-conforming field is not migrated and there is no compatibility path: rename the key in the source data and re-establish the model.

## ENDPOINTS

Model administration never runs inside a transaction. The operations below that change a model — import, delete, change level, lock, unlock, unique keys, and the workflow import of `cyoda help workflows` — refuse a request that carries a transaction token (`X-Tx-Token`, or the `tx-token` gRPC metadata a compute member echoes on a callback) with `400 MODEL_ADMIN_IN_JOINED_TRANSACTION`, before the token is verified and before anything is read or written. The read-only operations — list, export, validate — are not administration and take a token like any other request. A processor that needs to administer a model makes the request without the token. See `cyoda help errors MODEL_ADMIN_IN_JOINED_TRANSACTION`.

**GET /api/model/**

List all models for the authenticated tenant.

Response: `200 OK`, `application/json`, array of `EntityModelDto`.

**POST /api/model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}**

Import or update a model schema from sample data. Body size limit: 10 MiB.

- `dataFormat` (path): `JSON` or `XML`
- `converter` (path): `SAMPLE_DATA` (only supported converter; `JSON_SCHEMA` and `SIMPLE_VIEW` are defined in the OpenAPI but return `400 BAD_REQUEST` in this implementation)
- `entityName` (path): string — model name
- `modelVersion` (path): int32 — model version number

If the model does not exist, it is created with state `UNLOCKED`. If it exists and is `UNLOCKED`, the incoming schema is merged (additive). If it exists and is `LOCKED`, returns `409 CONFLICT`.

Response: `200 OK`, `application/json`, UUID string — the model ID.

**GET /api/model/export/{converter}/{entityName}/{modelVersion}**

Export a model schema in the specified format.

- `converter` (path): `JSON_SCHEMA` or `SIMPLE_VIEW`
- `entityName` (path): string
- `modelVersion` (path): int32

Response: `200 OK`, `application/json` — format depends on converter.

**POST /api/model/validate/{entityName}/{modelVersion}**

Validate a JSON payload against the model's schema. Returns a result object, not an HTTP error, on validation failure.

- `entityName` (path): string
- `modelVersion` (path): int32

Request body: any JSON object.

Response: `200 OK`, `application/json`, `EntityModelActionResultDto`.

**DELETE /api/model/{entityName}/{modelVersion}**

Delete a model. Blocked if the model is `LOCKED` or if any entities reference it (entity count > 0).

Response: `200 OK`, `application/json`, `EntityModelActionResultDto` on success. `409 MODEL_ALREADY_LOCKED` if the model is locked; `409 MODEL_HAS_ENTITIES` if entities reference it.

**POST /api/model/{entityName}/{modelVersion}/changeLevel/{changeLevel}**

Set or update the change level on a model. Meaningful for locked models; unlocked models always allow all changes.

- `changeLevel` (path): `ARRAY_LENGTH`, `ARRAY_ELEMENTS`, `TYPE`, or `STRUCTURAL`

Change levels are hierarchical (most restrictive to most permissive):

- `ARRAY_LENGTH` — permits no schema change at all: the floor of the ladder. An array's length is not part of the model, so a longer array is held here exactly as at every other level
- `ARRAY_ELEMENTS` — allows an array's element to learn its first scalar type, or to widen the one it declares; nothing outside an array may change
- `TYPE` — permits modifications to existing types
- `STRUCTURAL` — allows fundamental model changes: new fields, and giving a path a kind it does not yet declare

Response: `200 OK`, `application/json`, `EntityModelActionResultDto`.

**PUT /api/model/{entityName}/{modelVersion}/lock**

Lock a model. The model must be `UNLOCKED`. Returns `409 CONFLICT` if already locked.

Response: `200 OK`, `application/json`, `EntityModelActionResultDto`.

**PUT /api/model/{entityName}/{modelVersion}/unlock**

Unlock a model. The model must be `LOCKED` and have zero associated entities. Returns `409 CONFLICT` if entities exist or model is not locked.

Response: `200 OK`, `application/json`, `EntityModelActionResultDto`.

**PUT /api/model/{entityName}/{modelVersion}/unique-keys**

Declare composite unique keys for a model. Allowed only while the model is `UNLOCKED`. This call is idempotent — it replaces the model's entire key list atomically.

Request body (`application/json`):

```json
{
  "uniqueKeys": [
    { "id": "by-email", "fields": ["$.email"] },
    { "id": "by-org-and-handle", "fields": ["$.org", "$.handle"] }
  ]
}
```

- `uniqueKeys` — array of key definitions; `[]` clears all keys.
- `id` — stable string identifier, unique within the model.
- `fields` — ordered array of scalar leaf field paths (dotted paths matching the model's inferred schema). Array, object, and wildcard paths are rejected.

Validation is immediate: field paths must be known scalar leaves in the model's schema; duplicate `id` values and empty `fields` arrays are rejected.

Returns `200 OK` with `EntityModelActionResultDto` on success.

- `422 COMPOSITE_KEY_UNSUPPORTED` — the active storage backend does not support composite unique keys.
- `409 MODEL_ALREADY_LOCKED` — model is not in `UNLOCKED` state.
- `422 INVALID_UNIQUE_KEY_DEFINITION` — a field path references a non-scalar/unknown/array field, a key `id` is duplicated, or `fields` is empty.

**Transactional ordering.** When one transaction (e.g. a workflow) both frees a unique-key value — by deleting or re-keying its holder — and claims it on another entity, free it before claiming. Free-then-claim works on every backend; the reverse is rejected on write-time backends (PostgreSQL) and accepted on commit-time ones (in-memory, SQLite), so free-before-claim for portable behavior.

**GET /api/model/{entityName}/{modelVersion}/workflow/export**

Export all workflow configurations for the model. Returns `404 WORKFLOW_NOT_FOUND` if no workflows exist.

Response: `200 OK`, `application/json`:

```json
{
  "entityName": "nobel-prize",
  "modelVersion": 1,
  "workflows": []
}
```

**POST /api/model/{entityName}/{modelVersion}/workflow/import**

Import or replace workflow configurations for the model. See `workflows` topic.

## REQUEST SCHEMAS

**EntityModelDto** (returned by `GET /api/model/`):

```json
{
  "id": "31134900-d9cb-11ee-b913-ae468cd3ed16",
  "modelName": "nobel-prize",
  "modelVersion": 1,
  "currentState": "LOCKED",
  "modelUpdateDate": "2025-08-02T13:31:48.141053-07:00"
}
```

- `id` — UUID (deterministic v5, derived from name+version)
- `modelName` — string
- `modelVersion` — int32
- `currentState` — `"LOCKED"` or `"UNLOCKED"`
- `modelUpdateDate` — RFC 3339 timestamp, nullable

**EntityModelActionResultDto** (returned by lock, unlock, delete, changeLevel, validate):

```json
{
  "success": true,
  "message": "Model nobel-prize:1 locked",
  "modelId": "cee334fa-c0ac-11f0-ba79-ae468cd3ed16",
  "modelKey": {
    "name": "nobel-prize",
    "version": 1
  }
}
```

- `success` — boolean
- `message` — human-readable result string
- `modelId` — UUID
- `modelKey.name` — string
- `modelKey.version` — int32

**Import request body** (sample data, JSON format):

```json
{
  "category": "physics",
  "year": "2024",
  "laureates": [
    {
      "firstname": "John",
      "surname": "Hopfield",
      "id": "1037",
      "motivation": "for foundational discoveries",
      "share": "2"
    }
  ]
}
```

The importer walks the JSON structure and infers a typed schema. Subsequent imports are merged additively. A JSON array body is read as several sample documents and merged the same way.

**Export — SIMPLE_VIEW format**:

```json
{
  "currentState": "LOCKED",
  "model": {
    "$": {
      "#.laureates": "OBJECT",
      ".category": "STRING",
      ".year": "STRING"
    },
    "$.laureates[*]": {
      "#": "ARRAY_ELEMENT",
      ".firstname": "STRING",
      ".id": "STRING",
      ".motivation": "STRING",
      ".share": "STRING",
      ".surname": "STRING"
    }
  },
  "uniqueKeys": [
    { "id": "by-email", "fields": ["$.email"] }
  ]
}
```

The `"$"` bucket includes a `"#.fieldname": "OBJECT"` entry for each array field in the root object. The `"$.fieldname[*]"` bucket contains the array element schema with `"#": "ARRAY_ELEMENT"` as a type marker. `uniqueKeys` is omitted when the model declares no composite unique keys.

An array of arrays is spelled one `[*]` hop per level — `".m[*][*]": "STRING"`, and `"$.m[*][*]"` for the bucket when its elements are objects — matching the `jsonPath` a search uses to address them.

A field declared in more than one kind names each branch it declares: `".poly": "STRING"` alongside `".poly[*]": "STRING"`, or `".o": "STRING"` alongside `"#.o": "OBJECT"`. Both are enforced, so both are shown.

**Export — JSON_SCHEMA format**:

```json
{
  "currentState": "LOCKED",
  "model": {
    "type": "object",
    "properties": {
      "category": { "type": "string" },
      "year": { "type": "string" },
      "laureates": {
        "type": "array",
        "items": {
          "type": "object",
          "properties": {
            "firstname": { "type": "string" },
            "share": { "type": "string" },
            "id": { "type": "string" },
            "surname": { "type": "string" },
            "motivation": { "type": "string" }
          }
        }
      }
    }
  },
  "uniqueKeys": [
    { "id": "by-email", "fields": ["$.email"] }
  ]
}
```

## LIFECYCLE

A model moves between two states:

- `UNLOCKED` — initial state after first import; re-import is permitted; entities cannot be created
- `LOCKED` — entities can be created; re-import is blocked; change-level controls in-flight schema extension

Transitions:

- `UNLOCKED` → `LOCKED` via `PUT /model/{name}/{version}/lock`
- `LOCKED` → `UNLOCKED` via `PUT /model/{name}/{version}/unlock` (only when entity count is zero)

The `changeLevel` field controls schema evolution on locked models. When set, entity ingestion that introduces new structure triggers an additive schema extension (delta computed via `schema.Diff`, appended via `ModelStore.ExtendSchema`, committed with the entity transaction).

Entity ingestion here includes data returned by a workflow processor, not just data sent by a client. A processor that writes a field the model does not declare needs `changeLevel` set exactly as a client would.

## ERRORS

- `errors.MODEL_NOT_FOUND` — `404` — model does not exist for the given name and version
- `errors.MODEL_ALREADY_LOCKED` — `409` — re-import, relock, or delete attempted on a model already in `LOCKED` state
- `errors.MODEL_ALREADY_UNLOCKED` — `409` — unlock attempted on a model already in `UNLOCKED` state
- `errors.MODEL_HAS_ENTITIES` — `409` — unlock or delete blocked because entities of the model exist (`entityCount` in `properties`)
- `errors.MODEL_ADMIN_IN_JOINED_TRANSACTION` — `400` — a model-changing operation carries a transaction token; model and workflow administration never runs inside a transaction
- `errors.INVALID_CHANGE_LEVEL` — `400` — `POST /model/{name}/{version}/changeLevel/{changeLevel}` supplied a value that is not one of `ARRAY_LENGTH`, `ARRAY_ELEMENTS`, `TYPE`, `STRUCTURAL` (`entityName`, `entityVersion`, `suppliedValue`, `validValues` in `properties`)
- `errors.VALIDATION_FAILED` — `400` — workflow import validation failed (static analysis); sample data that is neither a document nor a collection of documents; a field name that is not addressable
- `errors.BAD_REQUEST` — `400` — unsupported converter, or a malformed body
- `errors.UNIQUE_VIOLATION` — `409` — entity write rejected because it would duplicate a composite unique key value-set held by another live entity
- `errors.INVALID_UNIQUE_KEY` — `422` — entity write rejected because a key is partially filled (all-or-nothing rule), the numeric value exceeded the allowed precision bound, or a key field path resolves to a non-scalar value
- `errors.COMPOSITE_KEY_UNSUPPORTED` — `422` — composite unique key declared on a backend that does not support the feature
- `errors.INVALID_UNIQUE_KEY_DEFINITION` — `422` — key declaration rejected: non-scalar or unknown field path, duplicate key `id`, or empty `fields` array

## EXAMPLES

**Import a model from sample JSON:**

```
curl -s -X POST \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"category":"physics","year":"2024","laureates":[{"firstname":"John","surname":"Hopfield","id":"1037"}]}' \
  "http://localhost:8080/api/model/import/JSON/SAMPLE_DATA/nobel-prize/1"
```

Response: `"1d1e1b10-1155-11f0-bcd5-ae468cd3ed16"`

**Lock the model:**

```
curl -s -X PUT \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/model/nobel-prize/1/lock"
```

**Set change level (allow structural evolution on locked model):**

```
curl -s -X POST \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/model/nobel-prize/1/changeLevel/STRUCTURAL"
```

**List all models:**

```
curl -s -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/model/"
```

**Export as SIMPLE_VIEW:**

```
curl -s -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/model/export/SIMPLE_VIEW/nobel-prize/1"
```

**Validate a payload against the model:**

```
curl -s -X POST \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"category":"physics","year":"2024"}' \
  "http://localhost:8080/api/model/validate/nobel-prize/1"
```

**Delete a model (must be unlocked and have zero entities):**

```
curl -s -X DELETE \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/model/nobel-prize/1"
```

## SEE ALSO

- crud
- workflows
- search
- errors.MODEL_NOT_FOUND
- errors.MODEL_ALREADY_LOCKED
- errors.MODEL_ALREADY_UNLOCKED
- errors.MODEL_HAS_ENTITIES
- errors.INVALID_CHANGE_LEVEL
- errors.VALIDATION_FAILED
- errors.UNIQUE_VIOLATION
- errors.INVALID_UNIQUE_KEY
- errors.COMPOSITE_KEY_UNSUPPORTED
- errors.INVALID_UNIQUE_KEY_DEFINITION
- openapi
