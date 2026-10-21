# Exact member names in typed client JSON

## Contract

Every piece of typed JSON a client sends is read under one rule: a member name
spells a member its shape declares, exactly, once. This covers every HTTP
request body, the `data` of every gRPC request event (a compute member's
keep-alive and acknowledgement events carry nothing the server reads), a
compute member's answer,
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
  and `operation` holding a value in one leaf are refused, as a duplicate is
  (a spelling written as `null` is not an operator). The condition
  parser refuses it, so the message names the two members and the leaf's
  place as its other shape errors do (`failed to parse group condition[0]:
  failed to parse simple condition: the operator is written twice, as
  "operatorType" and "operator": write one`), not by a JSON Pointer. A
  condition's `type` written in another case leaves the condition without
  one, and is refused in those words: `the condition has no "type" member`.
- On gRPC, a required member written in another case is answered as that
  member missing (`invalid JSON at "/payload/model": field name: required`):
  the generated types check for required members before the strict decode.

The refusal names the location as a JSON Pointer from the top of the value
(`invalid JSON at "/workflows/0/states/NEW/transitions/1": unknown member name
"Manual"`) and never a type of the implementation. A workflow or transition
criterion at workflow import is an exception, as every other criterion
refusal there is: the message names the workflow, and for a transition
criterion its state and transition, in words, then the location inside the criterion (`workflow "wf" state "A"
transition "go": criterion is not a valid condition: invalid JSON at
"/conditions/1": …`). A paging cursor is opaque to the client, so a refused
one answers `400 BAD_REQUEST: invalid cursor parameter` with no location. HTTP answers `400` with the
endpoint's existing code (`BAD_REQUEST`, `MALFORMED_REQUEST` on grouped stats,
`INVALID_CONDITION` on a search-shaped condition, `VALIDATION_FAILED` for a
criterion at workflow import). Where a condition sits inside a larger body, a
member written twice in it is found before the condition is parsed: inside a
grouped-stats `condition` it is `MALFORMED_REQUEST`. Where the body is the
condition itself (search, conditional delete) it is `INVALID_CONDITION`.
Anywhere in a workflow import, an entity create or update (one entity or a
collection) or model validate, the unstorable-payload check refuses it first,
with `BAD_REQUEST` and the field named by its path (`field
"workflows[0].states.A.transitions[0].manual" appears more than once…`).
Text that is not JSON is located by its byte offset. gRPC answers `InvalidArgument`, except on an
entity write (create, update, patch and the two collection events): there the
unstorable-payload check reads the whole event first, so a member written twice
anywhere in the event is answered by the event's error response (`CLIENT_ERROR`,
`BAD_REQUEST: field "…" appears more than once…`, the field named from the top
of the event). A compute member's answer that breaks the rule is unreadable and
fails the callout.

Entity and message payload contents are not typed JSON: there `"a"` and `"A"`
are two fields, and no member is unknown. A payload still has one reading: a
member written twice in one object, invalid UTF-8 and a lone UTF-16 surrogate
escape are refused in an entity payload (by the unstorable-payload check, as
before), in a message payload, and in a model import's sample data on both
doors, where anything after the value is refused too. In a compute member's processor answer they make the answer unreadable,
which fails the callout; before, the payload was read and then refused by the
unstorable-payload check naming the field. `encoding/json` replaced invalid
text with U+FFFD; in a typed string it is refused too.

Two schema members are declared:

| Event | Member | Meaning |
|---|---|---|
| `CalculationMemberJoinEvent` | `joinedLegalEntityId` | Optional. The member joins its token's tenant; when this is present, not empty and names another tenant, the join is refused with `PermissionDenied`. A cyoda-go contract, documented in `cyoda help grpc` and read by the tenant check, which the schema left out; Cloud has the member only on `CalculationMemberGreetEvent`. |
| `EntityDeleteAllRequest` | `pageSize` | No effect: the selection is streamed. Cloud's schema declares it, so Cloud's client may send it. |

Four members Cloud's client sends are refused, because the server has nothing
that would honour them and accepting them would drop their meaning without a
word: `audience` on `POST /oauth/keys/keypair` and on `POST
/oauth/keys/trusted` (key pairs and trusted keys have no audience; see
`signing-key-pairs.md`), `invalidateGracePeriodSec` on `POST
/oauth/keys/trusted` and `gracePeriodSec` on `POST
/oauth/keys/trusted/{keyId}/invalidate` (trusted keys have no grace period, and
the invalidate request has no body; see `trusted-key-tenant.md`). An empty body
or `{}` is accepted there. Cloud is already asked to drop all four.

## Query parameters

A query parameter is read by its exact name too, and an operation accepts only
the query parameters it declares: any other name, another letter case
included, is refused with `400 BAD_REQUEST: unknown query parameter "…"` and
`properties.parameter`. A query string that does not parse is refused whole,
and a parameter other than an array may be given once. This holds on every
operation. Before, an undeclared name was ignored, so
`DELETE /entity/{entityName}/{modelVersion}?PointInTime=…` with no body
deleted every current entity of the model instead of the entities as they
were at that instant. A date-time parameter (`pointInTime`, `fromUtcTime`,
`toUtcTime`) is RFC 3339, read as a body's date-time is; a malformed, empty
or bare-date value is `400 BAD_REQUEST: invalid value for parameter '…'`. The token endpoint declares no query
parameter and refuses any with `400 invalid_request`.

## Why

`encoding/json`, which cyoda-go used before, matched a name to a field ignoring
case and let a later duplicate replace or merge into an earlier one. Two
readers of the same bytes could then disagree about what the client sent. A
workflow import body holding `"transitions"`, `"Transitions"` and
`"TRANSITIONS"` stored a transition sent without `manual` as automatic; a
processor answer whose payload spelt `"Data"` was applied as the entity's data;
a compute answer that misspelt `success` was read as a success.

## Cloud today

Cloud's REST mapper is Jackson 3 under Spring Boot 4.1.1, configured with
`spring.jackson.use-jackson2-defaults=true` (the deployed configs set it). Names
match case sensitively, as here. In that mode Spring Boot's
`JacksonAutoConfiguration` applies `configureForJackson2()` and explicitly
disables `DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES`;
`StreamReadFeature.STRICT_DUPLICATE_DETECTION` is off by default; and
`CaasJsonMapperBuilderCustomizer` enables neither. So an unknown member is
ignored and the last of two duplicate names is kept. `@JsonAlias` on the
operator lets two spellings in one leaf through, the last one winning.

## Cloud obligation

- Refuse an unknown member and a duplicate member name in every client-typed
  body: REST bodies, CloudEvent `data`, compute answers, conditions.
- Refuse a condition leaf that writes its operator twice.
- Refuse a duplicate member name, invalid UTF-8 and a lone surrogate escape in
  an entity payload, a message payload and model-import sample data, and
  anything after model-import sample data.
- Refuse a query parameter an operation does not declare, with 400, on every
  operation; read a date-time parameter as RFC 3339, as a body's date-time is,
  refusing a bare date, an empty value and a comma before the fractional
  seconds.
- Declare `joinedLegalEntityId` on `CalculationMemberJoinEvent` and check it as
  described above, and keep `pageSize` declared on `EntityDeleteAllRequest`
  with no effect.
