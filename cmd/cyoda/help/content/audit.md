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

A third event type, `System`, is reserved for commercial backends. OSS backends never emit `System` events; `SystemAuditEventDto` stays in the contract for wire compatibility with Cyoda Cloud.

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

A walk that follows `nextCursor` page by page returns every event committed before the walk started — read outside a transaction — exactly once. An event committed while the walk is in progress may be missed; it is never duplicated or skipped among events that existed at the start.

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
      "version": 2,
      "actor": {"id": "alice", "name": "alice", "legalId": "acme", "kind": "user"},
      "executedBy": {"id": "OBOCLIENT0000001", "kind": "service"}
    },
    {
      "auditEventType": "StateMachine",
      "eventType": "STATE_MACHINE_FINISH",
      "severity": "INFO",
      "utcTime": "2025-08-01T10:04:55.000000000Z",
      "microsTime": 1754042695000000,
      "entityId": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
      "state": "APPROVED",
      "data": {
        "eventType": "STATE_MACHINE_FINISH",
        "state": "APPROVED",
        "success": true,
        "stopReason": {
          "type": "COMPLETED_NO_AUTO_TRANSITIONS",
          "description": "No automated transition from state \"APPROVED\""
        }
      },
      "eventId": "3f9b6a10-6f5e-11f0-8f3a-0242ac110002",
      "actor": {"id": "alice", "name": "alice", "legalId": "acme", "kind": "user"},
      "executedBy": {"id": "OBOCLIENT0000001", "kind": "service"}
    }
  ]
}
```

**EntityChangeAuditEventDto** fields (discriminated by `auditEventType: "EntityChange"`):

- `changeType`: `"CREATE"`, `"UPDATE"`, or `"DELETE"` — the type of entity change
- `version`: the entity's version number for this change, strictly increasing over the entity's whole history including across a delete and a later recreate. `(entityId, version)` identifies this event.
- `changes`: before/after diff — **not yet emitted by the server (deferred gap)**; the field is declared in the OpenAPI schema but the server currently omits it from all responses. Do not rely on `changes` being present.
- `executedBy`: `{id, kind}` — the principal that actually staged the change, independent of `actor` (see below). Present when the engine recorded one.
- `severity`, `utcTime`, `microsTime`, `entityId`, `transactionId`, `actor` — plus `entityModel`, `consistencyTime`, `details`, `system` — inherited from `AuditEventDto` (see the `openapi` topic for the full schema)

**StateMachineAuditEventDto** fields (discriminated by `auditEventType: "StateMachine"`):

- `eventType`: the type of the event. The set grows: a client accepts a value it does not know (see `cyoda help openapi`, GENERATED CLIENTS). The types cyoda-go records are `STATE_MACHINE_START`, `STATE_MACHINE_FINISH`, `CANCEL`, `FORCE_SUCCESS`, `WORKFLOW_FOUND`, `WORKFLOW_SKIP`, `TRANSITION_MAKE`, `TRANSITION_NOT_FOUND`, `TRANSITION_NOT_MATCH_CRITERION`, `TRANSITION_ABORTED`, `PAUSE_FOR_PROCESSING`, `STATE_PROCESS_RESULT`, and for scheduled transitions `SCHEDULED_TRANSITION_ARM`, `SCHEDULED_TRANSITION_FIRE`, `SCHEDULED_TRANSITION_EXPIRE`, `SCHEDULED_TRANSITION_CANCEL`, `SCHEDULED_TRANSITION_FAIL` (see `cyoda help scheduled-tasks`). `WORKFLOW_NOT_FOUND` and `PROCESS_NOT_MATCH_CRITERION` are in the contract cyoda-go shares with Cyoda Cloud and are never recorded by cyoda-go.
- `eventId`: a time-based UUID assigned by the server when it recorded the event. Unique per event and the same value on every read, on this endpoint and on the workflow-finished endpoint below.
- `state`: entity state at the time of the event
- `data`: the event's data, a `StateMachineEventDto`. The server sends it on every event, never `null`. Its `eventType` repeats the outer `eventType` and selects the schema of the rest of the object (see *Event data* below).
- `details`: a human-readable text. For `TRANSITION_MAKE` it reads `Transition "<name>": <from> → <to>`. The text is for people, not a parsing contract: read the transition from `data.transition`. Entity change history (`GET /entity/{entityId}/changes`) carries no transition name at all.
- `actor`: the attributed principal the transition ran for (see `AuditActorInfoDto.kind` below). Present when the engine recorded one.
- `executedBy`: `{id, kind}` — the principal that actually ran the transition, independent of `actor`. Present when the engine recorded one.

### Event data

Each event type's `data` holds `eventType` and the fields listed here. A field marked *optional* is present only when the engine knows its value; no field is filled with a placeholder.

- `STATE_MACHINE_START` — `state` (optional): the entity's state when the run started. Absent when the run creates the entity, which has no state yet.
- `STATE_MACHINE_FINISH` — `state`: the state the run settled in; `success`: always `true` (FINISH is recorded only for a run that succeeded); `stopReason`: why the run stopped (see *Stop reason* below).
- `CANCEL` — `reason`: the cascade limit the run exceeded (the visits of one state, or the cascade depth), and where.
- `FORCE_SUCCESS` — no fields. A loopback ran on an entity whose state the selected workflow does not declare.
- `WORKFLOW_FOUND` — `workflowName`; `functions` (optional).
- `WORKFLOW_SKIP` — `workflowName`, `reason`: why the workflow's criterion did not match; `functions` (optional).
- `TRANSITION_MAKE` — `workflowName`, `transition`, `newState`: the transition's target; `functions` (optional). The outer `state` is the state the transition left.
- `TRANSITION_NOT_FOUND` — `workflowName`, `transition`: the transition the request named. `details` says why it cannot be made: it is not declared in the entity's state, it is disabled, or it is scheduled.
- `TRANSITION_NOT_MATCH_CRITERION` — `workflowName`, `transition`, `criterion`: the function's name when a function condition is the whole criterion, otherwise the condition's type (`simple`, `group`, `lifecycle`, `array`); `reason`: why the criterion did not match; `functions` (optional).
- `TRANSITION_ABORTED` — `reason`: `ENTITY_MODIFIED`; `transition` (optional): the transition that was aborted, absent on a loopback, which has no transition; `expectedTxId`: the transaction id the caller's `ifMatch` named; `actualTxId`: the transaction id of the entity version the request started from.
- `PAUSE_FOR_PROCESSING` — `workflowName`, `transition`, `processNames`: the transition's processor names, in pipeline order.
- `STATE_PROCESS_RESULT` — one event per processor that ran: `processorName`: the processor's `name` in the workflow; `executionMode`: `SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX` or `COMMIT_BEFORE_DISPATCH` (a processor configured with no mode runs, and is recorded, as `SYNC`); `success`: `false` for a failed `ASYNC_NEW_TX` processor too, although the run goes on; `failReason` (optional): the failure text the compute member sent, present only when the processor failed and the member itself sent text. A failed processor's `details` reads `Processor "<name>" failed`.
- `SCHEDULED_TRANSITION_ARM` — `transition`, `sourceState`, `scheduledTime`: when the transition is due, in Unix milliseconds.
- `SCHEDULED_TRANSITION_FIRE` — `transition`, `sourceState`: the state the transition fired from; `workflowName`: the workflow selected for the entity. The outer `state` is the state the run settled in.
- `SCHEDULED_TRANSITION_EXPIRE` — `transition`, `sourceState`. When the task was first picked up after its deadline, `details` states the lateness and the timeout.
- `SCHEDULED_TRANSITION_CANCEL` — `transition`, `sourceState`; `workflowName` (optional): present when the task is cancelled as it comes due, absent when a write cancels it.
- `SCHEDULED_TRANSITION_FAIL` — `transition`, `sourceState`, `reason`: the scheduled task's `failureReason`, an open value set; `attempts`, `lostOwners`.

`functions` is present, possibly empty, when the criterion evaluated holds a function condition: one entry per function condition the evaluation reached, in evaluation order. See `cyoda help workflows`, *Function conditions*.

### Stop reason

`stopReason` on `STATE_MACHINE_FINISH` is `{type, description, rejectedTransitions}`. cyoda-go sends three types:

- `COMPLETED_NO_AUTO_TRANSITIONS` — the settled state has no automated transition: none that is not `disabled`, not `manual` and not scheduled. `description`: `No automated transition from state "<state>"`.
- `COMPLETED_ALL_CRITERIA_REJECTED` — the settled state has automated transitions and every one was refused by its criterion. `description`: `Every automated transition from state "<state>" was refused by its criterion`. `rejectedTransitions` lists each refused transition, in declaration order, as `{transitionName, criterion, reason}`, with the values of its `TRANSITION_NOT_MATCH_CRITERION` event. Only the last pass counts: a refusal in a state the run then left is not listed.
- `FORCE_SUCCESS` — a loopback on an entity whose state the selected workflow does not declare. `description`: `State "<state>" is not declared in workflow "<workflow>"`.

`rejectedTransitions` is present only for `COMPLETED_ALL_CRITERIA_REJECTED`. The other types the schema lists (`PAUSED_FOR_PROCESSING`, `CANCELED`, `NO_WORKFLOW_FOUND`, `WORKFLOW_SKIPPED`, `INCOMPLETE_TRANSACTION`, `UNKNOWN`) are in the contract cyoda-go shares with Cyoda Cloud and are never sent by cyoda-go.

### Recording

State machine events are written in the transaction the engine runs in and kept only when that transaction commits. When an operation fails, the events of its uncommitted transaction are rolled back with it, a failed `SYNC` or `ASYNC_SAME_TX` processor's `STATE_PROCESS_RESULT` included; a `COMMIT_BEFORE_DISPATCH` processor's segment boundary has already committed the events before it, and a `COMMIT_BEFORE_DISPATCH` processor that fails after that boundary has its `STATE_PROCESS_RESULT` written in a transaction of its own, under the same `transactionId` as the cascade's other events, so it is kept although the operation fails. When that boundary itself fails, the processor's record is rolled back with the transaction the boundary did not commit. If the boundary's commit was interrupted and its outcome is in doubt, the request is answered with a 5xx, the processor is not dispatched, and when that commit did land the trail holds the events before it, `PAUSE_FOR_PROCESSING` included, and no `STATE_PROCESS_RESULT`. A failed `ASYNC_NEW_TX` processor does not fail the operation (unless its savepoint cannot be created, undone or released, or a scheduled run refuses to dispatch it), so its record is kept when the operation commits; a compute node's callback that joined another operation's transaction leaves its events in that transaction, where they commit if that operation commits. Events are recorded best effort: a failure to record one is logged at WARN and does not fail the operation, so an audit trail can miss an event. Do not build an authorization check on finding one; record what a check needs in the entity itself (see `cyoda help auth integration`, *READING IDENTITY IN A COMPUTE NODE*).

No state-machine event holds the character U+0000: the audit trail cannot store it, so every text an event carries is refused where it enters, and nothing is recorded. A compute member's criterion reason or failure text holding it makes the answer unreadable, so the callout fails (`400 WORKFLOW_FAILED`, not retryable, where that failure fails the operation; see `cyoda help grpc`). A workflow import or a collection-update body holding it, and a transition name or If-Match value holding it, are refused with `400 BAD_REQUEST`. So no text can take the record of a state change out of the trail.

### Actor and executor

`AuditActorInfoDto` (the `actor` field on both event kinds) carries `id`, `legalId`, `name`, and `kind` — an open value set, known values `user`, `service`, and `system`. `kind` is empty on a legacy event recorded before attribution was stamped. `legalId` is the tenant. `name` repeats `id`: cyoda-go has no display names, and a user assertion cannot set one. The schema's optional `externalId` is never set by cyoda-go. `id` is the user id exactly as the on-behalf-of client asserted it, never normalised, so a segregation-of-duties check compares it byte for byte with a callout's `authid` (see `cyoda help auth integration`).

`executedBy` is an `AuditPrincipalDto` — `{id, kind}`, both populated when present. It names the principal that actually made the change, which can differ from `actor`: e.g. an on-behalf-of (OBO) write attributes to the user (`actor`) but is executed by the OBO client (`executedBy`, `kind: "service"`); a cascade write executed by a compute node inside another principal's transaction attributes to that transaction's origin but is executed by the compute node's service identity.

**GET /api/audit/entity/{entityId}/workflow/{transactionId}/finished** — Get workflow finished event

Retrieves the `STATE_MACHINE_FINISH` audit event for a specific entity and transaction. Provides direct access to the workflow outcome without scanning all audit events.

A transaction can carry more than one `STATE_MACHINE_FINISH` event for the entity — a compute node's callback that saves, inside the same transaction, an entity whose workflow already ran earlier in it (a processor writing its entity back) runs that workflow again and records its own START/FINISH pair. When that happens, this endpoint returns the one that sorts first in the audit order (see Order above; normally the last one recorded).

- `entityId` (path): UUID
- `transactionId` (path): UUID

Response: `200 OK`, `application/json` — `StateMachineAuditEventDto` (the finish event for the transaction), carrying the same `eventId` a search response returns for the same event.

Returns `404` with `ProblemDetail` (`application/problem+json`) when the entity or finished event is not found. A scheduled transition's fire records no `STATE_MACHINE_START` and no `STATE_MACHINE_FINISH` (its last event is `SCHEDULED_TRANSITION_FIRE`), so this endpoint answers `404 ENTITY_NOT_FOUND` for a fire's transaction — unless a compute node's callback saved the entity inside that transaction, which runs its workflow and records its own pair, as described above.

## ERRORS

- `errors.ENTITY_NOT_FOUND` — `404` — entity does not exist, or no events found for the given entity/transaction
- `errors.BAD_REQUEST` — `400` — `limit` parameter is not a valid integer or is less than 1, or `cursor` cannot be decoded or fails validation (including a cursor from an earlier server build)

A search that includes `StateMachine` events (the default, or an explicit `eventType` filter naming it) answers `503 STORAGE_UNAVAILABLE` on a storage outage, or a generic `500` with a ticket id on any other failure to read state machine events, rather than a `200` whose `items` silently omit them. A request filtered to `EntityChange` only is unaffected by a state machine store failure.

## EXAMPLES

**Fetch the 20 most recent audit events for an entity:**

```
curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID"
```

**Filter to EntityChange events only:**

```
curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?eventType=EntityChange"
```

**Filter to a specific transaction:**

```
curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?transactionId=$TX_ID"
```

**Paginate using a cursor:**

```
NEXT=$(curl -s -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?limit=10" \
  | jq -r '.pagination.nextCursor')

curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID?limit=10&cursor=$NEXT"
```

**Get the workflow finished event for a transaction:**

```
curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/audit/entity/$ENTITY_ID/workflow/$TX_ID/finished"
```

## SEE ALSO

- crud
- search
- models
- errors.ENTITY_NOT_FOUND
- errors.BAD_REQUEST
- openapi
