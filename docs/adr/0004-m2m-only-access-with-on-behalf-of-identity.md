# 0004. M2M-Only Access with On-Behalf-Of Identity

**Status:** Accepted
**Date:** 2026-10-02
**Supersedes:** 0002 (Federated Identity Provider Architecture)

## Context

cyoda-go is a database. Its callers are applications, and the people who use
those applications never call it directly. Two questions follow: who may call
cyoda-go, and how does a change made for a person record that person?

ADR 0002 answered the first question by federating identity: a tenant
registered OpenID Connect providers, and cyoda-go accepted the providers'
user tokens next to its own. That put cyoda-go in the business of deciding
what a person may do. It had no per-user permission model to make that
decision with — only the roles the provider's token named — so every user
token carried a tenant-wide role set, and cyoda-go could not tell a user's
request from the application's. It also made every provider a trust anchor
for the whole cluster: a JWKS fetch on the request path, a per-node copy of
every provider's keys, a broadcast to evict it, an SSRF surface on
registration, and a shared namespace of key ids that one provider could use
to shadow another tenant's.

The application already knows who its user is and what the user may do,
because it signed the user in and holds the business rules. What cyoda-go
must do is keep tenants apart, enforce what each calling application may do,
and record faithfully which application acted and for which user — in the
entity history, the audit trail and every callout to a compute node, where
processors apply rules such as "the submitter may not approve".

Constraints: correctness over availability (no fallback identity stands in
for a missing one); tenant isolation on every path; cluster mode is the
primary target, so identity must survive a callout handed over to another
node and a scheduled transition that fires later on any node.

## Decision

1. **Only M2M clients reach cyoda-go.** A client belongs to one tenant, holds
   a secret, and gets tokens from `POST /oauth/token`. A plain client holds
   `ROLE_M2M`, which every data operation requires; an admin client also
   holds `ROLE_ADMIN`. The one other token source is the platform operator's
   offline `cyoda token`, signed with `CYODA_JWT_SIGNING_KEY`, whose holder
   could sign any token anyway. The federated-provider subsystem is removed.

2. **The application authorizes; cyoda-go records.** cyoda-go has no
   per-user permissions. An application that acts for a user does so through
   an **on-behalf-of (OBO) client**: a client created with that permission,
   which may only perform the OAuth 2.0 token exchange (RFC 8693). The
   application signs a short user assertion (RS256, at most 300 s, `aud` =
   the cyoda issuer) with a **trusted key** it registered in its tenant, and
   the OBO client exchanges it. The issued token names the user as its
   subject and the client in `act`, and carries the client's roles — never
   roles from the assertion.

3. **Identity is recorded, not verified.** cyoda-go checks that the assertion
   is signed by a key of the client's own tenant and names a valid user id,
   and nothing more. Every change, audit event, callout and message records
   two principals: the attributed principal (who the work is for) and the
   executor (who made it). For an OBO request they are the user and the
   client; for a client's own request, the client twice; for a compute
   node's write-back inside a user's transaction, that user and the compute
   client; for a scheduled firing, the principal that armed it and `system`.

4. **An OBO principal acts on data only.** It never administers clients,
   keys or the platform, never opens a compute stream, and joins only a
   transaction begun for its own user. An OBO client never holds
   `ROLE_ADMIN` and never exists in the `PLATFORM` tenant.

5. **Revocation is bounded by the token lifetime.** Token issuance reads the
   client and the trusted key from the shared store on every request, so a
   delete, a secret reset or a key invalidation stops new tokens at once on
   every node. Issued tokens end within `CYODA_JWT_EXPIRY_SECONDS` (default
   300 s, at most 3600 s); a compute stream re-checks its client every 60 s.

## Consequences

**Positive.**

- The trust boundary is one key set: every token cyoda-go accepts was signed
  by its own key pairs, verified in-process with no network call on the
  request path. The OIDC registry, its node copy, broadcast, reconcile and
  SSRF controls, and the cross-tenant key-id contention they carried, are
  gone.
- Tenant binding is structural: both grants take the tenant from the stored
  client, so no token can name another tenant, and `PLATFORM` is reachable
  only through the signing key or a client a platform admin created there.
- The audit trail answers "who, for whom" on every path, including cascades,
  hand-overs between nodes and deferred work, and compute nodes receive both
  principals to enforce segregation-of-duties rules.
- A trusted private key and a plain client together cannot mint
  `ROLE_ADMIN` or an operator token: the exchange carries the client's roles
  and is open to OBO clients only.

**Negative.**

- cyoda-go cannot protect data from a compromised application: an
  application holding an OBO client and a trusted key can act as any user of
  its tenant, with that client's rights. This is the same boundary a
  database has with its application, and it is stated as such.
- Applications that forwarded identity-provider tokens must change: split
  their client into an OBO client, a plain client per compute node and an
  admin client; register a trusted key; sign assertions and cache one OBO
  token per user.
- Token-endpoint load grows with active users (one exchange per user per
  token lifetime). It is bounded per node: a verified-secret cache spares
  repeat bcrypt checks, concurrent checks are capped, and each client has a
  token bucket.

**Neutral.**

- The callout wire gains `authexecid` / `authexectype`; `authid` /
  `authtype` name the attributed principal and `authclaims` the executor's
  roles. Compute nodes that read `authid` as the caller read `authexecid`.
- The SPI gains `UserContext.Executor`, and `AttributionFor` honours it
  ahead of transaction-origin inheritance.

## Alternatives considered

- **Keep federation, add per-user permissions in cyoda-go.** Rejected: it
  duplicates the application's authorization model inside the database,
  where it would drift from the application's, and keeps every identity
  provider as a cluster-wide trust anchor.
- **Accept trusted-key JWTs as bearer tokens.** Rejected: the token's claims,
  written by the key holder, would then decide the tenant and the roles, so a
  key holder with credentials in a second tenant could mint any principal
  there.
- **A caller-supplied user header (`X-User-ID`) on each request.** Rejected:
  it must be remembered on every request and every entry point, HTTP and
  gRPC, and is forgotten silently; the OBO token carries the user in the
  signed credential itself, so no request can lose it.
- **Carry the assertion's roles into the issued token.** Rejected: a trusted
  key would then be able to mint `ROLE_ADMIN` or an operator principal.
- **Verify the user against a directory.** Rejected: cyoda-go holds no user
  directory, and the application, which signed the user in, is the
  authority on who the user is.
