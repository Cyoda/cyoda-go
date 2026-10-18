# Exact member names in typed client JSON

## Contract

Every piece of typed JSON a client sends is read under one rule: a member name
spells a member its shape declares, exactly, once. This covers every HTTP
request body, the `data` of every gRPC CloudEvent, a compute member's answer,
a condition wherever it appears (search, async search, grouped stats,
conditional delete, a workflow or transition criterion, a function's
pre-check), a workflow import and a paging cursor.

- A name that differs from a declared member only in case, or by Unicode case
  folding (`"tranſitions"` with U+017F), is not that member. It is an unknown
  member.
- A member the shape does not declare is refused.
- A member written twice is refused, whatever the two values.
- Anything after the value is refused.
- A condition leaf writes its operator once: two of `operatorType`, `operator`
  and `operation` in one leaf are refused, as a duplicate is.

The refusal names the location as a JSON Pointer from the top of the value
(`invalid JSON at "/workflows/0/states/NEW/transitions/1": unknown member name
"Manual"`) and never a type of the implementation. HTTP answers `400` with the
endpoint's existing code (`BAD_REQUEST`, `MALFORMED_REQUEST` on grouped stats,
`INVALID_CONDITION` on a search-shaped condition, `VALIDATION_FAILED` for a
criterion at workflow import). gRPC answers `InvalidArgument`. A compute
member's answer that breaks the rule is unreadable and fails the callout.

Entity and message payload contents are not typed JSON: there `"a"` and `"A"`
are two fields. A duplicate name inside an entity payload stays refused by the
unstorable-payload check, as before.

Two schema members are declared so that what Cloud's client sends is not
refused:

| Event | Member | Meaning |
|---|---|---|
| `CalculationMemberJoinEvent` | `joinedLegalEntityId` | Optional. The member joins its token's tenant; when this is present and names another tenant, the join is refused with `PermissionDenied`. Documented in `cyoda help grpc`; now in the schema too. |
| `EntityDeleteAllRequest` | `pageSize` | No effect: the selection is streamed. Cloud's schema declares it, so Cloud's client may send it. |

## Why

`encoding/json`, which cyoda-go used before, matched a name to a field ignoring
case and let a later duplicate replace or merge into an earlier one. Two
readers of the same bytes could then disagree about what the client sent. A
workflow import body holding `"transitions"`, `"Transitions"` and
`"TRANSITIONS"` stored a transition sent without `manual` as automatic; a
processor answer whose payload spelt `"Data"` was applied as the entity's data;
a compute answer that misspelt `success` was read as a success.

## Cloud today

Cloud's REST mapper is Jackson 3 under Spring Boot 4.1.1. Names match case
sensitively, as here. `DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES` and
`StreamReadFeature.STRICT_DUPLICATE_DETECTION` are off by Jackson 3's defaults,
and `CaasJsonMapperBuilderCustomizer` enables neither, so an unknown member is
ignored and the last of two duplicate names is kept. `@JsonAlias` on the
operator lets two spellings in one leaf through, the last one winning.

## Cloud obligation

- Refuse an unknown member and a duplicate member name in every client-typed
  body: REST bodies, CloudEvent `data`, compute answers, conditions.
- Refuse a condition leaf that writes its operator twice.
- Declare `joinedLegalEntityId` on `CalculationMemberJoinEvent` (the tenant
  check is described above) and keep `pageSize` declared on
  `EntityDeleteAllRequest` with no effect.
