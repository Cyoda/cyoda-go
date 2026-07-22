---
topic: search
title: "search — entity search API"
stability: stable
see_also:
  - crud
  - models
  - analytics
  - errors.MODEL_NOT_FOUND
  - errors.SEARCH_JOB_NOT_FOUND
  - errors.SEARCH_JOB_ALREADY_TERMINAL
  - errors.SEARCH_RESULT_LIMIT
  - errors.SCAN_BUDGET_EXHAUSTED
  - errors.SEARCH_SHARD_TIMEOUT
  - errors.INVALID_FIELD_PATH
  - errors.CONDITION_TYPE_MISMATCH
  - openapi
---

# search

## NAME

search — entity search API: synchronous direct search and asynchronous snapshot search. Entity statistics endpoints (`/api/entity/stats/...`) are documented in the `crud` topic.

## SYNOPSIS

```
POST   /api/search/direct/{entityName}/{modelVersion}
POST   /api/search/async/{entityName}/{modelVersion}
GET    /api/search/async/{jobId}
GET    /api/search/async/{jobId}/status
PUT    /api/search/async/{jobId}/cancel
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). All endpoints require `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`.

## DESCRIPTION

Search operates against a specific entity model `(entityName, modelVersion)`. Two modes are supported:

**Synchronous (direct) search**: `POST /search/direct/{entityName}/{modelVersion}`. Executes inline within the HTTP request. The response is an NDJSON stream (`application/x-ndjson`), one entity envelope per line. The default result limit is 1000 entities per request; the maximum is 10000 — values above 10000 are rejected with `400 BAD_REQUEST`.

**Asynchronous search**: `POST /search/async/{entityName}/{modelVersion}`. Submits a search job and returns a job UUID immediately. The search executes in a background goroutine (or in the plugin's own executor for `SelfExecutingSearchStore` plugins). Results are retrieved by polling status and then fetching pages.

Both modes accept the same `Condition` DSL as the request body. When the storage plugin implements `spi.Searcher`, the condition is translated to a plugin-level predicate and pushed down to the backend — including inside an active transaction, where the pushdown is read-your-own-writes correct against the transaction's own uncommitted writes (see `trackingRead` below and `docs/CONSISTENCY.md` §3c). Only when translation fails (unsupported condition type) does the service fall back to in-memory filtering after a full `GetAll` scan.

## CONDITION DSL

All search requests accept a `Condition` JSON document as the POST body. Conditions are parsed recursively up to a maximum nesting depth of 50. Body size limit: 10 MiB.

**SimpleCondition** — match a single JSON path against a scalar value:

```json
{
  "type": "simple",
  "jsonPath": "$.category",
  "operatorType": "EQUALS",
  "value": "physics"
}
```

- `type`: `"simple"`
- `jsonPath`: JSONPath string (e.g., `"$.year"`, `"$.laureates[0].firstname"`)
- `operatorType` (also accepted as `operator` or `operation`): operator string (see valid values below)
- `value`: any JSON scalar

**Valid `operatorType` values** (exhaustive):
- `EQUALS` — exact equality; numeric-aware (JSON number vs string representation)
- `NOT_EQUAL` — inequality; inverse of EQUALS
- `GREATER_THAN` — numeric or lexicographic greater-than
- `LESS_THAN` — numeric or lexicographic less-than
- `GREATER_OR_EQUAL` — greater-than or equal
- `LESS_OR_EQUAL` — less-than or equal
- `CONTAINS` — substring or array-element containment
- `NOT_CONTAINS` — inverse of CONTAINS
- `STARTS_WITH` — string prefix match
- `NOT_STARTS_WITH` — inverse of STARTS_WITH
- `ENDS_WITH` — string suffix match
- `NOT_ENDS_WITH` — inverse of ENDS_WITH
- `LIKE` — SQL-style LIKE pattern (`%` = any sequence, `_` = any single char)
- `IS_NULL` — field is absent or JSON null
- `NOT_NULL` — field is present and not JSON null
- `BETWEEN` — range check (exclusive bounds); `value` must be a two-element array `[low, high]`
- `BETWEEN_INCLUSIVE` — range check (inclusive bounds); same `value` shape as BETWEEN
- `MATCHES_PATTERN` — regular expression match
- `IEQUALS` — case-insensitive EQUALS
- `INOT_EQUAL` — case-insensitive NOT_EQUAL
- `ICONTAINS` — case-insensitive CONTAINS
- `INOT_CONTAINS` — case-insensitive NOT CONTAINS
- `ISTARTS_WITH` — case-insensitive STARTS_WITH
- `INOT_STARTS_WITH` — case-insensitive NOT STARTS_WITH
- `IENDS_WITH` — case-insensitive ENDS_WITH
- `INOT_ENDS_WITH` — case-insensitive NOT ENDS_WITH

Operator strings outside this list are rejected with `errors.BAD_REQUEST` at request time; the error detail includes the canonical list.

**LifecycleCondition** — match entity lifecycle metadata:

```json
{
  "type": "lifecycle",
  "field": "state",
  "operatorType": "EQUALS",
  "value": "APPROVED"
}
```

- `type`: `"lifecycle"`
- `field`: `state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave` (alias `previousTransition`), `transactionId`, `id`
- `operatorType` (also accepted as `operator` or `operation`): operator string — same valid values as for `SimpleCondition`
- `value`: any JSON scalar

`creationDate`/`lastUpdateTime` are temporal: compared chronologically at millisecond resolution, and accept only comparison operators (`EQUALS`, `NOT_EQUAL`, `GREATER_THAN`, `LESS_THAN`, `GREATER_OR_EQUAL`, `LESS_OR_EQUAL`, `BETWEEN`, `IS_NULL`, `NOT_NULL`) with offset-bearing RFC3339 values. A string/pattern operator or a non-timestamp value on either field is rejected `400 CONDITION_TYPE_MISMATCH`; an unknown meta filter field is rejected `400 INVALID_FIELD_PATH`.

**GroupCondition** — combine conditions with a logical operator:

```json
{
  "type": "group",
  "operator": "AND",
  "conditions": [
    { "type": "simple", "jsonPath": "$.year", "operatorType": "EQUALS", "value": "2024" },
    { "type": "lifecycle", "field": "state", "operatorType": "EQUALS", "value": "NEW" }
  ]
}
```

- `type`: `"group"`
- `operator`: `"AND"` or `"OR"` — these are the only supported values; any other string produces `errors.BAD_REQUEST` at match time ("unknown group operator")
- `conditions`: array of `Condition` objects (recursive; maximum nesting depth 50)

`"NOT"` is not supported. An `AND` group with an empty `conditions` array evaluates to `true` (vacuous conjunction). An `OR` group with an empty `conditions` array evaluates to `false` (vacuous disjunction).

**EMPTY CONDITION**: Submitting an empty body (`{}`) or a body with no `type` field as the top-level search condition is rejected with `errors.BAD_REQUEST` — the parser requires a valid `type` field. Submitting a valid `AND` group with an empty `conditions` array (`{"type":"group","operator":"AND","conditions":[]}`) is accepted and matches all entities — this is the correct way to retrieve all entities without filtering.

**ArrayCondition** — match positional values in a JSON array:

```json
{
  "type": "array",
  "jsonPath": "$.laureates",
  "values": ["John", null, "Hopfield"]
}
```

- `type`: `"array"`
- `jsonPath`: path to the array field
- `values`: positional values; `null` entries match any value at that index

**FunctionCondition** — server-side function predicate dispatched to a compute member:

```json
{
  "type": "function",
  "function": {
    "name": "my-criteria-fn",
    "config": {
      "calculationNodesTags": "approval-service",
      "attachEntity": true,
      "responseTimeoutMs": 30000
    }
  }
}
```

- `type`: `"function"`
- `function.name`: string — identifies the function; becomes `criteriaId` / `criteriaName` in the dispatch request; required for routing
- `function.config.calculationNodesTags`: string — comma-separated tags used to select a registered compute member; follows the same tag-intersection rules as processor dispatch
- `function.config.attachEntity`: boolean (optional, default `true`) — when `true`, the full entity payload is included in the dispatch request
- `function.config.responseTimeoutMs`: int64 (optional, default `30000`) — timeout in milliseconds

The function is dispatched as `EntityCriteriaCalculationRequest` to the matching compute member — see the `grpc` topic for the request/response shape. `FunctionCondition` cannot be translated to a storage-plugin pushdown filter; it always executes as a post-filter with in-memory entity loading.

## ENDPOINTS

**POST /api/search/direct/{entityName}/{modelVersion}** — Synchronous search

- `entityName` (path): string
- `modelVersion` (path): int32
- `pointInTime` (query, optional): RFC 3339 date-time — search against entity state at this instant.
  Point-in-time search uses the canonical inclusive (`<=`, no rounding) bound —
  see `cyoda help crud` ("Point-in-time semantics").
- `limit` (query, optional): string-encoded integer, maximum 10000 (values above 10000 are rejected with `400 BAD_REQUEST`); default 1000
- `trackingRead` (query, optional): boolean, default `false`. Only meaningful inside an active transaction (see `crud` topic and `docs/CONSISTENCY.md` §3c for the transactional read-set): when `true`, the entities this search returns are recorded into the transaction's read-set, so a concurrent commit touching any of them aborts with `409 Conflict` at commit time. When `false` (default), the search is a plain snapshot read that records nothing — cheap, but it does not protect the returned rows from concurrent writes, and neither setting protects against phantoms (a new entity matching the predicate after the snapshot was taken). Ignored outside a transaction.

Request body: `Condition` JSON document.

Response: `200 OK`, `Content-Type: application/x-ndjson`.

Each line is a complete entity envelope JSON object:

```
{"type":"ENTITY","data":{"category":"physics","year":"2024"},"meta":{"id":"74807f00-ed0d-11ee-a357-ae468cd3ed16","modelKey":{"name":"nobel-prize","version":1},"state":"NEW","creationDate":"2025-08-01T10:00:00.000000000Z","lastUpdateTime":"2025-08-01T10:00:00.000000000Z"}}
{"type":"ENTITY","data":{"category":"chemistry","year":"2023"},"meta":{"id":"89abc100-ed0d-11ee-a357-ae468cd3ed16","modelKey":{"name":"nobel-prize","version":1},"state":"APPROVED","creationDate":"2025-07-15T09:00:00.000000000Z","lastUpdateTime":"2025-07-20T14:00:00.000000000Z"}}
```

The stream is truncated on encode failure after the header has been sent; the client detects truncation via a connection error or incomplete last line.

**POST /api/search/async/{entityName}/{modelVersion}** — Submit async search job

- `entityName` (path): string
- `modelVersion` (path): int32
- `pointInTime` (query, optional): RFC 3339 — if not provided, the current time is captured at submission

Request body: `Condition` JSON document.

Response: `200 OK`, `application/json` — bare UUID string (job ID):

```
"a1b2c3d4-e5f6-11ee-9e63-ae468cd3ed16"
```

The job is stored with status `RUNNING`. For non-`SelfExecutingSearchStore` backends, a goroutine begins the search immediately using a background context derived from the submitting user's tenant context.

**GET /api/search/async/{jobId}/status** — Get async job status

- `jobId` (path): UUID

Response: `200 OK`, `application/json`:

```json
{
  "searchJobStatus": "SUCCESSFUL",
  "createTime": "2025-08-01T10:00:00.000000000Z",
  "entitiesCount": 42,
  "calculationTimeMillis": 145,
  "finishTime": "2025-08-01T10:00:00.145000000Z",
  "expirationDate": "2025-08-02T10:00:00.000000000Z"
}
```

- `searchJobStatus`: `"RUNNING"`, `"SUCCESSFUL"`, `"FAILED"`, `"CANCELLED"`, or `"NOT_FOUND"` (snapshot expired or not found on commercial backends)
- `createTime`: RFC 3339 with nanoseconds
- `entitiesCount`: total matching entities (0 while running)
- `calculationTimeMillis`: elapsed search time in milliseconds
- `finishTime`: RFC 3339 with nanoseconds; absent when status is `RUNNING`
- `expirationDate`: `createTime + 24h` — job results expire after this time

**GET /api/search/async/{jobId}** — Retrieve async job results (paginated)

- `jobId` (path): UUID
- `pageSize` (query, optional): string-encoded integer, default `1000`
- `pageNumber` (query, optional): string-encoded integer, default `0`; offset = `pageNumber * pageSize`

The job must be in `SUCCESSFUL` status. Returns `400 BAD_REQUEST` if the job is not yet complete.

Response: `200 OK`, `application/json`:

```json
{
  "content": [
    {
      "type": "ENTITY",
      "data": { "category": "physics", "year": "2024" },
      "meta": {
        "id": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
        "modelKey": {"name": "nobel-prize", "version": 1},
        "state": "NEW",
        "creationDate": "2025-08-01T10:00:00.000000000Z",
        "lastUpdateTime": "2025-08-01T10:00:00.000000000Z"
      }
    }
  ],
  "page": {
    "number": 0,
    "size": 1000,
    "totalElements": 42,
    "totalPages": 1
  }
}
```

Results are fetched from the stored entity snapshots at the job's `pointInTime`. Entities deleted or modified after submission are returned as they existed at submission time.

**PUT /api/search/async/{jobId}/cancel** — Cancel a running async job

- `jobId` (path): UUID

Cancellation succeeds only when the job status is `RUNNING`. If the job has already reached a terminal state (`SUCCESSFUL`, `FAILED`, or `CANCELLED`), the server returns `400 Bad Request`:

```json
{
  "detail": "snapshot by id=<jobId> is not running. current status=SUCCESSFUL",
  "properties": {
    "currentStatus": "SUCCESSFUL",
    "snapshotId": "<jobId>"
  },
  "status": 400,
  "title": "Bad Request",
  "type": "about:blank"
}
```

On successful cancellation, response: `200 OK`, `application/json`:

```json
{
  "isCancelled": true,
  "cancelled": true,
  "currentSearchJobStatus": "CANCELLED"
}
```

## SORTING

Both sync and async search accept one or more `sort` query parameters. Repeat the parameter for multi-key sorting; precedence follows declaration order.

**Grammar:** `[@]path[:asc|desc]`

- Direction defaults to `asc` when omitted.
- A leading `$.` on a data path is tolerated and stripped: `$.year:desc` equals `year:desc`.
- Prefix `@` to sort by a meta field: `@creationDate:asc`.

**Meta field allowlist** (only these are accepted with `@`): `state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`.

**Order semantics:**
- Strings: byte (lexicographic) order.
- Numbers: numeric order.
- Meta dates (`creationDate`, `lastUpdateTime`, `transitionForLatestSave`): chronological; millisecond resolution is the minimum precision enforced cross-engine.
- Absent or null values sort last regardless of direction.

**Tiebreaker:** `entity_id` ascending is always appended as the final key.

**Key cap:** configurable via `CYODA_SEARCH_MAX_SORT_KEYS` (default 16); exceeding the cap returns `errors.INVALID_FIELD_PATH` (`400`), like any other malformed `sort` value.

**Invalid paths:** unsortable, unknown, array, or non-scalar paths return `errors.INVALID_FIELD_PATH` (`400`).

## PAGINATION

Async search results use page-number pagination: `pageNumber=0` is the first page, `offset = pageNumber * pageSize`. `pageNumber` and `pageSize` are both string-encoded integers in query parameters.

Synchronous search does not paginate; use the `limit` parameter (maximum 10000; above rejects `400`) to bound results. For large datasets, use async search with page retrieval.

## ERRORS

- `errors.MODEL_NOT_FOUND` — `404` — model not registered for the calling tenant (search, async submit)
- `errors.SEARCH_JOB_NOT_FOUND` — `404` — async job UUID does not exist.
- `errors.SEARCH_JOB_ALREADY_TERMINAL` — `400` — cancel attempted on a job that is already `SUCCESSFUL`, `FAILED`, or `CANCELLED`; error code in response is `BAD_REQUEST`
- `errors.SEARCH_RESULT_LIMIT` — `400` — a backend that enforces a bounded result set matched more entities than the configured cap (direct search); async paths also surface it when a requested page or result set exceeds the maximum
- `errors.SCAN_BUDGET_EXHAUSTED` — `400` — a non-indexable condition (e.g. a regex or wildcard path) forced a residual scan that examined more rows than the backend's configured scan budget; narrow the query or add an indexable predicate
- `errors.SEARCH_SHARD_TIMEOUT` — per-shard search timeout exceeded (relevant for distributed backends)
- `errors.INVALID_FIELD_PATH` — `400` — condition references one or more JSONPath field paths absent from the model's locked schema, or a `lifecycle` condition names an unknown meta filter field; the response detail names each offending path
- `errors.CONDITION_TYPE_MISMATCH` — `400` — condition value type is incompatible with the target field's locked DataType, e.g. a string/pattern operator or a non-timestamp value on a temporal meta field (`creationDate`/`lastUpdateTime`)
- `errors.BAD_REQUEST` — `400` — malformed condition JSON, invalid limit/pageSize/pageNumber, result retrieval on non-SUCCESSFUL job, unknown async job ID in result retrieval

## EXAMPLES

**Synchronous search — match by field value:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"simple","jsonPath":"$.category","operatorType":"EQUALS","value":"physics"}' \
  "http://localhost:8080/api/search/direct/nobel-prize/1"
```

**Synchronous search — match by lifecycle state:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"APPROVED"}' \
  "http://localhost:8080/api/search/direct/nobel-prize/1"
```

**Synchronous search — AND group:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "type": "group",
    "operator": "AND",
    "conditions": [
      {"type":"simple","jsonPath":"$.year","operatorType":"EQUALS","value":"2024"},
      {"type":"lifecycle","field":"state","operatorType":"EQUALS","value":"NEW"}
    ]
  }' \
  "http://localhost:8080/api/search/direct/nobel-prize/1"
```

**Synchronous search at point in time with limit:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"group","operator":"AND","conditions":[]}' \
  "http://localhost:8080/api/search/direct/nobel-prize/1?pointInTime=2025-08-01T00:00:00Z&limit=100"
```

**Submit async search:**

```
JOB_ID=$(curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"simple","jsonPath":"$.year","operatorType":"EQUALS","value":"2024"}' \
  "http://localhost:8080/api/search/async/nobel-prize/1" | tr -d '"')
```

**Poll async job status:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/search/async/$JOB_ID/status"
```

**Retrieve async results (page 0):**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/search/async/$JOB_ID?pageNumber=0&pageSize=500"
```

**Cancel an async job:**

```
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/search/async/$JOB_ID/cancel"
```

## SEE ALSO

- crud
- models
- analytics
- errors.MODEL_NOT_FOUND
- errors.SEARCH_JOB_NOT_FOUND
- errors.SEARCH_JOB_ALREADY_TERMINAL
- errors.SEARCH_RESULT_LIMIT
- errors.SEARCH_SHARD_TIMEOUT
- openapi
