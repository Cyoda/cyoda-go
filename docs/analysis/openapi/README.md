# OpenAPI Contract Drift — Findings & Scale

**Status:** analysis / point-of-reference. Not a plan. The remediation approach is a
separate brainstorming + spec effort; this document exists only to size the problem.

**Date:** 2026-07-02
**Scope:** all 87 HTTP operations in `api/openapi.yaml`, audited against the actual Go
handler behaviour.

---

## 1. Why this document exists

`api/openapi.yaml` is a **deliberate design choice**: a definitive, static, explicitly
maintained contract that is the *common ground* between `cyoda-go` and `cyoda-cloud`.
Both sides are meant to conform to it. It is intentionally **not** generated from Go
types — the whole point is that the contract is authored and owned, not derived.

The consequence of that choice is that nothing mechanically holds either side to the
contract. Over time the spec and the `cyoda-go` server have drifted, in **both
directions**, and the drift is now large enough that an LLM (or a human) generating a
client from the published spec reaches materially wrong conclusions — the trigger for
this audit was an app-builder concluding, from `GET /entity/{entityId}`, that *there is
no way to learn the entity model*, because the `meta` object is documented as an opaque
bag with a stale field list.

This report is the reference for **how big the mess is** before we design the way out.

## 2. The key reframe: drift is bidirectional

Because the spec is the agreed contract (not a mirror of the server), a divergence is a
defect on **whichever side left the contract**. Every finding therefore carries a
**reconciliation direction**, and this is the crux of why a mechanical "make the spec
match the server" pass would be *wrong*:

| Direction | Meaning | Correct fix |
|---|---|---|
| **`spec-stale`** | Server evolved correctly; spec still describes the old shape. | Update the spec. |
| **`spec-incomplete`** | Server behaviour is right and intended; spec under-describes it (missing fields, missing real error codes). | Enrich the spec. |
| **`server-gap`** | Spec expresses the intended contract; the server never implemented it (or implemented something else). | Fix the **server**. |
| **`needs-decision`** | Genuine ambiguity about what the contract *should* be; neither side is self-evidently right. | Decide intent, then fix the losing side. |

The single most important observation for the brainstorming: **~a third of the findings
are `server-gap` or `needs-decision`**, not `spec-stale`. The spec is often *more*
correct about intent than the server. This is not a documentation-cleanup task.

## 3. Root cause

- `api/openapi.yaml` was imported wholesale from the upstream `cyoda-light-go` prototype
  (commit `d1f6875`, "Initial import from cyoda-light-go") and has been hand-maintained
  and `//go:embed`'d ever since (`embedded-spec: false` in `api/config.yaml` — see the
  existing note on why native embed is not used).
- Server responses are frequently built as inline `map[string]any` literals with **no Go
  struct behind them**, so there is no type the spec could even be checked against.
- Codegen (`oapi-codegen` → `api/generated.go`) covers only request/response *models* and
  the router interface; it does **not** verify that handlers populate those models, and
  `api/config.yaml` **excludes three whole tags** from generation (below) while the spec
  continues to publish them.
- The one prior reconciliation pass (commit `9c721b4`, "reconcile spec with server") was
  scoped to a handful of confirmed defects. It fixed the `Envelope` wrapper but left
  `meta` as an open bag and **copied the prototype's stale field list** (`previousTransition`)
  into the description — a microcosm of the whole problem.

Net: there is no enforced binding in **either** direction. Drift is structural, not
accidental, and will recur after any manual cleanup unless the binding is added.

## 4. Scale at a glance

| Metric | Value |
|---|---|
| HTTP operations audited | **87** |
| Operations with ≥1 finding | ~55 |
| **Dead operations** (published but unrouted → 404 at runtime) | **22** (25%) |
| High-severity findings | ~20 |
| Medium-severity findings | ~30 |
| Low-severity findings | ~12 |
| Findings requiring a **contract decision** (`server-gap` / `needs-decision`) | ~1/3 of total |
| Data-loss-class defects | **1** (`deleteEntities`) |

**Dead tags** (excluded from codegen in `api/config.yaml`, still shipped in the spec):

| Tag | Ops | Reality |
|---|---|---|
| `Stream Data` | 13 | Excluded from codegen; no route; 404 at runtime. |
| `SQL-Schema` | 9 | Excluded from codegen; no route; 404 at runtime. |
| `CQL Execution Statistics` | 0 | Vestigial exclude entry — no ops currently carry the tag. |

## 5. The drift taxonomy (six recurring patterns)

### P1 — Dead / unwired spec surface  (`needs-decision`)
22 operations are published in the contract but excluded from codegen and unrouted, so
they 404. Also here: `accountSubscriptionsGet` documents a `200` shape but the handler
always returns `501`. **Decision required:** are these part of the `cyoda-go` ↔ Cloud
contract at all? Either implement them or remove them from the published contract; do
not leave a quarter of the surface as advertised-but-absent.

### P2 — Advertised-but-unwired request semantics  (`server-gap` — the dangerous class)
The spec declares parameters/bodies expressing real intent that the handler never reads.
- 🔴 **`deleteEntities` — data loss.** Contract: delete only entities matching an
  `AbstractConditionDto` body (+ `pointInTime`, `verbose`). Handler ignores the body and
  params entirely and calls `DeleteAllEntities` → a client asking to delete a filtered
  subset **silently wipes the whole model**.
- **`getAllEntities`**, **`getAsyncSearchResults`** ignore `pointInTime` → "as-at"
  queries silently return *current* data (same class as the already-fixed
  `getOneEntity` `transactionId` bug).
- Message `transactionTimeoutMillis` / `transactionSize` are inert.

### P3 — Loose `additionalProperties:true` response bags hiding real fields  (`spec-incomplete`)

> **Update 2026-07-03 (fix policy — see ADR 0003 + `schema-strictness-research.md`):** the fix
> is **typed-but-open**, NOT `additionalProperties: false`. Enumerate the known properties so
> consumers get the shape, but leave the object open so additive fields stay non-breaking.
> Two consequences for this list: (1) genuinely-open values — entity `data`, `JsonNode`, and
> RFC 9457 `ProblemDetail.properties` extension bags — are **correct as open** and come *off*
> the tightenable set; (2) the state-machine audit-event union is tightened as a discriminated
> `oneOf` + unknown/default variant + open-enum discriminator, not a sealed object. "Tighten"
> below means "enumerate + keep open," never "seal."

The class that triggered this audit.
- **`Envelope.meta`** — documented as an opaque object; actually carries
  `modelKey{name,version}`, `state`, `creationDate`, `lastUpdateTime`, `transactionId`,
  and (conditionally) `transitionForLatestSave`. The described field `previousTransition`
  **does not exist**.
- **`exportMetadata`** hides a real top-level `uniqueKeys` array.
- **Message `metaData`** — server emits a flat map, not the typed `ValueMaps` buckets.

### P4 — Stale fossil names / over-promising enums  (`spec-stale`)
Inherited from the prototype and never trued-up:
`previousTransition` → `transitionForLatestSave`; `EntityChangeMeta.fieldsChangedCount`
(never emitted); trusted-key "EC / OKP supported" (RS256-only); multi-algorithm keypair
enums (RS256-only); `issued_token_type=access_token` (server sends `...jwt`); import
`converter` enum listing `JSON_SCHEMA` / `SIMPLE_VIEW` (only `SAMPLE_DATA` accepted).

### P5 — Conflated error envelopes + systematically missing error codes  (mixed)
- **Envelope family mismatch** (`needs-decision`): the entire OIDC-provider group is
  speced with the OAuth `application/json` `ErrorResponseDto` (`{error, error_description}`),
  but the server emits RFC-9457 `application/problem+json` `ProblemDetail` with the code
  in `properties.errorCode`. Only the genuine `/oauth/token` endpoint actually uses the
  OAuth shape. A client built to the spec cannot parse OIDC errors.
- **Missing real error codes** (`spec-incomplete`): documented-and-emitted `409`/`422`/
  `404`/`400` are omitted across nearly every model-management op (lock/unlock/delete/
  import/unique-keys), plus `501` everywhere in mock-IAM mode.
- **Fictional error codes** (`needs-decision`): stats/search document `404 model not
  found` and a `408` timeout the handlers never emit — unknown model returns `200` +
  empty. Should the server validate existence, or should the contract drop the 404?

### P6 — Mis-typed request bodies + stale prose defaults  (mixed)
- Request bodies typed as bare `object` / `string` that erase the real shape
  (`createCollection`, `create`, `deleteMessages`, `newMessage`, `updateTables`) — codegen
  clients send the wrong body type and get 400s.
- Prose defaults that lie: `getAsyncSearchResults` documents default page size **10**;
  server default is **1000** (100×). `issueJwtKeyPair` says algorithm "defaults to RS256";
  it is actually **required**.
- Malformed schema constructs: `type: array` with a sibling `$ref` (no `items`) erasing
  item typing (`genTables`, `getSchemas`), and examples that violate their own declared
  schema (`FieldConfigDto` uses a non-existent `arrayFields`, omits required `hidden`).

## 6. Full findings by area

Severity: **High** = wrong shape/type, data loss, or advertises non-existent behaviour
(misleads codegen or endangers clients). **Medium** = missing real fields / missing or
fictional error codes / silently-ignored params. **Low** = example or cosmetic drift.

Direction: `spec-stale` · `spec-incomplete` · `server-gap` · `needs-decision`.

### 6A — Entity CRUD & envelope  (`internal/domain/entity/*`)

| Op | Sev | Dir | Pattern | Detail |
|---|---|---|---|---|
| deleteEntities | High | server-gap | P2 | Condition body + `pointInTime` + `verbose` ignored; `DeleteAllEntities` wipes whole model (`handler.go:592-611`, `service.go:748`). **Data loss.** |
| getAllEntities | High | server-gap | P2 | `pointInTime` never read by the handler / not plumbed into `ListEntities` (`handler.go:613-654`, `service.go:825`). |
| getOneEntity, getAllEntities | High | spec-stale + spec-incomplete | P3/P4 | `Envelope.meta` opaque bag; desc names fossil `previousTransition`; server emits `modelKey`/`transactionId`/`lastUpdateTime`/`transitionForLatestSave` (`service.go:433-446`, `875-884`; `openapi.yaml:7913-7917`). |
| createCollection | High | spec-incomplete | P6 | Request body `type:object` empty; server requires array of `{model{name,version}, payload:<json-string>}` (`handler.go:797-816`). |
| getEntityChangesMetadata | Med | spec-stale | P4 | `EntityChangeMeta.fieldsChangedCount` advertised, never emitted (`handler.go:576-587`). |
| getEntityChangesMetadata | Med | spec-stale | — | Desc says chronological/ascending; server sorts newest-first (`service.go:719-722`). |
| create, createCollection | Med | spec-incomplete | P5 | Composite-unique-key `409 UNIQUE_VIOLATION` / `422 INVALID_UNIQUE_KEY[_DEFINITION]` emitted but undocumented; `createCollection` omits the `409` its sibling `updateCollection` documents (`service.go:1762-1767`). |
| updateSingle(+Loopback), patchSingle(+Loopback), updateCollection | Med | spec-incomplete | P5 | Same shared save path can emit `422` unique-key codes; none listed. |
| create | Med | spec-incomplete | P6 | `type:object` body hides accepted array/batch form (`handler.go:372-410`). |
| getOneEntity | Low | spec-incomplete | P3 | 200 example shows only `{id,state}`. |
| getEntityChangesMetadata | Low | spec-stale | — | Prose says `CREATE/UPDATE/DELETE`; enum & server use `CREATED/UPDATED/DELETED`. |
| deleteSingleEntity, getEntityTransitions, fetchEntityTransitions | — | clean | — | Shapes & error paths match. |

### 6B — Stats / audit / search  (`internal/domain/{entity,audit,search}/*`)

| Op | Sev | Dir | Pattern | Detail |
|---|---|---|---|---|
| searchEntities | High | needs-decision | P2 | `timeoutMillis` + `408` documented; handler never reads/emits them (`search/handler.go:105-191`). |
| searchEntities | High | needs-decision | — | Spec: result "silently limited to 10000"; server **hard-400s** on `limit>10000` (`search/handler.go:150-153`). |
| getEntityStatisticsForModel, ...ByStateForModel | Med | needs-decision | P5 | Documented `404 model not found`; unknown model returns `200`+`0`/empty (`entity/service.go:554-612`). |
| submitAsyncSearchJob, searchEntities | Med | needs-decision | P5 | Documented `404 model not found`; unknown model → job/stream over empty population (`search/service.go:122-199`). |
| getStateMachineFinishedEvent | Med | server-gap | — | Desc/`400` require time-based v1 UUIDs; handler does no version check (`audit/handler.go:231-270`). |
| getAsyncSearchResults | Med | server-gap | P2 | `pointInTime` never read (`search/handler.go:274-316`). |
| getAsyncSearchResults | Med | needs-decision | P6 | Documented default page size 10; server default 1000 (`search/handler.go:277`). |
| searchEntities | Med | spec-stale | P6 | 200 offers `application/json`; server always sends `application/x-ndjson` (`search/handler.go:178`). |
| searchEntityAuditEvents | Med | server-gap | P3 | `EntityChangeAuditEventDto.changes` before/after diff never emitted (`audit/handler.go:70-97`). |
| searchEntities | Low | spec-stale | — | Error bodies advertise ndjson variant; errors are `problem+json` only. |
| getAsyncSearchStatus | Low | spec-stale | — | `searchJobStatus` enum includes `NOT_FOUND`, unreachable (404 instead). |
| searchEntityAuditEvents | Low | spec-stale | — | `eventType` enum includes `System`; no System source exists. |
| getEntityStatistics, ...ByState, queryGroupedEntityStatisticsForModel, cancelAsyncSearch, getAsyncSearchStatus (shape) | — | clean | — | Generated-DTO or purpose-built; accurate. |

### 6C — Entity-model & workflow  (`internal/domain/{model,workflow}/*`)

| Op | Sev | Dir | Pattern | Detail |
|---|---|---|---|---|
| deleteEntityModel | High | spec-incomplete | P5 | `409 MODEL_HAS_ENTITIES` emitted, undocumented (`service.go:393-404`). |
| unlockEntityModel | High | spec-incomplete | P5 | `409 MODEL_ALREADY_UNLOCKED` / `409 MODEL_HAS_ENTITIES` undocumented (`service.go:318-352`). |
| lockEntityModel | High | spec-incomplete | P5 | `409 MODEL_ALREADY_LOCKED` undocumented (`service.go:274-286`). |
| setEntityModelUniqueKeys | High | spec-incomplete | P5 | `404 MODEL_NOT_FOUND` undocumented (`service.go:563-569`). |
| importEntityModel | High | spec-incomplete | P5 | `409 MODEL_ALREADY_LOCKED` undocumented (`service.go:115-125`). |
| importEntityModel | Med | needs-decision | P4 | `converter` enum lists `JSON_SCHEMA`/`SIMPLE_VIEW`; only `SAMPLE_DATA` accepted (`service.go:94-96`). |
| setEntityModelChangeLevel | Med | spec-incomplete | P5 | `400 INVALID_CHANGE_LEVEL` undocumented (`service.go:517-532`). |
| deleteEntityModel | Med | needs-decision | — | Desc claims "must be UNLOCKED"; handler never checks lock state (`service.go:365-408`). |
| setEntityModelUniqueKeys | Med | spec-incomplete | P5 | `400` on malformed body undocumented (`handler.go:230-233`). |
| importEntityModel | Med | spec-incomplete | P5 | `422 INVALID_UNIQUE_KEY_DEFINITION` on re-import undocumented (`service.go:164-178`). |
| exportMetadata | Med | spec-incomplete | P3 | 200 is opaque bag; hides real `uniqueKeys` array (`service.go:229-249`). |
| setEntityModelChangeLevel | Low | spec-stale | — | Desc "set to null to disallow"; `changeLevel` is a required enum path segment. |
| exportMetadata, deleteEntityModel | Low | spec-stale | — | Error example `detail` strings don't match server output. |
| importEntityModelWorkflow | Low | spec-incomplete | P5 | `VALIDATION_FAILED` / `WORKFLOW_SCHEMA_VERSION_UNSUPPORTED` folded under generic 400. |
| getAvailableEntityModels, validateEntityModel, exportEntityModelWorkflow, importEntityModelWorkflow (shapes) | — | clean | — | DTOs faithful to SPI types (`desc` not `description`, etc.). |

### 6D — SQL-Schema  (`/sql/schema/*` — entire tag)

| Op | Sev | Dir | Pattern | Detail |
|---|---|---|---|---|
| all 9 ops | High | needs-decision | P1 | Tag excluded in `api/config.yaml`; no route in `api/generated.go`; 404 at runtime (`app/app.go:645`). |
| genTables, getSchemas | High | spec-stale | P6 | `type:array` + sibling `$ref`, no `items` → untyped `[]interface{}` (`openapi.yaml:7013-7015,7147-7149`). |
| updateTables | High | spec-stale | P6 | Request body `type:string`, example is an array of `TableConfigDto`. |
| getSchemaByName, deleteSchemaByName, getSchema, deleteSchema | Med | spec-stale | P5 | 404 bodies lack `schema:` and use `application/json` where the platform uses `application/problem+json`. |
| all (FieldConfigDto) | Low | spec-stale | P6 | Examples use non-existent `arrayFields`, omit required `hidden`. |

### 6E — Auth / OIDC / clients / token  (`internal/api/*`, `internal/domain/*` adapters)

| Op | Sev | Dir | Pattern | Detail |
|---|---|---|---|---|
| all 7 OIDC ops | High | needs-decision | P5 | Errors speced `application/json ErrorResponseDto`; server emits `application/problem+json ProblemDetail` (`common/errors.go:234,240`; `oidc_adapter.go` throughout). |
| registerOidcProvider | Med | needs-decision | P5 | Duplicate → `400 OIDC_PROVIDER_DUPLICATE`, not the speced `409` (`oidc_adapter.go:176-178`). |
| updateOidcProvider | Med | spec-incomplete | P5 | `409 OIDC_PROVIDER_INACTIVE` undocumented (`oidc_adapter.go:297-299`). |
| getTechnicalUserToken | Med | spec-stale | P4 | `issued_token_type` enum lacks the emitted `...:jwt` (`token.go:240`); `grant_type` enum omits accepted `client_credentials`; `error` enum lacks `server_error`; `405` undocumented. |
| accountSubscriptionsGet | Med | server-gap | P1 | Documents `200`; always returns `501 NOT_IMPLEMENTED` (`handler.go:87-88`). |
| invalidateJwtKeyPair, deleteTrustedKey, invalidateTrustedKey | Med | spec-incomplete | P5 | `400` on bad id/body/grace undocumented (`keys_adapter.go:170-181`, `trusted_adapter.go:201-231`). |
| registerTrustedKey | Med | needs-decision | P4 | Desc says RSA/EC/OKP supported; only RSA accepted (`trusted_adapter.go:132-133`). |
| issueJwtKeyPair | Med | needs-decision | P6 | Desc "defaults to RS256"; `algorithm` is required (`keys_adapter.go:44-46`). |
| 14 clients/keys/trusted ops | Med | spec-incomplete | P5 | Return `501` in mock-IAM mode; never documented. |
| issue/getCurrent/reactivate JwtKeyPair | Low | needs-decision | P4 | `algorithm` enum lists many; only RS256 honoured. |
| listOidcProviders | Low | spec-stale | — | Documents `403`; no admin guard, never emitted. |
| accountGet | Low | spec-incomplete | — | `featureToggles` never populated. |
| listOidcProviders | Low | spec-stale | — | `activeOnly` typed `string`, compared literally to `"true"`. |
| **security check** | — | clean | — | No secret leakage in examples/schemas; private keys never marshalled; tenant scoping enforced with uniform 404. |

### 6F — Stream-data & message  (`internal/domain/messaging/*`; stream-data unrouted)

| Op | Sev | Dir | Pattern | Detail |
|---|---|---|---|---|
| all 13 stream-data ops | High | needs-decision | P1 | Tag `Stream Data` excluded in `api/config.yaml`; unrouted; 404 at runtime. Carries operationId fossils (`delete`, `exportAll`, `exportAll_1`). |
| deleteMessages | High | spec-incomplete | P6 | Body schema `type:string uuid`; server needs `[]string` (`messaging/handler.go:235-239`). |
| newMessage | High | spec-incomplete | P6 | Body schema `type:string`; server needs JSON object envelope (`handler.go:45-52`). |
| newMessage | Med | spec-stale | — | Desc claims array body accepted; handler unmarshals single struct only. |
| newMessage, deleteMessages | Med | server-gap | P2 | `transactionTimeoutMillis` / `transactionSize` params ignored (`handler.go:32-114,247`). |
| getMessage, deleteMessage, deleteMessages | Med | server-gap | P5 | Documented `400 "not a time-based UUID"`; no v1 validation performed. |
| getMessage | Med | spec-stale | P3 | `metaData` typed as `ValueMaps` buckets; server emits a flat map (`handler.go:165-177`). |
| getMessage | Low | spec-stale | — | 200 example omits always-injected `typeReferences:{}`. |
| message error examples | Low | spec-stale | — | Omit always-present `properties.errorCode` (`common/errors.go:234`). |

## 7. What this means for the way out

A mechanical "sync the spec to the server" is the wrong instinct and would *destroy*
contract intent (e.g. it would delete `deleteEntities`' conditional-delete and turn a
data-loss bug into a documented feature). The problems that need design, not cleanup:

1. **Restore an enforced binding** in a spec-first world — how do we make the server
   provably conform to the authored contract (contract tests? a generated conformance
   suite? the `entity_conformance_test.go` seed extended to every op?) *without* giving
   up authored ownership of the spec.
2. **A per-finding reconciliation-direction decision process** — who decides
   `server-gap` vs `spec-stale` vs `needs-decision`, and how it's recorded (this is a
   Cloud-parity concern; each decision is a contract decision).
3. **The dead-surface decision** — implement, or formally remove from the published
   contract, the 22 excluded-tag operations and the always-501 stubs. Shipping a
   contract that is 25% fiction is the loudest single signal of the problem.
4. **The one urgent correctness item** — `deleteEntities` data loss is not a doc issue
   and should not wait for the broader effort.

These four are the inputs to the brainstorming session; this document is the evidence
base for their scale.

---

*Generated from a 6-way parallel audit of `api/openapi.yaml` against the Go handlers.
Every "server does" claim above is cited to `file:line` in the working tree at the date
above; line numbers will drift as the code changes.*
