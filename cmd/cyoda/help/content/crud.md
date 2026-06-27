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
PATCH  /api/entity/{format}/{entityId}
PATCH  /api/entity/{format}/{entityId}/{transition}
DELETE /api/entity/{entityId}
DELETE /api/entity/{entityName}/{modelVersion}
GET    /api/entity/{entityName}/{modelVersion}
GET    /api/entity/{entityId}/changes
GET    /api/entity/{entityId}/transitions
GET    /api/entity/stats
GET    /api/entity/stats/states
GET    /api/entity/stats/{entityName}/{modelVersion}
GET    /api/entity/stats/states/{entityName}/{modelVersion}
POST   /api/entity/stats/{entityName}/{modelVersion}/query
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
- `transactionWindow` (query, optional): int32, default `100`, max `1000` — applies only when the request body is a JSON array. Maximum entities per transactional batch. Values outside (0, 1000] are rejected with `400 BAD_REQUEST`. Array bodies exceeding the window are split into multiple transactional batches committed sequentially; each chunk is one transaction. The response is then an array with one element per chunk in commit order; chunks committed before any later failure remain durable.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

If the request body is a JSON array, each element is treated as a separate entity of the same model and the collection-create chunking contract applies (see `transactionWindow` above and the `POST /api/entity/{format}` partial-success shape below).

Response: `200 OK`, `application/json`. Single-object body returns a one-element array; an array body returns one element per committed chunk in commit order:

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

Each item may also carry an optional `ifMatch` field — the entity's last-known `meta.transactionId` from a prior read. Items with `ifMatch` get a per-item cross-request optimistic-concurrency precondition: if the entity has been modified since, the item is rejected with `code=ENTITY_MODIFIED` and surfaces in the chunk's `failed` array, **without rolling the chunk back**. Items in the same chunk without `ifMatch` (or with a still-valid one) commit as usual. This mirrors the `If-Match` header on the single-item PUT endpoints, scoped per-item.

Request body: JSON array of update items:

```json
[
  {
    "id": "8824c480-c166-11ee-9e63-ae468cd3ed16",
    "payload": "{\"category\":\"physics\",\"year\":\"2024\"}",
    "transition": "UPDATE",
    "ifMatch": "733e7180-c055-11ef-a357-ae468cd3ed16"
  }
]
```

Failure handling within a chunk:

- **Per-item `ENTITY_MODIFIED` (only when `ifMatch` is supplied):** the item surfaces in `failed[]`; siblings still commit; the chunk's `transactionId` is reported. Per-item ENTITY_MODIFIED conflicts surface inside the `200` response body via the `failed[]` array, **not** as a `4xx` envelope. Inspect `failed[]` to detect them. The `4xx` envelope is reserved for chunk-wide infrastructure failures.
- **Any other per-item failure** (missing entity, validation, non-conflict engine error): the entire chunk rolls back, matching the pre-existing contract. Earlier chunks remain durable; if the first chunk fails the response is a standard `application/problem+json` 4xx envelope.

Each per-item `ENTITY_MODIFIED` is also reflected in the entity's state-machine audit log: the engine emits a paired `STATE_MACHINE_START` plus `TRANSITION_ABORTED` event (with `data.reason = "ENTITY_MODIFIED"`, `data.expectedTxId`, and `data.actualTxId`) so consumers can correlate the failure cleanly without orphaned start events.

Response: `200 OK`, `application/json`, `EntityTransactionResponse` array — one element per committed chunk in commit order. The optional `failed` array is omitted on chunks with no per-item `ENTITY_MODIFIED` failures:

```json
[
  {
    "transactionId": "733e7180-c055-11ef-a357-ae468cd3ed16",
    "entityIds": ["8824c480-c166-11ee-9e63-ae468cd3ed16"],
    "failed": [
      {
        "entityId": "31134900-d9cb-11ee-9e63-ae468cd3ed16",
        "error": {
          "code": "ENTITY_MODIFIED",
          "message": "entity has been modified since last read",
          "itemIndex": 1
        }
      }
    ]
  }
]
```

`itemIndex` is the failing item's zero-based position within its chunk's request slice (per-chunk relative). When every item in a chunk fails its `ifMatch` precondition, the chunk still commits as a zero-write transaction — `entityIds` is empty, `failed[]` lists every item, and `transactionId` remains meaningful for audit correlation.

**PATCH /api/entity/{format}/{entityId}** — Partial update of a single entity (loopback transition)

- `format` (path): `JSON` only — Merge Patch is JSON-only by RFC 7386; `XML` ⇒ `415 Unsupported Media Type`
- `entityId` (path): UUID
- `Content-Type` (header, required): `application/merge-patch+json` (RFC 7386 merge patch, implemented) or `application/json-patch+json` (RFC 6902, returns `501 Not Implemented`); any other value ⇒ `415 Unsupported Media Type`
- `If-Match` (header, required in some form): see the three-state table below
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

Request body: a sparse JSON object (the patch document). The patch is applied to the **stored** entity payload using RFC 7386 merge semantics:

- A key present in the patch with a non-null value overwrites the target key.
- A key whose value is itself an object merges recursively.
- A key present in the patch with an explicit `null` value deletes that key from the stored payload.
- Arrays are replaced wholesale — there are no element-level operations.
- The empty patch `{}` is a valid no-op merge: the data is unchanged, but the request still commits a new transaction and fires the loopback transition (consistent with PUT-with-empty-body).

**If-Match precondition (required).** Unlike `PUT`, where omitting `If-Match` means unconditional replace, PATCH requires `If-Match` to be present in some form, because the merge is applied relative to a base the caller read; silently patching a stale base risks lost updates. The token is the `meta.transactionId` from the caller's last `GET` of this entity — the same field the existing `PUT` `If-Match` uses.

Three states are accepted:

- `If-Match: "<transactionId>"` — Conditional: the stored entity's `transactionId` must still match; returns `412 Precondition Failed` if it has moved since the caller's read.
- `If-Match: *` — Unconditional opt-out: merge onto current state regardless of version (entity must exist); a concurrent-writer race can still surface as `409` (see below).
- Absent — returns `428 Precondition Required`; the response body explains the two valid choices.

Response: `200 OK`, same shape as the single-item PUT update.

**PATCH /api/entity/{format}/{entityId}/{transition}** — Partial update of a single entity with a named transition

- `format` (path): `JSON` only
- `entityId` (path): UUID
- `transition` (path): string — transition name defined in the model's workflow
- `Content-Type` (header, required): same two-value table as the loopback form
- `If-Match` (header, required in some form): same three-state table as the loopback form
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.
- `waitForConsistencyAfter` (query, optional): boolean, default `false` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

The merge patch is applied first; the named transition's processors then run on the merged state and may further mutate the entity. Response: `200 OK`, same shape as the loopback form.

**Two behaviours to be aware of:**

1. **Strict validation.** The merged result is validated strictly against the model schema — the model is never extended, regardless of its `ChangeLevel`. A PATCH cannot introduce a field that the schema does not already allow, even when the model is in an extend-permitting mode. To add a genuinely new field, use `PUT` (which may extend the schema), then PATCH thereafter.

2. **Processors run after the merge.** Under a named transition, the transition's processors run on the merged entity state and may overwrite fields the patch set. The observable result of a patch with a named transition is not guaranteed to retain the patched values when the transition has mutating processors. This is the same ordering as `PUT` plus a named transition.

**Partial-update error codes:**

- `404 Not Found` — entity does not exist
- `409 Conflict` (retryable) — a concurrent writer committed between the in-transaction base read and this save (read-set conflict at commit); may occur even with `If-Match: *`; caller may retry
- `412 Precondition Failed` — `If-Match: "<transactionId>"` supplied and the stored `transactionId` differs; see `errors.ENTITY_MODIFIED`
- `415 Unsupported Media Type` — `XML` format, or a `Content-Type` other than the two patch media types
- `428 Precondition Required` — no `If-Match` header present
- `501 Not Implemented` — `Content-Type: application/json-patch+json` (RFC 6902 is recognised but not yet implemented)
- Standard `4xx` domain errors — the **merged result** fails strict schema validation (full domain detail + error code)

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

**POST /api/entity/stats/{entityName}/{modelVersion}/query** — Grouped statistics with optional aggregations

Returns aggregate counts (and optional `sum`/`avg`/`min`/`max`/`stdev`) grouped by entity data fields and/or lifecycle state. Restricts the population by an optional `Condition` DSL predicate (same shape as `/search/*` — see the `search` topic). Supports `pointInTime` historical snapshots.

- `entityName` (path): string
- `modelVersion` (path): int32

Request body: `application/json`. Body size limit: 10 MiB (shared with `/search/*`).

```json
{
  "groupBy":      ["$.variantId", "state"],
  "condition":    { "type": "lifecycle", "field": "state", "operatorType": "NOT_EQUAL", "value": "shipped" },
  "aggregations": [
    { "op": "sum",   "field": "$.costPrice", "as": "totalCost" },
    { "op": "avg",   "field": "$.costPrice" },
    { "op": "stdev", "field": "$.costPrice" }
  ],
  "pointInTime":  "2026-06-14T12:00:00Z",
  "limit":        100
}
```

Request fields:

- `groupBy` (required, 1..N entries): each entry is the reserved token `"state"` or a scalar JSONPath. Order in the request determines order in the response's `groupKey` array. Duplicate entries (after normalization) → 400 `DUPLICATE_GROUP_BY`. Array projections (`[*]`, `[0]`) → 400 `INVALID_GROUP_BY_PATH`.
- `condition` (optional): the existing search `Condition` DSL (SimpleCondition, LifecycleCondition, GroupCondition with `AND`/`OR`, ArrayCondition, FunctionCondition). Omitted → match-all. See the `search` topic for the full DSL.
- `aggregations` (optional, 0..N): per entry, `op` ∈ {`sum`, `avg`, `min`, `max`, `stdev`}; `field` is a scalar JSONPath into the entity payload; optional `as` alias for the response key. When `as` is omitted the server synthesizes `<op>_<field>` with the leading `$.` stripped from the field (for example, `field: "$.costPrice"` → alias `sum_costPrice`). The server dedupes identical `(op, field)` pairs. Two aliases colliding on distinct `(op, field)` pairs → 400 `DUPLICATE_AGGREGATION_ALIAS`.
- `pointInTime` (optional RFC 3339): historical snapshot; default = now.
- `limit` (optional positive int): top-N. Must be `≤ CYODA_STATS_GROUP_MAX` (default 10000); `> CYODA_STATS_GROUP_MAX` → 400 `INVALID_LIMIT`. Default = unlimited (up to the cardinality ceiling).

Response: `200 OK`, `application/json`, array of `GroupedStatsBucket`:

```json
[
  {
    "groupKey": [
      { "path": "$.variantId", "value": "1111" },
      { "path": "state",       "value": "available" }
    ],
    "count": 812,
    "aggregations": { "totalCost": 41200.00, "avg_costPrice": 50.74, "stdev_costPrice": 18.42 }
  },
  {
    "groupKey": [
      { "path": "$.variantId", "value": null },
      { "path": "state",       "value": "available" }
    ],
    "count": 3,
    "aggregations": { "totalCost": null, "avg_costPrice": null, "stdev_costPrice": null }
  }
]
```

Each bucket's `aggregations` map is keyed by either the explicit `as` alias or the synthesized `<op>_<field>` label. The `aggregations` map is omitted entirely when the request supplied no aggregations.

Sort order is backend-independent: primary key is `count` descending; tiebreaker is `groupKey` lex order (element-wise; `null` sorts before any string; strings compared bytes-wise).

**JSONPath restrictions.** Every JSONPath in `groupBy` and aggregation `field` is scalar-only. Bracket-quoted property access (`$['my.field']`) is accepted. Array projections (`$.items[*]`, `$.items[0]`) are rejected at validation time with 400 `INVALID_GROUP_BY_PATH` (for `groupBy`) or 400 `INVALID_AGGREGATION_FIELD` (for aggregations). The reserved token `"state"` is accepted in `groupBy` only; it has no leading `$.` and refers to lifecycle state.

**Aggregation operators.** Five operators: `sum`, `avg`, `min`, `max`, `stdev`. `stdev` is the sample standard deviation (divisor `n − 1`); when `n < 2`, the value is `null` on both the pushdown and streaming paths. `sum`, `avg`, and `stdev` treat non-numeric and absent field values as NULL (the value is skipped, not zero). `min` and `max` are lexicographic over text values and numeric over numeric values; the comparison ordering matches the underlying backend's collation for the pushed-down path.

**Numeric coercion on postgres.** The postgres backend wraps every numeric aggregation argument in the `cyoda_try_float8(text)` SQL function (shipped as a built-in migration). The behavior is exhaustive:

- Empty string → NULL.
- Strict-numeric string matching `-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?` and within `float8` range → parsed `float8`.
- Strict-numeric string that overflows `float8` (for example `1e500`) → NULL (the value would be `Infinity`; stripped by an outer `NULLIF`).
- `NaN`, `Infinity`, `-Infinity`, `inf` → NULL (not strict-numeric per the regex above).
- Any other non-numeric string (`"n/a"`, `"unknown"`, etc.) → NULL.

The function is `IMMUTABLE PARALLEL SAFE` (the planner inlines and parallelizes). NULL values are skipped by `SUM`/`AVG`/`STDDEV_SAMP` per standard SQL semantics. A single dirty value does not abort the query.

**Non-scalar runtime values.** When a `groupBy` JSONPath resolves to a JSON object or array at runtime, the bucket key for that dimension is `null`. Numbers and booleans group by their canonical text representation (for example the integer `42` and the string `"42"` both bucket under `"42"`).

**In-transaction behavior.** Calls made under an active transaction (the request carried a transaction context) route through the streaming-tally path via the SPI `Iterable` interface. The native `GroupedAggregator` pushdown is skipped in this case to preserve read-your-writes semantics. Per backend:

- **memory** — `Iterate` captures a snapshot under the read lock, overlays `tx.Buffer` (buffered saves), and masks `tx.Deletes` (buffered deletes). The iteration runs lock-free. RYW-correct.
- **sqlite** — the iterator dispatches by `(in-tx, point-in-time)`. Non-tx, non-PIT queries the live `entities` table directly with `planQuery` WHERE-pushdown (no snapshot involved). Non-tx with `pointInTime` queries `entity_versions` with `submit_time <= pointInTime` to read the historical snapshot. In-tx, non-PIT materializes via the same `getAllTx` overlay that `GetAll` uses inside a tx (entity_versions snapshot at `tx.SnapshotTime`, plus `tx.Buffer` overlay, minus `tx.Deletes`), then iterates the slice; RYW-correct, with buffered writes visible and buffered deletes hidden. In-tx with `pointInTime` falls through to the plain PIT path — reads `entity_versions` at the supplied snapshot WITHOUT applying the tx-buffer overlay; PIT is historical-read by definition, so the in-flight buffer is a documented limitation, and the result reflects committed history rather than the caller's uncommitted edits at the requested instant. The fully-pushed-down `GroupedAggregate` query (against `entities`) is skipped in-tx by the SPI dispatcher so the service falls through to the streaming tally over `Iterate`, which now honours RYW.
- **postgres** — `Iterate` selects from the bi-temporal `entity_versions` table with `valid_time <= tx.SnapshotTime AND transaction_time <= CURRENT_TIMESTAMP`, and adds `(doc->'_meta'->>'deleted')::boolean IS NOT TRUE` to skip deletion-marker versions. The `GroupedAggregate` pushdown is skipped in-tx.

**Cardinality ceiling.** `CYODA_STATS_GROUP_MAX` (default 10000) bounds the number of distinct group buckets the endpoint will produce. When the result would exceed the ceiling, the request fails with 422 `GROUP_CARDINALITY_EXCEEDED` (retry with a more selective `condition` or fewer `groupBy` dimensions). The same value caps the request `limit`: `limit > CYODA_STATS_GROUP_MAX` is rejected up-front with 400 `INVALID_LIMIT`.

**Backend capability.** The endpoint requires the storage backend to implement at least one of the optional SPI interfaces `Iterable` or `GroupedAggregator`. The three plugins shipped in this repository (`memory`, `sqlite`, `postgres`) implement both. Backends that implement neither return 501 `NOT_IMPLEMENTED_BY_BACKEND`.

**Index guidance — postgres.**

- **State grouping/filtering on the non-tx pushdown path is index-backed out of the box.** The shipped migration creates `entities_state_idx` on `(tenant_id, model_name, model_version, (doc->'_meta'->>'state')) WHERE NOT deleted`. Queries grouping by or filtering on `state` use this index without operator action.
- **In-tx and `pointInTime` paths are not covered.** Those read from `entity_versions`, not `entities`. The state index does not apply. Add expression indexes on `entity_versions` if perf demands.
- **Hot data-field dimensions** (the dimensions that appear most often in `groupBy` and `condition`) are caller responsibility on the non-tx path. Example for `variantId`:

  ```sql
  CREATE INDEX entities_variantid_idx
  ON entities (tenant_id, model_name, model_version, (doc->>'variantId'))
  WHERE NOT deleted;
  ```

**Index guidance — sqlite.** The shipped schema indexes `(tenant_id, model_name, model_version)` covering the WHERE-clause prefix. For hot grouping dimensions, add expression indexes on the JSON path:

```sql
CREATE INDEX entities_variantid_idx
ON entities (json_extract(data, '$.variantId'));
```

**Memory backend cost.** The snapshot walk is `O(tenant entities)`, not `O(model entities)` — the underlying map is keyed `tenantID → entityID → versions` and is not partitioned by model. A tenant with many models pays a constant per-request walk cost proportional to all their entities, even when querying one model. Relevant for operators running memory-backed deployments with many models per tenant.

Error codes (response carries RFC 9457 problem+json with `properties.errorCode` set to the machine-readable code below):

- `MALFORMED_REQUEST` — `400` — JSON parse failed
- `UNKNOWN_MODEL` — `400` — path does not resolve for the calling tenant
- `MISSING_GROUP_BY` — `400` — `groupBy` empty or missing
- `INVALID_GROUP_BY_PATH` — `400` — empty entry, or array projection in a `groupBy` JSONPath
- `DUPLICATE_GROUP_BY` — `400` — duplicate entries after normalization
- `INVALID_AGGREGATION_OP` — `400` — `op` outside the set {`sum`, `avg`, `min`, `max`, `stdev`}
- `INVALID_AGGREGATION_FIELD` — `400` — aggregation `field` empty or contains array projection
- `DUPLICATE_AGGREGATION_ALIAS` — `400` — two aliases collide on distinct `(op, field)` pairs
- `INVALID_OPERATOR` — `400` — `condition` operator outside the canonical list (propagated from search validator)
- `INVALID_CONDITION` — `400` — `condition` malformed or unknown `type` (propagated from search validator)
- `INVALID_FIELD_PATH` — `400` — `condition` JSONPath absent from the locked schema (propagated from search validator)
- `CONDITION_TYPE_MISMATCH` — `400` — `condition` value type incompatible with the locked DataType (propagated from search validator)
- `INVALID_POINT_IN_TIME` — `400` — `pointInTime` not parseable as RFC 3339
- `INVALID_LIMIT` — `400` — `limit` non-positive or `> CYODA_STATS_GROUP_MAX`
- `GROUP_CARDINALITY_EXCEEDED` — `422` — result buckets would exceed `CYODA_STATS_GROUP_MAX`
- `NOT_IMPLEMENTED_BY_BACKEND` — `501` — backend implements neither `Iterable` nor `GroupedAggregator`
- Standard `401` (missing/invalid Bearer), `403` (authenticated but not authorized), `413` (body exceeds 10 MiB), `500` (internal/driver error with ticket UUID; full detail logged server-side) apply as elsewhere.

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
- `meta.transitionForLatestSave` — transition name that produced the latest save. Valid values: `"loopback"` (loopback update with no transition supplied by the client) or the named transition string. **Known bug:** the server currently stores the literal `"workflow"` for engine-driven initial-state writes; there is no valid `"workflow"` value and this is tracked for fix.

## OPTIMISTIC CONCURRENCY

Entity writes are protected by two independent guards:

1. **Transaction-level (always on):** every write runs under Snapshot Isolation with first-committer-wins (SI+FCW) read-set validation at commit time. A concurrent committer who changes an entity this transaction read will cause this transaction to abort at commit with `409 CONFLICT, retryable: true` (see `errors.CONFLICT`). On the postgres backend, PostgreSQL's own SQLSTATE 40001 detection (under `REPEATABLE READ`) covers write-write races equivalently. Callers do not opt in to this; it cannot be disabled.
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
- `errors.IDEMPOTENCY_CONFLICT` — `409` — reserved; not yet implemented. Future contract: returned on collection create/update when the `Idempotency-Key` header is re-used with a different payload body
- `errors.TRANSITION_NOT_FOUND` — `404` — named transition does not exist in the workflow
- `errors.BAD_REQUEST` — `400` — malformed request, invalid UUID, conflicting query parameters, states filter exceeds 1000 entries
- Grouped-stats query (`POST /api/entity/stats/{entityName}/{modelVersion}/query`) — `400` for validation failures (`MALFORMED_REQUEST`, `UNKNOWN_MODEL`, `MISSING_GROUP_BY`, `INVALID_GROUP_BY_PATH`, `DUPLICATE_GROUP_BY`, `INVALID_AGGREGATION_OP`, `INVALID_AGGREGATION_FIELD`, `DUPLICATE_AGGREGATION_ALIAS`, `INVALID_POINT_IN_TIME`, `INVALID_LIMIT`); `400` propagated from the search-condition validator (`INVALID_OPERATOR`, `INVALID_CONDITION`, `INVALID_FIELD_PATH`, `CONDITION_TYPE_MISMATCH`); `422 GROUP_CARDINALITY_EXCEEDED` when distinct buckets would exceed `CYODA_STATS_GROUP_MAX`; `501 NOT_IMPLEMENTED_BY_BACKEND` when the storage backend implements neither `Iterable` nor `GroupedAggregator`. The full enumeration with descriptions is in the grouped-stats endpoint section above.

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

**Grouped statistics — count by state and country, with sum/avg aggregations, top 5000:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "groupBy": ["state", "$.country"],
    "condition": {"type":"simple","jsonPath":"$.year","operatorType":"GREATER_OR_EQUAL","value":2000},
    "aggregations": [
      {"op":"sum","field":"$.amount","as":"totalAmount"},
      {"op":"avg","field":"$.amount"}
    ],
    "limit": 5000
  }' \
  "http://localhost:8080/api/entity/stats/nobel-prize/1/query"
```

**Grouped statistics — count-only (no aggregations), at a historical point in time:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "groupBy": ["$.variantId"],
    "pointInTime": "2026-01-01T00:00:00Z"
  }' \
  "http://localhost:8080/api/entity/stats/nobel-prize/1/query"
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
