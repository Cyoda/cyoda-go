# Tenant-id shape — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

The rule about the *shape* of a tenant identifier: which strings may be a
tenant id at all. It is a **wire-contract tightening**: tokens that
authenticated before stop authenticating. A second rule, on the spelling a
tenant had to have on the OIDC provider surface, is retired with that surface
(see Part 2).

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

Everything in use is admitted: `SYSTEM`, `CYODA`,
`mock-tenant`, `system-tenant`, `riskblocs`, `tenant-abc-123`,
`conformance-<uuid>`, canonical UUIDs, 32-character hex ids, and bare numerics.

## Where it is enforced

A tenant id enters cyoda-go from outside cyoda-go in exactly one place, and
the grammar is checked there:

| Door | Surface | Failure |
| --- | --- | --- |
| The `caas_org_id` JWT claim | Every authenticated HTTP request and every authenticated gRPC method — gRPC delegates to the same authenticator | `401`, the uniform RFC 9457 problem detail |

`cyoda token --tenant`, which signs an admin token offline with the signing
key, checks the same grammar before it signs (exit code `2`); the claim is
checked again at the door when the token is used. No configuration variable
carries a tenant id.

Everywhere else — peer dispatch bodies, gossip envelopes, scheduled-task rows,
search-job rows — carries a value this cluster already
admitted at that door. Re-checking there would guard against a corrupted store
or a compromised peer, a threat model in which tenant-id spelling is not what
saves you. The stored M2M clients are the one exception, and an internal one:
a stored tenant id names the storage namespace of a client's record, so
cyoda-go checks it again whenever it decodes a stored client and treats a
failing one as damaged data. That check adds nothing to the contract.

One operator-facing tenant value is covered by a test rather than by the check:
`cfg.IAM.MockTenantID` becomes the tenant of every request in the non-JWT IAM
mode. It has no environment binding — it is a literal in `DefaultConfig()`, so
it cannot be supplied from outside the binary and is not an ingress — and a
unit test pins it, along with every other shipped tenant constant, against the
grammar, so a later change to the literal cannot quietly produce a binary whose
own default tenant is unrepresentable.

Two consequences look like gaps and are not. The token endpoint needs no check:
it mints `caas_org_id` from a stored client row whose tenant is the tenant of
the admin who created the client, admitted at the door, and any token it mints
is presented back through the door before it can do anything. The token
exchange takes the same stored tenant, and refuses an assertion whose
`caas_org_id` differs from it.

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
- **Unbounded keys.** The model cache keys a map by the tenant with no length
  cap of its own, and the gossip payload that carries an eviction JSON-encodes
  the same field.

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
2. Enforce it at enrollment as well, so the grammar governs how a legal entity
   is created and not only how a token is read.

There is no third ask. Cloud has not been released and has no live deployment,
so there is no existing legal entity that could already fall outside the
grammar, and no migration to plan. The grammar can be enforced at the Cloud door
as soon as it is implemented — this is the cheapest moment it will ever be, and
the reason to do it now rather than after the first tenant exists.

### Carve-out: the audit sentinel

Cloud's audit trail uses `AuditActorInfoDto(legalId = "-")` as a
no-attributable-actor sentinel. A bare `-` fails the first-byte rule. It is
**not** in scope: that value is an audit-record field and does not reach
`caas_org_id`, so the grammar never sees it. It is recorded here so that a later
reader who finds `"-"` in the Cloud source does not read it as a
counter-example — and so that anyone tempted to route it into a tenant field
knows that doing so would break the contract.

---

# Part 2 — retired

Part 2 required a canonically spelled UUID tenant on the OIDC provider
surface. That surface, its `OIDC_INVALID_TENANT` error and its store are
removed (`obo-only-user-identity.md`), so the rule has nothing left to
govern. Cloud needed no change for it, and needs none now.

## Test surface

- `internal/common/tenant_id_test.go` — the grammar's accept/reject table,
  including every tenant constant the binary ships with, so a later change to a
  default cannot quietly produce an unbootable binary.
- `internal/auth/validator_test.go`, `internal/grpc/interceptor_test.go` — a
  hostile claim is a uniform rejection on both entry points, and the rejected
  value reaches neither a log field nor a response body.
- `internal/e2e/auth_failures_test.go` — `401` over real HTTP for a claim outside
  the grammar, and the accepted set still authenticating.
- `cmd/cyoda/token_test.go` — `cyoda token` refuses a tenant outside the
  grammar with exit code `2` and signs nothing.
