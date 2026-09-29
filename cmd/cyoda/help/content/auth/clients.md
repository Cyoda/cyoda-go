---
topic: auth.clients
title: "auth.clients — M2M client lifecycle"
stability: evolving
version_added: 0.8.0
see_also:
  - auth
  - auth.tokens
  - cli.token
  - config.auth
  - errors.M2M_CLIENT_NOT_FOUND
  - errors.M2M_CLIENT_CAP_REACHED
  - errors.FEATURE_DISABLED
  - errors.STORAGE_UNAVAILABLE
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# auth.clients

## NAME

auth.clients — provision and manage machine-to-machine (M2M) clients that authenticate against cyoda via the `client_credentials` grant.

## GOAL

You want a backend service or CI job to call cyoda APIs. Register an M2M client to obtain a `client_id` + `client_secret`. Your service then mints JWTs via `POST /api/oauth/token` (documented in `auth.tokens`) and presents them as `Authorization: Bearer …` on every request.

Use this path when you control both the service and its cyoda registration. For user-facing flows, federate via `auth.oidc` instead.

## PREREQUISITES

**Admin (cyoda operator) sets up:**

- `CYODA_IAM_MODE=jwt` (mock mode bypasses auth entirely — fine for dev, never for prod)
- `CYODA_JWT_SIGNING_KEY` (PEM RSA key; `_FILE` suffix supported)
- The first admin token: `cyoda token --tenant <tenantId>` signs one offline with `CYODA_JWT_SIGNING_KEY`, on any host or container that holds the key. Use it to create the first M2M clients. See `cli.token`.
- For admin-scoped M2M creation (`withAdminRole=true`): `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true`. Off by default; when off, the `withAdminRole=true` request shape returns `404 FEATURE_DISABLED`.

**Client (you) needs:**

- An `Authorization: Bearer …` token with `ROLE_ADMIN`. Every `/clients` endpoint (list, create, delete, reset-secret) requires admin role today.
- The created client is scoped to the caller's tenant — there is no per-request tenant parameter on these endpoints.
- A tenant holds at most `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` clients (default `100`, `0` = no cap). See STORAGE AND CONSISTENCY.

## REQUEST FLOW

The 4 `/clients` operations: provision, list, delete, reset-secret. Field names follow RFC 7591 (snake_case) for the credentials DTOs; list-item DTOs use cyoda's customary camelCase.

### Provision a client

The request takes no body. `withAdminRole` is a query parameter; the only role the new client receives unconditionally is `ROLE_M2M`, and `ROLE_ADMIN` is added when `withAdminRole=true` AND the IAM feature is enabled.

```bash
curl -X POST "https://cyoda.example.com/api/clients?withAdminRole=false" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) — schema `TechnicalUserCredentialsDto`:

```json
{
  "client_id":                "abc523BCD",
  "client_secret":            "mySecretKey123",
  "grant_type":               "client_credentials",
  "client_secret_expires_at": 0,
  "roles":                    ["ROLE_M2M"]
}
```

**`client_secret` is shown only at creation time.** Capture it now; the server cannot return it again. `client_secret_expires_at = 0` means the secret does not expire (per RFC 7591 §3.2.1).

A tenant that already holds `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` clients gets `400 M2M_CLIENT_CAP_REACHED` and no client is created. Delete a client to free a slot.

### List clients in the caller's tenant

```bash
curl -X GET https://cyoda.example.com/api/clients \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) — array of `TechnicalUserDto` (no secrets), sorted by `clientId`:

```json
[
  {
    "clientId":       "abc523BCD",
    "creationDate":   "2026-06-17T10:02:27.88Z",
    "lastUpdateDate": "2026-06-17T10:02:27.88Z",
    "roles":          ["ROLE_M2M"]
  }
]
```

### Delete a client

```bash
curl -X DELETE https://cyoda.example.com/api/clients/${CLIENT_ID} \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`):

```json
{
  "message":  "M2M client deleted successfully",
  "clientId": "abc523BCD"
}
```

The deleted client's tokens remain valid until their natural `exp`; deletion stops new token issuance.

### Reset a client secret

Rotates `client_secret` for an existing client. The verb is `PUT`, not `POST`.

```bash
curl -X PUT https://cyoda.example.com/api/clients/${CLIENT_ID}/secret \
  -H "Authorization: Bearer ${ADMIN_TOKEN}"
```

Response (`200 OK`) is `TechnicalUserCredentialsDto` — same shape as creation, carrying the new `client_secret`. Capture it before the connection closes. Existing JWTs minted with the previous secret remain valid until their natural `exp`; only new `/oauth/token` requests need the new secret.

## TOKEN

Clients are not tokens. After provisioning, the client uses `auth.tokens` (the `/oauth/token` endpoint) to mint JWTs from the `client_id` + `client_secret`. The JWT carries the client's tenant in `caas_org_id` and its roles in `scopes`. Full claim shape is in `auth.tokens`.

## STORAGE AND CONSISTENCY

Clients are stored in the cluster's database. A stored client holds a bcrypt hash of its secret, never the secret itself. No node keeps a copy: each `/clients` call, and each `/oauth/token` request with a well-formed client id, reads the store. So:

- A create, reset or delete takes effect on every node of the cluster when its call returns `200`. A new client can get a token from any node at once. After a reset, the old secret gets `401 invalid_client` on every node; after a delete, so does the client.
- Clients survive a restart of a node or of the whole cluster. The exception is the `memory` storage backend, which keeps nothing across a restart.
- A client belongs to the tenant that created it, and its tokens carry that tenant. Client ids are unique across all tenants.

The store has no compare-and-set. So when two admin calls for one tenant run at the same moment, on one node or on two:

- Two changes to one client: the later write wins. After two resets that both answer `200`, only the secret of the later one works.
- A failed reset and a successful one: a reset that answers `500` writes the client back as it was before that reset. If another reset succeeds in the meantime, the write-back can land after it. The secret that the successful reset returned then stops working, and the secret from before both resets works again. The fix is to reset again.
- A reset and a delete of one client: the reset can write the client back after the delete removed it. The client then appears in `GET /clients` but cannot get a token, and a reset of it answers `404 M2M_CLIENT_NOT_FOUND`. `DELETE` removes it.
- Two creates on two different nodes: each node checks the cap before the other's client is written, so a tenant can exceed the cap by at most one client per node. Creates on one node run one at a time and never exceed it.

A create writes the client's record, then its index entry (the stored mapping from a client id to its tenant). A write that reports a failure may still have landed, so a failed create removes what it may have written. A node that crashes between the two writes leaves a record with no index entry. If the clean-up of a failed create itself fails (logged at `ERROR`), what is left is an index entry with no record, which `GET /clients` does not list; a record with no index entry, which it lists; or, if both removals fail, a client whose secret was never returned. None of these authenticates anyone, and `DELETE` removes each of them.

A failed reset writes back the client as it was before the reset, so the old secret stays in force. If that restore also fails (logged at `ERROR`), the stored secret may be the new one, which was never returned: the client then needs another reset.

A stored client that cannot be read back (a damaged record) is left out of `GET /clients` and logged at `ERROR` with its client id. `DELETE` removes it; a reset of it answers `500`, and a token request for it `500 server_error`. A damaged record counts toward the cap until `DELETE` removes it; the log names its id. A damaged index entry makes a token request for that id answer `500`, and a reset too when the caller's tenant holds a record for that id; without one, a reset answers `404` and does not read the index entry. `DELETE` removes a damaged index entry too, as long as the caller's tenant holds a record for that id (own namespace proves ownership, the same rule applied to a damaged record); without a record in the caller's tenant, ownership cannot be proven and `DELETE` still answers `500` — the fix is then a direct edit of the storage backend: remove the key named by the client id from the `m2m-client-ids` namespace.

## ERRORS

- `errors.UNAUTHORIZED` (`401`) — bearer token missing, expired, signature invalid, or issuer untrusted.
- `errors.FORBIDDEN` (`403`) — caller lacks `ROLE_ADMIN` (required for every `/clients` endpoint today).
- `errors.M2M_CLIENT_NOT_FOUND` (`404`) — delete or reset: the referenced `clientId` does not exist or belongs to a different tenant. A reset also answers it for a client left without its index entry by the reset-and-delete race above.
- `errors.M2M_CLIENT_CAP_REACHED` (`400`) — create: the tenant already holds `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` clients.
- `errors.FEATURE_DISABLED` (`404`) — `withAdminRole=true` requested with `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=false`.
- `errors.BAD_REQUEST` (`400`) — query-string parameter invalid (e.g. malformed `withAdminRole` value), or a `clientId` in the path that does not match `^[A-Za-z0-9]{1,100}$`.
- `errors.NOT_IMPLEMENTED` (`501`) — `CYODA_IAM_MODE` is not `jwt`.
- `errors.SERVER_ERROR` (`500`) — any of the four operations: the store failed, or a damaged record or index entry blocks the operation (see above). The body carries a generic message and a `ticket`.
- `errors.STORAGE_UNAVAILABLE` (`503`) — any of the four operations: the store reports itself unavailable. Retryable.

## SEE ALSO

- `auth.tokens` — the `/oauth/token` endpoint and JWT claim contract
- `cli.token` — sign the first admin token with the signing key
- `config.auth` — `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`, `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`
- `openapi` — `cyoda help openapi tags` and look for the `User, Machine` tag
