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

config.auth — IAM mode, JWT issuer, HMAC secret, and admin bootstrap controls.

## SYNOPSIS

cyoda supports two IAM modes: `mock` (development) and `jwt` (production). Configure the
mode via `CYODA_IAM_MODE`. Use `CYODA_REQUIRE_JWT` as a production safety guard to refuse
startup unless JWT mode is properly configured.

## OPTIONS

### IAM mode

- `CYODA_IAM_MODE` — authentication mode: `mock` or `jwt` (default: `mock`)
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
- `CYODA_JWT_ISSUER` — JWT issuer claim (`iss`) (default: `cyoda`)
- `CYODA_JWT_AUDIENCE` — required audience claim (`aud`) on inbound JWTs;
  empty string disables the audience check (default: empty)
- `CYODA_JWT_EXPIRY_SECONDS` — token lifetime in seconds (default: `3600`)
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

The rule is checked at the two places a tenant identifier enters the binary
from outside it:

- **The `caas_org_id` claim on an inbound JWT**, which covers every
  authenticated HTTP request and every authenticated gRPC method. A claim
  outside the grammar is rejected like any other bad token: `401` with the
  uniform problem detail, and nothing in the response distinguishing it. The
  server log records the rejection at request time — one warning per rejected
  request, carrying the reason and a byte offset, never the offending value.
  If you mint tokens from an external IdP, constrain the
  claim there — an identifier outside this grammar is only diagnosable from
  cyoda's own logs.
- **`CYODA_BOOTSTRAP_TENANT_ID`** (below).

Nothing downstream re-checks it: peer dispatch, scheduled tasks, search jobs
and stored client records all carry a value already admitted at one of those
two doors.

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
- **`CYODA_BOOTSTRAP_USER_ID`** (below).

**`oidc:` is a reserved word.** The OIDC path builds every user id it creates
as `oidc:<providerId>:<sub>`. Every other user id — the first-party claim, the
token-exchange `sub` and `CYODA_BOOTSTRAP_USER_ID` — must not begin with
`oidc:`, in any case, so that it can never name the same user as an OIDC
principal. Such a value is rejected at the door like any other bad user id.

### HMAC secret (inter-node dispatch authentication)

- `CYODA_HMAC_SECRET` — hex-encoded HMAC secret for inter-node dispatch auth
- `CYODA_HMAC_SECRET_FILE` — file path for `CYODA_HMAC_SECRET` (takes precedence)

### Bootstrap M2M client

cyoda can provision a machine-to-machine client at startup for automation and CI.

- `CYODA_BOOTSTRAP_CLIENT_ID` — bootstrap M2M client ID (optional)
- `CYODA_BOOTSTRAP_CLIENT_SECRET` — bootstrap M2M client secret; must be set when
  `CYODA_BOOTSTRAP_CLIENT_ID` is set (and vice versa)
- `CYODA_BOOTSTRAP_CLIENT_SECRET_FILE` — file path for `CYODA_BOOTSTRAP_CLIENT_SECRET`
  (takes precedence)
- `CYODA_BOOTSTRAP_TENANT_ID` — tenant for the bootstrap client (default: `default-tenant`).
  Must match the tenant grammar above. In jwt mode, when a bootstrap client is
  configured and this value does not match, the binary refuses to start. A deployment
  that configures no bootstrap client never reads the value and is unaffected, even when
  it is set to the empty string — and mock mode ignores the whole bootstrap block, so
  the value is not checked there either.
- `CYODA_BOOTSTRAP_USER_ID` — user ID for the bootstrap client (default: `admin`).
  Must pass the user-id check above. In jwt mode, when a bootstrap client is configured
  and this value does not pass, the binary refuses to start.
- `CYODA_BOOTSTRAP_ROLES` — comma-separated roles granted to the bootstrap client
  (default: `ROLE_ADMIN,ROLE_M2M`)

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

Operators can rotate signing keys at runtime via
`POST /oauth/keys/keypair` (with `algorithm: RS256` and `audience: client`).
Of the active key pairs inside their window, the one with the latest
`validFrom` signs new tokens (on a tie, the greater key id). Setting
`invalidateCurrent: true` also invalidates the current key pair: cyoda stops
accepting tokens it signed at once, and `invalidateGracePeriodSec: N` only
keeps it published in JWKS for up to N more seconds (never past its
`validTo`), for external verifiers that
cache it. `invalidateCurrent` cannot be combined with a future `validFrom`,
and `validTo` must be in the future; both are `400`. Reactivating a key pair
also refuses a future `validFrom`. To schedule a rotation,
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
  decrypts them.
- **Deleting the bootstrap key is permanent** for that key: it cannot be
  reactivated; replacing `CYODA_JWT_SIGNING_KEY` starts a fresh bootstrap key
  with no stored state — it does not undelete the old one.
- **If `/oauth/token` answers 500 because the selected key pair is broken,**
  the fix depends on why. Broken because its vault kind is unrecognised: that
  check runs before the ownership check, so it stays broken whatever
  `CYODA_JWT_SIGNING_KEY` is set to — invalidate it or `DELETE` it. Broken
  because it is owned by the currently configured bootstrap key but cannot be
  opened: replacing `CYODA_JWT_SIGNING_KEY` also fixes this, since it changes
  what "owned" means and the record becomes retired (inert) instead of broken
  (blocking); invalidating or deleting it works too. Use an unexpired admin
  token or an admin from a federated OIDC provider; the log names the KID and
  the reason.
- **If `/oauth/token` answers 500 because a stored record cannot be decoded
  at all:** it blocks signing for every audience, not only the audience of
  the record that cannot be decoded. `invalidate`/`reactivate` answer `404`
  for it (it is not a key pair the API recognises); `DELETE` always succeeds
  instead, replacing the record with a deleted bootstrap-state record. For a
  genuinely malformed record elsewhere in the store, this node's bootstrap
  key is unaffected: authenticate the `DELETE` with an unexpired admin token
  or an admin from a federated OIDC provider. Replacing `CYODA_JWT_SIGNING_KEY`
  does not help there — the decode failure has nothing to do with which key
  owns it. One case does depend on the configured key, and disables the
  bootstrap key too while it lasts: an issued record stored at whatever this
  node currently derives as its own bootstrap key id is refused as
  undecodable (two keys can never share one KID), which makes the bootstrap
  key unusable for signing and verifying — a bootstrap-signed admin token
  will not verify here. Authenticate instead with a token signed by an active
  issued key pair, or an admin from a federated OIDC provider, and either
  call `DELETE` or replace `CYODA_JWT_SIGNING_KEY` — replacing it also fixes
  the classification directly, since it changes which KID the rule applies
  to and the record then decodes normally under the new key. At this node's
  own bootstrap key id, `DELETE` permanently deletes the bootstrap key (see
  above); at any other id it replaces the record with an inert, deleted
  bootstrap-state record. An ERROR log names the KV key either way.
- **If `/oauth/token` answers 500 with no signer** because the bootstrap key
  has no active state for the audience and no issued key pair is active
  either: with the default `client` bootstrap audience and no other key
  pairs, this also means no first-party token verifies at all, so an admin
  token minted earlier does not help — it was signed by a key that no
  longer signs or verifies. Recovery needs an admin from a federated OIDC
  provider, whose tokens do not depend on cyoda's own signing key: reactivate
  the bootstrap key if it was only invalidated (a deleted bootstrap key
  cannot be reactivated — see above), or issue a new key pair either way; or
  replace `CYODA_JWT_SIGNING_KEY`, which starts a fresh, active bootstrap key
  with no stored state.
- **No exportable signing key:** a KMS-backed key vault for issued key pairs,
  with the bootstrap key deleted once an issued key signs, is supported by the
  design; no KMS vault ships yet.

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

**With bootstrap client:**

```
CYODA_BOOTSTRAP_CLIENT_ID=ci-client
CYODA_BOOTSTRAP_CLIENT_SECRET_FILE=/etc/secrets/ci-secret
CYODA_BOOTSTRAP_ROLES=ROLE_ADMIN,ROLE_M2M
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
