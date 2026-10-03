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
  mock mode: `user`, `service`, or `system`. The default, `service`, makes
  every mock-mode caller a client, as in `jwt` mode. `user` and `system` let
  local/CI setups exercise user- or system-attributed code paths without
  standing up real JWT auth. Any other value, in either mode, refuses to
  start. (default: `service`)

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
  be an integer from 1 to 3600; any other value stops the
  server at startup and makes `cyoda token` exit 1. (default: `300`)

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
request, carrying the reason and a byte offset, never the offending value.
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
beyond this and the reserved id `system` described below. The excluded
characters are the ones the CloudEvents spec forbids in a string attribute — a
user id is sent to compute nodes as `authid` — plus U+FFFD, which a JSON
decoder puts in place of every invalid byte, so that two different claims can
never name one user.

The same check applies at every place a principal's user id enters the binary
from outside it:

- **The user claim on an inbound JWT** — `caas_user_id`, or `sub` when
  `caas_user_id` is absent. A claim outside the check is an ordinary `401`,
  logged like the tenant claim above: the reason, and for a rejected
  character its code point and position, never the value. A `caas_user_id`
  that is present but empty, not a string, or outside the check is rejected;
  it does not fall back to `sub`.
- **The `sub` of a user assertion** presented to the token exchange, which
  becomes the issued token's user id. A value outside the check is
  `400 invalid_request`.

`cyoda token --user` checks the same rule, the reserved id included, before it
signs. A stored M2M client whose user id fails the check is treated as
damaged (see `auth.clients`).

**`system` is a reserved user id.** It is the identity of the platform's own
principal, the executor of every scheduled firing. No user id from outside
cyoda — a token claim, an assertion's `sub`, `cyoda token --user` — may be
`system` in any letter case, so no caller can be recorded as that principal.
Such a value is rejected at the door like any other bad user id.

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
- `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` — token requests each M2M client may
  make per minute on one node, counted across both grants, with a burst of
  the same size. The limit applies after the client has authenticated; over
  it `POST /oauth/token` returns `429` with error `slow_down` and a
  `Retry-After` header (whole seconds until the next request is allowed).
  Each node counts on its own. `0` means unlimited; a negative value refuses
  to start. (default: `600`)
- `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS` — client-secret (bcrypt)
  operations that run at once on one node: the secret checks of
  `POST /oauth/token`, and the hashing of a new secret by `POST /clients` and
  `PUT /clients/{clientId}/secret`. An operation that gets no slot within 1
  second writes nothing and is refused with `Retry-After: 1`: `503` with
  error `temporarily_unavailable` on the token endpoint, `503` with error
  code `SERVER_BUSY` on the `/clients` calls. Each refusal increments the
  `cyoda.auth.secret_checks.refused` counter (see `cyoda help telemetry`);
  none is logged. Every token request with an unknown client id or a wrong
  secret pays one check, so a lookup costs the same either way. A node keeps
  a cache of secrets it has verified: a request whose secret matches the
  cache, and whose client record (read from the store on every request)
  still carries the hash it was verified against, skips the check; a secret
  reset or a client delete takes effect on the next request. Must be at
  least `1`; startup fails otherwise. (default: the number of CPUs the
  process may use (GOMAXPROCS), which follows a container CPU limit)

  The token endpoint authenticates callers that are not yet authenticated,
  with bcrypt. This bound keeps that work from taking every CPU of a node;
  past it the endpoint answers `503` (it fails closed), so a flood of bad
  credentials can keep legitimate clients getting `503`.
  `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` counts only after a client has
  authenticated and does not stop such a flood. Deployments must put a
  per-source rate limit in front of `/api/oauth/token` at the ingress,
  gateway or load balancer.
- `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` — per-tenant cap on trusted keys
  that can verify. It counts every active key whose `validTo` has not passed;
  an invalidated key frees its slot at once (trusted keys have no grace
  period). `0` means unbounded. (default: `10`)
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

The per-node signing-key cache pushes updates to peers on write and
additionally runs a periodic KV-reconcile as a backstop against missed
broadcasts. `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` sets that interval; each tick is jittered ±10% to avoid a cross-node
reconcile herd. A cache that goes 10× this interval without a successful
reconcile fails closed on verification rather than serving a potentially stale
answer: it refuses every token (`401`) and answers JWKS with `503`.
No node keeps a copy of a trusted key or an M2M client: every token request
and every client or trusted-key call reads the store, so a change applies to
the next token request on every node once the call returns. Tokens already
issued keep verifying until their `exp`: an API request is not checked
against the client store. The exception is a compute-node stream, which
reads its client when it opens and every 60 seconds after, and is refused or
closed once the client is deleted or its secret reset. To cut off tokens already issued, see *A leaked platform
admin-client secret* below. (Each node also keeps a verified-secret cache,
always on and bounded to a fixed number of entries: each entry holds the
SHA-256 of a secret that matched and the stored hash it matched, and is used
only while the client record just read still carries that hash.
`CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS` sets the bcrypt slots, not the
cache.)

- `CYODA_AUTH_CACHE_RECONCILE_INTERVAL` — reconcile interval for the signing-key cache
  (default: `60s`, floor: `1s`)

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
`POST /oauth/keys/keypair` (with `algorithm: RS256`).
Of the active key pairs inside their window, the bootstrap
key included, the one with the latest `validFrom` signs new tokens (on a tie,
the greater key id). The bootstrap key takes part with a zero `validFrom`
until a reactivation sets one. Until then, an active issued key pair inside
its window signs before it, and the bootstrap key signs whenever no issued key
pair is active and inside its window. A reactivation sets the
bootstrap key's `validFrom` to the request's value, which defaults to now:
from then on it signs before every issued key pair with an
earlier `validFrom`.

Setting `invalidateCurrent: true` also invalidates the issued key pairs that
have not ended (including one issued ahead of time), the first
rotation included. A rotation invalidates issued key pairs only: the
bootstrap key is never one of them, stays active, keeps verifying, and
`cyoda token` keeps working. Only an invalidate or a `DELETE` that names the
bootstrap key's key id ends it.

An invalidated key pair — issued, or the bootstrap key — never signs again
unless reactivated. Tokens it signed keep verifying until the end of its
grace period: `invalidateGracePeriodSec: N` on a rotation, or
`gracePeriodSec: N` on `POST /oauth/keys/keypair/{keyId}/invalidate`, sets
its `validTo` to N seconds from now, never later than its current
`validTo`. N is at most 3600, the longest token lifetime
(`CYODA_JWT_EXPIRY_SECONDS`), which no token outlives; a larger N is
`400 BAD_REQUEST`. The default is 0: it stops verifying at once on the node that
takes the call, and on each other node once that node applies the change
(see *Shared and persisted* below), plus the clock offset between nodes: the
node that takes the call stamps `validTo` from its own clock, and each node
checks it against its own. JWKS publishes a key pair from its
issue (ahead of its window, if `validFrom` is in the future) until it can
no longer verify. A node that has not yet applied an invalidation can still
sign with the key pair until it does; with a grace period, those tokens
verify on every node until the key pair's `validTo`.

**Emergency revocation of a leaked token:** revoke the key pair named by
the `kid` in the token's header. Every token cyoda-go accepts was signed by
one of its own key pairs, the bootstrap key or an issued one, and JWKS
(`/.well-known/jwks.json`) lists each key pair until it can no longer
verify. A rotation is not enough: it never ends the
bootstrap key, which signs every token from `cyoda token`, and every token
from `POST /oauth/token` while it wins signer selection
(before the first rotation, for example, or after a reactivation with the
default `validFrom`; see above).

- If the `kid` names an issued key pair, invalidate it with a grace period of
  0, or `DELETE` it, or rotate with `invalidateCurrent: true` and
  `invalidateGracePeriodSec: 0`. If it signs and no other key pair is
  active and inside its window (with the bootstrap key revoked,
  it may be the only one), invalidating or deleting it leaves no signer (see
  *No signer* below), so use the rotation instead.
- If the `kid` names the bootstrap key, invalidate it with a grace period
  of zero. If no issued key pair is active and inside its window,
  first issue one (`POST /oauth/keys/keypair`), and
  hold an admin client in `PLATFORM` whose token it signs: otherwise
  `POST /oauth/token` has no signer once the bootstrap key is invalidated,
  no operator token verifies, and the only way back is a new
  `CYODA_JWT_SIGNING_KEY`. If `cyoda token` is still wanted, reactivate the
  bootstrap key once the longest token lifetime in use has passed since
  the later of two times: every node having applied the invalidation (see
  *Shared and persisted*
  below), and the last `cyoda token` run. A token signed after the
  invalidation also verifies again on reactivation. The wait is the
  largest `CYODA_JWT_EXPIRY_SECONDS` among the servers and among every place
  `cyoda token` ran (its `--ttl` is capped by the value where it ran), plus
  30 seconds for the clock-skew allowance token validation applies, plus a
  margin for the clock offset between hosts: `exp` comes from the signing
  host's clock and is checked on the verifying node's clock. By then every
  token the bootstrap key signed has expired. Reactivating it sooner makes
  those tokens verify again. Pass an early `validFrom` on the reactivation,
  for example `1970-01-01T00:00:00Z`, so that the issued key pairs
  keep signing `POST /oauth/token`. With the default `validFrom`
  (now), the bootstrap key signs before them, and `POST /oauth/token` signs
  with it again. `DELETE` also ends the bootstrap key, but permanently.
- If the token carries `ROLE_ADMIN` in `PLATFORM`, follow *A leaked
  platform admin-client secret* below with the token in place of the
  secret: it can create clients and trusted keys, and a request is not
  checked against the client store.
- If it carries `ROLE_ADMIN` in another tenant, follow *A leaked admin
  token of another tenant* under *A leaked platform admin-client secret*
  below.

Revoking a key pair ends no open connection. A compute-node gRPC stream
opens only with an M2M client's own `client_credentials` token, and after
that it checks its client, not its token: once a minute it closes if the
client was deleted or its secret reset. Whoever holds a leaked signing key
can sign such a token for any existing client at its current secret
generation, and a stream opened with it keeps running while that client
stands. So reset the secret of every client whose streams you cannot vouch
for, or restart every node to end open streams. On the memory backend a
restart loses all data, an invalidation of the bootstrap key included: if
you revoked the bootstrap key, give every node a new
`CYODA_JWT_SIGNING_KEY` at that restart.

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
`PLATFORM`'s M2M clients and trusted keys, issue, invalidate, reactivate and
delete key pairs (deleting the bootstrap key is permanent), change each
node's log level and trace sampler, and open a compute-node gRPC stream.
They can also change any models, workflows, entities, scheduled tasks and
messages kept in `PLATFORM`; step 5 checks them.
A request is not checked against the client store, so deleting the client
or resetting its secret does not end a token they hold: it verifies until its
`exp`, at most `CYODA_JWT_EXPIRY_SECONDS` after it was issued. Only a
compute-node stream checks its client, once a minute (see below). Contain
first, then clean up, verify and restore.

**A leaked admin token of another tenant.** Follow this procedure with the
token in place of the secret, and apply steps 2, 3, 5 and 6 to that tenant as
well as to `PLATFORM`. Its holder can create clients and trusted keys in
the tenant as the flags allow; with both an on-behalf-of client and a
trusted key of the tenant, it can get tokens for any user id of the tenant,
with that client's roles. The client and trusted-key endpoints act on the
caller's own tenant, so you need an admin token of that tenant.

- Step 2: `cyoda token --tenant <id>` while the bootstrap key verifies;
  otherwise use the *Alternative to step 4*. That token stops verifying
  in step 4.
- Step 3: clean the tenant's clients and trusted keys as step 3 says, with
  the tenant in place of `PLATFORM`. If you keep no admin client in the
  tenant, create one there the same way (the same flag and the tenant
  client cap apply): step 5 needs it, unless you use the *Alternative to
  step 4*.
- Step 5: use an admin client of the tenant whose new secret you hold, or,
  after the *Alternative to step 4*, `cyoda token --tenant <id>`. Read the
  log lines step 5 names with the tenant's id in place of `PLATFORM`.
- Step 6: delete the admin client you created in the tenant, or give it to
  the tenant.

On the memory backend a restart loses all data, an invalidation of the
bootstrap key included. There, give every node a new
`CYODA_JWT_SIGNING_KEY` at the restart in step 1: it removes every client,
trusted key and key pair and ends every token cyoda-go signed, so steps 3
and 4 have nothing to clean. Give every later restart a new key too. Create
the clients and trusted keys you need after the last restart, once
`CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` and
`CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` are set the way they will
stay. In step 5, JWKS lists the bootstrap key of the latest
`CYODA_JWT_SIGNING_KEY`, and `/current` names it.

**1. Contain.** Stop client requests to the HTTP and gRPC APIs, on
connections already open too, and leave the traffic between nodes open, so
that only you reach the APIs. This stops service for every tenant. Keep the
block until step 6.

- Remove the Ingress or Gateway route of both APIs, or stop the proxy in
  front of the nodes. A firewall rule in front of the proxy is not enough:
  it usually lets established connections continue, and requests on them
  still reach the nodes.
- Block every other source inside your network too, tenant compute nodes
  included. The Helm chart's NetworkPolicy (on by default) admits any
  source to ports 8080 and 9090, and another policy cannot narrow it,
  because policies add up. Edit it so that those ports admit only the
  cyoda-go pods and the pod you work from; a `helm upgrade` restores it.
  With the chart's policy off, apply one with the same effect.
- Check the block. From outside the cluster, requests to the Ingress or
  Gateway host names of both APIs must fail, and, when `service.type` is
  `NodePort` or `LoadBalancer`, so must requests to the external IP and to
  the NodePort on every node. From a pod the policy does not admit,
  requests to ports 8080 and 9090 must fail on every pod IP and on the
  Service. If a request succeeds, fix the block before step 2.
- Where the network plugin does not enforce NetworkPolicy (the chart's
  values tell such clusters to turn the policy off), or where host-network
  pods can reach the nodes, stop every workload in the cluster that is not
  yours before step 2. On such a plugin, this replaces the check from a
  pod.
- A request through the Service reaches any node. To reach one node, call
  it from the pod you work from by its pod DNS name,
  `<name>-<n>.<name>-headless.<namespace>.svc.cluster.local`, where
  `<name>` is the chart's StatefulSet and `<n>` the pod's ordinal.
- Once the block is in place, restart every node (on the memory backend,
  see above). A block can leave connections to the nodes running, and a
  gRPC stream is authenticated when it opens and afterwards checks only
  that its client stands, so it outlives any change to keys and runs for
  up to a minute after its client is deleted or its secret reset. The
  restart ends every connection and stream.

**2. Get in** with a platform-operator token: `cyoda token --tenant
PLATFORM` while the bootstrap key verifies, an unexpired operator token
you hold, or `/oauth/token` with an admin client of `PLATFORM` whose
secret you hold, the leaked one included. If none works, use the new
signing key below.

**3. Clean `PLATFORM`.**

- Delete every M2M client you do not need (`DELETE /clients/{clientId}`).
- Reset the secret of every client you keep
  (`PUT /clients/{clientId}/secret`), the leaked one too if you keep it.
  A reset by the attacker shows only in `lastUpdateDate` and a log line.
- After step 4 you need an admin client in `PLATFORM` whose new secret
  you hold. If you keep none, create one
  (`POST /clients?withAdminRole=true`, which needs
  `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true` on the node you call, read at
  startup; the tenant client cap applies) and store its `client_id` and
  `client_secret`. If you cannot, use the new signing key below.
- Trusted keys. No on-behalf-of client can exist in `PLATFORM`, so a
  trusted key of `PLATFORM` gives no token; in another tenant, a trusted
  key and an on-behalf-of client of that tenant give tokens for any user id
  of the tenant. Clean them all the same. Keys
  registered while `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` was on
  still verify exchanges when it is off, but the trusted-key endpoints
  then answer `404 FEATURE_DISABLED`. Registration replaces the key
  material, issuers and window of an existing key id, so a key id you
  recognise proves nothing. If registration is on for any node, or was on
  at any time since the secret could have leaked (if you cannot tell,
  treat it as on), turn it on for the node you call (it is read at
  startup), list the keys (`GET /oauth/keys/trusted`), delete every one,
  and register again the keys you need from your own copies.

**4. End every outstanding token.**

- Rotate: `POST /oauth/keys/keypair` with `algorithm: RS256`,
  `invalidateCurrent: true` and
  `invalidateGracePeriodSec: 0`. This invalidates every issued
  key pair that has not ended, including one issued ahead of time, and
  the new pair signs. Do not invalidate or delete the signing pair
  instead: that can leave no signer when no other key pair is
  active and inside its window (see *No signer* below).
- Once every node has applied the rotation (see *Shared and persisted*
  above), invalidate with `gracePeriodSec: 0` every key id that
  `/.well-known/jwks.json` lists, except the new pair and any key pairs
  you deliberately keep active. Keep only key ids you know.
- This always includes the bootstrap key: it still verifies the tokens
  it signed, whatever `CYODA_JWT_SIGNING_KEY` says now.
  `cyoda token` then stops working (to bring it back later, see
  *Emergency revocation of a leaked token* above). The way in is your
  admin client's new secret, or a new signing key (see *Alternative to
  step 4*). `/oauth/token` signs with the pair the rotation created.
- A `401` means your own token was signed by a key you invalidated: get
  a new one from `/oauth/token` with your admin client.
- Every tenant's M2M and token-exchange tokens stop verifying, and
  clients fetch new ones.

**5. Restart and verify.** Read `GET /admin/log-level` on every node (see
step 1 to reach one), and compare it with the level the node starts with:
`CYODA_LOG_LEVEL` trimmed and in lower case, `warning` read back as `warn`,
and `info` when it is unset or unknown. Restart every node (on the memory
backend, see above). A restarted node loads the stored state. The restart
ends any stream opened before the tokens were ended, and resets each
node's log level and trace sampler to their configuration. Wait for
the clock offset between nodes (see the grace-period paragraph under *JWT
signing keypair rotation*). Then check on every node:

- JWKS lists only the new pair and any key pairs you deliberately keep
  active.
- `/current` names the new pair.
- `GET /clients` shows only the clients you kept or created, each with a
  `lastUpdateDate` from your own change.
- If you cleaned the trusted keys, the list shows only the keys you
  registered again (only a node where registration is on can list).
- `GET /admin/log-level` is the level the node starts with.

If anything is off, go back to step 3. A node that fails to start in step
5 stays out of service until it starts and passes these checks.

Compare the models, workflows, entities, scheduled tasks and messages kept
in `PLATFORM`, and in any other tenant this procedure covers, with your own
records (`GET /entity/{entityId}/changes` and `GET /audit/entity/{entityId}`
show who changed an entity and when), and restore what differs. A change
made before step 1 is damage to repair, not a failed cleanup. Scheduled
transitions keep firing during the block: a change whose `executedBy` has
`kind` `system` is such a firing, not an API call (match the kind, not
the id: the kind of a token's principal is always `user` or `service`, and
`system` is a reserved user id no token carries). List the scheduled tasks of each of those tenants
(`GET /scheduled-tasks`, with an admin token of the tenant), and end any
you do not recognise: move its entity out of the transition's source
state with a manual transition, delete the entity, or import the workflow
without that scheduled transition (see `cyoda help workflows`). Any other
change made after step 1 that you did not make means someone still
reaches the API (see below).

Then read the logs of every node since the secret could have leaked. The
INFO lines `M2M client created`, `M2M client deleted` and `M2M client
secret rotated` with `tenantId=PLATFORM`, `member joined` with
`tenantId=PLATFORM` (a compute-node stream), `log level changed` and
`trace sampler changed` show what was done. So does the WARN `the signing
key from CYODA_JWT_SIGNING_KEY is invalidated or deleted`, which the node
that takes the call writes when the bootstrap key is invalidated or
deleted: step 4 writes one, and another is the only trace left by someone
who deleted that key. A node that starts with the bootstrap key ended
writes the same line at INFO. None of these lines names who acted: tell
your own actions from others by the timestamp and your own record. The
INFO lines `trusted key registered`, `trusted key invalidated`, `trusted
key reactivated` and `trusted key deleted` name the tenant, the kid, and
the attributed principal and executor of the call. The other key-pair
calls log nothing; the checks above show their result. No INFO line is written
while a node's level is above `info`, configured or set. A level that
matches proves nothing on its own: the `log level changed` line is
written after the new level applies, so setting `warn` or `error` writes
no line. Setting the level back writes one whose `previous` is `warn` or
`error`, and that line ends such a period.

Someone still reaches the API if a node's level differed from the level it
starts with before the restart in step 5, or if a line was written
after step 1 that you did not cause, such as a `log level changed` line
that ends a period at `warn` or `error`. Then fix the block (a connection
it did not end, or a path around it), restart every node (on the memory
backend, see above), and start again from step 2.

**6. Restore.** Give the new secrets to their owners, turn off again what
you turned on in step 3 (it is read at startup, so this is a restart; on
the memory backend, see above), and lift the block.

Whichever key pair you issued in step 4 or in the *Alternative
to step 4* is then the only signer. Its `validTo` is
`CYODA_IAM_KEYPAIR_DEFAULT_VALIDITY_DAYS` days (365 by default) after its
issue. Once it passes, `/oauth/token` cannot sign for any tenant, and
only a new signing key brings it back (see *No signer* below). Rotate
before then. You can instead
reactivate the bootstrap key once the wait under *Emergency revocation of
a leaked token* has passed, with an early `validFrom` and a `validTo`
far ahead: it then signs whenever no issued key pair does.

**Alternative to step 4.** During the block, set a new
`CYODA_JWT_SIGNING_KEY` on every node and restart every node (this can be
the restart in step 1). Never restore the old key: that brings back every
issued key pair and the old bootstrap key, and the tokens the attacker
holds verify again until they expire. Once every node runs with the new
key, every token cyoda-go signed stops verifying and every issued key pair
is retired (see *Replacing `CYODA_JWT_SIGNING_KEY`* above).
`cyoda token --tenant PLATFORM` with the new key is then the way in, so
step 3 needs no admin client. Steps 3 and 5 still apply: M2M
clients and trusted keys survive the change (not on the memory backend).
In step 5, JWKS and `/current` show the new bootstrap key, or a pair you
issued since.

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

After a fix, `/oauth/token` signs only if a key pair is active
and inside its window; otherwise see *No signer* below.

**The selected key pair is broken.** The log names the KID and the reason.
`/oauth/token` cannot sign while the pair wins signer selection:
until it is invalidated or deleted, or a newer key pair outranks it.
A token signed by the broken pair itself does not verify. The fix depends on
why:

- Its vault kind is unrecognised. This check runs before the ownership
  check, so the pair stays broken whatever `CYODA_JWT_SIGNING_KEY` is set to.
  Invalidate it or `DELETE` it, with `cyoda token --tenant PLATFORM` while
  the bootstrap key verifies, or with an earlier token. Issued key pairs
  are unaffected. A rotation with `invalidateCurrent: true` also ends the
  pair, and also invalidates the other issued key pairs that have
  not ended, including one issued ahead of time; tokens they signed stop
  verifying as each node applies the change (plus the clock offset between
  nodes, see the grace-period paragraph under *JWT signing keypair
  rotation*), or when the `invalidateGracePeriodSec` grace period ends.
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

- It blocks signing globally, not only for the key that record would have
  been.
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
  and `/oauth/token` cannot sign. Two routes remain, and both permanently
  delete the old bootstrap key (see *Deleting the bootstrap key is
  permanent*). Prefer the first when the token it needs exists: `DELETE`
  the record with an earlier token, which leaves issued key pairs
  unaffected. The second needs no earlier token: set a new
  `CYODA_JWT_SIGNING_KEY` on every node, get a token from
  `cyoda token --tenant PLATFORM`, then `DELETE` the old key id with it.
  This also retires every issued key pair the old key sealed.

**An issued record is stored at this node's bootstrap key id.** Two keys can
never share one KID, so the record is refused as undecodable.

- The bootstrap key is then unusable for signing and verifying, whatever its
  state was. A bootstrap-signed admin token, a token from `cyoda token`
  included, does not verify on this node.
- `cyoda token` still signs offline — it needs no store access — but the
  token it produces does not verify here, for the same reason above.
  `/oauth/token` cannot sign at all while the record is there, so an admin
  M2M client in `PLATFORM` cannot get a fresh token either. Two routes
  remain; prefer the first when the token it needs exists.
- The first route: `DELETE` the record with an earlier token, for example
  one an admin M2M client in `PLATFORM` obtained while an issued key pair
  signed its tokens. The `DELETE` writes a deleted bootstrap-state record
  at the id, which ends the old bootstrap key for good. Issued key pairs
  are unaffected.
- The second route needs no earlier token: set a new
  `CYODA_JWT_SIGNING_KEY` on every node. This changes the bootstrap key id,
  so the record then decodes normally, as retired (its owner no longer
  matches). Nothing is written at the old id: the record stays there,
  `DELETE` answers `404` for it, and restoring the old key brings the
  incident back. This also retires every other issued key pair the old key
  sealed. If the record's vault kind is unrecognised, it is broken instead
  of retired: see *The selected key pair is broken*.

**No signer.** No key pair is active and inside its window.
While the bootstrap key is active it signs whenever no issued key pair does,
so this arises only
when the bootstrap key itself has been invalidated or deleted by its key id,
or a reactivation gave it a window that has since ended.

- While a grace period runs, tokens signed by the invalidated key pairs
  still verify, `cyoda token` tokens included while the bootstrap key's
  grace period runs. A platform-operator token of that kind can issue a new
  key pair, or reactivate the bootstrap key. Issuing a new key pair leaves
  the other issued key pairs unaffected, unless
  `invalidateCurrent: true` is set, which also invalidates them (a running
  grace period included), including one issued ahead of time; tokens they
  signed stop verifying as each node applies the change (plus the clock
  offset between nodes, see the grace-period paragraph under *JWT signing
  keypair rotation*), or when the `invalidateGracePeriodSec` grace period
  ends.
  Reactivation works only if the bootstrap key was invalidated or its
  window ended: a deleted bootstrap key cannot be reactivated (`404`). It
  makes the tokens the bootstrap key signed earlier verify again until they
  expire: if it was invalidated because a token leaked, wait for the
  longest token lifetime first (see *Emergency revocation of a leaked
  token*), or issue a new key pair instead.
- Once every grace period has ended, no token verifies: every token
  cyoda-go accepts is signed by one of its own key pairs. Recovery is a new
  `CYODA_JWT_SIGNING_KEY` on every node, needing
  no token. This starts a fresh, active bootstrap key with no stored state,
  and retires every issued key pair the old key sealed.

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
CYODA_JWT_EXPIRY_SECONDS=300
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

## SEE ALSO

- config
- run
