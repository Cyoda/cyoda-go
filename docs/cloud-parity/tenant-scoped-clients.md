# Tenant-scoped M2M clients — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

A client id is unique only inside its tenant. A token request names its tenant
in the URL, a client is found by (tenant, client id), and a tenant chooses its
client ids if it wants to. All of this is a breaking change.

## Rule

### Token endpoint

`POST {ctx}/tenants/{tenant}/oauth/token` (`{ctx}` is the API context path,
`/api` by default). The old path `POST {ctx}/oauth/token` is gone.

- The client is found by (tenant, client id). The same id in another tenant is
  another client.
- An unknown tenant, an unknown client and a wrong secret are the same
  response: `401 invalid_client`. Nothing tells them apart.
- A `401 invalid_client` decided after the client store was read is sent no
  earlier than 500 ms after the request arrived. A refusal decided without a
  store read (no Basic credentials, a client id outside the grammar) is
  immediate. Successful answers are not held.
- The `{tenant}` segment must be an API tenant: the tenant grammar
  `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$` (case significant, compared byte for
  byte, never folded or trimmed), and not `SYSTEM` in any letter case.

| Status | Error | When |
|---|---|---|
| `400` | `invalid_request`, description `invalid tenant` | the `{tenant}` segment is not an API tenant, or the request path carries any percent-encoding |
| `401` | `invalid_client` | unknown tenant, unknown client or wrong secret; held to the 500 ms floor |

The `400` is the same for every tenant id and reads no stored state, so it
reveals nothing about any tenant. Everything else about the endpoint is
unchanged: Basic authentication only, both grants, the checks and their order.

### Client ids

`POST /clients` takes an optional query parameter `clientId`. Present, with any
value including an empty one, it must match the client-id grammar:
`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, case significant, and not `system` in any
letter case (the id is the client's user id on its tokens and in audit records,
and the user-id rule reserves `system`; see `user-id-rule.md`). Absent, the
server generates an id. The same grammar applies to `/clients/{clientId}` and
to the client id carried in tokens.

| Status | `errorCode` | Retryable | When |
|---|---|---|---|
| `400` | `BAD_REQUEST` | no | `clientId` is present and outside the grammar |
| `409` | `M2M_CLIENT_EXISTS` | no | the id is taken in the caller's tenant; or another create of the same id, on any node, wrote first |

- The taken-id check comes before the cap check: a taken id answers `409`, not
  `M2M_CLIENT_CAP_REACHED`.
- The first write wins. Of two concurrent creates of one id, one is `200` and
  the other `409`.
- The `409` concerns the caller's own tenant only, so it reveals nothing to
  another tenant.
- Check order on create is that of `m2m-clients.md`, with the `clientId`
  grammar (`400`) after the admin check and before the admin-role flag check,
  and the taken-id `409` after the secret-hash slot (`503`) and before the cap.

### Reset and delete

- A secret reset that loses a race with another change to the same client (a
  concurrent reset, or a delete and re-create of the id) answers
  `409 CONFLICT`, retryable. Two concurrent resets never both answer `200`.
- A delete always wins: a reset racing it answers `404`, or `200` with a
  secret the delete then removes; the client does not come back.

### Secret generation

A new client's secret generation (`cgen`, carried on its `client_credentials`
tokens) starts at a random integer in `[1, 2^52]`, not at 1, so that a client
created under the id of a deleted client never accepts the deleted client's
tokens on a compute stream. A reset adds one. A generation outside `[1, 2^53)`
is refused.

### `SYSTEM` is not an API tenant

`SYSTEM`, in any letter case, is refused as a tenant everywhere a tenant
enters:

| Door | Refusal |
|---|---|
| the `caas_org_id` claim of a token (HTTP and gRPC) | `401` / gRPC `Unauthenticated` |
| the `{tenant}` segment of the token URL | `400 invalid_request` |
| `cyoda token --tenant` | the command fails before signing |

`PLATFORM` is unchanged: operators get tokens at
`/api/tenants/PLATFORM/oauth/token` (see `platform-operator.md`).

## Not a contract change

Per-client rate limits and verified-secret caches in cyoda-go are keyed by
(tenant, client id), so one tenant's `backend` never slows another's. That is
an implementation detail; a Cloud that finds clients by tenant and id gets it
for free.

## Cloud today

- The token endpoint is `POST /api/oauth/token`, Basic only. The client is
  found by id alone, in the users table it shares with human users, by a
  globally unique user name (`TechnicalUserService.kt:248-263`); the tenant
  comes from the stored user (`:234-236`).
- Ids are, by default, 6 characters of `[a-zA-Z0-9]` (length configurable,
  `CaasWebSecurityProperties.kt:19`), unique across all tenants by
  check-then-insert (`TechnicalUserService.kt:117-134`,
  `AccountUtils.kt:3-16`). A caller cannot choose an id, and there is no
  `409` (`openapi.yml:45-105`).
- No route puts a tenant in the path.
- Two defects, independent of this change: a wrong secret answers `400`
  (`error=unsupported_grant_type`) while an unknown client answers `401`
  (`TokenExceptions.kt:10-35`, `TechnicalUserControllerIT.kt:157-199`), which
  tells an anonymous caller which client ids exist; and ids and secrets come
  from `Random.Default`, not a cryptographic source (`AccountUtils.kt:14`).

## Cloud action

1. Tracked in CP-3983. Data model: a technical user is found by (legal entity,
   client id), not by a global user name in the table shared with human users.
   The same id may exist in two tenants.
2. Token URL `POST /api/tenants/{tenant}/oauth/token`, with the same `401` for
   an unknown tenant, an unknown client and a wrong secret, and `400
   invalid_request` for a `{tenant}` that is not an API tenant or a path with
   percent-encoding. Remove the old path.
3. Caller-chosen ids on `POST /clients?clientId=` with the client-id grammar,
   `system` reserved, and `409 M2M_CLIENT_EXISTS`; taken-id check before the
   cap.
4. First-write-wins create: of two concurrent creates of one id, one wins and
   the other is `409`. Reset losing a race is `409 CONFLICT`; delete wins.
5. Hold every store-decided `401 invalid_client` to 500 ms from the start of
   the request.
6. Refuse `SYSTEM` (any letter case) as a token tenant. This matters more on
   Cloud: its SYSTEM legal entity skips entitlement checks.
7. Start a new client's secret generation at a random number, so a re-created
   id never inherits the deleted client's streams.
8. Fix the two defects above: a wrong secret answers `401 invalid_client`, the
   same as an unknown client; generate ids and secrets from a cryptographic
   source.
