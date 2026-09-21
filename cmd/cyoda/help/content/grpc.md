---
topic: grpc
title: "grpc — gRPC service contract"
stability: stable
see_also:
  - config.grpc
  - workflows
  - cloudevents
  - cluster
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.DISPATCH_TIMEOUT
  - errors.SCHEDULE_FUNCTION_INVALID_RESULT
  - errors.DISPATCH_FORWARD_FAILED
  - errors.CALLOUT_FAILED
  - errors.CALLOUT_SUPERSEDED
---

# grpc

## NAME

grpc — gRPC service contract for compute members and entity management.

## SYNOPSIS

```
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext localhost:9090 org.cyoda.cloud.api.grpc.CloudEventsService/StartStreaming
```

## DESCRIPTION

cyoda-go exposes one gRPC service: `CloudEventsService` (package `org.cyoda.cloud.api.grpc`). All gRPC methods use the CloudEvents Protobuf envelope (`io.cloudevents.v1.CloudEvent`) as both request and response types. The event type string in the CloudEvent envelope selects the operation; the JSON payload in `text_data` (or `binary_data`) carries the operation-specific body.

The primary use case is the compute member protocol: external workflow processors subscribe via `StartStreaming`, receive processor and criteria calculation requests, and respond over the same bidirectional stream.

The secondary use case is programmatic entity and model management: `entityManage`, `entityManageCollection`, `entityModelManage`, `entitySearch`, and `entitySearchCollection` allow gRPC clients to perform the same CRUD operations as the REST API.

## CONNECTION

**Endpoint**: `host:CYODA_GRPC_PORT` (default `localhost:9090`).

**Transport**: plaintext TCP by default. TLS termination is handled by the ingress or service mesh in production deployments.

**Authentication**: Bearer token passed as gRPC metadata key `authorization`. The value is the same `Bearer <token>` string as used in the HTTP API. Both mock IAM and JWT modes apply identically to gRPC connections — the auth interceptor extracts the `authorization` metadata value, builds an `http.Request` with that `Authorization` header, and delegates to the configured `AuthenticationService`.

**OTel tracing**: when `CYODA_OTEL_ENABLED=true`, the gRPC server installs an `otelgrpc.NewServerHandler()` stats handler that creates spans for every inbound RPC.

## SERVICES

The proto package is `org.cyoda.cloud.api.grpc`. The Go package is `github.com/cyoda-platform/cyoda-go/api/grpc/cyoda`.

```proto
syntax = "proto3";

package org.cyoda.cloud.api.grpc;

import "cloudevents/cloudevents.proto";

service CloudEventsService {
  rpc startStreaming(stream io.cloudevents.v1.CloudEvent)
      returns (stream io.cloudevents.v1.CloudEvent);

  rpc entityModelManage(io.cloudevents.v1.CloudEvent)
      returns (io.cloudevents.v1.CloudEvent);

  rpc entityManage(io.cloudevents.v1.CloudEvent)
      returns (io.cloudevents.v1.CloudEvent);

  rpc entityManageCollection(io.cloudevents.v1.CloudEvent)
      returns (stream io.cloudevents.v1.CloudEvent);

  rpc entitySearch(io.cloudevents.v1.CloudEvent)
      returns (io.cloudevents.v1.CloudEvent);

  rpc entitySearchCollection(io.cloudevents.v1.CloudEvent)
      returns (stream io.cloudevents.v1.CloudEvent);
}
```

**startStreaming** — bidirectional streaming RPC for compute member lifecycle. Requires `ROLE_M2M`. First message must be `CalculationMemberJoinEvent`. Server sends processor and criteria requests; client sends responses and keep-alive acknowledgments.

**entityModelManage** — unary RPC for entity model operations. Accepts: `EntityModelImportRequest`, `EntityModelExportRequest`, `EntityModelTransitionRequest`, `EntityModelDeleteRequest`, `EntityModelGetAllRequest`.

**entityManage** — unary RPC for single-entity operations. Accepts: `EntityCreateRequest`, `EntityUpdateRequest`, `EntityDeleteRequest`, `EntityDeleteAllRequest`, `EntityTransitionRequest`.

**entityManageCollection** — server-streaming RPC for batch entity operations. Accepts: `EntityCreateCollectionRequest`, `EntityUpdateCollectionRequest`. Streams one response CloudEvent per entity.

**entitySearch** — unary RPC for entity retrieval. Accepts: `EntityGetRequest`, `EntityGetAllRequest`, `EntitySnapshotSearchRequest`, `EntitySearchRequest`, `SnapshotCancelRequest`, `SnapshotGetRequest`, `SnapshotGetStatusRequest`, `EntityStatsGetRequest`, `EntityStatsByStateGetRequest`, `EntityChangesMetadataGetRequest`.

**entitySearchCollection** — server-streaming RPC for collection retrieval. Streams results.

## MESSAGE TYPES

All CloudEvents are encoded in the Protobuf CloudEvent format. The `type` field selects the operation. The `text_data` field carries the JSON-encoded payload.

**CloudEvent envelope** (`io.cloudevents.v1.CloudEvent`):

```proto
message CloudEvent {
  string id = 1;          // UUID
  string source = 2;      // "cyoda"
  string spec_version = 3; // "1.0"
  string type = 4;         // event type constant

  map<string, CloudEventAttributeValue> attributes = 5;

  oneof data {
    bytes  binary_data = 6;
    string text_data   = 7;   // JSON payload
    google.protobuf.Any proto_data = 8;
  }
}
```

**Streaming event types** (compute member protocol):

- `CalculationMemberJoinEvent` — first message from client; registers the member
- `CalculationMemberGreetEvent` — server response to join; includes assigned member ID
- `CalculationMemberKeepAliveEvent` — bidirectional; server sends on interval, client echoes
- `EntityProcessorCalculationRequest` — server → client; processor dispatch request
- `EntityProcessorCalculationResponse` — client → server; processor result
- `EntityCriteriaCalculationRequest` — server → client; criteria dispatch request
- `EntityCriteriaCalculationResponse` — client → server; criteria result
- `EntityFunctionCalculationRequest` — server → client; function dispatch request
- `EntityFunctionCalculationResponse` — client → server; function result
- `EventAckResponse` — client → server; acknowledges any server event

**EventAckResponse `text_data` JSON shape:**

```json
{
  "id": "<uuid for this ack message>",
  "sourceEventId": "<id of the server event being acknowledged>",
  "success": true,
  "warnings": [],
  "error": null
}
```

Fields:
- `id` (string, required) — unique identifier for this ack message; any UUID
- `sourceEventId` (string, required) — the `id` field from the server CloudEvent being acknowledged
- `success` (boolean, optional, default `true`) — set to `true` for a normal ack; `false` if the client is reporting a processing error
- `warnings` (string array, optional) — diagnostic messages; may be omitted
- `error` (object, optional) — present only when `success=false`; shape: `{"code":"<code>","message":"<msg>","retryable":<bool|null>}`

The full CloudEvent envelope for an ack:

```json
{
  "id": "<ack-uuid>",
  "source": "client",
  "spec_version": "1.0",
  "type": "EventAckResponse",
  "text_data": "{\"id\":\"<ack-uuid>\",\"sourceEventId\":\"<server-event-id>\",\"success\":true}"
}
```

`EventAckResponse` updates the member's last-seen timestamp, preventing keep-alive timeout. It is used to acknowledge any server event for which the client has no substantive response (e.g. a keep-alive or a greet event).

**Entity management event types**:

- `EntityCreateRequest` / `EntityTransactionResponse`
- `EntityCreateCollectionRequest` / `EntityTransactionResponse` (streamed)
- `EntityUpdateRequest` / `EntityTransactionResponse`
- `EntityUpdateCollectionRequest` / `EntityTransactionResponse` (streamed)
- `EntityDeleteRequest` / `EntityDeleteResponse`
- `EntityDeleteAllRequest` / `EntityDeleteAllResponse`
- `EntityTransitionRequest` / `EntityTransitionResponse`
- `EntityPatchRequest` / `EntityTransactionResponse` — partial update; `PatchFormat` selects the dialect (`MERGE_PATCH`, RFC 7386 `application/merge-patch+json`, or `JSON_PATCH` `application/json-patch+json`). Requires `ifMatch` (the `transactionId` from the last read, or `"*"` for last-writer-wins); an optional `transition` names the transition to fire.

**Model management event types**:

- `EntityModelImportRequest` / `EntityModelImportResponse`
- `EntityModelExportRequest` / `EntityModelExportResponse`
- `EntityModelTransitionRequest` / `EntityModelTransitionResponse`
- `EntityModelDeleteRequest` / `EntityModelDeleteResponse`
- `EntityModelGetAllRequest` / `EntityModelGetAllResponse`
- `EntityModelSetUniqueKeysRequest` / `EntityModelSetUniqueKeysResponse`

**Search / query event types**:

- `EntityGetRequest` / `EntityResponse`
- `EntityGetAllRequest` / `EntityResponse` (streamed via entitySearchCollection)
- `EntitySnapshotSearchRequest` / `EntitySnapshotSearchResponse`
- `EntitySearchRequest` / `EntityResponse`
- `SnapshotCancelRequest` / `EntitySnapshotSearchResponse`
- `SnapshotGetRequest` / `EntitySnapshotSearchResponse`
- `SnapshotGetStatusRequest` / `EntitySnapshotSearchResponse`
- `EntityStatsGetRequest` / `EntityStatsResponse`
- `EntityStatsByStateGetRequest` / `EntityStatsByStateResponse`
- `EntityChangesMetadataGetRequest` / `EntityChangesMetadataResponse`

## COMPUTE MEMBER PROTOCOL

The compute member protocol allows external processes to serve as workflow processor and criteria nodes.

**Join sequence:**

1. Client opens `startStreaming` with `Authorization: Bearer <token>` metadata. Token must carry `ROLE_M2M`.
2. Client sends `CalculationMemberJoinEvent` as the first message:

```json
{
  "id": "<uuid>",
  "tags": ["approval-service", "notification"],
  "joinedLegalEntityId": "acme-corp"
}
```

`joinedLegalEntityId` must match the tenant ID in the bearer token. When present and mismatched, the server returns `codes.PermissionDenied`. When absent, the server uses the token's tenant ID implicitly. Include `joinedLegalEntityId` in all join messages — clients that omit it against a strict server may fail if validation is tightened.

3. Server registers the member and responds with `CalculationMemberGreetEvent`:

```json
{
  "id": "<server-assigned-member-uuid>",
  "memberId": "<server-assigned-member-uuid>",
  "joinedLegalEntityId": "<tenantId>",
  "success": true
}
```

**What a compute member must do:**

- The greet is always the first event on the stream, before any request.
- Read the stream continuously — a member that stops reading is treated as frozen and evicted after `CYODA_KEEPALIVE_TIMEOUT` seconds, and every callout in flight on it fails with `COMPUTE_MEMBER_DISCONNECTED`.
- Write to the stream from one goroutine at a time — the gRPC streaming API forbids concurrent sends on one stream.
- Answer requests, acknowledge events, or echo the server's keep-alive at least once per `CYODA_KEEPALIVE_TIMEOUT` seconds.
- Echo the transaction token (`cyodatxtoken`) on every callback — see `cyoda help cluster` for how the HTTP and gRPC doors carry it. A token belongs to one try of one callout. Once the server has given the callout to another member, or the callout has ended, a callback bearing the token is refused with `errors.CALLOUT_SUPERSEDED` (`410`) for as long as the transaction is open, and with `errors.TRANSACTION_NOT_FOUND` (`404`) once it has closed; a token naming no callout and try number at all is refused with `errors.UNAUTHORIZED` (`401`), the same as any malformed token; a token past its own expiry is refused with `errors.TRANSACTION_EXPIRED` (`410`). None of the four is retryable: stop working on that request.
- Expect callbacks of one transaction to run **one after another**. Every callback — a read or a search as much as a write — holds its transaction for the time the server works on it, so two callbacks sent in parallel are served in turn, not at once. The server reads the whole request before it takes the transaction and sends the response after it has let go, so a slow upload or a slow reader holds nothing up; a callback body over 10 MiB is refused with `413` before that happens (HTTP only).
- Do not queue callbacks without limit on one transaction. Because they are served one at a time, firing many at once buys no speed, and each one waiting holds its whole request in memory until its turn comes. At most `CYODA_CALLOUT_JOINED_MAX_WAITERS` (default 128) may wait; past that a callback is refused with `503` `errors.TOO_MANY_JOINED_REQUESTS`, having touched nothing. It is retryable: back off briefly and send the callback again. A processor that lets the refusal escape fails its callout, and the operation is rolled back.
- Keep a callback's **answer** under `CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES` (default 10 MiB). The answer is held in memory for the same reason the request is: an answer that would pass the ceiling fails the callback with `413` `errors.JOINED_RESPONSE_TOO_LARGE`, naming the ceiling, rather than being cut short — on either door, and on the gRPC one the frames of a chunked collection count together. Not retryable: page a large read — `pageSize` and `pageNumber` on a get-all or a search — instead of asking for everything in one callback.
- A callback is not abandoned by its member's connection dropping, nor by a deadline the member sets on its own callback call — see "API requests made under a transaction token" below for what continues, and what does not.

**Processor dispatch (server → client):**

Server sends `EntityProcessorCalculationRequest` when a workflow transition invokes an `externalized` processor whose `calculationNodesTags` matches one of the member's declared tags:

```json
{
  "id": "<requestId>",
  "requestId": "<requestId>",
  "entityId": "<entityUUID>",
  "processorId": "notify-approval",
  "processorName": "notify-approval",
  "workflow": {"id": "prize-lifecycle", "name": "prize-lifecycle"},
  "transition": {"id": "APPROVE", "name": "APPROVE"},
  "transactionId": "<txUUID>",
  "success": true,
  "payload": {
    "type": "JSON",
    "data": {<entity JSON body>},
    "meta": {
      "id": "<entityUUID>",
      "modelKey": {"name": "nobel-prize", "version": 1},
      "state": "NEW",
      "creationDate": "2025-08-02T13:31:48.141053Z",
      "lastUpdateTime": "2025-08-02T13:31:48.141053Z",
      "transactionId": "<txUUID>"
    }
  }
}
```

`payload` is omitted when `attachEntity=false` in the processor config.

Client responds with `EntityProcessorCalculationResponse`:

```json
{
  "requestId": "<same requestId>",
  "success": true,
  "payload": {
    "type": "JSON",
    "data": {<optionally updated entity JSON body>}
  },
  "warnings": [],
  "error": null
}
```

`success` is optional and defaults to `true`, as the published schema says: a
response that leaves the key out has reported success. A member reporting a
failure must therefore send `success: false` explicitly — an empty or partial
response is not read as a failure. **An `error` object on its own does not
report one either**: `success` is what says the work failed, and `error` only
says what went wrong once it has. A response carrying an `error` and no
`success: false` is a success, and its message reaches nobody — not the client,
not the warnings, not the audit trail.

The smallest successful answer is `{"requestId": "<same requestId>"}`: it says
the processor ran, changed nothing, and the workflow should carry on. There is
no shape that means "I did nothing and something is wrong" — that is
`success: false`.

When `success=false`, no other member is tried. The client's operation fails with `400 WORKFLOW_FAILED` carrying `error.message`, and `error.retryable: true` is passed on as the client's `retryable: true` — it tells the client that running the whole operation again may succeed; it does not make the server try another member. (An `ASYNC_NEW_TX` processor is the exception: its failure is logged and the operation continues.) When `payload.data` is non-null, the engine replaces the entity's data with the returned value before continuing the workflow; when `payload` is absent, or its `data` is null, the entity is left as it was and the transition continues.

Returned data is subject to the same checks as an HTTP client write: it must be storable, and it must satisfy the model's schema. A processor may introduce a field the model does not declare only where the model's `changeLevel` would allow a client to — otherwise the transition fails with `WORKFLOW_FAILED` and rolls back. The engine holds no privilege here: whatever it stores, the API must be able to accept back.

**Criteria dispatch (server → client):**

Server sends `EntityCriteriaCalculationRequest` when a workflow transition evaluates a `function`-type criterion:

```json
{
  "id": "<requestId>",
  "requestId": "<requestId>",
  "entityId": "<entityUUID>",
  "criteriaId": "my-criteria-fn",
  "criteriaName": "my-criteria-fn",
  "target": "TRANSITION",
  "workflow": {"id": "prize-lifecycle", "name": "prize-lifecycle"},
  "transition": {"id": "APPROVE", "name": "APPROVE"},
  "transactionId": "<txUUID>",
  "success": true,
  "payload": { ...same shape as processor payload... }
}
```

Client responds with `EntityCriteriaCalculationResponse`:

```json
{
  "requestId": "<same requestId>",
  "success": true,
  "matches": true,
  "warnings": [],
  "error": null
}
```

`success` is optional here too and defaults to `true`, so a response that
leaves the key out has reported success; a member reporting a failure must send
`success: false` explicitly, an `error` object on its own being no more a
failure report here than it is for a processor.

`matches` is required on a successful criteria response — which is any response
but an explicit `success: false` one. A response that omits it is not read as
`false` — a missing verdict would be an invented answer to the criterion, and
the criterion decides a transition — so the callout ends as an answer that
could not be read (`400 WORKFLOW_FAILED`, not retryable) and no other compute
member is tried. Omitting `success` therefore does not excuse omitting
`matches`: the default fills in the flag, never the verdict.

On `matches: false`, the response may also carry a `reason` string explaining
why the criterion blocked the passage. The reason is the criterion's own
business explanation, not a diagnostic, so it keeps a wider allowance than the
rest of a member's free text: it is kept to its first 2048 characters, marked
with `…` when it was cut, and the stored or reflected reason is capped well
above that so this second cap is never what bites. It surfaces in two
places: the manual-transition `400 WORKFLOW_FAILED` body
(`detail: transition "<name>" criterion not matched: <reason>` — the
guaranteed, backend-independent delivery for a manual rejection), and the
`TRANSITION_NOT_MATCH_CRITERION` / `WORKFLOW_SKIP` state-machine audit events'
`data.reason` — durable there only for the automated-cascade and
workflow-selection paths, since a manual rejection rolls its transaction back.
An omitted `reason` defaults to `"criterion did not match"` in the audit and
is left out of the 400 detail (bare `criterion not matched`).

**Function callout wire shape:**

`EntityFunctionCalculationRequest`/`EntityFunctionCalculationResponse` are the wire types for the Function callout — a third callout shape alongside processor and criteria that returns a declared typed value instead of a boolean or entity payload (e.g. computing a scheduled state transition's fire time). Request shape mirrors the processor request, naming the callout target `functionId`/`functionName`:

```json
{
  "id": "<requestId>",
  "requestId": "<requestId>",
  "entityId": "<entityUUID>",
  "functionId": "compute-fire-at",
  "functionName": "compute-fire-at",
  "workflow": {"id": "prize-lifecycle", "name": "prize-lifecycle"},
  "transition": {"id": "APPROVE", "name": "APPROVE"},
  "transactionId": "<txUUID>",
  "success": true
}
```

Response replaces criteria's `matches`/`reason` with `result` (an arbitrary JSON object) plus a `resultKind` discriminator string identifying its shape:

```json
{
  "requestId": "<same requestId>",
  "success": true,
  "result": {"fireAt": 1},
  "resultKind": "Schedule",
  "warnings": [],
  "error": null
}
```

`resultKind: "Schedule"` is the only shape currently defined — it drives a
scheduled transition's `schedule.function` (see `cyoda help workflows`).
`success` defaults to `true` here as everywhere, so a response that omits it
has reported success, and an `error` object on its own is not a failure report.
`success: false` — which a member reporting a failure
must send explicitly — fails the callout as it does for a processor: no other
member is tried, and the message and `retryable` verdict reach the client;
a `result` that doesn't parse against the
declared `resultKind` is rejected by the caller (a scheduled transition's
`SCHEDULE_FUNCTION_INVALID_RESULT`), not by this wire contract. Full JSON
Schemas for both messages: `docs/cyoda/schema/processing/EntityFunctionCalculationRequest.json`
and `EntityFunctionCalculationResponse.json` (see `cyoda help cloudevents`).

**API requests made under a transaction token:**

An API request made under a transaction token is not cancelled when its client
goes away: once it has the transaction to itself it runs to completion on the
node that holds the transaction. Until then — while it waits its turn behind
another request under the same token — it has touched nothing, and a client that
goes away is simply dropped. If the connection drops, or the node the request
arrived at answers `503` because forwarding it took longer than
`CYODA_PROXY_TIMEOUT`, the outcome of a write is **unknown** — it may have been
applied to the transaction. Do not assume it failed. A deadline the compute
member sets on its own gRPC call does not stop the request on the server either.

The token is issued for one request on one compute node. If cyoda gives the
same work to another compute node — this one did not answer within its answer
limit, or its connection dropped — or once the request has ended, every further
API request under that token is refused with `410 CALLOUT_SUPERSEDED`; a request
that was already in progress finishes and is answered normally. A compute node
that receives `CALLOUT_SUPERSEDED` must stop working on that request. While a
compute node's request is in progress it has the transaction to itself: API
requests under one token run one at a time, and cyoda does not interrupt one
because its client went away.

**Auth context on dispatched events:**

The server attaches CloudEvent Auth Context extension attributes to every dispatched request:

- `authtype` — `"user"`, `"service"`, or `"system"`, driven by the originating
  principal's explicit kind (not sniffed from roles). **Wire change:** this was
  previously `"user"` / `"service_account"` inferred from a `ROLE_M2M` role;
  it is now one of exactly these three values, always. Dispatch fails closed
  — no callout is sent — if the principal's kind is unset or unrecognized, so
  a bogus or absent `authtype` never reaches a compute node.
- `authid` — the user ID of the originating request
- `authclaims` — comma-separated roles of the originating user

## KEEPALIVE

The server sends `CalculationMemberKeepAliveEvent` to each connected member
every `CYODA_KEEPALIVE_INTERVAL` seconds, and evicts a member when either of
two things happens within `CYODA_KEEPALIVE_TIMEOUT` seconds: no inbound
activity has been seen from it, or one outbound write to it has stalled that
long. Processor responses, criteria responses, function responses, and
`EventAckResponse` all count as inbound activity, the same as a keep-alive
echo — any of them resets the eviction clock.

The same two values also drive grpc-go's HTTP/2 transport keepalive: a PING
is sent after `CYODA_KEEPALIVE_INTERVAL` seconds of transport idleness, and
the connection is closed if it goes unacknowledged for
`CYODA_KEEPALIVE_TIMEOUT` seconds — a second, independent layer that catches
a peer whose TCP connection is alive but whose process is gone. The
enforcement policy the server advertises to clients is deliberately
permissive — pings from a client are accepted no more often than every 5
seconds, well below grpc-go's default 5-minute floor, so a compute node
pinging on a normal cadence is never disconnected for it.

- `CYODA_KEEPALIVE_INTERVAL` — seconds between server-sent keep-alive events and the transport keepalive idle time (default: `10`)
- `CYODA_KEEPALIVE_TIMEOUT` — seconds of inactivity (or write stall) before the server evicts the member, and the transport keepalive ack timeout (default: `30`)

Both variables are applied to the gRPC server at construction. A value that
parses to zero or a negative number is a startup error; an unparseable value
falls back to the default.

## TAG ROUTING

A compute member declares its tags in `CalculationMemberJoinEvent.tags` as a string slice. The server routes a processor or criteria request to a member whose tags overlap with `calculationNodesTags` (comma-separated) from the processor or criteria config.

Among the members of the authenticated tenant whose tags match, the server picks **round robin**: the member that was picked longest ago goes next, and a member that has just joined has never been picked and goes first. Tag matching uses intersection: the member must declare at least one tag that appears in the callout's `calculationNodesTags`. A member of another tenant is never chosen, whatever its tags. Clients that need one particular member to receive a callout must give that member a tag of its own.

A callout may be tried on more than one member. **Every try carries the same `requestId`** (and the same `id`) in its payload, and the same `transactionId`. A member that de-duplicates on `requestId` will therefore treat a second delivery of the same callout — to itself after a reconnect, or seen by a shared de-duplication store behind several members — as a repeat, which is the intent. Criteria and functions must have no effects: they may be given to another member whenever one does not answer. A processor is given to another member after it was handed the work only if its configuration declares it `idempotent`.

When `calculationNodesTags` is empty, every member of the authenticated tenant matches, and the same round robin applies.

In cluster mode each node tells its peers which tags its members serve, per tenant. A node tries its own matching members first and then hands the callout, with the tries that are left, to a peer that advertises the tag — see `cyoda help cluster`.

## ERRORS

gRPC error codes returned by the service:

- `codes.Unauthenticated` — missing or invalid `authorization` metadata
- `codes.PermissionDenied` — `ROLE_M2M` required for `startStreaming`; tenant mismatch on join
- `codes.InvalidArgument` — first message is not `CalculationMemberJoinEvent`; malformed CloudEvent; invalid join payload
- `codes.DeadlineExceeded` — member timed out (keep-alive timeout exceeded)
- `codes.Internal` — server-side error constructing a response CloudEvent

Within `text_data` payloads, errors are reported as:

```json
{
  "success": false,
  "error": {
    "code": "SERVER_ERROR",
    "message": "SERVER_ERROR: internal error [ticket: <uuid>]",
    "retryable": null
  }
}
```

Operational 4xx errors carry the domain code (e.g. `CLIENT_ERROR`) and a human-readable message. Internal errors use `SERVER_ERROR` with a ticket UUID for server-side log correlation.

Callout errors the client of the failed operation sees (all `503`, retryable):

- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — no matching member appeared within `CYODA_DISPATCH_WAIT_TIMEOUT`
- `errors.COMPUTE_MEMBER_DISCONNECTED` — the member's stream dropped after it was given the work
- `errors.DISPATCH_TIMEOUT` — no answer within `responseTimeoutMs`
- `errors.DISPATCH_FORWARD_FAILED` — the answer of the node a callout was handed to was lost
- `errors.CALLOUT_FAILED` — more than one try failed; the message lists them

Errors a compute member sees on a callback:

- `errors.CALLOUT_SUPERSEDED` — `410` — the member was replaced, or its callout has ended
- `errors.TRANSACTION_NOT_FOUND` — `404` — the transaction has ended
- `errors.TRANSACTION_EXPIRED` — `410` — the token is past its expiry
- `errors.UNAUTHORIZED` — `401` — the token does not name a callout and a try number at all

## EXAMPLES

**List services (plaintext, no auth):**

```
grpcurl -plaintext localhost:9090 list
```

**List methods on CloudEventsService:**

```
grpcurl -plaintext localhost:9090 list org.cyoda.cloud.api.grpc.CloudEventsService
```

**Describe the CloudEventsService:**

```
grpcurl -plaintext \
  -import-path ./proto \
  -proto cyoda/cyoda-cloud-api.proto \
  localhost:9090 \
  describe org.cyoda.cloud.api.grpc.CloudEventsService
```

**Connect as a compute member (mock auth — no token required):**

```
grpcurl -plaintext \
  -import-path ./proto \
  -proto cyoda/cyoda-cloud-api.proto \
  -d '{"id":"join-1","source":"client","spec_version":"1.0","type":"CalculationMemberJoinEvent","text_data":"{\"id\":\"join-1\",\"tags\":[\"my-service\"],\"joinedLegalEntityId\":\"mock-tenant\"}"}' \
  localhost:9090 \
  org.cyoda.cloud.api.grpc.CloudEventsService/StartStreaming
```

**Connect as a compute member (JWT auth):**

```
grpcurl -plaintext \
  -H "authorization: Bearer $TOKEN" \
  -import-path ./proto \
  -proto cyoda/cyoda-cloud-api.proto \
  -d '{"id":"join-1","source":"client","spec_version":"1.0","type":"CalculationMemberJoinEvent","text_data":"{\"id\":\"join-1\",\"tags\":[\"my-service\"],\"joinedLegalEntityId\":\"acme-corp\"}"}' \
  localhost:9090 \
  org.cyoda.cloud.api.grpc.CloudEventsService/StartStreaming
```

## ACTION DETAILS

- `cyoda help grpc proto` — emit raw `.proto` source for `cyoda-cloud-api.proto` and `cloudevents.proto` (concatenated with separator comments)
- `cyoda help grpc json` — emit the gRPC service `FileDescriptorSet` as JSON (standard protobuf descriptor form)

## SEE ALSO

- config.grpc
- workflows
- cloudevents
- cluster
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- errors.DISPATCH_TIMEOUT
- errors.SCHEDULE_FUNCTION_INVALID_RESULT
- errors.DISPATCH_FORWARD_FAILED
- errors.CALLOUT_FAILED
- errors.CALLOUT_SUPERSEDED
