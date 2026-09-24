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

- `CYODA_JWT_SIGNING_KEY` — RSA private key in PEM format; required in jwt mode
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
- `CYODA_BOOTSTRAP_USER_ID` — user ID for the bootstrap client (default: `admin`)
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
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` — per-tenant cap on registered
  trusted keys. Counts only currently-valid keys (active and not past
  `validTo`). `0` means unbounded. (default: `10`)
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
  signed are rejected. The bootstrap signing key (`CYODA_JWT_SIGNING_KEY`)
  has no window: it lasts as long as the configuration that supplies it, and
  you rotate it by replacing that key. (default: `365`)

### Auth cache reconciliation

Both per-node auth caches (trusted keys, OIDC providers) push updates to peers
on write and additionally run a periodic KV-reconcile as a backstop against
missed broadcasts. `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` sets that shared
interval; each tick is jittered ±10% to avoid a cross-node reconcile herd. A
cache that goes 10× this interval without a successful reconcile fails closed
on verification rather than serving a potentially stale answer.

- `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` — reconcile interval for both caches
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
`POST /oauth/keys/keypair` (with `algorithm: RS256` and `audience: client`),
optionally setting `invalidateCurrent: true` and
`invalidateGracePeriodSec: N` to overlap the old and new keys.

v0.8.0 limitations:
- Runtime-issued keypairs are held in memory only; they do not survive
  process restart. The bootstrap key survives because its KID is derived
  deterministically from the PEM input. Persisted signing-key storage is
  tracked in a v0.8.x follow-up.
- Runtime-issued keypairs exist only on the node that issued them. In a
  cluster, a token signed with one is rejected by the other nodes, so do not
  issue, invalidate or reactivate keypairs through the API in cluster mode
  until key pairs are shared across nodes.

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
