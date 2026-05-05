---
topic: crud
title: "crud — entity lifecycle API"
stability: stable
see_also:
  - models
  - search
  - workflows
  - errors.ENTITY_NOT_FOUND
  - errors.ENTITY_MODIFIED
  - errors.MODEL_NOT_FOUND
  - errors.MODEL_NOT_LOCKED
  - errors.VALIDATION_FAILED
  - errors.INCOMPATIBLE_TYPE
  - errors.CONFLICT
  - errors.IDEMPOTENCY_CONFLICT
  - errors.TRANSITION_NOT_FOUND
  - openapi
---

# crud

## NAME

crud — entity create, read, update, delete, and transition REST API.

## SYNOPSIS

```
POST   /api/entity/{format}/{entityName}/{modelVersion}
POST   /api/entity/{format}
GET    /api/entity/{entityId}
PUT    /api/entity/{format}/{entityId}
PUT    /api/entity/{format}/{entityId}/{transition}
PUT    /api/entity/{format}
DELETE /api/entity/{entityId}
DELETE /api/entity/{entityName}/{modelVersion}
GET    /api/entity/{entityName}/{modelVersion}
GET    /api/entity/{entityId}/changes
GET    /api/entity/{entityId}/transitions
GET    /api/entity/stats
GET    /api/entity/stats/states
GET    /api/entity/stats/{entityName}/{modelVersion}
GET    /api/entity/stats/states/{entityName}/{modelVersion}
GET    /api/platform-api/entity/fetch/transitions
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). All endpoints require `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`.

## DESCRIPTION

Entities are instances of models. Each entity has a UUID, a model reference (`entityName`, `modelVersion`), and a lifecycle state managed by the workflow engine. Creating an entity requires the referenced model to be in `LOCKED` state. All write operations run within a Cyoda transaction and return a `transactionId` alongside the affected entity IDs.

Body size limit on all write endpoints: 10 MiB.

## ENDPOINTS

**POST /api/entity/{format}/{entityName}/{modelVersion}** — Create a single entity

- `format` (path): `JSON` or `XML`
- `entityName` (path): string — model name
- `modelVersion` (path): int32
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

If the request body is a JSON array, the handler delegates to the collection-create path: each element is treated as a separate entity of the same model.

Response: `200 OK`, `application/json`:

```json
[{
  "transactionId": "cb91fa80-d4a8-11ee-a357-ae468cd3ed16",
  "entityIds": ["74807f00-ed0d-11ee-a357-ae468cd3ed16"]
}]
```

**POST /api/entity/{format}** — Create a collection (mixed models)

- `format` (path): `JSON` or `XML`
- `transactionWindow` (query, optional): int32, default `100`, max `1000` — maximum entities per transactional batch. Values outside (0, 1000] are rejected with `400 BAD_REQUEST`. Collections exceeding the window are split into multiple transactional batches committed sequentially; each chunk is one transaction. The response is an array with one element per chunk in commit order.
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

**IMPORTANT — `payload` is a JSON-encoded string, not an object.**

The `payload` field must be a string containing the JSON-encoded entity body, not a nested JSON object. This is a deliberate API contract — it preserves the payload as an opaque blob through the pipeline.

Correct: `"payload": "{\"category\":\"physics\"}"`
Wrong:   `"payload": {"category":"physics"}`   (will be rejected with `errors.BAD_REQUEST`)

Request body: JSON array of `CreatePayload` objects:

```json
[
  {
    "model": { "name": "nobel-prize", "version": 1 },
    "payload": "{\"category\":\"physics\",\"year\":\"2024\"}"
  }
]
```

Each item may reference a different model. The collection is committed in transactional batches of at most `transactionWindow` items. Within a single chunk the create is all-or-nothing; chunks committed before any later failure remain durable.

Response: `200 OK`, `application/json`, `EntityTransactionResponse` array — one element per committed chunk in commit order:

```json
[{
  "transactionId": "cb91fa80-d4a8-11ee-a357-ae468cd3ed16",
  "entityIds": [
    "74807f00-ed0d-11ee-a357-ae468cd3ed16",
    "72428380-0704-11ef-a357-ae468cd3ed16"
  ]
}]
```

**Partial-success on chunk failure.** If a later chunk fails after earlier chunks have committed, the response is still HTTP 200 carrying the durable chunks plus an error element marking the failed chunk's index. Subsequent chunks are not attempted.

```json
[
  { "transactionId": "tx-0", "entityIds": ["..."] },
  { "transactionId": "tx-1", "entityIds": ["..."] },
  { "error": { "code": "MODEL_NOT_FOUND", "message": "...", "chunkIndex": 2 } }
]
```

When the very first chunk fails (no durable progress), the response is the standard `application/problem+json` 4xx error envelope instead.

**GET /api/entity/{entityId}** — Read a single entity by UUID

- `entityId` (path): UUID string
- `pointInTime` (query, optional): RFC 3339 date-time — load entity state at this instant
- `transactionId` (query, optional): UUID — load entity state as of the end of this transaction

`pointInTime` and `transactionId` are mutually exclusive; supplying both returns `400 BAD_REQUEST`.

Response: `200 OK`, `application/json`:

```json
{
  "type": "ENTITY",
  "data": { "category": "physics", "year": "2024" },
  "meta": {
    "id": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
    "state": "NEW",
    "creationDate": "2025-08-01T10:00:00Z",
    "lastUpdateTime": "2025-08-01T10:00:00Z",
    "transactionId": "cb91fa80-d4a8-11ee-a357-ae468cd3ed16",
    "transitionForLatestSave": "loopback"
  }
}
```

**PUT /api/entity/{format}/{entityId}** — Update a single entity (loopback transition)

- `format` (path): `JSON` or `XML`
- `entityId` (path): UUID
- `If-Match` (header, optional): transaction ID of last read — optimistic concurrency; if the entity was modified since, returns `412 Precondition Failed`
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

Request body: updated entity JSON/XML payload.

Response: `200 OK`, `application/json`:

```json
{
  "transactionId": "733e7180-c055-11ef-a357-ae468cd3ed16",
  "entityIds": ["cdcff600-bab1-11ee-a357-ae468cd3ed16"]
}
```

**PUT /api/entity/{format}/{entityId}/{transition}** — Update a single entity with a named transition

- `format` (path): `JSON` or `XML`
- `entityId` (path): UUID
- `transition` (path): string — transition name defined in the model's workflow
- `If-Match` (header, optional): transaction ID
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

Response: `200 OK`, same shape as loopback update.

**PUT /api/entity/{format}** — Update a collection (mixed entities)

- `format` (path): `JSON` (only supported format today; single-item PUT endpoints still accept XML)
- `transactionWindow` (query, optional): int32, default `100`, max `1000` — maximum entities per transactional batch. Values outside (0, 1000] are rejected with `400 BAD_REQUEST`. Collections exceeding the window are split into multiple transactional batches committed sequentially; each chunk is one transaction. The response is an array with one element per chunk in commit order; chunks committed before any later failure remain durable.
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

**IMPORTANT — `payload` is a JSON-encoded string, not an object.**

The `payload` field in each update item must be a string containing the JSON-encoded entity body, not a nested JSON object (same contract as collection create).

Correct: `"payload": "{\"category\":\"physics\"}"`
Wrong:   `"payload": {"category":"physics"}`   (will be rejected with `errors.BAD_REQUEST`)

Request body: JSON array of update items:

```json
[
  {
    "id": "8824c480-c166-11ee-9e63-ae468cd3ed16",
    "payload": "{\"category\":\"physics\",\"year\":\"2024\"}",
    "transition": "UPDATE"
  }
]
```

Within a single chunk the update is all-or-nothing: if any entity in the chunk is not found, the entire chunk rolls back and earlier chunks remain durable. If the very first chunk fails (no durable progress), the response is the standard `application/problem+json` 4xx error envelope; otherwise the response is HTTP 200 with the per-chunk array (see partial-success shape under collection-create).

Response: `200 OK`, `application/json`, `EntityTransactionResponse` array — one element per committed chunk in commit order:

```json
[
  {
    "transactionId": "733e7180-c055-11ef-a357-ae468cd3ed16",
    "entityIds": ["8824c480-c166-11ee-9e63-ae468cd3ed16"]
  }
]
```

**DELETE /api/entity/{entityId}** — Delete a single entity by UUID

- `entityId` (path): UUID

Response: `200 OK`, `application/json`:

```json
{
  "id": "a2242880-8d30-11ef-9e63-ae468cd3ed16",
  "modelKey": {
    "name": "nobel-prize",
    "version": 4
  },
  "transactionId": "9fe62d00-a727-11ef-9e63-ae468cd3ed16"
}
```

**DELETE /api/entity/{entityName}/{modelVersion}** — Delete all entities for a model

- `entityName` (path): string
- `modelVersion` (path): int32

Response: `200 OK`, `application/json`:

```json
[{
  "deleteResult": {
    "idToError": {},
    "numberOfEntitites": 42,
    "numberOfEntititesRemoved": 42
  },
  "entityModelClassId": "31134900-d9cb-11ee-b913-ae468cd3ed16"
}]
```

**GET /api/entity/{entityName}/{modelVersion}** — List all entities for a model (paginated)

- `entityName` (path): string
- `modelVersion` (path): int32
- `pageSize` (query, optional): int32, default `20`
- `pageNumber` (query, optional): int32, default `0`

Response: `200 OK`, `application/json`, array of entity envelopes (same shape as single-entity GET).

**GET /api/entity/{entityId}/changes** — Get entity change history metadata

- `entityId` (path): UUID
- `pointInTime` (query, optional): RFC 3339 — view history as it existed at this time

Response: `200 OK`, `application/json`, array of change entries:

```json
[
  {
    "changeType": "CREATED",
    "timeOfChange": "2025-08-01T10:00:00Z",
    "user": "admin",
    "transactionId": "cb91fa80-d4a8-11ee-a357-ae468cd3ed16"
  },
  {
    "changeType": "UPDATED",
    "timeOfChange": "2025-08-02T09:00:00Z",
    "user": "admin",
    "transactionId": "733e7180-c055-11ef-a357-ae468cd3ed16"
  }
]
```

- `changeType`: `CREATED`, `UPDATED`, or `DELETED`
- `transactionId`: present only when `hasEntity` is true (i.e., entity payload exists at that version)

**GET /api/entity/{entityId}/transitions** — List available transitions for an entity

- `entityId` (path): UUID
- `pointInTime` (query, optional): RFC 3339
- `transactionId` (query, optional): UUID — derive point-in-time from transaction submit time

`pointInTime` and `transactionId` are mutually exclusive; supplying both returns `400 BAD_REQUEST`. When neither is provided, the current time is used.

Response: `200 OK`, `application/json`, array of available transition names (as returned by the workflow engine).

**GET /api/platform-api/entity/fetch/transitions** — List available transitions (platform-api format)

- `entityClass` (query, required): string in `Name.Version` format, e.g., `Offer.1`
- `entityId` (query, required): UUID string

Response: `200 OK`, `application/json`, array of available transition names.

**GET /api/entity/stats** — Entity count statistics across all models

Response: `200 OK`, `application/json`:

```json
[
  { "modelName": "nobel-prize", "modelVersion": 1, "count": 42 },
  { "modelName": "family-member", "modelVersion": 3, "count": 7 }
]
```

**GET /api/entity/stats/states** — Entity count by state across all models

- `states` (query, optional): comma-separated list of state names to filter by; maximum 1000 entries

Response: `200 OK`, `application/json`:

```json
[
  { "modelName": "nobel-prize", "modelVersion": 1, "state": "NEW", "count": 10 },
  { "modelName": "nobel-prize", "modelVersion": 1, "state": "APPROVED", "count": 32 }
]
```

**GET /api/entity/stats/{entityName}/{modelVersion}** — Entity count for a specific model

- `entityName` (path): string
- `modelVersion` (path): int32

Response: `200 OK`, `application/json`, single `ModelStatsDto`.

**GET /api/entity/stats/states/{entityName}/{modelVersion}** — Entity count by state for a specific model

- `entityName` (path): string
- `modelVersion` (path): int32
- `states` (query, optional): list of state names to filter by; maximum 1000 entries

Response: `200 OK`, `application/json`, array of `ModelStateStatsDto`.

## ENTITY ENVELOPE

All entity read operations return entities in the standard envelope:

```json
{
  "type": "ENTITY",
  "data": { ... },
  "meta": {
    "id": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
    "modelKey": { "name": "nobel-prize", "version": 1 },
    "state": "NEW",
    "creationDate": "2025-08-01T10:00:00.000000000Z",
    "lastUpdateTime": "2025-08-01T10:00:00.000000000Z",
    "transactionId": "cb91fa80-d4a8-11ee-a357-ae468cd3ed16",
    "transitionForLatestSave": "UPDATE"
  }
}
```

- `type` — always `"ENTITY"`
- `data` — the entity's JSON payload (decoded with `json.Number` for numeric precision)
- `meta.id` — UUID string
- `meta.modelKey` — object with `name` (string) and `version` (int32) identifying the model; present in single-entity `GET /entity/{id}` responses. Omitted from list/search results because the model is already part of the request path (`/api/entity/{entityName}/{modelVersion}`).
- `meta.state` — current workflow state string
- `meta.creationDate` — RFC 3339 with nanoseconds
- `meta.lastUpdateTime` — RFC 3339 with nanoseconds
- `meta.transactionId` — present when a transaction ID exists
- `meta.transitionForLatestSave` — transition name that produced the latest save. Valid values: `"loopback"` (loopback update with no transition supplied by the client) or the named transition string. **Known bug (#94):** the server currently stores the literal `"workflow"` for engine-driven initial-state writes; there is no valid `"workflow"` value and this is tracked for fix.

## OPTIMISTIC CONCURRENCY

Entity writes are protected by two independent guards:

1. **Transaction-level (always on):** every write runs in a SERIALIZABLE transaction with first-committer-wins (SI+FCW) read-set validation at commit time. A concurrent committer who changes an entity this transaction read will cause this transaction to abort at commit with `409 CONFLICT, retryable: true` (see `errors.CONFLICT`). PostgreSQL's own SQLSTATE 40001 detection covers write-write races equivalently. Callers do not opt in to this; it cannot be disabled.
2. **Cross-request precondition (opt-in via `If-Match`):** the `If-Match` header carries the `meta.transactionId` from the caller's earlier read in a separate HTTP request. If the entity's current `meta.transactionId` does not match, the server returns `412 Precondition Failed` (see `errors.ENTITY_MODIFIED`). This catches the race window between the caller's GET and PUT — a window the transaction-level guard cannot see, because the GET and PUT happen in different transactions.

To use the cross-request precondition: read the entity (`GET /entity/{id}`), note `meta.transactionId`, include it in `If-Match` on the subsequent update. Omitting `If-Match` does not turn off transaction-level conflict detection, but it does mean the PUT will reconcile against whatever state the entity has at PUT-time — including any concurrent commits that happened between the caller's GET and PUT.

See `cyoda help errors ENTITY_MODIFIED` for the recovery flow on a `412`.

## ERRORS

- `errors.ENTITY_NOT_FOUND` — `404` — entity UUID does not exist
- `errors.ENTITY_MODIFIED` — `412` — `If-Match`-guarded update rejected; supplied transaction ID does not match the entity's current version
- `errors.MODEL_NOT_FOUND` — `404` — model referenced during create does not exist
- `errors.MODEL_NOT_LOCKED` — `409` — model exists but is not in `LOCKED` state; entities cannot be created until the model is locked
- `errors.VALIDATION_FAILED` — `400` — payload fails schema validation against the model
- `errors.INCOMPATIBLE_TYPE` — `400` — entity payload's leaf value type is not assignable to the schema's declared DataType for that field; carries `fieldPath`, `expectedType`, `actualType` in `properties`
- `errors.CONFLICT` — `409` — storage-level transaction serialization conflict (retryable)
- `errors.IDEMPOTENCY_CONFLICT` — `409` — reserved; not yet implemented (#91). Future contract: returned on collection create/update when the `Idempotency-Key` header is re-used with a different payload body
- `errors.TRANSITION_NOT_FOUND` — `404` — named transition does not exist in the workflow
- `errors.BAD_REQUEST` — `400` — malformed request, invalid UUID, conflicting query parameters, states filter exceeds 1000 entries

## EXAMPLES

**Create a single entity:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"category":"physics","year":"2024"}' \
  "http://localhost:8080/api/entity/JSON/nobel-prize/1"
```

**Read an entity:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/74807f00-ed0d-11ee-a357-ae468cd3ed16"
```

**Read an entity at a point in time:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/74807f00-ed0d-11ee-a357-ae468cd3ed16?pointInTime=2025-08-01T10:00:00Z"
```

**Update an entity with loopback transition:**

```
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "If-Match: cb91fa80-d4a8-11ee-a357-ae468cd3ed16" \
  -d '{"category":"chemistry","year":"2024"}' \
  "http://localhost:8080/api/entity/JSON/74807f00-ed0d-11ee-a357-ae468cd3ed16"
```

**Update an entity with a named transition:**

```
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"category":"chemistry","year":"2024"}' \
  "http://localhost:8080/api/entity/JSON/74807f00-ed0d-11ee-a357-ae468cd3ed16/APPROVE"
```

**Delete a single entity:**

```
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/74807f00-ed0d-11ee-a357-ae468cd3ed16"
```

**Delete all entities for a model:**

```
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/nobel-prize/1"
```

**List all entities for a model (page 0, size 20):**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/nobel-prize/1?pageSize=20&pageNumber=0"
```

**Get entity change history:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/74807f00-ed0d-11ee-a357-ae468cd3ed16/changes"
```

**Get available transitions:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/74807f00-ed0d-11ee-a357-ae468cd3ed16/transitions"
```

**Create a multi-model collection:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '[{"model":{"name":"nobel-prize","version":1},"payload":"{\"category\":\"physics\",\"year\":\"2024\"}"}]' \
  "http://localhost:8080/api/entity/JSON"
```

**Get statistics by state for a model:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/entity/stats/states/nobel-prize/1"
```

## SEE ALSO

- models
- search
- workflows
- errors.ENTITY_NOT_FOUND
- errors.ENTITY_MODIFIED
- errors.MODEL_NOT_FOUND
- errors.MODEL_NOT_LOCKED
- errors.VALIDATION_FAILED
- errors.INCOMPATIBLE_TYPE
- errors.CONFLICT
- errors.TRANSITION_NOT_FOUND
- openapi
