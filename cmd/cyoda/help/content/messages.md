---
topic: messages
title: "messages — edge message store API"
stability: stable
see_also:
  - crud
  - cloudevents
  - openapi
  - errors.ENTITY_NOT_FOUND
  - errors.BAD_REQUEST
---

# messages

## NAME

messages — edge message create, read, and delete REST API.

## SYNOPSIS

```
POST   /api/message/new/{subject}
GET    /api/message/{messageId}
DELETE /api/message/{messageId}
DELETE /api/message
```

Context path prefix is `CYODA_CONTEXT_PATH` (default `/api`). All endpoints require `Authorization: Bearer <token>` except when `CYODA_IAM_MODE=mock`. No role is required beyond a valid token.

## DESCRIPTION

An edge message is an arbitrary JSON payload stored under a server-generated time-UUID together with a fixed set of AMQP-aligned headers and an optional flat metadata map. The store is standalone: a message is not an entity, and creating one does not touch the workflow engine — no transition fires, no processor or criterion runs. Edge messaging is a durable, tenant-scoped staging buffer at the platform edge, a peer of the entity store rather than part of it.

The surface is HTTP-only; there is no gRPC message service.

Messages are keyed by `(tenant, messageId)`. The tenant is derived from the authenticated caller, never from a request parameter, so a message ID only resolves within the tenant that created it (see TENANT ISOLATION).

Body size limit on all write endpoints: 10 MiB.

## MESSAGE SHAPE

The create request and the read response are different shapes: you POST a payload plus metadata, and you GET back the same payload and metadata wrapped alongside the stored header. The metadata values round-trip unchanged — a flat map in, the same flat map out.

**Create request** — a single JSON object:

- `payload` (required): any valid JSON value — object, array, string, number, boolean, or `null`. It is stored verbatim (whitespace is compacted). `Content-Type` is informational only; the store does not parse the payload against it. Non-JSON content must be stringified by the client (for example base64) and sent as a JSON string.
- `metaData` (optional): a flat map of string keys to JSON values, stored alongside the message and returned verbatim on read. There is no message-query endpoint — messages are retrieved by ID, not searched by metadata.

**Read response** (`EdgeMessageDto`) — three top-level fields:

- `header`: the stored `EdgeMessageHeader` (below); empty optional fields are omitted.
- `metaData`: the metadata map, returned flat and unchanged — the same `metaData` supplied at creation.
- `content`: the payload, echoed back verbatim as it was stored.

**Header** (`EdgeMessageHeader`) — AMQP-aligned:

- `subject` (string): taken from the `{subject}` path segment on create.
- `contentType` (string): from the `Content-Type` request header; informational.
- `contentLength` (int64): from the `Content-Length` request header.
- `contentEncoding` (string): from the `Content-Encoding` request header (default `UTF-8`).
- `messageId`, `userId`, `recipient`, `replyTo`, `correlationId` (all optional strings): from the `X-Message-ID`, `X-User-ID`, `X-Recipient`, `X-Reply-To`, `X-Correlation-ID` request headers respectively. These are caller-supplied envelope fields and are distinct from the server-generated store ID in the URL.

## ENDPOINTS

**POST /api/message/new/{subject}** — Create and store a message

- `subject` (path): string matching `^[a-zA-Z0-9._-]{1,256}$`
- `Content-Type` (header, required): MIME type of the payload (informational)
- `Content-Length` (header, required): payload size in bytes
- `Content-Encoding` (header, optional): default `UTF-8`
- `X-Message-ID`, `X-User-ID`, `X-Recipient`, `X-Reply-To`, `X-Correlation-ID` (headers, optional): AMQP envelope fields, each 1..1024 chars
- `transactionTimeoutMillis` (query, optional): int64, default `10000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

Request body: a `NewMessageRequest` object `{ payload, metaData }` as described in MESSAGE SHAPE. Missing `payload` returns `400 BAD_REQUEST`.

The server generates a time-UUID for the message and a separate time-UUID for the transaction. Response: `200 OK`, `application/json`, an `EntityTransactionResponse` array with a single element:

```json
[{
  "entityIds": ["8824c480-c166-11ee-bf9f-ae468cd3ed16"],
  "transactionId": "9f3a0000-c166-11ee-bf9f-ae468cd3ed16"
}]
```

**GET /api/message/{messageId}** — Retrieve a message by ID

- `messageId` (path): UUID

Response: `200 OK`, `application/json`, an `EdgeMessageDto`:

```json
{
  "header": {
    "subject": "nobel.prize.events",
    "contentType": "application/json",
    "contentLength": 1024,
    "contentEncoding": "UTF-8",
    "messageId": "msg-nobel-2024-physics",
    "userId": "nobel-committee",
    "recipient": "scientific-community",
    "replyTo": "announcements@nobelprize.org",
    "correlationId": "nobel-2024-physics-announcement"
  },
  "metaData": {
    "eventType": "nobel.prize.announced",
    "timestamp": "2024-10-09T12:00:00Z"
  },
  "content": { "category": "physics", "year": "2024" }
}
```

A malformed `messageId` returns `400 BAD_REQUEST`; a well-formed UUID that does not exist for the calling tenant returns `404 ENTITY_NOT_FOUND`.

**DELETE /api/message/{messageId}** — Delete a single message

- `messageId` (path): UUID

Deletes the message and its payload blob. Returns `404 ENTITY_NOT_FOUND` if the ID does not exist for the tenant. Response: `200 OK`, `application/json`, a `MessageDeleteResponse`:

```json
{ "entityIds": ["8824c480-c166-11ee-bf9f-ae468cd3ed16"] }
```

Message delete is not transactional in the same sense as entity create/update.

**DELETE /api/message** — Delete multiple messages by ID

- `transactionSize` (query, optional): int32, default `1000` — accepted for Cyoda Cloud parity; parsed but currently has no behavioural effect in cyoda-go.

Request body: a JSON array of UUID strings. Every element must parse as a UUID; otherwise `400 BAD_REQUEST`. Response: `200 OK`, `application/json`, a `MessageDeleteBatchResponse` array:

```json
[{
  "entityIds": [
    "8824c480-c166-11ee-9cc7-ae468cd3ed16",
    "31134900-d9cb-11ee-9cc7-ae468cd3ed16"
  ],
  "success": true
}]
```

## TENANT ISOLATION

Every operation is scoped to the authenticated caller's tenant. The tenant is resolved from the user context, and each backend keys and filters messages by `(tenant, messageId)`:

- postgres / sqlite — the primary key is `(tenant_id, message_id)` and every query filters on `tenant_id`.
- memory — metadata is held per tenant and each payload blob lives under a per-tenant directory.

A message ID is an unguessable time-UUID and is only ever retrievable or deletable by the tenant that created it. A cross-tenant read or delete resolves to `404 ENTITY_NOT_FOUND`, not a leak.

## ERRORS

Errors are RFC 9457 `application/problem+json` with `properties.errorCode` set to the machine-readable code:

- `errors.BAD_REQUEST` — `400` — invalid JSON, missing `payload`, malformed `messageId`, or a delete body that is not a JSON array of UUID strings
- `errors.ENTITY_NOT_FOUND` — `404` — no message with this ID exists for the calling tenant (get and single-delete)
- `413` — request body exceeds the 10 MiB limit (create and batch-delete)
- `401` — missing or invalid Bearer token
- `403` — authenticated but not authorized
- `500` — internal error; generic message plus a ticket UUID, full detail logged server-side

## EXAMPLES

**Create a message:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Correlation-ID: nobel-2024-physics" \
  -d '{
    "metaData": { "eventType": "nobel.prize.announced" },
    "payload":   { "category": "physics", "year": "2024" }
  }' \
  "http://localhost:8080/api/message/new/nobel.prize.events"
```

**Retrieve a message:**

```
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/message/8824c480-c166-11ee-bf9f-ae468cd3ed16"
```

**Delete a single message:**

```
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/message/8824c480-c166-11ee-bf9f-ae468cd3ed16"
```

**Delete several messages by ID:**

```
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '["8824c480-c166-11ee-9cc7-ae468cd3ed16","31134900-d9cb-11ee-9cc7-ae468cd3ed16"]' \
  "http://localhost:8080/api/message"
```

## SEE ALSO

- crud
- cloudevents
- openapi
- errors.ENTITY_NOT_FOUND
- errors.BAD_REQUEST
