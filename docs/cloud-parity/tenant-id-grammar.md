# Tenant-id shape — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

Two rules about the *shape* of a tenant identifier, which is why they share one
document. The first says which strings may be a tenant id at all. The second
says how a tenant id is compared once the surface that reads it has declared it
a UUID. The first is a **wire-contract tightening**: tokens that authenticate
today stop authenticating. The second is a **wire-visible status change** on the
OIDC management surface.

---

# Part 1 — the tenant-id grammar

## Rule

A tenant identifier is:

```
^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$
```

1 to 100 bytes. The first byte is an ASCII letter or digit. The rest are ASCII
letters, digits, `.`, `_` and `-`. Case is preserved and significant.

The definition lives in `internal/common/tenant_id.go` as `ValidateTenantID`,
a byte loop rather than a regex — it runs on every authenticated request.

### Why each clause

**Charset.** Every tenant-id literal shipped by either tier is drawn from
`[A-Za-z0-9_-]`. The dot is admitted because Cloud already declares that charset
on a neighbouring tenant-scoped identifier — `@Pattern("^[a-zA-Z0-9._-]{1,256}$")`
on edge-message subjects, `EdgeMessageController.kt:157` — and a domain-shaped
tenant is a plausible future. Admitting it now is cheaper than widening the
grammar later.

**First byte alphanumeric.** This is what makes `""`, `.`, `..`, `.hidden` and
`-leading` unrepresentable structurally rather than enumerated one by one. An
implementer who writes the rule as "reject `..`" has a different rule and will
find a different hole.

**Case significant.** Folding case would both reject real tenants and merge ones
that must stay distinct: `spi.SystemTenantID` is `SYSTEM`, and Cloud's
local-issuer fallback is `CYODA`.

**100 bytes.** This matches the only length Cloud declares on anything
tenant-shaped: `@ColumnValidation(maxLength = 100)` on `CSUser.legalEntityId`
(`CSUser.kt:45`), which Cloud's auto-enrollment already writes the `caas_org_id`
claim into. A longer tenant works in cyoda-go and fails to persist a user in
Cloud; capping at the same figure keeps the two tiers telling the caller the
same thing. The longest value either tier ships today is 48 bytes
(`conformance-<uuid>`).

Everything in use is admitted: `SYSTEM`, `CYODA`, `default-tenant`,
`mock-tenant`, `system-tenant`, `riskblocs`, `tenant-abc-123`,
`conformance-<uuid>`, canonical UUIDs, 32-character hex ids, and bare numerics.

## Where it is enforced

A tenant id enters cyoda-go from outside cyoda-go in exactly two places, and the
grammar is checked at exactly those two:

| Door | Surface | Failure |
| --- | --- | --- |
| The `caas_org_id` JWT claim | Every authenticated HTTP request and every authenticated gRPC method — gRPC delegates to the same authenticator | `401`, the uniform RFC 9457 problem detail |
| `CYODA_BOOTSTRAP_TENANT_ID` | Process startup, **only** when a bootstrap client is configured | Non-zero exit |

Everywhere else — peer dispatch bodies, scheduler RPC payloads, gossip
envelopes, scheduled-task rows, search-job rows, OIDC provider records, the
stored M2M client table — carries a value this cluster already admitted at one
of those two doors. Re-checking there would guard against a corrupted store or a
compromised peer, a threat model in which tenant-id spelling is not what saves
you.

Two consequences look like gaps and are not. The token endpoint needs no check:
it mints `caas_org_id` from a stored client row whose tenant came through door 1
or door 2, and any token it mints is presented back through door 1 before it can
do anything. The federated OIDC path needs no check: its tenant is a
`uuid.UUID`, structurally incapable of failing the grammar.

## Response

A claim outside the grammar is an **ordinary `401`** — the same uniform problem
detail as an expired token, an unknown `kid` or a bad signature. The caller
learns nothing from the difference. Over gRPC it is `codes.Unauthenticated` with
`Success=false` in the envelope. `api/openapi.yaml` already declares `401` on
every authenticated path, so the schema is unchanged.

A structured `slog.Warn` records the rejection with `reason=token-invalid`. The
detail carries the *reason* and a byte offset or length — **never the rejected
value**. An implementer must hold to that: the claim is attacker-chosen, and
echoing it into a log record is the log-injection the grammar exists to prevent.

A bad `CYODA_BOOTSTRAP_TENANT_ID` refuses to start, but only inside the branch
that actually provisions a bootstrap client. A deployment that configures no
bootstrap client is unaffected even when the variable is explicitly set to the
empty string.

## What the grammar is and is not for

It is **not** what stands between a token and a path traversal. The one place in
the product that built a filesystem path from a tenant id — the in-memory
backend's blob store — now encodes both segments as hex, so a traversing,
colliding or case-folding name is unrepresentable rather than rejected. The
grammar would be the wrong layer for that job anyway.

It earns its place on ordinary input-validation grounds, which are the reasons an
implementer should keep it even if their storage layer is safe:

- **Log injection.** The tenant is written verbatim into ~20 structured log
  fields; newlines and control bytes forge records.
- **Response injection.** The cluster dispatcher interpolates the raw tenant into
  a caller-visible error string.
- **Cross-tier consistency.** Cloud's 100-character user column, above.
- **Unbounded keys.** The model cache concatenates the tenant into a cache key
  with no length bound of its own.

## The Cloud side — CP-3968

**Cloud constrains `caas_org_id` nowhere today.** The claim is minted from
free-text Auth0 organization metadata, falling back to a generated 32-character
lowercase hex id, and becomes the `LegalEntity` `@Id` verbatim with no
transformation (`AbstractCaasOidcComponents.kt:158` → `AutoEnrollmentSupport.kt:14`).

The practical consequence of the asymmetry: a Cloud-issued token whose
`caas_org_id` falls outside the grammar is accepted by Cloud and answered with a
**silent `401`** by cyoda-go — indistinguishable from a bad signature, and with
nothing in the response to diagnose it. The tenant is not broken at the tier that
issued the token; it is broken only where it is used.

**CP-3968** carries the Cloud-side half:

1. Validate `caas_org_id` against this grammar where Cloud mints it, so a tenant
   outside the grammar is refused at enrollment with a diagnosable error rather
   than becoming an undiagnosable `401` later.
2. Answer the one question the source cannot: **whether any live legal entity is
   already outside the grammar.** Cloud has never constrained the value, so
   existing data is the only evidence. A tenant found outside it needs a
   migration decision before the grammar is enforced at the Cloud door.

### Carve-out: the audit sentinel

Cloud's audit trail uses `AuditActorInfoDto(legalId = "-")` as a
no-attributable-actor sentinel. A bare `-` fails the first-byte rule. It is
**not** in scope: that value is an audit-record field and does not reach
`caas_org_id`, so the grammar never sees it. It is recorded here so that a later
reader who finds `"-"` in the Cloud source does not read it as a
counter-example — and so that anyone tempted to route it into a tenant field
knows that doing so would break the contract.

---

# Part 2 — a tenant is compared as a UUID value, not as text

## Rule

On the OIDC provider surface, where the data model types the owner as a UUID
(`JWKOIDCEntity.ownerLegalEntityId` in Cloud, `OwnerLegalEntityID uuid.UUID`
here), the caller's tenant is **parsed as a UUID and compared by value**. Two
spellings of the same UUID address the same providers:

```
1A2B3C4D-5E6F-7080-9A0B-C1D2E3F4A5B6
1a2b3c4d-5e6f-7080-9a0b-c1d2e3f4a5b6
{1a2b3c4d-5e6f-7080-9a0b-c1d2e3f4a5b6}
urn:uuid:1a2b3c4d-5e6f-7080-9a0b-c1d2e3f4a5b6
```

Storage keys by the canonical lowercase form. Canonicalisation happens once, at
the single entry point to the OIDC service, so every operation — register, get,
list, update, invalidate, reactivate, delete — addresses the same key.

This is compatible with Part 1 by design: the grammar admits uppercase
deliberately, so it does not and must not be relied on to normalise a UUID.

## The wire-visible change

A tenant that is **not a UUID in any accepted spelling** now receives
`400 OIDC_INVALID_TENANT` from **every** OIDC provider operation that takes a
tenant, including the list one:

| Operation | Before | Now |
| --- | --- | --- |
| `POST /oauth/oidc/providers` | `400 OIDC_INVALID_TENANT` | unchanged |
| `GET /oauth/oidc/providers` | **empty `200`** | `400 OIDC_INVALID_TENANT` |
| `GET`/`PATCH`/`DELETE /oauth/oidc/providers/{id}` and the invalidate/reactivate forms | `404` | `400 OIDC_INVALID_TENANT` |
| `POST /oauth/oidc/providers/reload` | `200` | unchanged — tenant-independent |

The empty `200` was the defect worth naming: it reported "you have no providers"
to a tenant that could never have had one, because its prefix scan matched
nothing. `400` is the same answer registration already gave, delivered at the
point the caller asks. `api/openapi.yaml` and the `OIDC_INVALID_TENANT` help
topic previously described the code as registration-only; both now describe the
whole surface.

## Why an implementer should care about the spelling half

Keying writes by the canonical form and reads by the caller's raw string is not
merely an unreachable record. In cyoda-go it made registration itself fail: the
post-write index read-back missed the entry it had just written, the service
rolled the registration back, answered `500`, and the rollback — keyed the same
wrong way — left the provider blob behind. A tenant spelled in any non-canonical
UUID form could not register a provider at all.

Token validation was never affected on either tier, because the provider
registry is built from the *stored* provider's own owner id, which is already
canonical. Only the management API strands. That asymmetry is what makes the
defect easy to ship and hard to notice.

## Test surface

- `internal/common/tenant_id_test.go` — the grammar's accept/reject table,
  including every tenant constant the binary ships with, so a later change to a
  default cannot quietly produce an unbootable binary.
- `internal/auth/validator_test.go`, `internal/grpc/interceptor_test.go` — a
  hostile claim is a uniform rejection on both entry points, and the rejected
  value reaches neither a log field nor a response body.
- `internal/e2e/auth_failures_test.go` — `401` over real HTTP for a claim outside
  the grammar, and the accepted set still authenticating.
- `app/app_bootstrap_test.go` — door 2: a bad `CYODA_BOOTSTRAP_TENANT_ID` refuses
  to start; an empty one with no bootstrap client starts.
- `internal/e2e/oidc_providers_test.go`, `internal/domain/account/oidc_adapter_test.go`
  — a non-canonically-spelled UUID tenant registers, lists, updates and deletes
  the same provider; a non-UUID tenant gets `400 OIDC_INVALID_TENANT` from every
  operation.
- `e2e/parity/oidc.go` — `RunOidcInvalidTenantUUIDRejected_Skip` records why this
  one is not a cross-backend scenario: the parity fixture's tenants are always
  UUID-shaped, because its HTTP server requires real JWTs. Both behaviours are
  engine-level and reach no storage backend, so a parity scenario would buy no
  backend signal.
