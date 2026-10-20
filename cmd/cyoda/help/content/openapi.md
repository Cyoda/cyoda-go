---
topic: openapi
title: "openapi — OpenAPI 3 spec and discovery endpoints"
stability: stable
see_also:
  - crud
  - models
  - search
  - workflows
  - errors
  - config.auth
  - cli.serve
---

# openapi

## NAME

openapi — OpenAPI 3.1 specification and discovery endpoints for the cyoda-go REST API.

## SYNOPSIS

```
GET /openapi.json
GET /docs
GET {CYODA_CONTEXT_PATH}/help
GET {CYODA_CONTEXT_PATH}/help/{topic}
```

Discovery endpoints are served at the root of the HTTP listener (port `CYODA_HTTP_PORT`, default 8080). They are mounted outside `CYODA_CONTEXT_PATH` — no prefix is applied.

## DESCRIPTION

cyoda-go generates its OpenAPI 3.1 specification from the embedded `api/openapi.yaml` file compiled into the binary at build time. The spec is served at `/openapi.json` with runtime-patched server URLs. The Scalar API Reference UI is served at `/docs` and loads the spec from `/openapi.json`.

The `/openapi.json` and `/docs` endpoints require no authentication. The help endpoints (`/help`, `/help/{topic}`) also require no authentication.

## DISCOVERY ENDPOINTS

**GET /openapi.json**

Returns the OpenAPI 3.1 specification as JSON. The `servers` array is overridden at request time to reflect the actual runtime host and `CYODA_CONTEXT_PATH`. The scheme (`http` or `https`) is derived from whether the incoming connection is TLS.

Response: `200 OK`, `application/json`.

The spec itself sets `info.version` to `"1.0"` (a fixed spec version, not the binary version). The binary version is reported separately in the `GET /help` payload under the `version` field.

**GET /docs**

Returns an HTML page embedding the Scalar API Reference UI. The UI loads the spec from `/openapi.json` and provides an interactive browser. No JavaScript execution is required on the server side — the HTML page references the Scalar CDN script.

Response: `200 OK`, `text/html; charset=utf-8`.

**GET {CYODA_CONTEXT_PATH}/help**

Returns the full help topic tree as JSON. The response includes the binary version string and an array of all topic descriptors.

Response: `200 OK`, `application/json`:

```json
{
  "schema": 1,
  "version": "dev",
  "topics": [
    {
      "topic": "quickstart",
      "title": "cyoda quickstart — minimum invocations",
      "stability": "stable",
      "tagline": "minimum commands to run a cyoda-go server.",
      "see_also": ["cli", "config"]
    }
  ]
}
```

**GET {CYODA_CONTEXT_PATH}/help/{topic}**

Returns a single help topic descriptor by dotted path. The `topic` path segment uses dots as separators (e.g. `config.database`, `errors.MODEL_NOT_FOUND`). Topics are resolved via the embedded help tree.

Response: `200 OK`, `application/json` — same shape as a single element of the `topics` array above, with the addition of a `body` field containing the full Markdown content.

`404 HELP_TOPIC_NOT_FOUND` when the topic path does not exist.
`400 BAD_REQUEST` when the topic path contains disallowed characters (only `A-Za-z0-9`, `.`, `_`, `-` are allowed; no leading/trailing dots or hyphens).

## SPEC SHAPE

The spec declares 68 paths across these tag groups:

- **Entity Management** — create, update, delete, transition, stats and consistency-time endpoints under `/entity/`
- **Entity Model** — model import, export, lock, unlock, delete, changeLevel, and workflow under `/model/`
- **Search** — snapshot and direct search under `/search/`
- **User, Account** — account info and subscriptions under `/account/`
- **User, Machine** — M2M client management under `/clients/`
- **Entity, Audit** — audit log retrieval under `/audit/`
- **Scheduled Tasks** — the tenant's scheduled-transition tasks under `/scheduled-tasks`
- **Messaging** — message CRUD under `/message/`
- **IAM** — OAuth token and key management under `/oauth/`. As of v0.8.0, the 10 `/oauth/keys/*` admin operations (keypair + trusted-key lifecycle) and the 4 `/clients` operations are conformant. Remaining stubbed surface across `/account/*` and adjacent paths is tracked in `docs/cyoda/cloud-divergences.md`.
- **SQL Schema** — SQL schema generation and management under `/sql/schema/` (excluded from cyoda-go — see below)
- **Platform API** — stream-data operations under `/platform-api/stream-data/` (excluded from cyoda-go — see below)

The **Stream Data** and **SQL-Schema** tag groups (22 operations) are excluded from the cyoda-go
shipped API via `api/config.yaml`'s `exclude-tags` list. These operations are not served by
cyoda-go and are filtered from the generated `ServerInterface` and from E2E coverage tracking.

Named schemas added or refined since the initial spec import:

- `Envelope` — entity get/list response wrapper `{type, data, meta}`
- `EdgeMessagePayload` — polymorphic message content schema
- `MessageDeleteResponse` — single-message delete response `{entityIds: []string}`
- `MessageDeleteBatchResponse` — batch message delete response `{entityIds, success}`
- `TransitionNameList` — transitions query response (array of strings)
- `WorkflowImportSuccessDto` — workflow import 200 response `{success: bool}`
- `AuditEventDto` — the base of the audit events, discriminated by `auditEventType`: `EntityChangeAuditEventDto`, `StateMachineAuditEventDto`, `SystemAuditEventDto`
- `StateMachineEventDto` — the base of a state-machine event's `data`, discriminated by `eventType`, with one member per event type (see `cyoda help audit`)
- `QueryConditionDto` — the base of every condition, discriminated by `type`: `SimpleConditionDto`, `LifecycleConditionDto`, `ArrayConditionDto`, `GroupConditionDto`, `FunctionConditionDto`

Every polymorphic type has one form: a **base** schema with a discriminator and a `mapping` that names each member, each member including the base with `allOf`, and every use of the type a `$ref` to the base. No `oneOf` or `anyOf` carries a discriminator.

All paths in the spec are relative to the `servers[0].url`, which is set at runtime to `{scheme}://{host}{CYODA_CONTEXT_PATH}`. The default context path is `/api`, so `GET /entity/{entityId}` is served at `http://localhost:8080/api/entity/{entityId}`.

## REQUEST BODIES

A request body spells its member names exactly as the spec declares them, each once. A name in another letter case is not that member, a member the operation's schema does not declare is refused, a member written twice is refused, and so is anything after the body's JSON value. The refusal is `400` with the operation's code, and its detail names the member and where it is, as a JSON Pointer (text that is not JSON is located by its byte offset): `invalid JSON at "/workflows/0": unknown member name "Name"`. Where a body also carries text to store — workflow import, the entity writes and model validate — a member written twice is found first by the check for text no store can hold: `400 BAD_REQUEST`, `field "…" appears more than once…`. The rule covers every typed body and every condition. Two condition refusals come from the condition parser and are named in words, without a pointer: a condition whose `type` is written in another case has none (`the condition has no "type" member`), and an operator written twice is named by its two spellings and the clause's place. A criterion refused at workflow import is `400 VALIDATION_FAILED`, naming the workflow, and for a transition criterion its state and transition, in words, then the location inside the criterion as a pointer from the criterion's top. Invalid UTF-8 and a lone surrogate escape in a string are refused too. The contents of an entity or message payload, and model-import sample data, are your data: no member there is unknown, and `"a"` and `"A"` are two fields, but a member written twice in one object is refused there as well. `cyoda help cloudevents` gives the same rule for the gRPC events.

## QUERY PARAMETERS

A query parameter is read by its exact name, as a body member is, and an operation accepts only the parameters it declares: any other name, another letter case included, is refused with `400 BAD_REQUEST: unknown query parameter "…"` and `properties.parameter`, so a misspelt parameter never silently changes a request. A date-time parameter (`pointInTime`, `fromUtcTime`, `toUtcTime`) is RFC 3339, read as a body's date-time is: fractional seconds after `.` only, a two-digit hour, a bounded offset, and never a bare date or an empty value; every repeated value is checked. A malformed value is `400 BAD_REQUEST: invalid value for parameter '…'`. The token endpoint takes its parameters in the form body only (see `cyoda help auth tokens`).

## AUTHENTICATION

The spec declares two security schemes:

```yaml
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      description: >-
        Authorization header: `Bearer <access_token>`
    basicAuth:
      type: http
      scheme: basic
```

Global security is applied to all operations: `security: [{bearerAuth: []}]`. The `basicAuth` scheme is declared for spec validity — it is referenced by Platform API (Stream Data) operations that are out of scope for the cyoda-go server. Discovery endpoints (`/openapi.json`, `/docs`) and help endpoints are not part of the spec and carry no security requirement.

When `CYODA_IAM_MODE=mock`, the server accepts requests without a token. When `CYODA_IAM_MODE=jwt`, a valid JWT Bearer token is required on all protected endpoints.

## CONFORMANCE VALIDATOR

The E2E test suite (`internal/e2e/`) includes an OpenAPI conformance validator
(`internal/e2e/openapivalidator/`) that runs in the background during every E2E
test run. For each HTTP response the in-process test server produces, the
middleware captures the status code and body and validates them against the
spec's declared schema for that operation.

A mismatch always fails the run. Mismatches are collected and written to
`internal/e2e/_openapi-conformance-report.md` after the suite completes, and
`TestOpenAPIConformanceReport`, which runs last, fails when any remain. In a
`-run`-filtered E2E run, a mismatch also fails the test that made the request.
A mismatch that no test reported — one on a request that named no test, or one
that arrived after its test returned — fails the run through
`TestOpenAPIConformanceReport` when it runs, otherwise when `TestMain` ends.

The validator checks responses against a view of the spec that gives JSON Schema's answers where kin-openapi, the validating library, differs: each use of a base is checked against the member its discriminator selects, and an untyped value (a condition's `value`, for example) accepts `null` unless its schema excludes it. The published spec does not change.

The conformance layer also tracks which operationIds are exercised across the
E2E suite. An operation that appears in the spec with no E2E coverage fails the
full suite unless it carries an `x-cyoda-status` marker, and a marked operation
that answers 2xx fails it too. See ADR 0003 in `docs/adr/` for the design
rationale.

## GENERATED CLIENTS

A client generated from this spec keeps working as the spec grows, if it does what the contract requires of a client.

**What the contract promises.** The contract grows by addition only. A field, a member of a base and an event type can be added in a later release. Nothing published is renamed or removed: not a mapping key, not an `enum` value, not an event type. A value cyoda-go stops sending stays in the spec, and its description says it is not sent.

**What a client must do.** A client generated from an earlier spec reads every later response, if:

- it ignores a field it does not know;
- it reads an object whose discriminator value it does not know as no object. An audit event of an unknown kind is an empty entry (`null`) in `items`. An event of an unknown type is read with its `eventType`, and its `data` is empty (`null`).

For a Jackson client these are two settings, both disabled: `DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES` and `DeserializationFeature.FAIL_ON_INVALID_SUBTYPE`. Spring Boot's mapper and the mapper of openapi-generator's `java` client already disable the first. With Jackson's defaults, one unknown field, audit kind or event type fails the read of the whole page.

**Value sets that grow.** A value set that grows is a plain string, not an `enum`: every discriminator property, and the state-machine event's outer `eventType`, whose known values are the keys of the mapping of `StateMachineEventDto`. A client accepts a value of such a string that it does not know. An `enum` in a response, such as `severity` or the stop-reason `type`, is a value set that is not expected to grow. A generated client cannot read a value that a response `enum` does not list, so adding a value to a response `enum` is a breaking change, and a release that makes one declares it as breaking.

**A use of a base is never required in a response.** `StateMachineAuditEventDto.data` is not in `required`, so that a client can read the event when its `data` is of an unknown type. The server sends `data` on every event.

cyoda-go's CI generates clients from this spec with openapi-generator 7.18.0 — `kotlin-spring` and `java` (library `native`, Jackson) — and checks that each reads and writes back a set of wire samples, and reads responses of a later spec as described above.

## CONTEXT PATH

`CYODA_CONTEXT_PATH` (default `/api`) is the prefix applied to all API routes. It affects:

- The `servers[0].url` field in the served `/openapi.json` response.
- All route registrations for entity, model, search, audit, messaging, OAuth, and SQL endpoints.
- The help route prefix (`{CYODA_CONTEXT_PATH}/help` and `{CYODA_CONTEXT_PATH}/help/{topic}`).

The discovery routes `/openapi.json` and `/docs` are always at the root — `CYODA_CONTEXT_PATH` does not prefix them.

Setting `CYODA_CONTEXT_PATH=` (empty string) mounts all routes at root with no prefix.

## VERSIONING

The spec `info.version` field is `"1.0"` — a fixed spec format version. It does not track the binary version.

The binary version is injected at build time via `-ldflags` and reported in:
- The startup banner printed to stderr.
- The `GET {CYODA_CONTEXT_PATH}/help` JSON payload's `version` field.

## ERRORS

The REST API uses `application/problem+json` (RFC 9457 Problem Details) error responses. See `errors` for the canonical shape and the full error code catalogue.

Error response shape (4xx example):

```json
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "MODEL_NOT_FOUND: model nobel-prize:1 not found",
  "instance": "/api/model/nobel-prize/1",
  "properties": {
    "errorCode": "MODEL_NOT_FOUND",
    "retryable": false
  }
}
```

Error response shape (5xx example):

```json
{
  "type": "about:blank",
  "title": "Internal Server Error",
  "status": 500,
  "detail": "SERVER_ERROR: internal error [ticket: 3fa85f64-5717-4562-b3fc-2c963f66afa6]",
  "instance": "/api/entity/abc",
  "ticket": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "properties": {
    "errorCode": "SERVER_ERROR"
  }
}
```

`CYODA_ERROR_RESPONSE_MODE=sanitized` (default) suppresses internal detail from 5xx `detail` fields. `CYODA_ERROR_RESPONSE_MODE=verbose` includes full error detail — for development environments only.

## EXAMPLES

**Fetch the OpenAPI spec:**

```
curl -s http://localhost:8080/openapi.json | jq '.info'
```

**Fetch with custom context path:**

```
CYODA_CONTEXT_PATH=/v1 \
curl -s http://localhost:8080/openapi.json | jq '.servers'
```

Response:
```json
[{"url": "http://localhost:8080/v1"}]
```

**Open the interactive UI:**

```
curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/docs
```

**Fetch the full help topic tree:**

```
curl -s http://localhost:8080/api/help | jq '.topics[].topic'
```

**Fetch a specific help topic:**

```
curl -s http://localhost:8080/api/help/models | jq '.title'
```

**Fetch the spec from a Docker container:**

```
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -e CYODA_HTTP_BIND_ADDRESS=0.0.0.0 \
  -e CYODA_STORAGE_BACKEND=memory \
  ghcr.io/cyoda/cyoda:latest &
sleep 2
curl -s http://localhost:8080/openapi.json | jq '.paths | keys | length'
```

## ACTION DETAILS

- `cyoda help openapi json` — emit the embedded OpenAPI spec as JSON to stdout
- `cyoda help openapi yaml` — emit the embedded OpenAPI spec as YAML to stdout
- `cyoda help openapi tags` — list every tag in the spec as `<slug>  <canonical name>` pairs (tabular, sorted by slug)
- `cyoda help openapi <slug>` — emit a standalone OpenAPI 3.1 document scoped to the named tag; paths filtered to operations carrying that tag, components pruned to only the transitively-referenced members. Pass `--format=yaml` to emit YAML instead of the default JSON. Discover valid slugs via `cyoda help openapi tags`. Unknown slugs exit 2 with the full valid-slug list in the error.

The emitted spec is the binary's compile-time baseline. The `servers` array reflects whatever is embedded at build time; the running server's HTTP endpoint (`GET /openapi.json`) rewrites `servers` per request, but the CLI does not.

Per-tag filtering is useful for AI agents and downstream tooling that only need the slice of the API corresponding to one concern (e.g. just `entity-management` or just `search`) — the pruned spec is typically 3-5× smaller than the full spec and is still a valid standalone OpenAPI 3.1 document (every `$ref` resolves within its own `components`).

## SEE ALSO

- crud
- models
- search
- workflows
- errors
- config.auth
- cli.serve
