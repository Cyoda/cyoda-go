# M2M-only access with on-behalf-of user identity — Cloud twin-alignment spec

cyoda-go **defines** the contract; Cyoda Cloud aligns to it.

CaaS ticket: to be filed

## Rule

Only M2M clients reach the data API. An application signs its own users in
and decides what each may do; the database has no per-user permissions. A
client acting for a user exchanges a user assertion the application signed
for an on-behalf-of (OBO) token. The token carries the client's rights and
the user's identity. The platform records the user and the client with every
change and passes both to compute nodes. It records what the client states
about the user and does not verify the user.

The decision record is `docs/adr/0004-m2m-only-access-with-on-behalf-of-identity.md`;
`docs/access-to-the-cyoda-api.html` walks through the scenarios; `cyoda help
auth tokens` is the wire reference.

## Contract

### Clients

| Client | Roles | `onBehalfOf` | Grant |
|---|---|---|---|
| plain | `ROLE_M2M` | `false` | `client_credentials` |
| admin | `ROLE_M2M`, `ROLE_ADMIN` | `false` | `client_credentials` |
| on-behalf-of | `ROLE_M2M` | `true` | token exchange only |

- `POST /clients?onBehalfOf=true` creates an OBO client. The flag is set at
  creation and never changes. `TechnicalUserDto`, the list items and
  `TechnicalUserCredentialsDto` carry `onBehalfOf`; the credentials DTO's
  `grant_type` is `urn:ietf:params:oauth:grant-type:token-exchange` for an OBO
  client and `client_credentials` otherwise.
- Check order on `POST /clients`: not a tenant admin, or an OBO token → `403
  FORBIDDEN`; `withAdminRole=true` while the admin-role flag is off → `404
  FEATURE_DISABLED`; `withAdminRole=true` with `onBehalfOf=true` → `400
  BAD_REQUEST`; `onBehalfOf=true` in the tenant `PLATFORM` → `400
  BAD_REQUEST`; cap → `400 M2M_CLIENT_CAP_REACHED`.
- `client_credentials` by an OBO client, and a token exchange by any other
  client, answer `400 unauthorized_client`. The exchange refuses a non-OBO
  client before it reads the assertion.

### Token exchange

Request: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, client
authentication by HTTP Basic, `subject_token` = the user assertion,
`subject_token_type=urn:ietf:params:oauth:token-type:jwt`. `actor_token`,
`actor_token_type`, `resource`, `audience`, `scope` and
`requested_token_type` are refused, even empty (`400 invalid_request`).

The user assertion: RS256, `kid` = a trusted key of the client's tenant;
`sub` = the user id; `caas_org_id` = the client's tenant (`403 access_denied`
otherwise); `aud` contains the cyoda issuer; `iat` and `exp` required, `exp −
iat` at most 300 s; `nbf` honoured; 30 s clock skew on every time claim; `iss`
one of the key's issuers when it lists any. Every other refusal is `400
invalid_request`, with a fixed description that never repeats the user, the
tenant or the key id.

The OBO token: `sub` = `caas_user_id` = the user; `act` = `{"sub": "<client
id>"}`, one level, never nested; `caas_org_id` = the client's tenant;
`scopes` = the client's roles (the assertion's roles are ignored; no user
lookup); `exp` = min(assertion `exp`, now + token lifetime); no `cgen`. A
`client_credentials` token carries `cgen`, the client's secret generation
(1 at creation, one more per reset). `expires_in` is the token's remaining
life on both grants.

### Data access and administration

- Every data operation, HTTP and gRPC, requires `ROLE_M2M` in the caller's
  roles: `403 FORBIDDEN` / `PermissionDenied` otherwise. The exceptions are
  `GET /account`, the client and trusted-key operations (tenant admin) and
  the key-pair operations (platform operator).
- An OBO token never administers: client, trusted-key, key-pair and
  platform-operator operations answer it `403 FORBIDDEN` whatever its roles.
- An OBO token never opens a compute stream (`PermissionDenied`). A stream
  needs a client's own `client_credentials` token; it re-reads its client
  every 60 s and closes when the client is gone or its secret was reset.
- An OBO request joins only a transaction begun for its own user: `403
  FORBIDDEN` over HTTP, the RPC's error envelope over gRPC.

### Identity recorded

Two principals everywhere: the attributed principal (who the work is for)
and the executor (who made it).

| Situation | Attributed | Executor |
|---|---|---|
| OBO client acts for alice | alice (`user`) | the OBO client (`service`) |
| a client works for no user | the client (`service`) | the client |
| compute write-back in alice's transaction | alice | the compute client |
| scheduled transition armed for alice fires | alice | `system` |
| platform operator's offline token | the operator's user | the same |

- **Callouts:** `authid` / `authtype` = the attributed principal; new
  `authexecid` / `authexectype` = the executor; `authclaims` = the executor's
  roles. See `authcontext-attribution.md`.
- **Audit events:** `actor` (with `kind`) and `executedBy`, on EntityChange
  and StateMachine events alike. See `audit-event-identity-and-order.md`.
- **Entity history and messages:** `user` / `userId`, `attributedKind`,
  `executedBy`. A message no longer takes a caller-supplied sender header.
- **Scheduled transitions:** `armedBy` is the arming write's attributed
  principal. See `scheduled-transitions.md`.

### Trusted keys

Never accepted as bearer tokens; key ids unique per tenant; register is an
upsert on `(tenant, kid)`; no grace period (invalidation ends a key at once,
and the invalidate request has no body); no `audience`; read from the store
on every exchange. See `trusted-key-tenant.md`.

### Token lifetime

`CYODA_JWT_EXPIRY_SECONDS`: default 300 s, at most 3600 s. An issued token is
not checked against the client store, so a deleted client's or reset
secret's tokens end within that lifetime.

### Token-endpoint cost controls

Per node, so one client's load cannot starve another's beyond the bcrypt
bound:

- **Verified-secret cache.** Keyed by client id, holding the client record's
  secret hash and the SHA-256 of the secret that matched it. A request whose
  record (read from the store every time) still carries that hash, and whose
  secret has that SHA-256 (constant-time compare), skips bcrypt. A reset or a
  delete takes effect on the next request. A wrong secret leaves the entry in
  place.
- **Bounded secret checks.** At most
  `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS` bcrypt operations at once
  (default: the CPUs the process may use, `GOMAXPROCS`). A request that gets
  no slot within 1 s answers `503 temporarily_unavailable` with
  `Retry-After: 1`. Unknown client ids and wrong secrets pay one bcrypt
  each, so a lookup costs the same either way. Hashing a new secret
  (`POST /clients`, `PUT /clients/{clientId}/secret`) takes a slot from the
  same bound and answers `503 SERVER_BUSY` with `Retry-After: 1`, writing
  nothing, when none is free within 1 s.
- **Per-client token bucket.** After authentication and the grant check,
  each client has `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` requests per minute
  per node (default 600, burst of the same size, `0` = unlimited), shared by
  both grants. Over it: `429 slow_down` with `Retry-After` (whole seconds
  until the next request is allowed).

The numbers are deployment configuration; the contract is the refusals and
their codes. `slow_down` and `temporarily_unavailable` are used with the
meaning of RFC 8628 and RFC 6749 §4.1.2.1.

### Retired

The OIDC provider endpoints (`/oauth/oidc/providers*`), their error codes
and settings; the caller-supplied message sender header; signing key-pair
`audience`. See `signing-key-pairs.md`.

## Cloud action

1. Require `ROLE_M2M` on every data endpoint; human principals (`ROLE_USER`)
   do not reach data. The exceptions are those listed above.
2. Add the on-behalf-of permission to technical users, with the creation
   rules and check order above; refuse `client_credentials` to an OBO client
   and the exchange to every other client.
3. Issue OBO tokens with the OBO client's roles, never the user's, with no
   user lookup, and `act` one level deep.
4. Refuse an OBO token on every administrative endpoint, on compute streams,
   and when it joins another user's transaction.
5. Trusted keys: never bearer tokens; per-tenant ids; register is an upsert;
   no grace period; no `audience` (see `trusted-key-tenant.md`, CP-3974).
6. Retire the OIDC provider endpoints.
7. Callouts: `authid`/`authtype` = attributed principal, new
   `authexecid`/`authexectype`, `authclaims` = the executor's roles (see
   `authcontext-attribution.md`).
8. Audit events carry `actor` (with `kind`) and `executedBy`, StateMachine
   events included (see `audit-event-identity-and-order.md`).
9. Token lifetime: default 300 s, at most 3600 s.
10. Key pairs have no `audience` (see `signing-key-pairs.md`).
11. Bound the token endpoint's cost per node as above — a verified-secret
    cache, bounded secret checks with `503` + `Retry-After`, a per-client
    bucket with `429 slow_down` — or record where Cloud differs.
