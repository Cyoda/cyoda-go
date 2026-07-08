---
topic: analytics
title: "analytics — Trino SQL analytics interface"
stability: evolving
see_also:
  - search
  - models
  - grpc
  - errors.BAD_REQUEST
---

# analytics

**Cloud-only.** The analytics surface documented here is served by Cyoda Cloud (`cloud.cyoda.com`). It is not part of the cyoda-go binary. Running a local cyoda-go server does not expose any of the endpoints below.

## NAME

analytics — Trino SQL analytics interface for querying Cyoda entity data via SQL.

## SYNOPSIS

```
# SQL Schema Management REST API (Cyoda Cloud)
GET    /api/sql/schema/{schemaId}
DELETE /api/sql/schema/{schemaId}
GET    /api/sql/schema/?schemaName={name}
POST   /api/sql/schema/
DELETE /api/sql/schema/?schemaName={name}
PUT    /api/sql/schema/putDefault/{schemaName}
GET    /api/sql/schema/listAll
GET    /api/sql/schema/genTables/{entityModelId}
POST   /api/sql/schema/updateTables/{entityModelId}

# WebSocket (STOMP) Messaging API (Cyoda Cloud)
treeNode.getSchemas
treeNode.getSchema
treeNode.getData
treeNode.getRawData
view.getViews
view.addView
view.renameView
view.dropView
reports.runReport
reports.getReportConfigs
reports.getReportDefinition
reports.getReportHistories
reports.getReportStatistics
reports.getReportGroups
reports.getReportRows
reports.deleteReports
```

## DESCRIPTION

Cyoda Cloud exposes entity data as Trino SQL tables through a Trino connector. The connector uses the Schema Management REST API to discover table definitions and the WebSocket (STOMP) messaging API to stream entity rows at query time.

## SQL SCHEMA MANAGEMENT REST API

All endpoints are served under `/api/sql/schema` in Cyoda Cloud.

Authentication is required on all endpoints. Errors follow RFC 7807 Problem Details format: `{ "type", "title", "status", "detail", "instance", "properties" }`.

**GET /api/sql/schema/{schemaId}**

Retrieve a complete schema configuration by its time-based UUID. Returns all fields including hidden ones. Returns `400` if the UUID is not a time-based (version 1) UUID; returns `404` if not found.

- `schemaId` (path): UUID (version 1 / time-based)

Response: `200 OK`, `application/json`, `SchemaConfigDto`.

**DELETE /api/sql/schema/{schemaId}**

Delete a schema by UUID. Requires both read and delete permissions. Returns confirmation string on success.

Response: `200 OK`, `application/json`, string — e.g., `"SQL Schema with id 8824c480-c166-11ee-822a-ae468cd3ed16 deleted"`.

**GET /api/sql/schema/?schemaName={name}**

Retrieve a schema by its unique name. Returns `404` if not found.

- `schemaName` (query, required): case-sensitive string, 1–1024 characters

Response: `200 OK`, `application/json`, `SchemaConfigDto`.

**POST /api/sql/schema/**

Save a schema (create or update). Uses upsert semantics: if a schema with the same `schemaName` exists, its tables are replaced with the incoming definition. If no schema with that name exists, a new one is created.

The `id` field in the request body is ignored; matching is done exclusively by `schemaName`. Returns the time-based UUID of the saved schema.

Validation: `schemaName` must be 1–1024 characters.

Request body: `SchemaConfigDto`.

Response: `200 OK`, `application/json`, UUID string — e.g., `"8824c480-c166-11ee-822a-ae468cd3ed16"`.

**DELETE /api/sql/schema/?schemaName={name}**

Delete a schema by name. Returns `404` if not found.

Response: `200 OK`, `application/json`, string — e.g., `"SQL Schema with name NOBEL_PRIZES deleted"`.

**PUT /api/sql/schema/putDefault/{schemaName}**

Create a new schema by scanning all Entity Models the user has read access to. Generates the complete set of tables (object, array, JSON) for every model. Validates for duplicate table names and duplicate field names within a table. On validation failure, returns `422 Unprocessable Entity`.

- `schemaName` (path): string, 1–1024 characters

Response: `200 OK`, `application/json`, `SchemaConfigDto`.

**GET /api/sql/schema/listAll**

List all schemas the user has read access to. Schemas the user cannot read are silently omitted. Returns all fields including hidden ones.

Response: `200 OK`, `application/json`, array of `SchemaConfigDto`.

**GET /api/sql/schema/genTables/{entityModelId}**

Preview: generate SQL table configurations from a specific Entity Model without saving. Read-only.

- `entityModelId` (path): UUID of the Entity Model

Response: `200 OK`, `application/json`, array of `TableConfigDto`.

**POST /api/sql/schema/updateTables/{entityModelId}**

Merge existing table configurations with the current state of an Entity Model. Read-only; does not save. Tables are matched by `uniformedPath`. Fields from the current model are added; existing table custom settings are preserved.

- `entityModelId` (path): UUID of the Entity Model

Request body: array of `TableConfigDto`.

Response: `200 OK`, `application/json`, array of `TableConfigDto`.

## SCHEMA DTO STRUCTURES

**SchemaConfigDto:**

```json
{
  "id": "8824c480-c166-11ee-822a-ae468cd3ed16",
  "schemaName": "NOBEL_PRIZES",
  "tables": [ ...TableConfigDto... ]
}
```

- `id` — UUID (time-based, version 1); server-assigned; null when creating via POST; required for GET/DELETE by ID
- `schemaName` — string, unique, 1–1024 characters
- `tables` — array of `TableConfigDto`; sorted by `uniformedPath` in responses

**TableConfigDto:**

```json
{
  "tableName": "nobel_prize",
  "metadataClassId": "a1b2c3d4-e5f6-11ee-b789-0242ac120002",
  "uniformedPath": "$",
  "fields": [ ...FieldConfigDto... ],
  "hidden": false,
  "modelUpdateDate": 1702900000
}
```

- `tableName` — SQL table display name; auto-generated from model name, version, and path; customizable
- `metadataClassId` — UUID of the Cyoda Entity Model; all tables from the same model share this value
- `uniformedPath` — structural key identifying which node in the model this table represents. Values: `"$"` (root level), `"$.fieldName"` (nested object outside arrays), `"$.fieldName[*]"` (object table under array element), `"$.fieldName[*]^"` (detached array table — individual array elements), `"json"` (special JSON table, full reconstructed entity as string)
- `fields` — array of `FieldConfigDto`
- `hidden` — boolean; hidden tables excluded from WebSocket schema discovery (`filterHidden=true`) but always included in REST responses
- `modelUpdateDate` — int64 (epoch seconds), nullable; last Entity Model update timestamp

**Table naming rules:**

- Base: `{modelName}` when version = 1; `{modelName}_{version}` when version > 1
- Path segments appended with underscores (e.g., `nobel_prize_laureates` for path `$.laureates`)
- Multi-level array nesting: `_{n}d` suffix (e.g., `_2d` for depth 2)
- Detached arrays: `_array` suffix
- JSON table: `_json` suffix

**FieldConfigDto:**

```json
{
  "fieldName": "category",
  "fieldKey": "category",
  "fieldCategory": "DATA",
  "dataType": "STRING",
  "isArray": false,
  "hidden": false,
  "flatten": null,
  "arrayFields": null
}
```

- `fieldName` — SQL column name; display name; defaults to `fieldKey` for DATA fields; snake_case aliases for ROOT fields
- `fieldKey` — internal resolution key; interpretation depends on `fieldCategory`
- `fieldCategory` — one of: `DATA` (entity payload fields keyed by value map short key), `ROOT` (entity metadata: `creationDate`, `lastUpdateTime`, `state`), `INDEX` (array index fields keyed by nesting level position), `SPECIAL` (synthetic columns: `ENTITY_ID`, `POINT_TIME`, `JSON`)
- `dataType` — one of: `STRING`, `INTEGER`, `LONG`, `DOUBLE`, `BOOLEAN`, `DATE`, `ZONED_DATE_TIME`, `UUID_TYPE`, `TIME_UUID_TYPE`, `BYTE_ARRAY`
- `isArray` — boolean; when true, `arrayFields` contains element-level sub-fields
- `hidden` — boolean
- `flatten` — boolean or null; for INDEX array fields: `true` flattens indices into separate columns (`index_0`, `index_1`, etc.), `false` keeps as array
- `arrayFields` — array of `FieldConfigDto` or null; sub-fields when `isArray=true`

**Reserved field names (cannot be used for DATA fields):** `index`, `entity_id`, `point_time`, `creation_date`, `last_update_date`, `state`.

**Always-present field categories per table:**

- `SPECIAL`: `entity_id` (`UUID_TYPE`), `point_time` (allows point-in-time queries)
- `ROOT`: `creation_date` (`DATE`), `last_update_date` (`DATE`), `state` (`STRING`)
- `SPECIAL` JSON table only: `entity` (`JSON` key, full reconstructed JSON string)
- `INDEX`: present only on tables derived from array nodes

## WEBSOCKET (STOMP) MESSAGING API

The WebSocket API is served by Cyoda Cloud over STOMP. The concrete WebSocket endpoint URL is environment-specific and not part of this contract.

**TreeNode Data Access (treeNode.*):**

- `treeNode.getSchemas` — retrieve all schemas the user can read (hidden items filtered out); payload: userId string
- `treeNode.getSchema` — retrieve a schema by name (hidden items filtered); payload: schemaName string; runs in anonymous context
- `treeNode.getData` — primary data channel; the Trino connector calls this to fulfil SQL SELECT queries; payload: `DataRequestDto`; runs in authenticated context; returns stream of `EntityContentDto`
- `treeNode.getRawData` — returns raw tree node structure of all entities (diagnostic; no filtering by model, condition, or fields); payload: userId string; returns stream of `RawEntityContentDto`

**DataRequestDto fields:**

- `metaClassId` — UUID — which Entity Model to query
- `path` — string — structural path / `uniformedPath` value of the target table
- `domainCondition` — map of `ColumnCategory` → field key → condition; organized by category (`SPECIAL`, `ROOT`, `INDEX`, `DATA`)
- `expressionCondition` — `GroupConditionDto` — alternative condition format using AND/OR groups
- `selectedFields` — optional list of value map keys to include; null returns all fields
- `userId` — string — user ID for authenticated context and permission-filtered entity access

Condition pushdown by category:

- `SPECIAL` / `ENTITY_ID` equality — direct entity ID lookup, bypasses full scan
- `SPECIAL` / `POINT_TIME` equality — sets snapshot time for the query (ISO-8601 instant)
- `ROOT` — applied as range conditions on entity-level fields (`creationDate`, `lastUpdateTime`, `state`)
- `INDEX` — applied as in-memory predicates on array index values
- `DATA` — applied as value map conditions, pushed to the storage layer

Data is fetched in pages (default page size: 1000). Point-in-time is derived from the `POINT_TIME` condition or defaults to the current consistency time.

Point-in-time search uses the canonical inclusive (`<=`, no rounding) bound —
see `cyoda help crud` ("Point-in-time semantics").

**Views (view.*):**

- `view.getViews` — get all views the user can read; payload: userId string; returns stream of `TrinoViewDto`
- `view.addView` — add a view; payload: `TrinoViewDto`; returns null on success, error string on conflict (when `replace=false` and view already exists)
- `view.renameView` — rename a view; payload: `TrinoViewDto` with `newName`; returns null on success, error string on failure
- `view.dropView` — delete a view; payload: `TrinoViewDto`; returns null on success, error string if not found

View identity: composite key of `(schemaName, tableName)`, both automatically lowercased.

**Reports (reports.*):**

- `reports.getReportConfigs` — list saved report configurations (GridConfigs); payload: userId; returns stream of `{id, creationDate}`
- `reports.runReport` — execute a report; payload: `{configId, userId}`; returns reportId string
- `reports.getReportDefinition` — get a saved GridConfig definition; anonymous context
- `reports.getReportHistories` — completed non-failed executions as `Map<String, Any>`; anonymous context
- `reports.getReportStatistics` — all executions as `DistributedReportInfoDto`; anonymous context
- `reports.getReportGroups` — grouping headers for an execution; payload: `{reportId, groupingVersion, userId}`; anonymous context
- `reports.getReportRows` — paginated data rows; payload: `{reportId, groupJsonBase64, startRow, endRow, userId}`; anonymous context
- `reports.deleteReports` — delete report executions; authenticated context

## CATALOG AND SCHEMA LAYOUT

In Trino, cyoda entity data is exposed as tables under the Trino catalog and schema configured by the Trino connector deployment. Table names match `tableName` fields in `TableConfigDto`. Each SQL table corresponds to one `uniformedPath` node in an Entity Model.

A single Entity Model produces multiple tables:

- One object table per object-level node (e.g., root `$`, `$.address`)
- One detached array table per array node (path ending with `^`)
- One JSON table per model (path = `json`)

Querying a table returns one row per entity (object and JSON tables) or one row per array element (detached array tables). The `entity_id` column is the entity UUID. The `point_time` column accepts a point-in-time value for historical queries.

## ERRORS

- `errors.BAD_REQUEST` — `400` — schema name too long (> 1024 chars), UUID is not time-based (version 1)
- `404 Not Found` (RFC 7807) — schema not found by ID or name
- `422 Unprocessable Entity` — default schema generation failed due to duplicate table or field names

## EXAMPLES

**List all schemas:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://cloud.cyoda.com/api/sql/schema/listAll"
```

**Get schema by name:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://cloud.cyoda.com/api/sql/schema/?schemaName=NOBEL_PRIZES"
```

**Create default schema from all entity models:**

```
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  "https://cloud.cyoda.com/api/sql/schema/putDefault/Nobel_PRIZES"
```

**Preview tables for a model (by model UUID):**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://cloud.cyoda.com/api/sql/schema/genTables/a1b2c3d4-e5f6-11ee-b789-0242ac120002"
```

**Save a schema:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "schemaName": "NOBEL_PRIZES",
    "tables": [
      {
        "tableName": "nobel_prize",
        "metadataClassId": "a1b2c3d4-e5f6-11ee-b789-0242ac120002",
        "uniformedPath": "$",
        "fields": [],
        "hidden": false
      }
    ]
  }' \
  "https://cloud.cyoda.com/api/sql/schema/"
```

**Delete a schema by name:**

```
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "https://cloud.cyoda.com/api/sql/schema/?schemaName=NOBEL_PRIZES"
```

**Trino CLI connection (example; concrete JDBC URL is deployment-specific):**

```
trino --server https://trino.cloud.cyoda.com \
      --user "$TRINO_USER" \
      --password \
      --catalog cyoda \
      --schema NOBEL_PRIZES \
      --execute "SELECT entity_id, category, year FROM nobel_prize LIMIT 10"
```

## SEE ALSO

- search
- models
- grpc
- errors.BAD_REQUEST
