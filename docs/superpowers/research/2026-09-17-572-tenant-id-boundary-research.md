# Research — #572: the tenant id reaches the filesystem unvalidated

Date: 2026-09-17
Issue: [#572](https://github.com/cyoda/cyoda-go/issues/572) (milestone v0.9.0)

This records what the tenant id actually *is* today, everywhere it enters the
system and everywhere its shape matters, so the design can pick an accepted set
from evidence rather than taste.

---

## 1. The type

`spi.TenantID` is a bare named string in the SPI root package
(`cyoda-go-spi/context.go:5`):

```go
type TenantID string
const SystemTenantID TenantID = "SYSTEM"
```

No methods, no constructor, no parser, no validator, no sentinel for a
malformed value. Every producer writes `spi.TenantID(someString)`.

The SPI's own idiom for this kind of rule is a free function plus a sentinel in
the root package — `ValidateFilterPath`/`ErrInvalidFilterPath`
(`filter_path.go:127`), `ValidateLeafPattern`/`ErrInvalidPattern`
(`eval_leaf.go:795`), `ValidateChangeLevel` (`types.go:128`). There is no
`validation/` or `ids/` subpackage; the SPI has exactly three packages (`spi`,
`predicate`, `spitest`).

## 2. Where a tenant id enters the system

| # | Site | Source | Shape check today |
|---|------|--------|-------------------|
| 1 | `internal/auth/validator.go:113` | JWT `caas_org_id` claim (HTTP **and** gRPC — gRPC delegates to the same `AuthenticationService`) | non-empty only |
| 2 | `internal/auth/token.go:191` | `caas_org_id` on an **externally signed** subject token during OBO exchange; echoed verbatim into the minted token at `:221` | none; only an equality check against the M2M client's tenant (`:206`) |
| 3 | `internal/auth/token.go:90` | `client.TenantID` from the M2M client record → minted `caas_org_id` | none |
| 4 | `internal/auth/oidc/usercontext.go:65` | `p.OwnerLegalEntityID` (a `uuid.UUID`) | structurally a UUID; `uuid.Nil` refused at `:36` |
| 5 | `internal/cluster/dispatch/handler.go:146` | peer HTTP JSON body `DispatchCalloutRequest.TenantID` | none (peer auth only) |
| 6 | `internal/cluster/scheduler_rpc.go:272` | peer RPC `req.Task.TenantID` → `common.SystemUserContext` | none |
| 7 | `internal/auth/oidc/broadcast.go:83` | cluster gossip envelope `{"t": ...}` | length ≤ 36 only (`maxBroadcastTenantIDLen`) |
| 8 | `app/app.go:407` | `CYODA_BOOTSTRAP_TENANT_ID` (default `default-tenant`) | none; `validateBootstrapConfig` (`app.go:1051`) checks the id/secret coupling only |
| 9 | `app/app.go:431` | `cfg.IAM.MockTenantID` — hardcoded `mock-tenant`, not env-overridable | n/a |
| 10 | `internal/grpc/streaming.go:64` | client `joinedLegalEntityId` | compared, never adopted |
| 11 | storage rehydration | `plugins/{sqlite,postgres}` row scans, `internal/auth/kv_trusted_store.go:771`, `internal/auth/oidc/service.go:384` | none (round-trip of a previously admitted value) |

Registration paths never take the tenant from a request body: both
`POST /clients` (`internal/domain/account/m2m_adapter.go:129`) and trusted-key
registration (`trusted_adapter.go:112`) inherit it from the caller's JWT. So
site 1 governs them.

## 3. Where the shape matters

### 3a. Filesystem — memory backend only

`plugins/memory/message_store.go:77` is **the only place in the product where a
path is built from data**:

```go
p := filepath.Join(root, tenant, id)
```

guarded by the hand-rolled check at `:68-83`. The blob root is a process-wide
`os.MkdirTemp` (`store_factory.go:132`), removed on `Close` (`:231`). Nothing
else in `plugins/memory` touches the filesystem, and neither sqlite nor postgres
touches files at all for messages.

### 3b. Composite KV keys — every backend

`internal/auth/kv_trusted_store.go:97`:

```go
func trustedKeyKey(tenantID spi.TenantID, kid string) string {
	return string(tenantID) + ":" + kid
}
```

`trustedKeyKey("victim", "a:b")` and `trustedKeyKey("victim:a", "b")` produce
the same key, and the `kid` is caller-supplied at trusted-key registration.
Nothing re-verifies the tenant after the lookup. A tenant id containing `:` is
therefore a cross-tenant read/overwrite of trusted signing keys — **backend
agnostic**, unlike the filesystem hazard.

`internal/auth/oidc/kv_keys.go:14,19` has the same layout
(`"<tenant>:<provider>"`, `"<tenant>:uri:<sha>"`) and a prefix scan at
`kv_store.go:121-122`, but that path is UUID-only by construction *and*
re-verifies `p.OwnerLegalEntityID` after unmarshal (`kv_store.go:134`), so it is
not independently exploitable.

### 3c. SQL — safe everywhere

Every backend binds the tenant as a parameter against a `tenant_id` column;
nothing interpolates it into an identifier. PostgreSQL's RLS GUC is bound too:
`SELECT set_config('app.current_tenant', $1, true)` (`transaction_manager.go:137`,
rationale in `plugins/postgres/doc.go:57`). Cassandra likewise binds
`tenant_id = ?` (55 sites) and derives its keyspace from config, not the tenant.

### 3d. Logs and a client-facing message

The tenant is emitted verbatim in ~20 `slog` fields, and interpolated into a
caller-visible error at `internal/cluster/dispatch/cluster_dispatcher.go:303`:

```go
fmt.Sprintf("no peer with tags %q for tenant %s after %v", tags, tenantID, d.waitTimeout)
```

An unconstrained tenant id is therefore also a log-injection and
response-shaping surface.

### 3e. Map keys, in-memory

`map[spi.TenantID]...` throughout `plugins/memory`, the OIDC registry, and the
search quota table. Any string works; no hazard.

## 4. What the accepted set has to admit

Every tenant-id value that exists anywhere in the tree today:

| Value | Where |
|-------|-------|
| `SYSTEM` | `spi.SystemTenantID`, used at `app/app.go:271` — **uppercase** |
| `default-tenant` | `app/config.go:333`, `.env.jwt.example`, `deploy/helm/cyoda/values.yaml:70` |
| `mock-tenant` | `app/config.go:371` |
| `system-tenant` | `e2e/parity/fixtureutil/fixtureutil.go:206,489` |
| `riskblocs` | `scripts/multi-node-docker/start-cluster.sh:96` |
| `my-tenant` | `scripts/multi-node-docker/README.md:34` |
| `tenant-abc-123` | `docs/cyoda/openapi.yml:7927` (`legalEntityId` example) |
| canonical lowercase UUID | `e2e/parity/fixtureutil` (`uuid.NewString()`), all OIDC fixtures |
| `conformance-<uuid>` (48 chars) | `spitest.defaultNewTenant()` — every SPI conformance run |
| `tenant-A` / `tenant-a`, `org-7`, `t1`, `acme`, … | ~40 test literals |

Facts that constrain the choice:

- **No value anywhere contains a character outside `[A-Za-z0-9_-]`.** No dot, no
  space, no `:`, `@`, `/`, `\`, `%`, `{}`, and nothing non-ASCII. `"Tenant A"`,
  `"Mock Tenant"` and `"System"` appear only as `Tenant.Name`, never as an ID.
- **Uppercase must be accepted.** `SYSTEM` is uppercase, and fixtures use
  case-varied pairs (`tenant-a` vs `tenant-A`) *deliberately*, to assert
  case-sensitive isolation (`plugins/postgres/attribution_test.go:151,227`). A
  case-folding validator would break those tests and weaken isolation.
- **Longest existing value is 48 chars** (`conformance-` + UUID); the longest
  production-shaped one is 36 (a canonical UUID). The only length cap in code is
  `maxBroadcastTenantIDLen = 36`, and it applies only to gossip envelopes.
- The empty string is already refused in ~10 places and is used as a negative
  fixture.

## 5. The one existing shape rule, and why it is narrow

`internal/domain/account/oidc_adapter.go:155` requires the caller's tenant to
parse as a UUID before an OIDC provider may be registered, answering
`OIDC_INVALID_TENANT` (400) otherwise. Its help topic states the platform's
position plainly:

> cyoda treats legal entity identifiers as UUIDs. […] Production deployments use
> UUID-shaped legal entity identifiers and are not affected by this restriction.

So the platform's *intent* is UUID tenants, but the engine must keep accepting
the non-UUID convenience literals (`SYSTEM`, `default-tenant`, `mock-tenant`,
`conformance-<uuid>`) that bootstrap, mock mode and the conformance suite
depend on. A UUID-only rule at the token boundary is therefore not available.

## 6. Conformance and parity coverage today

- `spitest/message.go` runs 7 message-store subtests (Save/Get, NotFound,
  Delete, DeleteBatch, 4 MB payload, double-Close, TenantIsolation) — enough to
  exercise a rooted-filesystem rewrite's happy path on every backend.
- No conformance test feeds a hostile tenant id: `spitest.defaultNewTenant()`
  returns `conformance-<uuid>` and **no backend overrides it**.
- The only adversarial-input test that exists is backend-local and covers
  *message ids* only: `plugins/memory/message_store_test.go:268-305`
  (`"../escape"`, `"../../escape"`, `"a/../../escape"`, `".."`).
- `e2e/parity/fixtureutil.MintTenantJWT` mints `caas_org_id: uuid.NewString()`,
  so parity fixtures are unaffected by any reasonable accepted set.

## 7. `os.Root` availability

Go 1.26.7 (root and all three plugin `go.mod` files). Every method the rewrite
needs exists on `*os.Root`: `MkdirAll`, `OpenFile`, `Rename`, `Open`, `Remove`,
`Close`. There is **no** `CreateTemp` equivalent, so the temp blob needs an
exclusive-create (`O_CREATE|O_EXCL`) retry loop. The documented platform
weaknesses (`GOOS=js` TOCTOU, `plan9` name-not-descriptor) are not build
targets; the Unix caveat covers `Chmod`/`Chown`/`Chtimes`, which this code never
calls.

## 8. What Cloud actually mints

Read from the cyoda-cloud monorepo (`~/dev/cyoda`) and the Trino connector.

**Claim name:** `AbstractCaasOidcComponents.kt:40` —
`const val CLAIM_CYODA_LEGAL_ENTITY_ID: String = "caas_org_id"`.

**Three minting paths, plus a fallback:**

1. **Auth0 post-login action** (`scripts/auth0/action-setup-cyoda-token-claims.js:33`)
   — `event.organization?.metadata?.caas_org_id || caasUserId`. The fallback
   `caasUserId` is generated at `action-assign-cyoda-user-id.js:89-91` as
   `uuidv4().replaceAll('-','')` → **32-character lowercase hex**. This is the
   dominant production shape for any user not in an Auth0 Organization.
2. **M2M client credentials** — `TechnicalUserService.kt:234` puts
   `techUser.legalEntityId` in the claim.
3. **OBO / token exchange** — `TechnicalUserService.kt:399` copies
   `subjectLegalEntityId` through.
4. **Local-issuer fallback** — `AbstractCaasOidcComponents.kt:50` substitutes the
   literal `"CYODA"` when the claim is absent.

**The claim becomes the primary key verbatim.** `AbstractCaasOidcComponents.kt:158`
→ `AutoEnrollmentSupport.kt:14` assigns `legal.legalEntityIdentifier =
enrollment.identifier` with no transformation. `LegalEntity.kt:20-41` declares a
plain `String` `@Id` with no `@Pattern`, no `@Size`, no `maxLength`, and the
Cassandra DDL (`schema.dynamic.entities.cql:8`) stores blobs — so there is **no
charset or length constraint anywhere in Cloud**.

**Observed Cloud values:** 32-char lowercase hex (generated); `CYODA`; `SYSTEM`
(`ILegalEntity.SYSTEM_LEGAL_ENTITY_ID`); `TEST`, `TEST1`, `NOT_IN_LIST`,
`TEST_LEGAL_ENTITY`, `ANOTHER_LEGALENTITY`; `tenant-abc-123` (the published
OpenAPI example for `legalEntityId`), `other-tenant`, `test-legal-entity`,
`le-id`; purely numeric `123`, `456`; and `caas_<org_id>` — underscore-bearing.
**Every literal found is drawn from `[A-Za-z0-9_-]`.**

**The only declared length cap in Cloud is 100**, and it is indirect —
`CSUser.kt:45`:

```kotlin
@ColumnValidation(nullable = false, maxLength = 100)
private lateinit var legalEntityId: String
```

Auto-enrollment mirrors the claim into that field, so a `caas_org_id` longer
than 100 characters already fails to persist a user in Cloud — but nothing
rejects it at the token layer first.

**A charset precedent already exists in Cloud**, on a different input —
`EdgeMessageController.kt:157`:

```kotlin
@Pattern(regexp = "^[a-zA-Z0-9._-]{1,$MAX_PATH_VARIABLE_LENGTH}$")
@PathVariable subject: String,
```

**Cloud never uses the tenant as a path, keyspace, table or index name.** The
keyspace is a fixed config value; column families are compile-time constants;
Trino schema names come from user input, not the tenant. It *is* used as an
unbounded-cardinality Micrometer tag value (`CalcMemberMetrics.kt:124`).

The important caveat: the absence of exotic characters in Cloud's corpus is a
property of Auth0's generator and of the fixtures, **not an enforced
invariant**. Anything an Auth0 Organization admin types into the
`caas_org_id` metadata field — a space, `/`, `..`, `%`, a quote, non-ASCII, the
empty string — flows verbatim into the claim today. Under Gate 7 cyoda-go
defines the grammar and Cloud aligns to it; this is exactly such a case.
