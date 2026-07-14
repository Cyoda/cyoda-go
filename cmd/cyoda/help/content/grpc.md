---
topic: grpc
title: "grpc — gRPC service contract"
stability: stable
see_also:
  - config.grpc
  - workflows
  - errors.COMPUTE_MEMBER_DISCONNECTED
  - errors.NO_COMPUTE_MEMBER_FOR_TAG
  - errors.DISPATCH_TIMEOUT
  - errors.DISPATCH_FORWARD_FAILED
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

When `success=false`, the workflow engine fails the processor dispatch. When `payload.data` is non-null, the engine replaces the entity's data with the returned value before continuing the workflow.

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

**Auth context on dispatched events:**

The server attaches CloudEvent Auth Context extension attributes to every dispatched request:

- `authtype` — `"user"` or `"service_account"` (based on whether the originating user has `ROLE_M2M`)
- `authid` — the user ID of the originating request
- `authclaims` — comma-separated roles of the originating user

## KEEPALIVE

The server sends `CalculationMemberKeepAliveEvent` to each connected member every `CYODA_KEEPALIVE_INTERVAL` seconds. If a member does not respond (via keep-alive echo, processor response, criteria response, or `EventAckResponse`) within `CYODA_KEEPALIVE_TIMEOUT` seconds of the last seen activity, the server closes the stream.

- `CYODA_KEEPALIVE_INTERVAL` — seconds between server-sent keep-alive events (default: `10`)
- `CYODA_KEEPALIVE_TIMEOUT` — seconds of inactivity before the server terminates the stream (default: `30`)

Both variables are read by `DefaultConfig()` and applied at gRPC server construction time.

## TAG ROUTING

A compute member declares its tags in `CalculationMemberJoinEvent.tags` as a string slice. The server routes a processor or criteria request to a member whose tags overlap with `calculationNodesTags` (comma-separated) from the processor or criteria config.

`FindByTags` selects the first matching member for the authenticated tenant by iterating over the internal member map. Tag matching uses intersection: the member must declare at least one tag that appears in the processor's `calculationNodesTags`. Because the internal store is a Go map, iteration order is random (non-deterministic per Go specification). When multiple members share a tag, the selected member is chosen at random on each dispatch. Clients requiring deterministic routing must use distinct tags per member.

When `calculationNodesTags` is empty, any member for the authenticated tenant matches (still chosen at random when multiple exist).

In cluster mode, the `ClusterDispatcher` propagates member tag sets across nodes via gossip so any node can forward dispatches to a node that has a matching member.

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

Processor dispatch errors surfaced to the workflow engine:

- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — no member registered for the requested tags
- `errors.COMPUTE_MEMBER_DISCONNECTED` — member disconnected while a dispatch was in flight (all pending requests receive `"member disconnected"` error)
- `errors.DISPATCH_TIMEOUT` — processor or criteria response not received within `responseTimeoutMs`
- `errors.DISPATCH_FORWARD_FAILED` — cluster forwarder failed to forward dispatch to remote node

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
- errors.COMPUTE_MEMBER_DISCONNECTED
- errors.NO_COMPUTE_MEMBER_FOR_TAG
- errors.DISPATCH_TIMEOUT
- errors.DISPATCH_FORWARD_FAILED
