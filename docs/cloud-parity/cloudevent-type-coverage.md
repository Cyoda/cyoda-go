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
  them all. A request without `uniqueKeys`, or with `"uniqueKeys": null`, is
  refused (`InvalidArgument`) and changes nothing; it does not remove the
  keys. The HTTP door refuses the same bodies with `400 BAD_REQUEST`.
- **Response** (`model/EntityModelSetUniqueKeysResponse.json`): `BaseEvent`
  plus `modelId` (uuid) and `state`, both required, as in
  `EntityModelTransitionResponse`. A failure is the usual envelope:
  `success: false`, the error code as the message prefix of a `CLIENT_ERROR`
  (`MODEL_NOT_FOUND`, `MODEL_ALREADY_LOCKED`, `INVALID_UNIQUE_KEY_DEFINITION`,
  `COMPOSITE_KEY_UNSUPPORTED`), `modelId` the nil UUID and `state` empty.
- The request is model administration: with a transaction token it is refused
  (`model-administration-not-joined.md`).

### A null required member

In a client's request event and in `CalculationMemberJoinEvent` — every event
the server reads through its generated types — a required member whose schema
states a type is refused when it is `null`, as when it is absent
(`InvalidArgument`, `field name: must not be null`); no stated type admits
`null`. Five required members state no type and hold any JSON value, `null`
included: the entity `data` (create, update), the `patch`, the model-import
`payload` and the model-export `payload`. A `null` entity `data`, merge
`patch` or import `payload` is refused by the operation, in the event's error
envelope (`CLIENT_ERROR`, `VALIDATION_FAILED`), as on HTTP: an entity's data
is a JSON object at every change level, and a model-import sample is an
object or an array of objects. A compute member's answer is not covered by
this rule: it is read through the members its schema declares, without
required-member checks.

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
`EntityConsistencyTimeGetRequest`/`EntityConsistencyTimeResponse` (`consistency-time.md`), and its
`CloudEventType` enum lists none of the seven. Cloud has no composite unique
keys on either door. Aligning means adding each event type's schema and enum
value when Cloud implements the operation, and refusing a required member that
is missing, or `null` where its schema states a type.
