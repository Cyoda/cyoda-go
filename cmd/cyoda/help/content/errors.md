---
topic: errors
title: "cyoda error reference"
stability: stable
see_also:
  - openapi
  - grpc
  - config
---

# errors

## NAME

errors — error model and code catalogue.

## SYNOPSIS

REST responses use RFC 9457 Problem Details:

```json
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "ENTITY_NOT_FOUND: entity id=abc not found",
  "instance": "/api/v1/entities/abc",
  "properties": {
    "errorCode": "ENTITY_NOT_FOUND",
    "retryable": false
  }
}
```

gRPC error envelope example (returned in the CloudEvent response payload):

```json
{
  "error": {
    "code": "ENTITY_NOT_FOUND",
    "message": "entity id=abc not found",
    "retryable": false
  }
}
```

The gRPC response also carries `errorCode` and `retryable` in trailer metadata.

## DESCRIPTION

Every error response from the Cyoda REST API carries a structured `errorCode` in the `properties` object. Multiple codes may share the same HTTP status. Programmatic handling keys on `errorCode`, not HTTP status.

The `retryable` property is present and `true` only when the operation is safe to retry as-is (e.g., transient cluster conditions). When absent or `false`, the request or system state must change before retrying.

5xx responses include a `ticket` UUID for server-side log correlation. Share this value when reporting issues.

`CYODA_ERROR_RESPONSE_MODE` controls 5xx detail level. `sanitized` (default): generic message plus `ticket` UUID. `verbose`: internal error detail included in the response body; intended for development environments only.

## ERROR CODE INDEX

- `errors.BAD_REQUEST` — `400` — not retryable — request body, query parameter, or header is malformed or structurally invalid
- `errors.CLUSTER_NODE_NOT_REGISTERED` — `503` — retryable — target cluster node is not present in the gossip registry
- `errors.COMPUTE_MEMBER_DISCONNECTED` — `503` — retryable — compute member holding a processor assignment has disconnected
- `errors.CONFLICT` — `409` — retryable — generic 409 used by storage-level transaction serialization aborts (`RetryableConflict`); permanent business-logic conflicts use a specific code instead (e.g. `MODEL_ALREADY_LOCKED`, `ENTITY_MODIFIED`)
- `errors.DISPATCH_FORWARD_FAILED` — `503` — retryable — HTTP forwarding call to peer node failed
- `errors.DISPATCH_TIMEOUT` — `503` — retryable (see note) — compute member did not respond within the dispatch timeout; completion on the remote node is not guaranteed
- `errors.ENTITY_MODIFIED` — `412` — not retryable — `If-Match`-guarded entity update rejected; supplied transaction ID does not match the entity's current version
- `errors.ENTITY_NOT_FOUND` — `404` — not retryable — entity UUID does not exist or is not accessible to the caller
- `errors.EPOCH_MISMATCH` — `409` — retryable — writing node's cached shard epoch is stale; another node has since taken ownership
- `errors.FEATURE_DISABLED` — `404` — not retryable — Optional feature not enabled in this deployment.
- `errors.FORBIDDEN` — `403` — not retryable — authenticated caller lacks the required role or the tenant does not match
- `errors.HELP_TOPIC_NOT_FOUND` — `404` — not retryable — help topic path does not resolve to any topic in the tree
- `errors.IDEMPOTENCY_CONFLICT` — `409` — not retryable — request with the same idempotency key was received but payload differs from the original
- `errors.INCOMPATIBLE_TYPE` — `400` — not retryable — entity payload's leaf value type is not assignable to the schema's declared DataType for that path; carries `fieldPath`, `expectedType`, `actualType` in `properties` (Cloud's `FoundIncompatibleTypeWithEntityModelException` equivalent)
- `errors.KEY_OWNED_BY_DIFFERENT_TENANT` — `409` — not retryable — Trusted-key registration collides with another tenant.
- `errors.KEYPAIR_NOT_FOUND` — `404` — not retryable — Referenced signing keypair does not exist.
- `errors.INVALID_CHANGE_LEVEL` — `400` — not retryable — `POST /model/{name}/{version}/changeLevel/{changeLevel}` supplied a value that is not one of `ARRAY_LENGTH`, `ARRAY_ELEMENTS`, `TYPE`, `STRUCTURAL`
- `errors.INVALID_FIELD_PATH` — `400` — not retryable — search condition references one or more JSONPath field paths absent from the target model's locked schema; bounded refresh did not surface the path
- `errors.MODEL_ALREADY_LOCKED` — `409` — not retryable — admin operation requires `UNLOCKED` state but the model is `LOCKED` (relock attempt or re-import on a locked model)
- `errors.MODEL_ALREADY_UNLOCKED` — `409` — not retryable — admin operation requires `LOCKED` state but the model is `UNLOCKED` (unlock-of-already-unlocked-model)
- `errors.MODEL_HAS_ENTITIES` — `409` — not retryable — unlock or delete blocked because at least one entity of the model exists
- `errors.MODEL_NOT_FOUND` — `404` — not retryable — referenced entity model does not exist in the tenant's model registry
- `errors.MODEL_NOT_LOCKED` — `409` — not retryable — model exists but is not in `LOCKED` state; entity writes require a locked model
- `errors.NO_COMPUTE_MEMBER_FOR_TAG` — `503` — retryable — no live cluster node advertises the compute tag required by the processor
- `errors.NOT_FOUND` — `404` — not retryable — generic resource not found, used by admin endpoints (key pair lifecycle, trusted-key lifecycle); domain-specific resources have their own codes
- `errors.NOT_IMPLEMENTED` — `501` — not retryable — endpoint is defined but has no functional implementation in this version
- `errors.POLYMORPHIC_SLOT` — `400` — not retryable — payload discriminator selects an unrecognised variant or fails the variant schema
- `errors.SEARCH_JOB_ALREADY_TERMINAL` — `409` — not retryable — operation attempted on a search job that has already completed, failed, or been cancelled
- `errors.SEARCH_JOB_NOT_FOUND` — `404` — not retryable — referenced search job does not exist in the current tenant
- `errors.SEARCH_RESULT_LIMIT` — `400` — not retryable — search query matched more results than the server-enforced maximum
- `errors.SEARCH_SHARD_TIMEOUT` — `503` — retryable — one or more search shards did not respond within the configured timeout
- `errors.SERVER_ERROR` — `500` — retryable with caution — unclassified internal error; response includes `ticket` UUID for log correlation
- `errors.TRANSACTION_EXPIRED` — `400` — not retryable — transaction token's `exp` claim is in the past
- `errors.TRANSACTION_NODE_UNAVAILABLE` — `503` — retryable — cluster node that owns the open transaction is unreachable
- `errors.TRANSACTION_NOT_FOUND` — `404` — not retryable — transaction ID does not correspond to an active transaction on this node
- `errors.TRANSITION_NOT_FOUND` — `404` — not retryable — requested workflow transition is not defined for the entity's current state
- `errors.TRUSTED_KEY_CAP_REACHED` — `400` — not retryable — Per-tenant trusted-key cap reached.
- `errors.TRUSTED_KEY_NOT_FOUND` — `404` — not retryable — referenced trusted-key KID is not present in the registry (delete / invalidate / reactivate target missing)
- `errors.TX_CONFLICT` — `409` — retryable — transaction aborted due to storage-level serialization conflict
- `errors.TX_COORDINATOR_NOT_CONFIGURED` — `503` — not retryable — distributed transaction coordinator is disabled or misconfigured on this node
- `errors.TX_NO_STATE` — `404` — not retryable — coordinator has no state record for the given transaction ID
- `errors.TX_REQUIRED` — `400` — not retryable — operation requires a transaction context but none was provided
- `errors.UNAUTHORIZED` — `401` — not retryable — `Authorization` header is missing, token is expired, signature is invalid, or issuer is untrusted
- `errors.UNSUPPORTED_ALGORITHM` — `400` — not retryable — Requested JWT algorithm not supported in this version.
- `errors.UNSUPPORTED_KEY_TYPE` — `400` — not retryable — JWK `kty` not supported in this version.
- `errors.VALIDATION_FAILED` — `400` — not retryable — payload is structurally valid JSON but fails the model's schema or workflow validation rules
- `errors.WORKFLOW_FAILED` — `400` — not retryable — workflow processor or guard condition returned a failure during state transition
- `errors.WORKFLOW_NOT_FOUND` — `404` — not retryable — workflow definition referenced by the entity model does not exist

## SEE ALSO

- openapi
- grpc
- config
