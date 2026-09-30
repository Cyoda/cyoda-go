---
topic: config.auth
title: "auth configuration"
stability: stable
see_also:
  - config
  - run
---

# config.auth

## NAME

config.auth — IAM mode, JWT settings, identifier rules, HMAC secret, IAM features and signing-key rotation.

## SYNOPSIS

cyoda supports two IAM modes: `mock` (development) and `jwt` (production). Configure the
mode via `CYODA_IAM_MODE`. Use `CYODA_REQUIRE_JWT` as a production safety guard to refuse
startup unless JWT mode is properly configured.

## OPTIONS

### IAM mode

- `CYODA_IAM_MODE` — authentication mode: `mock` or `jwt` (default: `mock`). Any
  other value, including a different case, fails startup.
- `CYODA_REQUIRE_JWT` — production safety floor. When `true`, the binary refuses to
  start unless `CYODA_IAM_MODE=jwt` *and* `CYODA_JWT_SIGNING_KEY` are both set.
  Prevents accidentally deploying with mock auth enabled. The canonical Helm chart
  enables this by default. Desktop and Docker leave it off so the mock-auth fallback
  still applies to evaluators. (default: `false`)

### Mock mode (`CYODA_IAM_MODE=mock`)

- `CYODA_IAM_MOCK_ROLES` — comma-separated default user roles assigned to all requests
  in mock mode (default: `ROLE_ADMIN,ROLE_M2M`)
- `CYODA_IAM_MOCK_KIND` — principal kind assigned to the default UserContext in
  mock mode: `user`, `service`, or `system`. Lets local/CI setups exercise
  service- or system-attributed code paths without standing up real JWT auth.
  (default: `user`)

When running in mock mode, the binary emits a prominent `MOCK AUTH IS ACTIVE`
warning banner at startup so operators see the security posture of the running
instance. `CYODA_SUPPRESS_BANNER=true` silences both the startup banner and the
mock-auth warning. It is intended only for CI/test harnesses where the warning
is noise — never set it in production, since the banner is the only in-process
signal that requests are unauthenticated.

### JWT mode (`CYODA_IAM_MODE=jwt`)

- `CYODA_JWT_SIGNING_KEY` — RSA private key in PEM format; required in jwt mode.
  Also derives the key that encrypts stored signing key pairs, so treat it as
  the root secret. Replacing it retires every issued key pair sealed by the
  wrapped vault (see *JWT signing keypair rotation*).
- `CYODA_JWT_SIGNING_KEY_FILE` — file path for `CYODA_JWT_SIGNING_KEY` (takes precedence)
- `CYODA_JWT_ISSUER` — JWT issuer claim (`iss`). Unset means the default; an
  empty value stops the server at startup and makes `cyoda token` exit 1.
  (default: `cyoda`)
- `CYODA_JWT_AUDIENCE` — required audience claim (`aud`) on inbound JWTs,
  also set as `aud` on every token cyoda-go issues (`POST /oauth/token`, both
  grants, and `cyoda token`); empty string disables the audience check and
  issued tokens carry no `aud` (default: empty)
- `CYODA_JWT_EXPIRY_SECONDS` — token lifetime in seconds, and the upper bound
  of `cyoda token --ttl`. Unset or empty means the default. Otherwise it must
  be an integer from 1 to 31622400 (366 days); any other value stops the
  server at startup and makes `cyoda token` exit 1. (default: `3600`)
- `CYODA_JWT_BOOTSTRAP_AUDIENCE` — audience for the bootstrap signing key
  derived from `CYODA_JWT_SIGNING_KEY`. Must be `client` or `human`. The
  M2M token-issuance path (`POST /oauth/token`) always uses the
  client-audience key. Set to `human` only in deployments where M2M token
  issuance is disabled and the bootstrap key signs human tokens through
  an external flow. (default: `client`)

### Tenant identifiers

A tenant identifier must match:

```
^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$
```

1 to 100 bytes; the first an ASCII letter or digit; the rest letters, digits,
`.`, `_` and `-`. Case is preserved and significant — `Acme` and `acme` are two
tenants.

The rule is checked at the one place a tenant identifier enters the binary
from outside it: **the `caas_org_id` claim on an inbound JWT**, which covers
every authenticated HTTP request and every authenticated gRPC method. A claim
outside the grammar is rejected like any other bad token: `401` with the
uniform problem detail, and nothing in the response distinguishing it. The
server log records the rejection at request time — one warning per rejected
request, carrying the reason and a byte offset, never the offending value. If
you mint tokens from an external IdP, constrain the claim there — an
identifier outside this grammar is only diagnosable from cyoda's own logs.
`cyoda token --tenant` checks the same rule before it signs, and the claim is
checked again when the token is used.

Peer dispatch, scheduled tasks and search jobs carry a value already
admitted at that door and do not re-check it. Stored M2M clients are the
exception: a stored tenant id names the storage namespace of a client's
record, so it is checked again whenever a stored client is decoded, and one
whose tenant id fails the check is treated as damaged (see `auth.clients`).

### User identifiers

A user identifier must be valid UTF-8, 1 to 255 characters (not bytes) long,
and contain none of these:

- a control character: U+0000–U+001F and U+007F–U+009F;
- a noncharacter: U+FDD0–U+FDEF, and U+FFFE and U+FFFF in every plane;
- U+FFFD, the replacement character.

Any other character is admitted, including non-ASCII, and nothing is
normalised. A user id is not a key or a path segment, so it has no grammar
beyond this and the reserved word `oidc:` described below. The excluded characters are the ones the CloudEvents spec forbids
in a string attribute — a user id is sent to compute nodes as `authid` — plus
U+FFFD, which a JSON decoder puts in place of every invalid byte, so that two
different claims can never name one user.

The same check applies at every place a principal's user id enters the binary
from outside it:

- **The user claim on an inbound first-party JWT** — `caas_user_id`, or `sub`
  when `caas_user_id` is absent. A claim outside the check is an ordinary
  `401`, logged like the tenant claim above: the reason, and for a rejected
  character its code point and position, never the value. A `caas_user_id`
  that is present but empty, not a string, or outside the check is rejected;
  it does not fall back to `sub`.
- **The `sub` of a federated OIDC token.** The principal's user id is then
  `oidc:<providerId>:<sub>`, so it can be longer than 255 characters; the
  limit applies to `sub`.
- **The `sub` of a token-exchange subject token**, which becomes the issued
  token's user id. A value outside the check is `400 invalid_grant`.

`cyoda token --user` checks the same rule, the reserved word below included,
before it signs.

**`oidc:` is a reserved word.** The OIDC path builds every user id it creates
as `oidc:<providerId>:<sub>`. Every other user id — the first-party claim and the
token-exchange `sub` — must not begin with
`oidc:`, in any case, so that it can never name the same user as an OIDC
principal. Such a value is rejected at the door like any other bad user id.

### HMAC secret (inter-node dispatch authentication)

- `CYODA_HMAC_SECRET` — hex-encoded HMAC secret for inter-node dispatch auth
- `CYODA_HMAC_SECRET_FILE` — file path for `CYODA_HMAC_SECRET` (takes precedence)

### First admin token

No credential is defined by configuration except the signing key. In jwt
mode, the first admin token comes from `cyoda token`, which signs a
short-lived token with `CYODA_JWT_SIGNING_KEY`; use it to create the M2M
clients that applications and compute nodes use (`POST /clients`). See
`cyoda help cli token`.

### IAM features

These environment variables tune the IAM admin endpoints under `/oauth/keys/*` and `/clients`.

- `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` — gates all 5 endpoints under
  `/oauth/keys/trusted/*`. When `false`, every trusted-key endpoint returns
  `404 FEATURE_DISABLED`. (default: `false`)
- `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` — gates the `withAdminRole=true`
  query parameter on `POST /clients`. When `false` (default), that request
  shape returns `404` with error code `FEATURE_DISABLED` and no client is
  created. When `true`, the created M2M client receives both `ROLE_M2M`
  and `ROLE_ADMIN`. Toggling does not affect existing clients. (default: `false`)
- `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` — per-tenant cap on M2M clients.
  `POST /clients` at the cap returns `400` with error code
  `M2M_CLIENT_CAP_REACHED`. `0` means unbounded; a negative value refuses to
  start. Creates on several nodes at the same moment can each pass the check,
  so a tenant can exceed the cap by at most one client per node.
  (default: `100`)
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` — per-tenant cap on trusted keys
  that can verify. It counts an active key, and one in its grace period after
  invalidation until its `validTo`. `0` means unbounded. (default: `10`)
- `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` — default validity for trusted
  keys when the registration request omits `validTo`. No clamp on
  user-supplied `validTo` values. (default: `365`)
- `CYODA_IAM_TRUSTED_KEY_MAX_JWK_PROPERTIES` — caps the number of properties
  in a registered JWK to guard against absurdly large payloads. (default: `20`)
- `CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` — default validity of a key pair
  issued via `POST /oauth/keys/keypair` when the request omits `validTo`. A
  key pair signs and verifies tokens only inside its window, from
  `validFrom` to `validTo`: one issued with a future `validFrom` is published
  in JWKS but not used until then, and once `validTo` passes, tokens it
  signed are rejected. The bootstrap signing key has no window unless the
  key-pair API invalidates or reactivates it; that state is stored and shared
  by the cluster. (default: `365`)

### Auth cache reconciliation

All three per-node auth caches (trusted keys, signing key pairs, OIDC
providers) push updates to peers on write and additionally run a periodic
KV-reconcile as a backstop against missed broadcasts. `CYODA_AUTH_CACHE_RECONCILE_INTERVAL`
sets that shared interval; each tick is jittered ±10% to avoid a cross-node
reconcile herd. A cache that goes 10× this interval without a successful
reconcile fails closed on verification rather than serving a potentially stale
answer. A stale signing-key cache refuses first-party tokens (`401`) and
answers JWKS with `503`.

- `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` — reconcile interval for all three caches
  (default: `60s`, floor: `1s`)

### Federated OIDC providers (`POST /oauth/oidc/providers`)

These variables control the federated OIDC provider registration behaviour
(JWT mode only). They apply to all tenants at the process level.

- `CYODA_OIDC_REQUIRE_HTTPS` — when `true`, `POST /oauth/oidc/providers` rejects
  any `wellKnownConfigUri` whose scheme is not `https`. Set to `true` in
  production to prevent accidental registration of plaintext-HTTP providers.
  (default: `true`)
- `CYODA_OIDC_ALLOW_PRIVATE_NETWORKS` — when `true`, the SSRF blocklist check is
  bypassed so private-network OIDC providers (e.g. `https://127.0.0.1/...`) can
  be registered. Intended for integration tests and local development only.
  Never set in production. (default: `false`)
- `CYODA_OIDC_ROLES_CLAIM` — the JWT claim name from which role values are read
  for tokens issued by a federated OIDC provider. Overrideable per-provider via
  the `rolesClaim` field on the registration or update API. See
  `cyoda help auth oidc` for the accepted claim value shapes (string array,
  JSON object — keys are roles, space-delimited string). (default: `roles`)
- `CYODA_OIDC_CONNECT_TIMEOUT_MS` — TCP connect timeout in milliseconds for
  OIDC discovery and JWKS endpoint fetches. (default: `5000`)
- `CYODA_OIDC_SOCKET_TIMEOUT_MS` — HTTP read timeout in milliseconds for
  OIDC discovery and JWKS endpoint fetches. (default: `5000`)
- `CYODA_OIDC_CONNECTION_REQUEST_TIMEOUT_MS` — connection-pool request timeout
  in milliseconds for OIDC discovery and JWKS endpoint fetches. (default: `5000`)

### JWT signing keypair rotation

The bootstrap signing key derived from `CYODA_JWT_SIGNING_KEY` (or
`CYODA_JWT_SIGNING_KEY_FILE`) is the default signing key for the
`POST /oauth/token` flow. Its KID is deterministic across nodes sharing
the same PEM (SHA-256 of the public key).

The key-pair endpoints (`/oauth/keys/keypair*`) need a platform operator:
`ROLE_ADMIN` in the tenant `PLATFORM`. An admin of any other tenant gets
`403 FORBIDDEN`. Get an operator token with `cyoda token --tenant PLATFORM`
(see `cyoda help cli token`), or create an admin M2M client in `PLATFORM`.

Operators can rotate signing keys at runtime via
`POST /oauth/keys/keypair` (with `algorithm: RS256` and `audience: client`).
Of the active key pairs of an audience inside their window, the bootstrap
key included, the one with the latest `validFrom` signs new tokens (on a tie,
the greater key id). The bootstrap key takes part with a zero `validFrom`
until a reactivation sets one. Until then, an active issued key pair inside
its window signs before it, and the bootstrap key signs whenever no issued key
pair of its audience is active and inside its window. A reactivation sets the
bootstrap key's `validFrom` to the request's value, which defaults to now:
from then on it signs before every issued key pair of its audience with an
earlier `validFrom`.

Setting `invalidateCurrent: true` also invalidates the issued key pairs of the
audience that have not ended (including one issued ahead of time), the first
rotation included. A rotation invalidates issued key pairs only: the
bootstrap key is never one of them, stays active, keeps verifying, and
`cyoda token` keeps working. Only an invalidate or a `DELETE` that names the
bootstrap key's key id ends it.

An invalidated key pair — issued, or the bootstrap key — never signs again
unless reactivated. Tokens it signed keep verifying until the end of its
grace period: `invalidateGracePeriodSec: N` on a rotation, or
`gracePeriodSec: N` on `POST /oauth/keys/keypair/{keyId}/invalidate`, sets
its `validTo` to N seconds from now, never later than its current
`validTo`. The default is 0: it stops verifying at once on the node that
takes the call, and on each other node once that node applies the change
(see *Shared and persisted* below), plus the clock offset between nodes: the
node that takes the call stamps `validTo` from its own clock, and each node
checks it against its own. JWKS publishes a key pair from its
issue (ahead of its window, if `validFrom` is in the future) until it can
no longer verify. A node that has not yet applied an invalidation can still
sign with the key pair until it does; with a grace period, those tokens
verify on every node until the key pair's `validTo`.

**Emergency revocation of a leaked token:** revoke the key pair named by the
`kid` in the token's header. A rotation is not enough: it never ends the
bootstrap key, which signs every token from `cyoda token`, and every token
from `POST /oauth/token` while it wins signer selection for its audience
(before the first rotation, for example, or after a reactivation with the
default `validFrom`; see above).

- If the `kid` names an issued key pair, invalidate it with a grace period of
  0, or `DELETE` it, or rotate with `invalidateCurrent: true` and
  `invalidateGracePeriodSec: 0`. If it signs and no other key pair of its
  audience is active and inside its window (with the bootstrap key revoked,
  it may be the only one), invalidating or deleting it leaves no signer (see
  *No signer* below), so use the rotation instead.
- If the `kid` names the bootstrap key, invalidate it with a grace period of
  0. If `cyoda token` is still wanted, reactivate the bootstrap key once the
  longest token lifetime in use has passed since the later of two times:
  every node having applied the invalidation (see *Shared and persisted*
  below), and the last `cyoda token` run. A token signed after the
  invalidation also verifies again on reactivation. The wait is the
  largest `CYODA_JWT_EXPIRY_SECONDS` among the servers and among every place
  `cyoda token` ran (its `--ttl` is capped by the value where it ran), plus
  30 seconds for the clock-skew allowance token validation applies, plus a
  margin for the clock offset between hosts: `exp` comes from the signing
  host's clock and is checked on the verifying node's clock. By then every
  token the bootstrap key signed has expired. Reactivating it sooner makes
  those tokens verify again. Pass an early `validFrom` on the reactivation,
  for example `1970-01-01T00:00:00Z`, so that the issued key pairs of its
  audience keep signing `POST /oauth/token`. With the default `validFrom`
  (now), the bootstrap key signs before them, and `POST /oauth/token` signs
  with it again. `DELETE` also ends the bootstrap key, but permanently.

A grace period already running is cut short by invalidating the key pair again
with 0, or by `DELETE`; a deleted key pair never verifies.

`invalidateCurrent` cannot be combined with a future `validFrom`,
and `validTo` must be in the future; both are `400`. Reactivating a key pair
also refuses a future `validFrom`. A `validFrom` or `validTo` (the default
included) whose UTC year is outside 1–9999 is `400` on every key-pair and
trusted-key endpoint, and so is a key-pair `keyId` that is not 32 lowercase
hex characters. To schedule a rotation,
issue the new key pair ahead of time, then invalidate the old one once the
new window has opened.

- **Shared and persisted.** Key pairs, and changes to the bootstrap key's
  state, are stored and apply on every node; they survive restarts (not on the
  memory backend). The node that takes the call applies the change before
  answering; other nodes apply it when the change message arrives (normally
  under a second), at the latest after up to 1.1× the reconcile interval (66 s
  by default — the periodic re-read timer is jittered ±10%), plus however
  long that re-read itself takes; a node that cannot read its database keeps
  its last copy until it is stale (10 intervals), then refuses all keys.
- **Rotating without refusals in a cluster.** Issue the new key pair with
  `validFrom` a few seconds ahead, then invalidate the old one once the new
  window has opened; a token signed with a brand-new key can otherwise be
  refused by a node that has not yet received the change.
- **Replacing `CYODA_JWT_SIGNING_KEY`** retires every issued key pair sealed
  by the wrapped vault: they stop signing, verifying and appearing in JWKS,
  and the new bootstrap key signs. Restoring the old key brings them back. A
  key pair broken because its vault kind is unrecognised is not sealed by
  this node's wrapped vault and stays broken regardless (see the broken-key
  recovery below).
- **If `CYODA_JWT_SIGNING_KEY` may be exposed:** generate a new key; update
  the secret for every node; restart every node. Tokens signed by the old key
  or by issued key pairs stop verifying; clients fetch new tokens. Issue new
  key pairs if you use API rotation. Invalidating or deleting the bootstrap
  key through the API does not protect stored key pairs: the key still
  decrypts them. A rotation does not help either, for the same reason.
- **Invalidating or deleting the bootstrap key** revokes the root key:
  tokens from `cyoda token` are refused after the grace period, if one was
  given, and the node that takes the call logs a WARN that says so. The
  platform operator's ways back are then an admin M2M client in `PLATFORM`
  created beforehand while an issued key pair signs its tokens, or a new
  `CYODA_JWT_SIGNING_KEY` on every node (see `cyoda help cli token`). Store
  that client's secret like `CYODA_JWT_SIGNING_KEY`. If it may have leaked,
  follow *A leaked platform admin-client secret* below.
- **Deleting the bootstrap key is permanent** for that key: it cannot be
  reactivated; replacing `CYODA_JWT_SIGNING_KEY` starts a fresh bootstrap key
  with no stored state — it does not undelete the old one.
- **No exportable signing key:** a KMS-backed key vault for issued key pairs,
  with the bootstrap key deleted once an issued key signs, is supported by the
  design; no KMS vault ships yet.

#### A leaked platform admin-client secret

Assume whoever holds the secret can act as a platform operator until this
procedure has taken effect on every node: they can create, reset and delete
`PLATFORM`'s M2M clients and trusted keys, issue, invalidate and reactivate
key pairs, and change each node's log level and trace sampler. Verification
does not check that a client still exists, so deleting the client or
resetting its secret does not end a token they hold. Contain first, then
clean up, verify and restore.

**1. Contain.** Block client traffic to the HTTP and gRPC APIs at the load
balancer or network edge, leaving the traffic between nodes open, so that
only you can reach the nodes (for example from inside the cluster). This
stops service for every tenant. Keep the block until step 6.

**2. Get in** with a platform-operator token: `cyoda token --tenant
PLATFORM` while the bootstrap key verifies, an unexpired operator token
you hold, or `/oauth/token` with an admin client of `PLATFORM` whose
secret you hold, the leaked one included. If none works, use the new
signing key below.

**3. Clean `PLATFORM`.**

- On every node, check `GET /admin/log-level` and
  `GET /admin/trace-sampler`, and undo any change you did not make. Both
  are per node. At a level above `info`, the lines step 5 reads are not
  written.
- Delete every M2M client you do not need (`DELETE /clients/{clientId}`).
- Reset the secret of every client you keep
  (`PUT /clients/{clientId}/secret`), the leaked one too if you keep it.
  A reset by the attacker shows only in `lastUpdateDate` and a log line.
- After step 4 you need an admin client in `PLATFORM` whose new secret
  you hold. If you keep none, create one
  (`POST /clients?withAdminRole=true`, which needs
  `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true`; the tenant client cap applies)
  and store its `client_id` and `client_secret`. If you cannot, use the
  new signing key below.
- Trusted keys. A trusted key of `PLATFORM` and any client of the tenant
  exchange for a token with the roles the subject token names. Keys
  registered while `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` was on
  still verify exchanges when it is off, but the trusted-key endpoints
  then answer `404 FEATURE_DISABLED`. If registration is on, or was on at
  any time since the secret could have leaked, turn it on for the node you
  call (it is read at startup), list the keys (`GET /oauth/keys/trusted`)
  and delete every key you did not register.

**4. End every outstanding token.**

- Rotate: `POST /oauth/keys/keypair` with `algorithm: RS256`,
  `audience: client`, `invalidateCurrent: true` and
  `invalidateGracePeriodSec: 0`. This invalidates every issued `client`
  key pair that has not ended, including one issued ahead of time, and
  the new pair signs. Do not invalidate or delete the signing pair
  instead: that can leave no signer when no other `client` key pair is
  active and inside its window (see *No signer* below).
- Once every node has applied the rotation (see *Shared and persisted*
  above), get a token from `/oauth/token` with your admin client.
  `GET /oauth/keys/keypair/current?audience=client` must name the new
  pair.
- Invalidate with `gracePeriodSec: 0` every other key id that
  `/.well-known/jwks.json` lists. This includes the bootstrap key if it
  is listed and its audience is `client`, which ends `cyoda token` (to
  bring it back later, see *Emergency revocation of a leaked token*
  above). You may keep `human` key pairs you issued yourself, since
  nothing signs a `/oauth/token` token with a `human` key; JWKS does not
  show a key's audience, so keep only key ids you know. Do not invalidate
  the pair `/current` names.
- Every tenant's M2M and token-exchange tokens stop verifying, and
  clients fetch new ones.

**5. Verify on every node,** once every change has applied everywhere (see
*Shared and persisted* above), plus the clock offset between nodes (see
the grace period above):

- JWKS lists only the new pair and the `human` key pairs you kept.
- `/current?audience=client` names the new pair.
- `GET /clients` shows only the clients you kept or created, each with a
  `lastUpdateDate` from your own change.
- The trusted-key list, if you listed it, shows only your keys.
- `GET /admin/log-level` is `info` or `debug`.

If anything is off, go back to step 3. Then read the logs of every node
since the secret could have leaked. The INFO lines `M2M client created`,
`M2M client deleted` and `M2M client secret rotated` with
`tenantId=PLATFORM`, `log level changed` and `trace sampler changed` show
what was done. Setting a level above `info` writes no line, and no INFO
line is written while it stays there. The key-pair and trusted-key
endpoints log no INFO line; the checks above show their result. A line written after step 1 that you did not cause means someone
still reaches the API: fix the block and start again from step 2.

**6. Restore.** Give the new secrets to their owners, turn off again what
you turned on in step 3, and lift the block.

**Alternative to step 4.** During the block, set a new
`CYODA_JWT_SIGNING_KEY` on every node and restart every node. Once every
node runs with it, every token cyoda-go signed stops verifying and every
issued key pair is retired (see *Replacing `CYODA_JWT_SIGNING_KEY`* above).
`cyoda token --tenant PLATFORM` with the new key is then the way in, so
step 3 needs no admin client. Steps 3 and 5 still apply: M2M clients and
trusted keys survive the change. In step 5, JWKS and `/current` show the new
bootstrap key, or a pair you issued since. The restart also resets each
node's log level and trace sampler to their configuration.

#### Recovering from a `/oauth/token` 500

**The M2M client store failed, or holds a damaged client record or index
entry for the client id.** The `ticket` in `error_description` names the
ERROR log line that carries the cause; a damaged record or index entry is
also logged at ERROR with its client id. A damaged record is removed with
`DELETE /clients/{clientId}`. `DELETE` also removes a damaged index entry,
as long as the caller's tenant holds a record for that id (own namespace
proves ownership); without a record in that tenant, ownership cannot be
proven and `DELETE` still answers `500` — the fix is then a direct edit of
the storage backend: remove the key named by the client id from the
`m2m-client-ids` namespace. See `auth.clients`.

The signing-key incidents below leave `/oauth/token` unable to sign, so an
admin M2M client in `PLATFORM` cannot get a fresh token. The routes use one
of three ways in. Verification looks up only the token's own key id:

- **`cyoda token --tenant PLATFORM`** verifies only while the bootstrap key
  does: its stored state is readable and not deleted, it is inside the
  window a reactivation gave it (if any), and it is active or inside the
  grace period of an invalidation.
- **An earlier token:** an unexpired platform-operator token that an issued
  key pair signed verifies while that key pair verifies, whatever else is
  stored.
- **A new `CYODA_JWT_SIGNING_KEY` on every node** needs no token. The new
  bootstrap key id has no stored state, so a fresh `cyoda token --tenant
  PLATFORM` then verifies. It retires every issued key pair the old key
  sealed: their tokens stop verifying and clients fetch new ones. Prefer a
  route that needs no new key whenever you hold a token that verifies.

After a fix, `/oauth/token` signs only if a key pair of its audience is active
and inside its window; otherwise see *No signer* below.

**The selected key pair is broken.** The log names the KID and the reason.
`/oauth/token` cannot sign while the pair wins signer selection for its
audience: until it is invalidated or deleted, or a newer key pair outranks it.
A token signed by the broken pair itself does not verify. The fix depends on
why:

- Its vault kind is unrecognised. This check runs before the ownership
  check, so the pair stays broken whatever `CYODA_JWT_SIGNING_KEY` is set to.
  Invalidate it or `DELETE` it, with `cyoda token --tenant PLATFORM` while
  the bootstrap key verifies, or with an earlier token. Issued key pairs
  are unaffected. A rotation with `invalidateCurrent: true` also ends the
  pair, and also invalidates the audience's other issued key pairs that have
  not ended, including one issued ahead of time; tokens they signed stop
  verifying as each node applies the change (plus the clock offset between
  nodes, see above), or when the `invalidateGracePeriodSec` grace period
  ends.
  With neither token: set a new `CYODA_JWT_SIGNING_KEY` on every node, get
  a token from `cyoda token --tenant PLATFORM`, then invalidate or `DELETE`
  the pair. This retires every issued key pair the old key sealed.
- It is owned by the configured bootstrap key but cannot be opened.
  Invalidate it or `DELETE` it, with either token as above. Issued key pairs
  are unaffected. A new `CYODA_JWT_SIGNING_KEY` on every node also fixes it,
  with no token: the record is then retired (inert), not broken (blocking),
  and invalidate and `DELETE` answer `404` for it. This retires every other
  issued key pair the old key sealed.

**A stored record cannot be decoded at all.**

- It blocks signing for every audience, not only the audience of that
  record.
- `invalidate` and `reactivate` answer `404` for it: it is not a key pair the
  API recognises.
- `DELETE` always succeeds. It replaces the record with a deleted
  bootstrap-state record. An ERROR log names the KV key.
- A record at a KV key that is not 32 lowercase hex characters cannot be a
  key id: it is ignored (it does not block signing) and logged at ERROR.
- At any id other than this node's bootstrap key id, the replacement is
  inert and the bootstrap key is unaffected. `DELETE` the record with
  `cyoda token --tenant PLATFORM` while the bootstrap key verifies, or with
  an earlier token. Issued key pairs are unaffected. Replacing
  `CYODA_JWT_SIGNING_KEY` does not make the record decodable: the decode
  failure does not depend on which key owns the record. With neither token:
  set a new `CYODA_JWT_SIGNING_KEY` on every node, get a token from
  `cyoda token --tenant PLATFORM`, then `DELETE` the record. This retires
  every issued key pair the old key sealed.
- At this node's bootstrap key id, the record takes the place of the
  bootstrap key's state, so whatever that state was, a token from
  `cyoda token` does not verify on this node (the bootstrap key is unusable)
  and `/oauth/token` cannot sign. Two routes remain. Both permanently delete
  the old bootstrap key (see *Deleting the bootstrap key is permanent*);
  prefer route 1 when the token it needs exists.
  1. An earlier token, used to `DELETE` the record. Issued key pairs are
     unaffected.
  2. No earlier token needed: set a new `CYODA_JWT_SIGNING_KEY` on every
     node, get a token from `cyoda token --tenant PLATFORM`, then `DELETE`
     the old key id with it. This also retires every issued key pair the old
     key sealed.

**An issued record is stored at this node's bootstrap key id.** Two keys can
never share one KID, so the record is refused as undecodable.

- The bootstrap key is then unusable for signing and verifying, whatever its
  state was. A bootstrap-signed admin token, a token from `cyoda token`
  included, does not verify on this node.
- `cyoda token` still signs offline — it needs no store access — but the
  token it produces does not verify here, for the same reason above.
  `/oauth/token` cannot sign at all while the record is there, so an admin
  M2M client in `PLATFORM` cannot get a fresh token either. Two routes
  remain; prefer route 1 when the token it needs exists.
  1. An earlier token — for example one an admin M2M client in `PLATFORM`
     obtained while an issued key pair signed its tokens — used to `DELETE`
     the record. The
     `DELETE` writes a deleted bootstrap-state record at the id, which ends
     the old bootstrap key for good. Issued key pairs are unaffected.
  2. No earlier token needed: set a new `CYODA_JWT_SIGNING_KEY` on every
     node. This changes the bootstrap key id, so the record then decodes
     normally, as retired (its owner no longer matches). Nothing is written
     at the old id: the record stays there, `DELETE` answers `404` for it,
     and restoring the old key brings the incident back. This also retires
     every other issued key pair the old key sealed. If the record's vault
     kind is unrecognised, it is broken instead of retired: see *The
     selected key pair is broken*.

**No signer.** No key pair of the audience is active and inside its window.
While the bootstrap key is active it signs whenever no issued key pair does,
so for the bootstrap key's audience (`client` by default) this arises only
when the bootstrap key itself has been invalidated or deleted by its key id,
or a reactivation gave it a window that has since ended.

- While a grace period runs, tokens signed by the invalidated key pairs
  still verify, `cyoda token` tokens included while the bootstrap key's
  grace period runs. A platform-operator token of that kind can issue a new
  key pair, or reactivate the bootstrap key. Issuing a new key pair leaves
  the audience's other issued key pairs unaffected, unless
  `invalidateCurrent: true` is set, which also invalidates them (a running
  grace period included), including one issued ahead of time; tokens they
  signed stop verifying as each node applies the change (plus the clock
  offset between nodes, see above), or when the `invalidateGracePeriodSec`
  grace period ends.
  Reactivation works only if the bootstrap key was invalidated or its
  window ended: a deleted bootstrap key cannot be reactivated (`404`). It
  makes the tokens the bootstrap key signed earlier verify again until they
  expire: if it was invalidated because a token leaked, wait for the
  longest token lifetime first (see *Emergency revocation of a leaked
  token*), or issue a new key pair instead.
- Once every grace period has ended, no token can reach these endpoints
  independently of cyoda's own signing key: `PLATFORM` cannot own an OIDC
  provider. Recovery is a new `CYODA_JWT_SIGNING_KEY` on every node, needing
  no token. This starts a fresh, active bootstrap key with no stored state,
  and retires every issued key pair the old key sealed.

#### Upgrading from v0.7.x

KV-backed trusted-key entries written by versions < v0.8.0 are orphaned.
Within the `trusted-keys` namespace, entries are now keyed `<tenantID>:<kid>`
(was bare `<kid>`). v0.8.0 does not query the old shape; affected entries are
left in place but not loaded. Operators must re-register affected keys. To audit,
look for entries in the `trusted-keys` namespace whose key contains no `:`
separator (the exact query depends on the KV backend; for the SQLite plugin:
`SELECT key FROM kv_store WHERE namespace='trusted-keys' AND key NOT LIKE '%:%'`).
cyoda-go has no known production users on this surface.

## EXAMPLES

**Development (mock auth):**

```
CYODA_IAM_MODE=mock
CYODA_IAM_MOCK_ROLES=ROLE_ADMIN,ROLE_M2M
```

**Production (JWT auth):**

```
CYODA_IAM_MODE=jwt
CYODA_REQUIRE_JWT=true
CYODA_JWT_SIGNING_KEY_FILE=/etc/secrets/signing.pem
CYODA_JWT_ISSUER=https://auth.example.com
CYODA_JWT_AUDIENCE=cyoda-api
CYODA_JWT_EXPIRY_SECONDS=3600
```

**First admin token (JWT auth, same environment as the server):**

```
TOKEN=$(cyoda token --tenant acme)
```

**With trusted-key registration enabled:**

```
CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true
CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT=10
```

**With M2M admin-role grants enabled:**

```
CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true
```

**With federated OIDC providers (JWT mode):**

```
CYODA_OIDC_REQUIRE_HTTPS=true
CYODA_OIDC_ROLES_CLAIM=roles
```

## SEE ALSO

- config
- run
