# Every CloudEvent type has a schema and a CloudEventType value

cyoda-go is the authoritative implementation. This file is the contract Cyoda
Cloud follows.

## Behaviour

The CloudEvent schema tree (`docs/cyoda/schema/`, emitted by
`cyoda help cloudevents json`) is the contract for CloudEvent payloads: clients
generate their event types from it. So every event type the server accepts or
sends has a schema of its own name in the tree and is a value of
`common/CloudEventType.json`, and the enum names no type the server does not
know (`BaseEvent`, the shape every event extends, is the one exception). A test
in cyoda-go reads the declared event types and fails on any gap in either
direction.

### `EntityModelSetUniqueKeys`

`entityModelManage` accepts `EntityModelSetUniqueKeysRequest` and answers
`EntityModelSetUniqueKeysResponse`. It is the gRPC form of
`PUT /model/{entityName}/{modelVersion}/unique-keys`.

- **Request** (`model/EntityModelSetUniqueKeysRequest.json`): `BaseEvent`
  plus `model` (`ModelSpec`, required) and `uniqueKeys` (required), an array
  of `{id, fields}` with both members required — the shape of the HTTP
  `UniqueKeyDto`. The array replaces the model's keys; an empty array removes
  them all. A request without `uniqueKeys` is refused (`InvalidArgument`); it
  does not remove the keys.
- **Response** (`model/EntityModelSetUniqueKeysResponse.json`): `BaseEvent`
  plus `modelId` (uuid) and `state`, both required, as in
  `EntityModelTransitionResponse`. A failure is the usual envelope:
  `success: false`, the error code as the message prefix of a `CLIENT_ERROR`
  (`MODEL_NOT_FOUND`, `MODEL_ALREADY_LOCKED`, `INVALID_UNIQUE_KEY_DEFINITION`,
  `COMPOSITE_KEY_UNSUPPORTED`), `modelId` the nil UUID and `state` empty.
- The request is model administration: with a transaction token it is refused
  (`model-administration-not-joined.md`).

### `CloudEventType` values

The enum gains `EntityModelSetUniqueKeysRequest`,
`EntityModelSetUniqueKeysResponse`, `EntityPatchRequest`,
`EntityFunctionCalculationRequest` and `EntityFunctionCalculationResponse`.
The last three already had schemas in the tree, and the server already
accepted or sent them (`EntityPatchRequest`: `entity-patch.md`; the function
pair is the compute-member callout for a function); only their enum values
were missing.

## Cloud today

Checked against Cloud's tree at `client/src/main/resources/schema/` (checkout
of 2026-09-24): it has no schema for any of the five types above, nor for
`EntityConsistencyTimeGetRequest`/`Response` (`consistency-time.md`), and its
`CloudEventType` enum lists none of the seven. Cloud has no composite unique
keys on either door. Aligning means adding each event type's schema and enum
value when Cloud implements the operation, and generating the request types
from the schemas so a missing required member is refused.
