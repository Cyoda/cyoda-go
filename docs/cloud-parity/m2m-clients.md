# M2M clients: a per-tenant cap, on-behalf-of clients — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

Clients are scoped to their tenant (the token URL names it, ids are unique per
tenant and may be chosen): see `tenant-scoped-clients.md`.

## Rule

A tenant holds at most a configured number of M2M clients (technical users).
`POST /clients` in a tenant that already holds that many is refused:

| Status | `errorCode` | Retryable | When |
|---|---|---|---|
| `400` | `M2M_CLIENT_CAP_REACHED` | no | the caller's tenant already holds the maximum number of M2M clients |

The body is the usual RFC 9457 problem detail; `detail` begins
`M2M_CLIENT_CAP_REACHED:`. No client is created and no secret is issued.

- The cap is per tenant. A client of another tenant never counts.
- Every client the tenant holds counts, whatever its roles (`withAdminRole`
  makes no difference).
- Deleting a client frees a slot at once.
- The cap does not apply to a secret reset, which creates no client.

In cyoda-go the cap is `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`: default `100`,
`0` means no cap, and a negative value refuses to start. The value is
deployment configuration; the contract is the refusal and its code, not the
number.

cyoda-go checks the cap on each node before it writes, with no
compare-and-set in its store, so creates on several nodes at the same moment
can exceed the cap by at most one client per node. Creates on one node never
exceed it. Cloud may enforce the cap exactly; if it overshoots, by no more
than one client per node.

## On-behalf-of clients

`POST /clients?onBehalfOf=true` creates an on-behalf-of (OBO) client: one that
may only perform the token exchange, for the application's users (see
`obo-only-user-identity.md`).

| Status | `errorCode` | When |
|---|---|---|
| `400` | `BAD_REQUEST` | `withAdminRole=true` together with `onBehalfOf=true` (an OBO client never holds `ROLE_ADMIN`) |
| `400` | `BAD_REQUEST` | `onBehalfOf=true` in the tenant `PLATFORM` (no OBO client exists there) |
| `403` | `FORBIDDEN` | the caller's token is itself an OBO token (it never administers) |

- `onBehalfOf` is set at creation and never changes.
- `TechnicalUserDto`, the list items and `TechnicalUserCredentialsDto` carry
  `onBehalfOf`. The credentials DTO's `grant_type` is
  `urn:ietf:params:oauth:grant-type:token-exchange` for an OBO client and
  `client_credentials` otherwise.
- `client_credentials` with an OBO client, and the token exchange with any
  other client, answer `400 unauthorized_client`.
- Check order on create: tenant admin (`403`) → `clientId` outside its grammar
  (`400`) → `withAdminRole=true` while the admin-role flag is off
  (`404 FEATURE_DISABLED`) → both flags (`400`) → `PLATFORM` (`400`) → taken
  id (`409 M2M_CLIENT_EXISTS`) → cap (`400 M2M_CLIENT_CAP_REACHED`). See
  `tenant-scoped-clients.md`.
- Hashing a new secret on create or reset shares the node's bound on secret
  checks with the token endpoint: no free slot within 1 s is
  `503 SERVER_BUSY` with `Retry-After: 1`, and nothing is written. See
  `obo-only-user-identity.md` (token-endpoint cost controls).
- A client-credentials token carries `cgen`, the client's secret generation;
  a compute stream opened with it closes within a minute of a reset or a
  delete. Other requests are not checked against the client store, so a
  client's issued tokens end within the token lifetime (default 300 s, at
  most 3600 s).

## Not a contract change

M2M clients in cyoda-go are stored in the cluster's database: a create, reset
or delete takes effect on every node when it returns, and clients survive a
restart. Cloud already behaves so — it saves a technical user as a user
record in its store
(`backend/src/main/kotlin/net/cyoda/saas/account/TechnicalUserService.kt:441-472`
in the Cloud repository). Nothing changes for Cloud here.

## Cloud today

Cloud has no cap. `TechnicalUserService.addTechnicalUser`
(`TechnicalUserService.kt:86-115`) checks the admin-role flag, generates an
id and a secret, and saves the user; nothing counts the tenant's existing
technical users.

## Cloud action

1. Tracked in CP-3980. Add a per-tenant cap on technical users. At the cap,
   `POST /clients` answers `400` with `errorCode` `M2M_CLIENT_CAP_REACHED` and
   creates nothing. Choose a default that suits Cloud's tiers, or record here
   where Cloud differs.
2. Add the on-behalf-of permission, its creation rules and the DTO fields
   above (tracked with `obo-only-user-identity.md`).
