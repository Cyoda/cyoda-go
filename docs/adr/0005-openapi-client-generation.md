# 0005. OpenAPI Polymorphism and Client Generation

**Status:** Accepted
**Date:** 2026-10-15

## Context

cyoda-go leads `api/openapi.yaml`, and clients are generated from it. Cyoda
Cloud generates its classes with openapi-generator 7.18.0, generator
`kotlin-spring`; a Java client generated with the `java` generator (library
`native`, Jackson) is the other common consumer. A spec that validates, and that
a server conforms to, can still produce generated code that does not compile, or
that compiles and fails to read the responses the server sends. Nothing in the
build checked this.

The spec carried several polymorphic forms at once: discriminated `oneOf` unions
(inline and named), bases with a discriminator but no mapping, and members that
included more than one base. The state-machine audit event's `data` was an
untyped object. Probes of the generators on prototypes of the spec measured what
each form produces:

- For any `oneOf`, inline or named, `kotlin-spring` generates an interface that
  the member classes do not implement, so every read fails ("Class
  `SimpleConditionDto` not subtype of …"). No generator setting changes it. A
  schema that includes two bases fails the same way. The `java` generator ignores
  a discriminator on a `oneOf`.
- Without a `mapping`, OpenAPI defines a discriminator value as the schema's
  name (`SimpleConditionDto`), which is not what the wire carries.
- For a `oneOf` of scalar types, or a `type` list with more than one non-null
  type, `kotlin-spring` generates a class that cannot hold a scalar ("Cannot
  construct instance of `SimpleConditionDtoAllOfValue`").
- An inline `enum` on a property of a base does not compile: each member's class
  names the enum as its own (`StateMachineAuditEventDto.Severity`), and that
  type does not exist. An inline enum on a member's own property compiles.
- A member that declares its base's discriminator property, for example as a
  one-value `enum`, does not compile ("'type' hides member of supertype").
- `kotlin-spring` makes a required property non-null. An object of a member the
  client does not know is read as `null`, so a required use of a base fails the
  whole response.
- A `kotlin-spring` client cannot read a value that a required response `enum`
  does not list, whatever its settings: the generated `forValue` throws; with
  `READ_UNKNOWN_ENUM_VALUES_AS_NULL` the value becomes `null`, which the
  non-nullable property refuses; the generator's `enumUnknownDefaultCase` option
  adds a case that `forValue` never returns.
- With Jackson's default settings, a field, an audit kind or an event type that
  the client does not know fails the read of the whole page. With
  `FAIL_ON_UNKNOWN_PROPERTIES` and `FAIL_ON_INVALID_SUBTYPE` disabled, both
  generators read all three: the field is ignored, an object of an unknown
  member is read as no object (`null`), and its siblings are read.

ADR 0003 decision 4 requires a discriminated union to carry "an explicit
unknown/default variant". Neither generator can declare such a variant: there is
no schema form that both generators read as "any other value".

The e2e response validator, kin-openapi v0.149.0, differs from JSON Schema in two
ways that matter for the base form: it checks a value against a base's own
properties only and follows a mapping only on a `oneOf`, so `{"type":"nosuch"}`
passes `QueryConditionDto`; and it refuses `null` for an untyped schema, which
JSON Schema accepts.

## Decision

**Rule 1 — polymorphism has one form: a base.** Every polymorphic type is a
schema with a discriminator and a `mapping` that names every member. Each member
includes the base with `allOf`, and every schema that includes the base is in
its mapping. Every use refers to the base with `$ref`. No `oneOf` or `anyOf` has
a discriminator. A schema includes at most one base. This is Cloud's form and the
only one `kotlin-spring` reads. The bases are `QueryConditionDto`,
`AuditEventDto`, `StateMachineEventDto` (the state-machine event's `data`, keyed
on `eventType`) and `ProcessorDefinitionDto`.

**Rule 5 — the client generator reads every shape.** The spec generates Kotlin
(with Cloud's `configOptions`) and Java that compile, and every sample reads into
the right class and writes back unchanged. So:

- no union of scalar types. A value that may be one of several scalar types is an
  untyped schema — none of `type`, `$ref`, `allOf`, `oneOf`, `anyOf` — whose
  other keywords state the rule (for a condition's `value`: `not: {type: object}`,
  `minItems: 2`, `maxItems: 2`, `items: {type: [string, number, boolean]}`);
  `kotlin-spring` generates `Any?` for it. The only `oneOf` or `anyOf` allowed is
  `[{type: "null"}, {$ref}]`, which `kotlin-spring` reads as a nullable
  reference;
- no inline `enum` on a property of a base: such an enum is a named schema
  (`AuditEventSeverity`, `GroupOperator`);
- no member declares its base's discriminator property; the mapping states the
  set of values.

**Rule 6 — the contract grows by addition only.** This applies ADR 0003
decision 4 to the forms of Rule 1; it does not change that decision.

- A field, a member of a base and an event type can be added. A client generated
  from an earlier spec reads every response if it ignores a field it does not
  know (ADR 0003 decision 3) and reads an object whose discriminator value it
  does not know as no object: an audit event of an unknown kind is an empty entry
  in `items`, and the `data` of an event of an unknown type is empty while the
  event's `eventType` is still read. For a Jackson client these are
  `FAIL_ON_UNKNOWN_PROPERTIES` and `FAIL_ON_INVALID_SUBTYPE`, both disabled.
  Decision 4's "unknown/default variant" is that empty object: it is what the
  client reads, not a variant the spec declares.
- A use of a base in a response is never a required property, so an object read
  as empty does not fail its parent. `StateMachineAuditEventDto.data` is not
  required; the server sends it on every event.
- A value set that grows is a plain string, never an `enum`. Every discriminator
  property is a plain string whose known values are its mapping's keys, and so is
  the outer `eventType` of a state-machine audit event. Adding a value to a
  response `enum` is a breaking change, declared as one. The `enum`s that remain
  are value sets not expected to grow.
- Nothing published is renamed or removed: no mapping key, `enum` value or event
  type. A value cyoda-go stops sending stays, and its description says it is not
  sent. A stored audit event therefore always carries a value the contract
  lists, and no stored event is rewritten.
- The server sends no event type the mapping lacks: the set of
  `spi.StateMachineEventType` constants declared in the SPI and in cyoda-go
  equals the keys of `StateMachineEventDto`'s mapping.

**Enforcement.**

- *Spec tests* (`api/spec_rules_test.go`, `api/event_schemas_test.go`) check
  Rules 1, 5 and 6 statically on the embedded spec, including the walk of every
  schema a response reaches for a required use of a base, and the event-type
  constants against the mapping.
- *The validator view* (`internal/e2e/openapivalidator`, `ApplyView`). The e2e
  validator checks responses against a view of the spec built once at load; the
  published spec does not change. Each use of a base becomes a `oneOf` of the
  mapping's targets carrying the base's discriminator, so kin checks the member
  the discriminator selects and refuses an unmapped value. Each untyped schema
  whose `not` does not exclude `null` is marked nullable, and so is each
  `[{type: "null"}, {$ref}]` union. No other schema is marked: a member, a
  replaced use and a typed schema keep refusing `null`. kin accepts any value at
  a position where a schema recurs (a group's `conditions`, a function's
  pre-check), so the validator's recursive walk checks those values with a fresh
  visit. `ValidateValue` runs kin and the same walk; the `api/` tests validate
  the spec's examples and the wire samples with it, so samples, examples and
  responses are checked the same way.
- *The client-generation check* (`api/clientprobe/`, CI job
  `openapi-client-generation.yml`, `make client-probe` locally). A Maven project
  runs openapi-generator 7.18.0 twice — `kotlin-spring` with Cloud's
  `configOptions`, and `java` (library `native`, Jackson,
  `containerDefaultToNull=true`) — compiles both, and runs one JUnit test per
  generator over the samples in `api/clientprobe/samples/`. A valid sample is
  read as its schema's class, the concrete class is asserted, and the value
  written back equals the sample with `null` object members removed. A future
  sample — an audit event of an unknown kind, an event of an unknown type, a
  known event with added fields — is read with the two settings and must give
  the result Rule 6 states, and read without them it must fail with the
  exception of its case. An `api/` spec test validates every sample that is not
  a future sample against its schema through the view, and checks that every
  mapping entry of every base has a sample. The check is a merge gate. It runs on
  every pull request and every push to `main` and `release/**`, with no path
  filter.
- *The severity file* (`.github/oasdiff-severity-levels.txt`). oasdiff v1.21.0
  rates the changes Rule 6 forbids below `error`, so the gate (`--fail-on ERR`)
  would pass them: adding a value to a response `enum` is a warning, and removing
  a mapping key or a response `enum` value is info. The file raises to `err`:
  `response-property-enum-value-added`; the removal of a response or request
  `enum` value; the deletion of a discriminator mapping key; and a change of a
  discriminator property's name. A new mapping key stays `info`: it is an
  addition. `internal/oasdiffcheck` tests the premise: an added mapping key
  passes the gate, a deleted one fails it, and an added response `enum` value
  fails it.

## Consequences

**Positive.** Cloud's generator, with Cloud's `configOptions`, and the Java
generator read every shape the spec declares; a spec change that either cannot
read fails cyoda-go's merge gate rather than Cloud's build. A new event type, audit kind or field reaches a client
generated earlier without breaking it. The validator checks a member's fields
under a use of a base and refuses an unmapped discriminator value.

**Negative / cost.** Polymorphic values are declared in one, more verbose form,
and value schemas are untyped schemas with constraints rather than unions. The
generated create and import body parameters are `Any`/`Object`, so the check
proves only that they compile. A client must disable two Jackson settings; with
the defaults it fails on the first addition. Growing a response `enum` is no
longer an additive change, so a value set that may grow must be a plain string
from the start. CI needs Java 21 and Maven for the check; `make test` does not.

**The check copies Cloud's `configOptions` only.** Cloud's `client/pom.xml` also
sets `modelNameSuffix` `Dto`, maps `object` and `any` to Jackson 3's `JsonNode`
(`typeMappings`, with `importMappings` to `tools.jackson.databind.JsonNode`), and
replaces some generated classes with others (`importMappings`,
`schemaMappings`): `ProcessorDefinitionDto` is a hand-written interface that
reads a definition with no `type` as externalized, and `QueryConditionDto` is
the class Cloud generates once from its own `openapi-common.yml`. The check sets
none of these, so it does not show what Cloud's mappings make of cyoda-go's
spec: an untyped schema is `Any?` in the probe's Kotlin, where Cloud's `type:
any` is a `JsonNode`, and the probe reads `ProcessorDefinitionDto` and
`QueryConditionDto` into the classes it generates from cyoda-go's spec.

**Neutral.** The published spec is unchanged by the validator view. Other client
generators (TypeScript, Python, C#) are not probed.

## Alternatives considered

- **Keep discriminated `oneOf` unions.** `kotlin-spring` cannot read them in any
  setting, and Cloud generates its classes with it.
- **Declare an explicit unknown/default variant**, as ADR 0003 decision 4 words
  it. Neither generator can express one; the empty object a client reads under
  Rule 6 fills the role.
- **Keep the outer `eventType` and the discriminators as `enum`s** and rely on
  `READ_UNKNOWN_ENUM_VALUES_AS_NULL` or `enumUnknownDefaultCase`. Measured: the
  first yields `null` in a non-nullable property, and the second adds a case that
  `forValue` never returns.
- **Make `data` a required property.** A client would fail the whole page on an
  event of a type it does not know.
- **Validate with kin-openapi alone.** It passes an unmapped discriminator value
  and a member's field defects under a use of a base, and refuses `null` where
  JSON Schema accepts it.
- **Rely on oasdiff's default severities.** They pass a grown response `enum`
  and a removed mapping key.
