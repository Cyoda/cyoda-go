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
criterion at workflow import). gRPC answers `InvalidArgument`, except on an
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
doors. In a compute member's processor answer they make the answer unreadable,
which fails the callout; before, the payload was read and then refused by the
unstorable-payload check naming the field. `encoding/json` replaced invalid
text with U+FFFD; in a typed string it is refused too.

Two schema members are declared:

| Event | Member | Meaning |
|---|---|---|
| `CalculationMemberJoinEvent` | `joinedLegalEntityId` | Optional. The member joins its token's tenant; when this is present and names another tenant, the join is refused with `PermissionDenied`. A cyoda-go contract, documented in `cyoda help grpc` and read by the tenant check, which the schema left out; Cloud has the member only on `CalculationMemberGreetEvent`. |
| `EntityDeleteAllRequest` | `pageSize` | No effect: the selection is streamed. Cloud's schema declares it, so Cloud's client may send it. |

Three members Cloud's client sends are refused, because the server has nothing
that would honour them and accepting them would drop their meaning without a
word: `audience` on `POST /oauth/keys/keypair` and on `POST
/oauth/keys/trusted` (key pairs and trusted keys have no audience; see
`signing-key-pairs.md`), and `invalidateGracePeriodSec` on `POST
/oauth/keys/trusted` (trusted keys have no grace period; see
`trusted-key-tenant.md`). Cloud is already asked to drop all three there.

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
  an entity payload, a message payload and model-import sample data.
- Declare `joinedLegalEntityId` on `CalculationMemberJoinEvent` and check it as
  described above, and keep `pageSize` declared on `EntityDeleteAllRequest`
  with no effect.
