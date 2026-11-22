# #650 research: how M2M clients are found today

Facts the #650 design rests on. Each was checked in the code on
`release/v0.9.0` at 34986708, or in the sibling checkout named.

## The client store

- Records live in the SYSTEM-tenant KV store, namespace `m2m-clients:<tenant>`,
  key = client id (`internal/auth/kv_m2m_codec.go:17`, `:48`). A global
  namespace `m2m-client-ids` maps a client id to its tenant
  (`kv_m2m_codec.go:18`, read by `getIndex`, `internal/auth/kv_m2m_store.go:81`).
- `decodeClientRecord` binds a record to its key and namespace tenant: a record
  whose `clientId`/`tenantId` differ from the key/namespace is undecodable
  (`kv_m2m_codec.go:118-120`).
- `Authenticate(ctx, clientID, secret)` reads the index, then the record; on an
  index miss it reads a decoy key so every decided request makes two reads
  (`kv_m2m_store.go:140-160`). A malformed id burns one bcrypt and answers
  `ErrInvalidClient` without a read (`:142-147`).
- `Lookup(ctx, clientID)` — same index-then-record path, no secret
  (`kv_m2m_store.go:217-238`). Its one caller is the compute-stream re-check,
  `internal/grpc/streaming.go:219`, which then compares the record's tenant with
  the stream's (`:229`).
- `Create` checks the index for the id across all tenants, then writes record,
  then index entry, under a per-tenant stripe lock (`kv_m2m_store.go:247-298`).
  `Delete` and `ResetSecret` carry most of their complexity for the index: a
  damaged entry, an entry naming another tenant, a record without an entry
  (`:352-430`). The help topic documents those cases for operators
  (`cmd/cyoda/help/content/auth/clients.md`, STORAGE AND CONSISTENCY).
- The KV SPI has no compare-and-set (`cyoda-go-spi/persistence.go:545-552`).
  Paul's #286 ruling: no-CAS races are documented, not fixed.

## Keyed by client id alone, outside the store

- Per-node verified-secret cache: `map[string]verifiedSecret` keyed by client id
  (`internal/auth/secret_check.go:104-143`).
- Per-node, per-client token rate limit: `map[string]*rate.Limiter` keyed by
  client id (`internal/auth/client_bucket.go:17-49`, called at
  `internal/auth/token.go:155`).

With ids unique only inside a tenant, both would let one tenant's client affect
another tenant's client with the same id: share its rate limit (a cross-tenant
denial of service) and evict its cache entry.

## Client ids

- Grammar `^[A-Za-z0-9]{1,100}$` (`kv_m2m_codec.go:34`), used by the store, by
  `/clients/{clientId}` (`internal/domain/account/m2m_adapter.go:63`), and by the
  validator for a `cgen` token's `caas_user_id` and an OBO token's `act.sub`
  (`internal/auth/validator.go:208`, `:232`). OpenAPI repeats it in four places
  (`api/openapi.yaml` `/clients/{clientId}` DELETE and PUT parameters,
  `TechnicalUserCredentialsDto.client_id`, `TechnicalUserDto.clientId`).
- Generated ids: 16 characters of upper-case base32-hex, 80 bits
  (`m2m_adapter.go:17-35`), one retry on collision (`:184-215`).
- A client's user id IS its client id: `Create(…, cid, cid, …)`
  (`m2m_adapter.go:191`); the record's `UserID` must pass `ValidateUserID`
  (`kv_m2m_codec.go:73`), which refuses `system` in any letter case
  (`internal/common/user_id.go:71`). A generated id can never be `system`; a
  chosen one could, and would fail at encode time as a 500.

## The token endpoint

- Mounted at `/oauth/token` on the inner mux before the authenticated
  catch-all (`app/app.go:580`, `internal/auth/service.go:107`); the context path
  (default `/api`) is stripped by the outer mux (`app/app.go:660-685`). Two tests
  pin the public route list (`app/route_registration_test.go:27`,
  `app/route_classification_test.go:79`).
- Order: method → Content-Type → Basic → store → form → grant
  (`internal/auth/token.go:96-148`).
- The cluster proxy routes on `X-Tx-Token` only; the path does not affect it
  (`internal/cluster/proxy/http.go:50-95`). CORS is path-independent.
- In mock IAM mode, the generated `GetTechnicalUserToken` answers 501
  (`internal/domain/account/handler.go:81-83`).

## Tenants

- Grammar `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, case significant, checked at two
  doors: the `caas_org_id` claim (`internal/auth/validator.go:140-149`) and
  `cyoda token --tenant` (`internal/auth/operator_token.go:32`). No tenant
  registry exists: any grammar-valid tenant is addressable.
- No value is reserved. `SYSTEM` (`spi.SystemTenantID`) is the machinery's
  tenant; its KV holds the signing keys, trusted keys and M2M clients
  (`app/app.go:274-281`). Nothing refuses `caas_org_id: SYSTEM`; only the auth
  stores use the SYSTEM KV (`grep KeyValueStore(`: `app/app.go:281` is the only
  non-test caller besides the model-cache wrapper).
- `PLATFORM` is the operator tenant (`internal/auth/operator_guard.go:19`); an
  OBO client there is refused (`m2m_adapter.go:168`).
- No route has a tenant in its path (searched `api/openapi.yaml`, help content,
  `PathValue("tenant`, `URLParam(…tenant`: no hits).
- KV keys compare byte for byte on every backend: postgres `TEXT` primary key
  with deterministic collation, sqlite `TEXT` (BINARY) primary key
  (`plugins/*/migrations/000001_initial_schema.up.sql`), memory a Go map,
  cassandra `text` clustering/partition keys.

## Cloud (~/dev/cyoda)

- Token endpoint `POST /api/oauth/token`, Basic only; the client is found by id
  alone (`TechnicalUserService.kt:248-263`), the tenant from the stored user
  (`:234-236`).
- Ids: 6 characters of `[a-zA-Z0-9]`, globally unique by check-then-insert
  (`TechnicalUserService.kt:117-134`, `AccountUtils.kt:3-16`). No caller-chosen
  id, no name field, no `409` (`openapi.yml:45-105`).
- No route anywhere puts a tenant in the path.
- Defects seen on the way (for the CaaS ticket, not cyoda-go):
  - a wrong secret answers `400` (`error=unsupported_grant_type`), an unknown
    client `401` — an existence oracle on client ids
    (`TokenExceptions.kt:10-35`, `TechnicalUserControllerIT.kt:157-199`);
  - ids and secrets come from `Random.Default`, not a cryptographic source
    (`AccountUtils.kt:14`).

## cassandra (../cyoda-go-cassandra)

- No auth code of its own: the binary is `app.New` plus the plugin
  (`cmd/cyoda-go/main.go:16-84`). It pins cyoda-go v0.8.3, which predates the KV
  client store, so no cassandra keyspace holds `m2m-client-ids` rows.
- Its e2e runs cyoda-go's parity registry (`e2e/cassandra_test.go:84-99`) and gets
  tokens by signing (v0.8.3 fixtures). At its next cyoda-go bump the parity
  client and compute-test-client from cyoda-go bring the new URL with them.
- KV namespace → `store_type = "kv:" + namespace`, one listing partition per
  (tenant, namespace) (`internal/store/data_store.go:265-267`). No CAS.

## References to update

182 hits of `oauth/token` outside `docs/superpowers/` (help topics, README,
CHANGELOG, `docs/access-to-the-cyoda-api.html`, ARCHITECTURE, PRD, FEATURES,
CONCURRENCY, cloud-parity docs, helm gateway doc, OpenAPI, oasdiff ignore list,
e2e and parity helpers, `cmd/compute-test-client/token_source.go:41`). The
`internal/e2e` suite builds the URL in one helper, `postTokenRaw`
(`internal/e2e/token_reconciliation_test.go:39`), plus two direct builds
(`token_exchange_test.go:235`, `:262`). `test/recon/oauth.go:41` targets Cloud
and keeps Cloud's URL.
