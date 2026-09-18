# Design — a tenant-id grammar at the door, and a blob store that cannot spell a path wrong

Date: 2026-09-18
Issue: [#572](https://github.com/cyoda/cyoda-go/issues/572) (milestone v0.9.0)
Research: [`2026-09-17-572-tenant-id-boundary-research.md`](../research/2026-09-17-572-tenant-id-boundary-research.md)

## Problem

The tenant id is a free-form string lifted from a JWT claim with only a
non-empty check (`internal/auth/validator.go:113`). In the memory backend it
becomes a filesystem directory name — `filepath.Join(blobDir, tenant, id)` at
`plugins/memory/message_store.go:77`, the only place in the product where a
path is built from data. The sole defence is a hand-rolled string check inside
that one plugin, which is also the only tenant-format validation anywhere in
the codebase.

Nothing ships that exploits this: the claim must arrive on a token signed by a
trusted key. But a tenant-isolation control lives in the wrong layer, guards by
string comparison, and is invisible to static analysis.

Two facts found during design change the shape of the fix.

**The hand-rolled check does not cover the hazard that actually bites.** It
rejects `..` and separators, but two tenants differing only in case share one
directory on any case-insensitive filesystem. Verified on this machine, Go
1.26.7, APFS:

```
MkdirAll("tenant-a")                       -> nil
OpenFile("tenant-a/id1", O_CREATE|O_EXCL)  -> nil
MkdirAll("tenant-A")                       -> nil
OpenFile("tenant-A/id1", O_CREATE|O_EXCL)  -> openat tenant-A/id1: file exists
```

`os.Root` does not close this either — it guarantees confinement, not
distinctness. In the same run `"."` was accepted and wrote the blob at the root,
merging tenants; `../victim` and `a/../../victim` were both rejected. macOS and
Windows are both release targets, and the memory backend is what an unconfigured
binary runs.

**Cloud does not constrain the claim at all.** `caas_org_id` is minted from
free-text Auth0 organization metadata, falling back to a generated 32-character
lowercase hex id, and becomes the `LegalEntity` `@Id` verbatim with no
transformation (`AbstractCaasOidcComponents.kt:158` → `AutoEnrollmentSupport.kt:14`).
Under Gate 7 cyoda-go defines the contract and Cloud follows; this is such a case,
and it carries a Cloud-side action item.

## The rule: validate where data enters cyoda-go

A tenant id enters cyoda-go from outside cyoda-go in exactly two places.

1. **A JWT claim** — `internal/auth/validator.go`. Every HTTP and gRPC request
   arrives this way; gRPC delegates to the same `contract.AuthenticationService`
   (`internal/grpc/interceptor.go:83` and `internal/api/middleware/auth.go:24`).
2. **Operator config** — `CYODA_BOOTSTRAP_TENANT_ID` at startup.

Everywhere else — peer dispatch bodies, scheduler RPC payloads, gossip
envelopes, scheduled-task rows, search-job rows, OIDC provider records, the
stored M2M client table — carries a value this cluster already admitted at one
of those two doors. Re-checking it there would be guarding against a corrupted
store or a compromised peer, a different threat model in which tenant-id
spelling is not what saves you, and `.claude/rules` forbids keeping branches as
guards.

Two consequences worth stating, because both look like gaps and are not:

- **The token endpoint needs no check.** `handleClientCredentials`
  (`internal/auth/token.go:90`) mints `caas_org_id` from the stored M2M client
  row, whose tenant came from `POST /clients` (inherited from a validated JWT at
  `internal/domain/account/m2m_adapter.go:129`) or from bootstrap. The
  token-exchange path (`token.go:191`) is gated by the existing equality test at
  `token.go:205`, so the subject's claim must equal that same validated value.
  And any token this endpoint mints is presented back through door 1 before it
  can do anything.
- **The federated OIDC path needs no check.** The tenant is
  `p.OwnerLegalEntityID.String()` (`internal/auth/oidc/usercontext.go:65`) — a
  `uuid.UUID`, structurally incapable of failing the grammar.

## The grammar

```
^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$
```

In `internal/common/tenant.go`, beside `TenantFromContext` and
`SystemUserContextValue`, which auth, cluster and app already import:

```go
var ErrInvalidTenantID = errors.New("invalid tenant id")

func ValidateTenantID(id spi.TenantID) error
```

A hand-rolled byte loop, not `regexp` — it runs on every authenticated request.

**Charset.** Every tenant-id literal in cyoda-go and in the Cloud monorepo is
drawn from `[A-Za-z0-9_-]`; the dot is admitted because Cloud already ships
`@Pattern("^[a-zA-Z0-9._-]{1,256}$")` on edge-message subjects
(`EdgeMessageController.kt:157`) and a domain-shaped tenant is a plausible
future. Case is preserved: `spi.SystemTenantID` is `"SYSTEM"` and Cloud's
local-issuer fallback is `"CYODA"`.

**First character.** Restricting it to alphanumeric makes `""`, `.`, `..`,
`.hidden` and `-leading` unrepresentable structurally rather than by
enumeration.

**Length.** 100 matches Cloud's only declared cap —
`@ColumnValidation(maxLength = 100)` on `CSUser.legalEntityId`
(`CSUser.kt:45`), which a Cloud auto-enrollment already writes the claim into.
A longer tenant works in cyoda-go and fails to persist a user in Cloud; 100
keeps the two tiers consistent. The longest value in cyoda-go today is 48
(`conformance-<uuid>`).

Everything that exists is admitted: `SYSTEM`, `CYODA`, `default-tenant`,
`mock-tenant`, `system-tenant`, `riskblocs`, `tenant-abc-123`,
`conformance-<uuid>`, canonical UUIDs, 32-char hex, and bare numerics.

### What the grammar is and is not for

Encoding the blob path (below) makes the filesystem safe whatever the claim
says, so the grammar is **not** what stands between a token and a path
traversal. It earns its place on ordinary input-validation grounds, which Gate 3
requires at a system boundary:

- **Log injection** — the tenant is written verbatim into ~20 `slog` fields;
  newlines and control characters forge records.
- **Response injection** — `internal/cluster/dispatch/cluster_dispatcher.go:303`
  interpolates the raw tenant into a caller-visible error string.
- **Cross-tier consistency** — Cloud's 100-character cap, above.
- **Unbounded keys** — `internal/cluster/modelcache/payload.go:31` checks only
  non-empty, with no length cap, and concatenates the tenant into a cache key.

## Enforcement

### Door 1 — the JWT claim

`buildUserContext` returns an error for a claim failing the grammar. `Validate`
propagates it; `DelegatingAuthenticator.Authenticate`
(`internal/auth/delegating.go:73`) maps every validator failure to the existing
generic `ErrAuthenticationFailed`, so the caller sees the same uniform **401**
RFC 9457 problem detail as any other bad token and learns nothing from the
difference. A structured `slog.Warn` records `reason=token-invalid` with the
validator's detail, as today.

The detail string must not echo the rejected tenant: an attacker-chosen claim
would otherwise reach the log through `logAuthFailure`'s `detail` field, which is
the log-injection case the grammar exists to prevent. The error reports the
reason and the offending length or byte offset, never the value.

### Door 2 — bootstrap config

`validateBootstrapConfig` (`app/app.go:1051`) gains the check, **inside** the
existing `cfg.Bootstrap.ClientID != ""` branch. `envString` uses `LookupEnv`, so
an explicitly-empty `CYODA_BOOTSTRAP_TENANT_ID` overrides the default; validating
outside that branch would abort startup for deployments that configure no
bootstrap client at all. Failure exits non-zero on the existing bootstrap path.

A unit test asserts every tenant constant the binary ships with satisfies the
grammar — `spi.SystemTenantID`, `cfg.IAM.MockTenantID`, and the
`CYODA_BOOTSTRAP_TENANT_ID` default — so a later change to a default cannot
quietly produce an unbootable binary.

## The memory blob store

### Encode the path; delete the check

`MessageStore` addresses blobs by `hex(tenant)/hex(id)` rather than by the raw
strings. Hex output is `[0-9a-f]`, so traversal, separators, case collision,
empty and dot segments, and Windows reserved device names are all
unrepresentable — not rejected, unrepresentable. `blobPath`'s hand-rolled check
(`message_store.go:68-84`) and `tenantBlobDir`'s `"placeholder"` trick are
deleted rather than kept: with encoding they guard nothing.

Hex doubles each segment, so a 100-character tenant yields a 200-character
directory name and an id is usable up to 127 characters before hitting the
255-byte filename limit. Message ids are server-generated time UUIDs (36), so
this is slack, not a constraint; an over-long id surfaces as an ordinary write
error.

The empty tenant is not re-checked here. `resolveTenant`
(`plugins/memory/store_factory.go:164`) already refuses one before a
`MessageStore` can be constructed; a test asserts that invariant instead of
duplicating the check.

### Confine at the OS level

`StoreFactory` opens a `*os.Root` on `blobDir` at construction — the same place
that already panics if `os.MkdirTemp` fails — and closes it before
`os.RemoveAll` in `Close`. `MessageStore` moves to `root.MkdirAll`,
`root.OpenFile`, `root.Rename`, `root.Open`, `root.Remove`.

`*os.Root` has no `CreateTemp`, so the temp blob uses a bounded
`O_CREATE|O_EXCL` retry loop over a `crypto/rand` suffix: a fixed maximum number
of attempts, an error rather than a spin on exhaustion, and the handle closed
before any `Remove` so the cleanup works on Windows. Every failure path that
cleans up today — `io.Copy`, `Close`, path resolution, `Rename` — cleans up
against `root.Remove`.

Encoding and `os.Root` are not redundant. Encoding makes a bad *name*
impossible; `os.Root` makes a bad *resolution* impossible, which is what a
symlink planted under the blob root would otherwise achieve.

### Make Save atomic

`Save` renames the blob into place (step 2) and inserts the metadata under
`msgMu` (step 3) as two separate operations. Two concurrent saves of the same id
can therefore leave one writer's blob paired with the other's metadata. The
rename moves inside the same critical section as the metadata insert; the
payload copy stays outside the lock, as today. Unreachable through HTTP, where
ids are server-generated, but the SPI admits any id and this function is being
rewritten anyway.

## One consistency fix, not a grammar matter

`DispatchCalloutRequest` carries two tenants: its own `TenantID`, which becomes
the user context (`internal/cluster/dispatch/handler.go:55`), and
`EntityMeta.TenantID` (`internal/cluster/dispatch/types.go:21`), which is handed
verbatim to the local dispatcher (`handler.go:59`). Nothing compares them, so a
peer can dispatch a callout whose entity names a different tenant than the
request runs as. `handleCallout` rejects a mismatch with **400**, matching the
malformed-body precedent four lines above at `handler.go:51`.

## Error and status codes

No new error code, so no `errors/<CODE>.md` and no `TestErrCode_Parity` churn.

| Entry point | Condition | Status | Code |
|---|---|---|---|
| Any authenticated HTTP endpoint | `caas_org_id` fails the grammar | 401 | existing `UNAUTHORIZED`, uniform RFC 9457 problem |
| Any authenticated gRPC method | same | `codes.Unauthenticated` | envelope `Success=false` |
| `POST /oauth/token` | — | unchanged | minted tokens are validated at door 1 on use |
| `POST /internal/dispatch/callout` | `EntityMeta.TenantID != TenantID` | 400 | plain text, as the sibling malformed-body path |
| process startup | `CYODA_BOOTSTRAP_TENANT_ID` fails the grammar, with a bootstrap client configured | exit non-zero | — |
| `POST /oauth/token` | key lookup or signing fails | 500 | `server_error`, now `error_description: server_error [ticket: <uuid>]` |
| OIDC management endpoints | caller's tenant is not a UUID in any accepted spelling | 400 | existing `OIDC_INVALID_TENANT` |

`api/openapi.yaml` already declares 401 on every authenticated path, so the
schema is unchanged.

## Coverage

| Scenario | unit | e2e (postgres) | parity | gRPC |
|---|---|---|---|---|
| Grammar accept/reject table, including every shipped tenant constant | yes | — | — | — |
| Hostile `caas_org_id` → 401 uniform problem detail | yes | yes | waived | yes |
| Rejected tenant never reaches a log field or a response body | yes | yes | — | — |
| Accepted set still authenticates (`SYSTEM`, `default-tenant`, 32-hex, UUID, `conformance-*`) | yes | yes | — | — |
| Bad `CYODA_BOOTSTRAP_TENANT_ID` → non-zero exit | yes | yes (subprocess) | — | — |
| Empty `CYODA_BOOTSTRAP_TENANT_ID`, no bootstrap client → starts | yes | — | — | — |
| Blob round-trip under `os.Root` with adversarial tenant and id | yes | — | existing message scenarios | — |
| Two tenants differing only in case keep separate blobs | yes | — | — | — |
| Concurrent `Save`: same tenant, and same id | yes (isolated) | — | never | — |
| Temp-file loop: exhaustion errors, cleanup on every failure path | yes | — | — | — |
| Dispatch `EntityMeta` tenant mismatch → 400 | yes | — | multinode fixture | — |
| An uppercase/braced/urn-spelled UUID tenant can read, list, update and delete the provider it registered | yes | yes | — | — |
| A non-UUID tenant gets `400 OIDC_INVALID_TENANT` from every OIDC endpoint, not an empty 200 | yes | yes | — | — |
| A token-endpoint 500 carries a ticket, and the same ticket appears in the log | yes | yes | — | — |

**Parity waiver, one line:** the hostile-claim 401 is rejected at the
authenticator and never reaches a storage backend, so a cross-backend scenario
buys no backend signal and costs shared-suite time. The parity budget goes to
blob-store distinctness, which *is* backend-specific.

Concurrency tests live in `plugins/memory`, never in the shared parity suite,
per `.claude/rules/test-coverage.md`. Any new parity scenario bumps
`wantParityScenarioCount` (`e2e/parity/registry_count_test.go:9`).

The SPI conformance suite exercises the rewritten store unchanged —
`spitest/message.go` runs Save/Get, NotFound, Delete, DeleteBatch, a 4 MB
payload, double-`Close`, and tenant isolation against every backend.

## Documentation

- `docs/cloud-parity/tenant-id-grammar.md` — the grammar, why cyoda-go defines
  it, and the Cloud-side action item: Cloud validates nothing today, so a
  Cloud-issued token outside the grammar becomes a silent 401 rather than a
  diagnosable rejection. Records the carve-out that Cloud's audit sentinel
  `AuditActorInfoDto(legalId = "-")` fails the first-character rule and never
  reaches `caas_org_id`.
- `CHANGELOG.md` under `### Breaking` — token acceptance narrows.
- `cmd/cyoda/help/content/config/auth.md`, `cmd/cyoda/help/config_registry.go:101`,
  `README.md`, `docs/ARCHITECTURE.md:1588` — the accepted set for
  `CYODA_BOOTSTRAP_TENANT_ID`.

## Two adjacent defects fixed in the same change

Both were found while designing this and are small enough that splitting them
into their own PRs would cost more review than it saves.

### The OIDC provider store keys by spelling (#587)

`KVOidcProviderStore.Register` writes the provider blob and its URI index under
the **canonical lowercase** UUID — `spi.TenantID(p.OwnerLegalEntityID.String())`
at `internal/auth/oidc/kv_store.go:43` — while `Get` (`:58`), `GetByURI`
(`:77`), `Delete` (`:105`), `ListByTenant` (`:121`) and `RaceValidateIndex` all
key off the caller's raw tenant string. `uuid.Parse`
(`internal/domain/account/oidc_adapter.go:155`) accepts uppercase, braced and
`urn:uuid:` forms that `String()` normalises away, so a tenant whose
`caas_org_id` is spelled any of those ways registers a provider it can then
never list, read, update or delete.

This is engine code on `spi.KeyValueStore` (`app/app.go:317`), so it behaves the
same on every backend — it is not a memory-plugin defect.

Authentication is unaffected: the registry's provider map and kid index are both
populated from the stored provider's own `OwnerLegalEntityID`
(`internal/auth/oidc/registry.go:188`), so token validation already uses the
canonical form. Only the management API strands. Nor does it leak across
tenants — the stale-index defence at `kv_store.go:134` re-checks
`OwnerLegalEntityID` on the way out. The record is simply unreachable by its
owner.

The grammar in this change does not fix it, because the grammar admits uppercase
deliberately (`spi.SystemTenantID` is `SYSTEM`; Cloud's local-issuer fallback is
`CYODA`).

**Fix.** `internal/domain/account/oidc_adapter.go` is the sole entry point to the
OIDC service — `internal/domain/account/handler.go:107-155` routes all seven
operations through it — and calls `tenantFromCtx` at `:148`, `:238`, `:311`,
`:345` and `:369`. A single helper canonicalises the caller's tenant to
`uuid.UUID.String()` once and is used at all five, returning the existing
`OIDC_INVALID_TENANT` (400) when the tenant is not a UUID at all. `Register`
already parses at `:155`; it stops passing the raw form on as
`RegisterInput.TenantID`.

**Behaviour change to record.** A non-UUID tenant such as `default-tenant`
currently receives an empty `200` from `GET /oauth/oidc/providers`, because its
prefix scan matches nothing. It now receives `400 OIDC_INVALID_TENANT` — the
same answer registration already gives it, and what that code's help topic
already documents. Returning an empty list implied a registration that could
never have succeeded.

### The token endpoint's 500 carries no ticket (#588)

Gate 3 requires every 5xx to carry a generic message plus a ticket UUID.
`writeTokenError` (`internal/auth/token.go:272`) emits only the RFC 6749 §5.2
pair, and the four `server_error` call sites (`token.go:78`, `:97`, `:211`,
`:230`) log nothing correlatable.

**Fix.** Mint a ticket, `slog.Error` it with the underlying error, and render
`server_error [ticket: <uuid>]` into `error_description` — the same shape
`internal/common/errors.go:296` already uses for `LevelInternal`. No OpenAPI
change: `error_description` is a declared string
(`api/openapi.yaml:9161`). The underlying error stays out of the response.

## Explicitly not in scope

- **`"<tenant>:<kid>"` in `internal/auth/kv_trusted_store.go:97`.** Safe already,
  and not for the reason the issue assumed: `kid` is globally unique across
  tenants — `s.keys` is keyed by kid alone (`:123`) and a cross-tenant kid is
  refused with 409 `KEY_OWNED_BY_DIFFERENT_TENANT` (`:475`). The grammar makes
  the parse unambiguous, which is a second and weaker guarantee. Re-keying would
  be a storage migration. A comment records that the uniqueness property is what
  carries it.
- **A shared validator in the SPI (issue item 4).** Resolved as unnecessary
  rather than deferred: the engine is the only caller of a storage plugin, so a
  plugin cannot be handed a tenant that did not pass door 1 or door 2. Putting
  the rule in the SPI would ask the storage layer to re-enforce an engine
  admission policy, and a backend pinned to an older SPI would enforce an older
  grammar — the divergence this change exists to remove.
- **CodeQL alert 92.** Re-checked after the rewrite. If the query still reports,
  it is dismissed as a false positive, never as unused — the memory backend
  ships in the binary and is what an unconfigured binary runs.
- **Cloud's own validation of `caas_org_id`** — tracked as CP-3968. That ticket
  also asks the one question the source cannot answer: whether any live legal
  entity already falls outside the grammar.
