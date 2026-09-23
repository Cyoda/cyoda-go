---
topic: audit
title: "audit — entity audit event API"
stability: stable
see_also:
  - crud
  - search
  - models
  - errors.ENTITY_NOT_FOUND
  - errors.BAD_REQUEST
  - openapi
---

# audit

## NAME

audit — entity audit event API: retrieve change and workflow audit events for a specific entity.

## SYNOPSIS

```
GET  /api/audit/entity/{entityId}
GET  /api/audit/entity/{entityId}/workflow/{transactionId}/finished
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). All endpoints require `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`.

## DESCRIPTION

The audit API exposes a per-entity event log covering entity lifecycle changes (`EntityChange` events) and workflow state-machine progression (`StateMachine` events). Events are returned in a single deterministic order and support cursor-based pagination.

Order: `utcTime` newest first; then `EntityChange` before `StateMachine`; then, within a tie, `EntityChange` events by `version` descending and `StateMachine` events by `eventId` (time field, then the UUID's bytes) descending. Every event has a distinct sort key, so the order is total and reproduces on every request over the same data. For `StateMachine` events of one instant the order is fixed but not promised to be the order the events were recorded in.

A third event type, `System`, is reserved for commercial backends. OSS backends never emit `System` events; the enum value is retained in the contract for wire compatibility with Cyoda Cloud.

## ENDPOINTS

**GET /api/audit/entity/{entityId}** — Search audit events for an entity

- `entityId` (path): UUID — the entity to query

Query parameters (all optional):

- `eventType`: multi-value filter. Accepted values: `StateMachine`, `EntityChange`, `System`. When omitted, defaults to all types except `System`. **`System` events are excluded from the default set. Including them may increase latency and load — use with caution.**
- `severity`: `ERROR`, `INFO`, `WARN`, or `DEBUG`
- `fromUtcTime`: RFC 3339 date-time — include events at or after this time (inclusive)
- `toUtcTime`: RFC 3339 date-time — include events before this time (exclusive)
- `transactionId`: UUID — filter to a single transaction
- `cursor`: opaque string — encodes the sort key (see order above) of the last event on the previous page; pass `nextCursor` from the previous response to fetch the next page; omit for the first page. Filters may change between pages — the walk continues from the cursor's position under the new filters. A cursor that cannot be decoded or fails validation, including a cursor from an earlier server build, answers `400 BAD_REQUEST`.
- `limit`: string-encoded integer, 1–1000 (default 20; values above 1000 are clamped to 1000)

A walk that pages through with a fixed `cursor`/`limit` returns every event committed before the walk started exactly once. An event committed while the walk is in progress may be missed; it is never duplicated or skipped among events that existed at the start.

Response: `200 OK`, `application/json` — `EntityAuditEventsResponseDto`:

```json
{
  "pagination": {
    "hasNext": true,
    "nextCursor": "eyJ2IjoxLCJ0IjoiMjAyNS0wOC0wMVQxMDowNTowMFoiLCJrIjoiRW50aXR5Q2hhbmdlIiwibiI6Mn0"
  },
  "items": [
    {
      "auditEventType": "EntityChange",
      "changeType": "UPDATE",
      "severity": "INFO",
      "utcTime": "2025-08-01T10:05:00.000000000Z",
      "microsTime": 1754042700000000,
      "entityId": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
      "transactionId": "9f1a2b3c-ed0e-11ee-a357-ae468cd3ed16",
      "version": 2
    },
    {
      "auditEventType": "StateMachine",
      "eventType": "STATE_MACHINE_FINISH",
      "severity": "INFO",
      "utcTime": "2025-08-01T10:04:55.000000000Z",
      "microsTime": 1754042695000000,
      "entityId": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
      "state": "APPROVED",
      "data": {"success": true},
      "eventId": "3f9b6a10-6f5e-11f0-8f3a-0242ac110002"
    }
  ]
}
```

**EntityChangeAuditEventDto** fields (discriminated by `auditEventType: "EntityChange"`):

- `changeType`: `"CREATE"`, `"UPDATE"`, or `"DELETE"` — the type of entity change
- `version`: the entity's version number for this change, strictly increasing over the entity's whole history including across a delete and a later recreate. `(entityId, version)` identifies this event.
- `changes`: before/after diff — **not yet emitted by the server (deferred gap)**; the field is declared in the OpenAPI schema but the server currently omits it from all responses. Do not rely on `changes` being present.
- `severity`, `utcTime`, `microsTime`, `entityId`, `transactionId`, `actor` — plus `entityModel`, `consistencyTime`, `details`, `system` — inherited from `AuditEventDto` (see the `openapi` topic for the full schema)

**StateMachineAuditEventDto** fields (discriminated by `auditEventType: "StateMachine"`):

- `eventType`: one of `STATE_MACHINE_START`, `STATE_MACHINE_FINISH`, `CANCEL`, `FORCE_SUCCESS`, `WORKFLOW_FOUND`, `WORKFLOW_NOT_FOUND`, `WORKFLOW_SKIP`, `TRANSITION_MAKE`, `TRANSITION_NOT_FOUND`, `TRANSITION_NOT_MATCH_CRITERION`, `TRANSITION_ABORTED`, `PROCESS_NOT_MATCH_CRITERION`, `PAUSE_FOR_PROCESSING`, `STATE_PROCESS_RESULT`
- `eventId`: a time-based UUID assigned by the server when it recorded the event. Unique per event and the same value on every read, on this endpoint and on the workflow-finished endpoint below.
- `state`: entity state at the time of the event
- `data`: optional event-specific payload (e.g. `{"success": true}` for `STATE_MACHINE_FINISH`; null for most event types). `TRANSITION_ABORTED` carries `{reason, transitionName, expectedTxId, actualTxId}`.

**GET /api/audit/entity/{entityId}/workflow/{transactionId}/finished** — Get workflow finished event

Retrieves the `STATE_MACHINE_FINISH` audit event for a specific entity and transaction. Provides direct access to the workflow outcome without scanning all audit events.

- `entityId` (path): UUID
- `transactionId` (path): UUID

Response: `200 OK`, `application/json` — `StateMachineAuditEventDto` (the finish event for the transaction), carrying the same `eventId` a search response returns for the same event.

Returns `404` with `ProblemDetail` (`application/problem+json`) when the entity or finished event is not found.

## ERRORS

- `errors.ENTITY_NOT_FOUND` — `404` — entity does not exist, or no events found for the given entity/transaction
- `errors.BAD_REQUEST` — `400` — `limit` parameter is not a valid integer or is less than 1, or `cursor` cannot be decoded or fails validation (including a cursor from an earlier server build)

A search that includes `StateMachine` events (the default, or an explicit `eventType` filter naming it) answers `503 STORAGE_UNAVAILABLE` on a storage outage, or a generic `500` with a ticket id on any other failure to read state machine events, rather than a `200` whose `items` silently omit them. A request filtered to `EntityChange` only is unaffected by a state machine store failure.

## EXAMPLES

**Fetch the 20 most recent audit events for an entity:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID"
```

**Filter to EntityChange events only:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?eventType=EntityChange"
```

**Filter to a specific transaction:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?transactionId=$TX_ID"
```

**Paginate using a cursor:**

```
NEXT=$(curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?limit=10" \
  | jq -r '.pagination.nextCursor')

curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?limit=10&cursor=$NEXT"
```

**Get the workflow finished event for a transaction:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID/workflow/$TX_ID/finished"
```

## SEE ALSO

- crud
- search
- models
- errors.ENTITY_NOT_FOUND
- errors.BAD_REQUEST
- openapi
