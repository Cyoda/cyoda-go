---
topic: scheduled-tasks
title: "scheduled-tasks — the tenant's scheduled-transition tasks"
stability: stable
version_added: 0.9.0
see_also:
  - workflows
  - audit
  - openapi
  - errors.BAD_REQUEST
  - errors.UNAUTHORIZED
  - errors.SERVER_ERROR
  - errors.STORAGE_UNAVAILABLE
---

# scheduled-tasks

## NAME

scheduled-tasks — the tenant's scheduled-transition tasks: list the tasks a scheduled workflow transition has armed and that still exist.

## SYNOPSIS

```
GET  /api/scheduled-tasks
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). Requires `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`. HTTP only — there is no gRPC equivalent.

## DESCRIPTION

A scheduled workflow transition arms a task each time the entity enters the transition's source state. `GET /scheduled-tasks` lists the caller's tenant's scheduled-transition tasks that still exist: `WAITING`, `RUNNING` or `FAILED`. A task that fired, was declined, expired or was cancelled is removed; its outcome is in the entity's audit trail (see the `audit` topic), not here.

Results are sorted by `scheduledTime`, then `taskId`, ascending, and paged with an opaque cursor. Filters combine with AND. A `modelName`, `modelVersion` or `entityId` naming an unknown model or entity, or one of another tenant, returns an empty list rather than an error. Any authenticated user of the calling tenant may call this endpoint; the tenant is always the token's — no parameter selects a different one.

## PARAMETERS

All are optional query parameters.

- `status`: return tasks in any of these statuses. Repeat the parameter for several. Accepted values: `WAITING`, `RUNNING`, `FAILED`. Any other value is rejected with `400`.
- `modelName`: return tasks of entities of this model. 1 to 256 characters of valid UTF-8, without NUL.
- `modelVersion`: return tasks of entities of this model version. Only together with `modelName` — alone it is rejected with `400`.
- `entityId`: UUID — return the tasks of this entity.
- `cursor`: opaque, at most 256 characters. Pass `nextCursor` from the previous response to fetch the next page; omit for the first page. A cursor that cannot be read is rejected with `400`, and its value is never echoed back.
- `limit`: integer, 1 to 1000 (default 20). A value outside the range is rejected with `400`, not clamped.

## RESPONSE

`200 OK`, `application/json` — `ScheduledTaskPageDto`:

```json
{
  "items": [
    {
      "taskId": "3f9b6a106f5e11f08f3a0242ac110002",
      "entityId": "74807f00-ed0d-11ee-a357-ae468cd3ed16",
      "modelName": "order",
      "modelVersion": 1,
      "sourceState": "PENDING",
      "transition": "escalate",
      "status": "WAITING",
      "scheduledTime": "2025-08-01T10:05:00.000000000Z",
      "armedTime": "2025-08-01T09:05:00.000000000Z",
      "attempts": 0,
      "lostOwners": 0,
      "nextAttemptTime": "2025-08-01T10:05:00.000000000Z"
    }
  ],
  "pagination": {
    "hasNext": false
  }
}
```

**ScheduledTaskDto** fields. Claim tokens, arm tokens and node identities are never returned.

- `taskId`: opaque, stable identifier. The same (entity, source state, transition) keeps the same id when it is armed again.
- `entityId`: UUID.
- `modelName`, `modelVersion`: the entity's model.
- `sourceState`: the state the entity must be in for the transition to fire.
- `transition`: the transition's name.
- `status`: open value set; accept a value not listed here. Known values: `WAITING` — due at `nextAttemptTime`; `RUNNING` — a node has claimed it and is running it; `FAILED` — it will not run again and never moves the entity (see `failureReason`). A write to the entity in `sourceState` arms it again; the entity leaving `sourceState` removes it.
- `scheduledTime`: when the transition is due.
- `armedTime`: when the task was last armed.
- `expiresTime`: present when the transition's schedule sets `timeoutMs` — `scheduledTime` plus `timeoutMs`.
- `attempts`: failed attempts recorded since the task was last armed.
- `lostOwners`: times the node running the task was lost since the task was last armed.
- `nextAttemptTime`: present when `status` is `WAITING` — the earliest time the next attempt may start.
- `lastAttemptTime`: present after a failed attempt.
- `lastError`: for a `FAILED` task, present with the failure's text (pairs with `failedTime`), even when the text is empty. For every other status, present when `lastAttemptTime` is set, paired with it. Client-safe text — a `CODE: detail` message, a compute node's own message, or `internal error [ticket: <uuid>]`.
- `failureReason`: present when `status` is `FAILED`. Open value set; accept a value not listed here. Known values: `UNSAFE_WORK_NOT_COMPLETED` — a processor not declared `idempotent` was handed to a compute node and the run did not commit, so it is not repeated; `OWNER_LOST_REPEATEDLY` — the node running the task was lost too many times; `EXPIRED_AFTER_FAILED_ATTEMPTS` — `expiresTime` passed after a failed attempt or a lost node; `RUN_PANICKED` — the run failed with an internal error; `STOPPED_AFTER_PARTIAL_COMMIT` — the run committed the entity into another state and then stopped.
- `failedTime`: present when `status` is `FAILED`.
- `armedBy`: the principal whose write armed the task — `{id, kind}`, `kind` one of `user`, `service`, `system`. Present when known.

## ERRORS

- `errors.BAD_REQUEST` — `400` — an invalid `status`, an invalid `modelName`, a `modelVersion` without `modelName`, an out-of-range `modelVersion` or `limit`, or an unreadable `entityId` or `cursor`
- `errors.UNAUTHORIZED` — `401` — missing or invalid bearer token
- `errors.SERVER_ERROR` — `500` — internal failure; the response carries a ticket id, never the cause
- `errors.STORAGE_UNAVAILABLE` — `503` — a transient storage outage; retryable

## EXAMPLES

**List the first page of WAITING tasks:**

```
curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?status=WAITING"
```

**List the tasks of one entity:**

```
curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?entityId=$ENTITY_ID"
```

**Filter to one model version and page through the results:**

```
NEXT=$(curl -s -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?modelName=order&modelVersion=1&limit=10" \
  | jq -r '.pagination.nextCursor')

curl -s \
  -H @- <<<"Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/scheduled-tasks?modelName=order&modelVersion=1&limit=10&cursor=$NEXT"
```

## SEE ALSO

- workflows
- audit
- openapi
- errors.BAD_REQUEST
- errors.UNAUTHORIZED
- errors.SERVER_ERROR
- errors.STORAGE_UNAVAILABLE
