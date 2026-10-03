---
topic: auth.integration
title: "auth.integration — integrate an application with cyoda authentication, step by step"
stability: evolving
version_added: 0.9.0
see_also:
  - auth
  - auth.clients
  - auth.tokens
  - auth.trusted-keys
  - cli.token
  - config.auth
  - grpc
  - audit
  - errors
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# auth.integration

## NAME

auth.integration — what an application team does, step by step, to authenticate its backend, its users' requests, its background jobs and its compute nodes against cyoda.

## GOAL

You build an application on cyoda. Your users sign in to your application, not to cyoda. Your application decides what each user may do, then calls cyoda for them, and cyoda records each change for the user and for the client that made it. This guide tells you what to create, what to send, what you get back, how to handle each error, how to rotate and revoke credentials, and how to run the whole flow on your own machine.

The common shape of an application:

- a **backend** that calls cyoda for signed-in users (it uses an on-behalf-of client);
- **background jobs and scripts** that call cyoda for no particular user (each uses a plain client);
- **compute nodes** that run your workflow processors and criteria over the gRPC stream (each uses a plain client of its own);
- a **tenant admin** who provisions the clients and keys above (uses an admin client).

The reference topics hold the full tables: `auth.tokens` (the token endpoint, every claim and every error), `auth.clients` (client lifecycle), `auth.trusted-keys` (key registration), `grpc` (the compute-node protocol), `audit` (recorded identities), `config.auth` (server settings and operator procedures). Run them as `cyoda help auth tokens`, `cyoda help auth clients`, `cyoda help auth trusted-keys`, `cyoda help grpc`, `cyoda help audit` and `cyoda help config auth`.

## GLOSSARY

- **Tenant** — one customer's isolated space in cyoda. Its id is the `caas_org_id` claim of a cyoda token. cyoda's API calls a tenant a *legal entity*: `legalId`, `legalEntityId` and `joinedLegalEntityId` all hold a tenant id. No request can reach another tenant's data.
- **caas** — "Cyoda as a Service". The claims `caas_org_id` (the tenant), `caas_user_id` (the principal's id) and `caas_tier` (a tier label, always `unlimited` in cyoda-go) carry this prefix because cyoda-go and Cyoda Cloud share one token format.
- **Principal** — whoever a request runs as. Its **kind** is `user` (a person), `service` (an M2M client) or `system` (cyoda itself, for scheduled firings).
- **Service principal** — a principal of kind `service`: an M2M client acting as itself, with a token from `client_credentials`.
- **Cyoda token** — a JWT cyoda signs with one of its own key pairs, the only kind of bearer token the API accepts. `/oauth/token` issues them to M2M clients; the platform operator's CLI command `cyoda token` signs them offline.
- **M2M client** — machine-to-machine credentials (a client id and a secret) that belong to one tenant and get cyoda tokens from `/oauth/token`.
- **Plain client** — an M2M client with the role `ROLE_M2M`. Every data operation requires `ROLE_M2M`.
- **Admin client** — an M2M client with `ROLE_M2M` and `ROLE_ADMIN`. `ROLE_ADMIN` manages the tenant's clients and trusted keys.
- **Tenant admin** — whoever holds an admin client of the tenant.
- **On-behalf-of (OBO) client** — an M2M client created with `onBehalfOf=true`. It may only exchange user assertions for tokens; it never uses `client_credentials` and never holds `ROLE_ADMIN`.
- **Trusted key** — an RSA public key a tenant admin registers. Your application signs user assertions with the matching private key.
- **kid** — "key id", a JWT header field naming the key that signed the token. In a user assertion it is the trusted key's `keyId`.
- **JWK** — JSON Web Key (RFC 7517): a public key written as JSON (`kty`, `n`, `e`). RFC 7518 defines the RSA members and the `RS256` algorithm.
- **User assertion** — a short JWT your application signs with its trusted key, naming the user (`sub`). cyoda records the user it names and does not verify the user.
- **Token exchange** — the OAuth 2.0 grant of RFC 8693: a client presents a token (here, the user assertion) and receives a new token. cyoda uses it for on-behalf-of access only.
- **OBO token** — the cyoda token a token exchange returns. Its subject is the user; its rights are the OBO client's roles.
- **act** — the claim in an OBO token that names the client acting for the user: `{"sub": "<client id>"}`.
- **cgen** — the claim in a `client_credentials` token that holds the client's secret generation: 1 at creation, plus one for each secret reset. A compute-node stream uses it to notice a reset.
- **Attributed principal** — who a change is for: the user of an OBO request, otherwise the caller (with the exceptions in READING IDENTITY IN A COMPUTE NODE). Shown as `user` in change history and `actor` in audit events.
- **Executor** — who actually made a change: the M2M client whose token made the call, `system` for a scheduled firing, or the operator's user for a `cyoda token`. Shown as `executedBy`.
- **Compute node** — your process that holds a gRPC stream open to cyoda and runs processors, criteria and functions when cyoda asks.
- **Callout** — one request cyoda sends to a compute node: run processor P, evaluate criterion C, or compute function F, for one entity.
- **Callback** — an API request a compute node makes back into cyoda while it handles a callout.
- **Transaction token (pass)** — the token cyoda attaches to a callout (CloudEvent attribute `cyodatxtoken`). A callback that echoes it (HTTP header `X-Tx-Token`, gRPC metadata `tx-token`) joins the transaction the callout belongs to. Some topics call it a *pass*. It is valid for one try of one callout.
- **Joined transaction** — the transaction a callback runs in when it presents a transaction token. Its writes commit or roll back with the operation that began the transaction.
- **Transaction origin** — the principal recorded when a transaction begins: the attributed principal of the request that began it (for an OBO request for alice: alice, kind `user`; for a client's own request: the client; for a scheduled firing: the principal that armed it).
- **Cascade** — the further transitions, processors and criteria a request sets off inside cyoda. They carry the identity of the request that set them off.
- **Commit-before-dispatch** — a processor execution mode (`COMMIT_BEFORE_DISPATCH`) that commits the work so far before it calls the compute node. With `startNewTxOnDispatch: false` the callout carries no transaction token, so its callbacks are independent requests.
- **Scheduled firing** — a scheduled transition that cyoda runs later, with no request in flight. Its executor is `system`; its attributed principal is the one that armed it.
- **CloudEvents Auth Context** — a CloudEvents extension that names the principal behind an event. cyoda attaches its attributes (`authtype`, `authid`, `authclaims`, plus its own `authexectype`, `authexecid`) to every callout.
- **RFC 6749** — OAuth 2.0, the framework for `/oauth/token`, the `client_credentials` grant and the error body shape. **RFC 7591** — OAuth dynamic client registration; cyoda borrows its field names (`client_id`, `client_secret`, `client_secret_expires_at`) for the client-creation response.
- **Platform operator** — whoever runs the cyoda deployment. They hold `CYODA_JWT_SIGNING_KEY` and can sign a token for any tenant with `cyoda token`.
- **PLATFORM tenant** — the tenant named `PLATFORM`. `ROLE_ADMIN` there is the platform operator's role: it manages signing keys and node settings. Applications do not use it.
- **Bootstrap key** — the signing key pair cyoda derives from `CYODA_JWT_SIGNING_KEY`. It signs every `cyoda token` and, until the operator issues other key pairs, every token from `/oauth/token`.

## STEP 1 — GET A TENANT AND ITS FIRST ADMIN CLIENT

Who: the platform operator, once per tenant.

cyoda has no tenant registry. A tenant exists as soon as a token names it, and only the platform operator's offline `cyoda token` can name a tenant: a client's token always carries the client's own tenant. Pick a tenant id that matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$` (case matters: `Acme` and `acme` are two tenants).

```bash
# On a host or container that holds CYODA_JWT_SIGNING_KEY (or _FILE).
# Kubernetes: kubectl exec <pod> -- /cyoda token ...
# Docker Compose: docker compose exec <service> /cyoda token ...
ADMIN_TOKEN=$(cyoda token --tenant acme)

# Create the tenant's first admin client. The node that takes the call
# must run with CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true.
curl -X POST "https://cyoda.example.com/api/clients?withAdminRole=true" \
  -H @- <<<"Authorization: Bearer ${ADMIN_TOKEN}"
```

The answer carries `client_id` and `client_secret`. The secret is shown once. Hand both, and the tenant id, to the tenant admin over a secure channel. `cyoda token` details: `cli.token`.

## STEP 2 — CREATE ONE CLIENT PER PART

Who: the tenant admin, with an admin client token.

```bash
# The tenant admin's own token.
curl -X POST https://cyoda.example.com/api/oauth/token \
  -K- <<<"user = \"${ADMIN_CLIENT_ID}:${ADMIN_CLIENT_SECRET}\"" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials"

# The backend's on-behalf-of client.
curl -X POST "https://cyoda.example.com/api/clients?onBehalfOf=true" \
  -H @- <<<"Authorization: Bearer ${TENANT_ADMIN_TOKEN}"

# One plain client for each compute node, background job or script.
curl -X POST https://cyoda.example.com/api/clients \
  -H @- <<<"Authorization: Bearer ${TENANT_ADMIN_TOKEN}"
```

Each answer is a `TechnicalUserCredentialsDto`: `client_id` (16 characters, upper-case letters and digits), `client_secret` (64 lower-case hex characters, shown once), `roles`, `onBehalfOf`, and `grant_type` — the grant that client uses. The `-K-` and `-H @-` forms keep secrets off the command line, where other local users could read them.

**Use one client per part.** Then each part can be rotated or revoked alone, and the executor recorded on every change names the part that made it.

**Record the credentials when you create them.** A client has no name or label, and its secret cannot be read again. Store the `client_id` and `client_secret` at once in your deployment's secret store, under a name that says which part uses them (for example `cyoda/acme/backend-obo`). This is your inventory. To provision idempotently, read the secret store first and create a client only for a part that has none. To reconcile, list the tenant's clients (`GET /clients` returns `clientId`, `creationDate`, `lastUpdateDate`, `roles` and `onBehalfOf`, never a secret) and delete every `clientId` your secret store does not hold. A tenant holds at most `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT` clients (100 by default); at the cap, a create answers `400 M2M_CLIENT_CAP_REACHED`.

**There is no least-privilege split beyond the three kinds.** Every plain or admin client holds `ROLE_M2M`, which reaches every data operation of its tenant: read and write every entity, import models and workflows, and open a compute-node stream with any tags. The only extra permission is `onBehalfOf`, and the only extra role is `ROLE_ADMIN`. A compute node's client can therefore also act as a background job, and any plain client can join as a compute node and receive callouts that match the tags it declares. Treat every client secret as access to all of the tenant's data.

Your application must know its tenant id from configuration: the credentials answer does not carry it, and an OBO client cannot get a token to call `GET /account` before it knows the tenant. The tenant admin reads it from `GET /account` (`legalEntity.id`). Full lifecycle: `auth.clients`.

## STEP 3 — REGISTER THE APPLICATION'S TRUSTED KEY

Who: the tenant admin, with an admin client token. The platform operator must run every node with `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true` (see CLUSTER NOTES below).

Generate the key pair where the backend runs; the private key never leaves it.

```bash
openssl genrsa -out app-key.pem 2048

# The public key as a JWK modulus (n), base64url without padding.
N=$(openssl rsa -in app-key.pem -noout -modulus | cut -d= -f2 \
    | xxd -r -p | openssl base64 -A | tr '+/' '-_' | tr -d '=')
# openssl genrsa uses the public exponent 65537, which is "AQAB".

curl -X POST https://cyoda.example.com/api/oauth/keys/trusted \
  -H @- -H "Content-Type: application/json" \
  -d "{\"keyId\":\"app-key-2026-10\",\"jwk\":{\"kty\":\"RSA\",\"n\":\"${N}\",\"e\":\"AQAB\",\"alg\":\"RS256\",\"use\":\"sig\"}}" \
  <<<"Authorization: Bearer ${TENANT_ADMIN_TOKEN}"
```

The `keyId` is 1 to 128 characters from `A`–`Z`, `a`–`z`, `0`–`9`, `.`, `_` and `-` (`^[A-Za-z0-9._-]{1,128}$`). It becomes the `kid` you set when you sign. If the JWK carries a `kid`, it must equal `keyId`. The modulus must have at least 2048 bits; a JWK with a private member (`d`, `p`, `q`, …) is refused. Without `validTo`, the key is valid for `CYODA_IAM_TRUSTED_KEY_MAX_VALIDITY_DAYS` days (365 by default) from `validFrom`. Optional `issuers` makes cyoda check the assertion's `iss`.

A trusted key belongs to the tenant, not to a client: any OBO client of the tenant can exchange an assertion signed with any active trusted key of the tenant. Full reference: `auth.trusted-keys`.

## STEP 4 — ACT FOR A SIGNED-IN USER

Who: your backend, with its OBO client. Do this only after your application has authenticated the user and decided that the user may do what they ask: cyoda applies the OBO client's roles, never the user's.

**4a. Sign a user assertion.** A JWT with:

- header: `"alg": "RS256"`, `"kid": "<keyId>"` (a `typ` is allowed and ignored);
- `sub`: the user id — your application's stable id for the user, 1 to 255 characters, no control characters, not `system` in any letter case (the full rule is in `config.auth`, *User identifiers*). cyoda records it byte for byte and never normalises it: `Alice` and `alice` are two users, and two OBO clients of one tenant that assert the same id record the same user;
- `caas_org_id`: your tenant id;
- `aud`: cyoda's issuer, the value of `CYODA_JWT_ISSUER` (default `cyoda`), as a string or inside an array. `CYODA_JWT_AUDIENCE` plays no part in the assertion. Learn the issuer from the operator's configuration, or read the `iss` of any cyoda token, for example your admin client's;
- `iat`: now, and `exp`: at most `iat + 300` (both required, in seconds since the epoch);
- `nbf`: optional;
- `iss`: required only if the trusted key lists `issuers`, and then one of them.

cyoda allows 30 seconds of clock skew on `iat`, `exp` and `nbf`, and refuses an `iat` more than 30 seconds in the future. Every other claim is ignored, roles included. cyoda does not check `jti` and keeps no record of used assertions: an assertion can be exchanged again until it expires. So sign a fresh assertion for each exchange, keep its life short, and never let it leave your backend.

**4b. Exchange it.**

```bash
curl -X POST https://cyoda.example.com/api/oauth/token \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=urn:ietf:params:oauth:grant-type:token-exchange" \
  -d "subject_token_type=urn:ietf:params:oauth:token-type:jwt" \
  -K- <<EOF
user = "${OBO_CLIENT_ID}:${OBO_CLIENT_SECRET}"
data = "subject_token=${USER_ASSERTION}"
EOF
```

Client authentication is HTTP Basic only (`client_secret_basic`). A `client_id` and `client_secret` sent as form fields are not read, and the request answers `401 invalid_client`. Per RFC 6749 §2.3.1 the id and secret may be form-urlencoded before base64; cyoda decodes them. Generated ids and secrets contain only letters and digits, so encoding leaves them unchanged. Send no other RFC 8693 parameter (`actor_token`, `audience`, `scope`, `resource`, `requested_token_type`, `actor_token_type`): each is refused, even empty.

The answer:

```json
{
  "access_token":      "eyJhbGciOiJSUzI1NiIs…",
  "token_type":        "Bearer",
  "expires_in":        300,
  "issued_token_type": "urn:ietf:params:oauth:token-type:jwt"
}
```

There is no refresh token and no `scope`. The OBO token expires at the earlier of the assertion's `exp` and now + `CYODA_JWT_EXPIRY_SECONDS`. Because an assertion lives at most 300 seconds, an OBO token never lives more than 300 seconds past its assertion's `iat`, whatever `CYODA_JWT_EXPIRY_SECONDS` says. Sign each assertion just before the exchange with `exp = iat + 300` to get the longest token.

**4c. Call cyoda with it** as `Authorization: Bearer <access_token>` on HTTP, or gRPC metadata `authorization: Bearer <access_token>`. Every change is recorded for the user (kind `user`) and executed by your OBO client (kind `service`).

**4d. Cache one token per user.** Keep the token with its expiry (receipt time + `expires_in`, by your own clock) and reuse it for that user's further requests. Exchange a new assertion when less than 60 seconds of life remain, and once more if a call answers `401`. Each client may make `CYODA_IAM_TOKEN_REQUESTS_PER_MINUTE` token requests per minute on each node (600 by default, both grants together): at 300-second tokens, one OBO client sustains about 3000 concurrently active users per node.

An OBO token works on data operations only. It never administers clients, trusted keys, key pairs or `/admin/*` (`403 FORBIDDEN`), never opens a compute-node stream (`PermissionDenied`), and joins only a transaction whose origin is its own user (`403 FORBIDDEN`).

## STEP 5 — WORK FOR NO USER

Who: background jobs, scripts, migrations — anything that acts as itself. Each uses a plain client and `client_credentials`:

```bash
curl -X POST https://cyoda.example.com/api/oauth/token \
  -K- <<<"user = \"${CLIENT_ID}:${CLIENT_SECRET}\"" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials"
```

The answer has `access_token`, `token_type` and `expires_in` (the token's remaining life, at most `CYODA_JWT_EXPIRY_SECONDS`, 300 by default). Changes are recorded with the client as both attributed principal and executor (kind `service`). Cache the token and get a new one when less than 60 seconds remain. Never use an OBO token for work no user asked for, and never use a plain client's token for work a user asked for: the record would name the wrong principal.

## STEP 6 — CONNECT A COMPUTE NODE

Who: each compute node, with its own plain client.

1. Get a token with `client_credentials` (STEP 5). Only a client's own `client_credentials` token opens a stream: an OBO token, a `cyoda token` or a token without `ROLE_M2M` is refused with `PermissionDenied`.
2. Open `startStreaming` on the gRPC port (default 9090) with metadata `authorization: Bearer <token>`, and send `CalculationMemberJoinEvent` with your `tags` and `joinedLegalEntityId` = your tenant id.
3. Keep the stream. It outlives the token that opened it; you do not reconnect when you fetch a new token. cyoda re-reads your client when the stream opens and every 60 seconds.
4. For each **callback**, send your client's current `client_credentials` token as the bearer, and echo the callout's transaction token (`cyodatxtoken`) as HTTP header `X-Tx-Token` or gRPC metadata `tx-token` when the callback must run in the callout's transaction. Without the transaction token, the callback is an independent request.

When the stream ends:

- `Unauthenticated` — your client was deleted or its secret was reset, or (at open) the token predates a reset. Read the current credentials from your secret store, get a new token, reconnect. If a freshly fetched token is refused again, the client is gone: stop and alert, do not reconnect in a loop.
- `Unavailable` — cyoda could not read its client store. Reconnect with backoff.
- `PermissionDenied` — wrong kind of token, or `joinedLegalEntityId` names another tenant. Fix the configuration.

The `CYODA_COMPUTE_*` variables in `config.grpc` are the convention of cyoda's own test compute node (`cmd/compute-test-client`); cyoda does not read them, and your compute node may take its endpoint and credentials from any configuration. Full protocol: `grpc`.

## READING IDENTITY IN A COMPUTE NODE

Every callout carries these CloudEvent attributes:

- `authtype`, `authid` — the attributed principal's kind and id: who the work is for.
- `authexectype`, `authexecid` — the executor's kind and id: who does it.
- `authclaims` — the executor's roles, comma separated. Absent when the executor is `system`. They are never the user's roles: for an OBO request they are the OBO client's (`ROLE_M2M`). A compute node that read `authclaims` as the user's roles must stop: cyoda has no user roles; your application decides what a user may do.

`authtype`, `authid` and `authclaims` are attributes of the CloudEvents Auth Context extension, with one difference: cyoda's kinds are `user`, `service` and `system`, where the extension's list uses `service_account` for a service. `authexectype` and `authexecid` are cyoda's own attributes, outside that extension.

The values, per situation (attributed / executor):

- An OBO request for alice, and every callout of its cascade: `user` alice / `service` the OBO client.
- A client's own request, and its cascade: `service` the client / `service` the client.
- A compute node's callback that joins a transaction (it presents the transaction token with its own client token), and its cascade: the transaction origin — alice for a transaction an OBO request for alice began, the client for one a client began, the arming principal for a scheduled firing — / `service` the compute node's client.
- A compute node's independent request (no transaction token), including the callback of a commit-before-dispatch processor with `startNewTxOnDispatch: false`, and its cascade: `service` the compute node's client / the same.
- A request with an OBO token from a compute node that holds an OBO client of its own: `user` the asserted user / `service` that OBO client. It may join only a transaction whose origin is that user.
- A scheduled firing, and every callout of its cascade: the principal that armed it (the attributed principal of the arming request: alice if an OBO request for alice armed it, the client if a client's own request did) / `system` `system`. This includes the callout of a commit-before-dispatch processor with `startNewTxOnDispatch: false`, which is dispatched outside a transaction.
- A callout another node runs: the values the dispatching node computed.

**Segregation of duties ("the submitter may not approve").** In the approval processor or criterion:

1. Require `authtype == "user"`. A `service` principal is a client's own work and a `system` principal is cyoda's: neither is a person approving.
2. Require `authexectype == "service"`. A scheduled firing carries the user who armed it in `authid`, with `authexectype == "system"`; that user did not approve now.
3. Compare `authid` with the submitter's id, byte for byte. Record the submitter when they submit: copy `authid` (with `authtype == "user"`) into the entity in the submit processor, or read the submit change's `actor.id` (with `actor.kind == "user"`) from `GET /audit/entity/{entityId}`. Both hold exactly what your application asserted; nothing is normalised.
4. Optionally require `authexecid` to be one of your application's OBO client ids, so that only your backend can approve. Keep that list in configuration and update it when you rotate the client. A callout of a compute node's write-back cascade carries the compute node's client as executor, so such a check refuses approvals made inside a write-back.

Go compute nodes can use the public package `github.com/cyoda-platform/cyoda-go/api/grpc/authctx`: `Type` and `ID` return the attributed principal, `ExecutorType` and `ExecutorID` the executor, `Roles` the executor's roles. `Require(ce, role)` returns true only when the executor's kind is `service` and `role` is in `authclaims`: it gates on the executor, never on the user, and a scheduled firing never passes it. Other languages read the attributes directly.

**Trust the attributes only over TLS.** A compute node relies on them only if it verified cyoda's server certificate. See SECURITY.

## WHAT CYODA RECORDS

- Entity change history, `GET /entity/{entityId}/changes`: `user` (the attributed id), `attributedKind`, and `executedBy {id, kind}`. Over gRPC, `EntityChangesMetadataGetRequest` returns the same three fields in each change's `changeMeta`.
- Audit events, `GET /audit/entity/{entityId}`: `actor {id, name, kind, legalId}` and `executedBy {id, kind}`, for entity changes and state-machine events. `actor.name` repeats `actor.id`: cyoda has no display names, and an assertion cannot set one. `externalId` is never set.
- Messages, `GET /message/{messageId}`: `userId`, `attributedKind` and `executedBy`.
- Async search jobs belong to the tenant, not to the user: any principal of the tenant that knows a job id can read its results.

See `audit`, `crud` and `messages`.

## ERRORS AND RETRIES

**`POST /oauth/token`** answers in the OAuth error shape, `{"error": "...", "error_description": "..."}`, with fixed descriptions that `auth.tokens` lists one by one. The token endpoint changes nothing, so a retry is always safe; the question is only whether it can succeed.

- `400 invalid_request`, `400 unsupported_grant_type`, `400 unauthorized_client`, `403 access_denied`, `405 method_not_allowed` — your request or your setup is wrong. Do not retry; fix it. The `error_description` tells the causes apart (for example `unknown or inactive trusted key`, `subject token signature or issuer rejected`, `subject token claims rejected` for `aud`/`exp`/`iat`/`nbf`, `subject token sub rejected`, `tenant mismatch`).
- `401 invalid_client` — wrong client id or secret, no Basic header, or the client was deleted or reset. Re-read the credentials from your secret store once; if they still fail, stop and alert.
- `429 slow_down` — this client used its per-node token budget. Wait `Retry-After` seconds; cache tokens per user so that you exchange less.
- `503 temporarily_unavailable` — the node could not read its store, or had no free secret-check slot within 1 second. Wait `Retry-After` seconds and retry, with backoff.
- `500 server_error` — a store failure, a damaged client record or a signing failure; `error_description` carries a ticket. Retry a few times with backoff; if it persists, give the ticket to the platform operator.

**Data calls (HTTP and gRPC).**

- `401 UNAUTHORIZED` / `Unauthenticated` — authentication runs before any handler, before any transaction is joined: the request did nothing. Get a new token and send the same request again, once, even if it writes. A second `401` with a fresh token means a configuration error (issuer, audience, revoked signing key): stop and alert.
- `403 FORBIDDEN` / `PermissionDenied` — "this operation requires ROLE_M2M" is decided before the handler; "on-behalf-of tokens cannot administer" and "an on-behalf-of request may join only its own user's transaction" are decided before the operation reads or writes anything. Not retryable: use the right client.
- `5xx` — a generic message and a ticket. Whether a write took effect follows the rules of the operation (see `errors`).

## ROTATION

**A client secret, without downtime.** A secret reset (`PUT /clients/{clientId}/secret`) refuses the old secret at once on every node, and a compute-node stream opened before it closes within 60 seconds. To rotate without a gap, use a second client:

1. Create a new client of the same kind (STEP 2). It needs one free slot under `CYODA_IAM_M2M_CLIENT_MAX_PER_TENANT`.
2. Store its credentials and roll every instance of the part onto it. Old and new clients work side by side.
3. When no instance uses the old client, delete it (`DELETE /clients/{clientId}`). Tokens it already issued remain valid until they expire (at most `CYODA_JWT_EXPIRY_SECONDS`); its streams close within 60 seconds.

If you check `authexecid` against a list of client ids (the segregation-of-duties check in READING IDENTITY IN A COMPUTE NODE), add the new id before step 2 and remove the old one after step 3. Use a reset instead only when a short outage is acceptable or the secret has leaked.

**A trusted key.** Register the new key under a new `keyId` while the old one stays active (a tenant holds at most `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT` active keys, 10 by default). Switch the backend to sign with the new key. When no assertion signed with the old key can still be in flight (300 seconds after the switch), invalidate the old key (`POST /oauth/keys/trusted/{keyId}/invalidate`) or delete it. OBO tokens already issued are signed by cyoda, not by your key, and stay valid. `invalidatePrevious: true` on the registration instead ends every other key of the tenant at once.

## INCIDENTS

- **The trusted key's private key leaked.** Invalidate the key now: exchanges with it stop at once on every node. Register a new key and roll the backend onto it. Anyone holding the private key and the credentials of any OBO client of the tenant can get tokens for any user id of the tenant, so if your OBO client secrets are stored with the key, rotate them too. OBO tokens already issued end within 300 seconds of their assertion's `iat`.
- **An OBO client's secret leaked.** On its own it gives no token: `client_credentials` refuses an OBO client, and an exchange needs an assertion signed with a trusted key of the tenant. Reset the secret (or rotate, ROTATION above). If the private key may have leaked too, follow the item above.
- **A plain client's secret leaked** (a compute node, a job). Its holder can read and write all of the tenant's data and join as a compute node. Reset the secret or delete the client at once: a gradual rotation leaves the attacker the old secret. Tokens already issued stay valid for at most `CYODA_JWT_EXPIRY_SECONDS` (300 seconds by default); streams opened with the client close within 60 seconds.
- **An admin client's secret leaked.** Follow *A leaked admin token of another tenant* in `config.auth`, with the platform operator.
- **A cyoda token leaked.** It is valid until its `exp`. Only the platform operator can end it sooner, by revoking the signing key pair named by its `kid`, which ends every token that key pair signed, for every tenant. See *Emergency revocation of a leaked token* in `config.auth`.

Afterwards, review what the credentials did: entity changes and audit events whose `executedBy.id` is the leaked client (`GET /entity/{entityId}/changes`, `GET /audit/entity/{entityId}`), and the INFO log lines the operator can read (`M2M client created`, `trusted key registered` and the others listed in `config.auth`).

## SECURITY

- **TLS.** cyoda's HTTP (default 8080) and gRPC (default 9090) listeners have no TLS of their own: they serve plaintext. In production, TLS is terminated by the ingress, gateway or service mesh in front of cyoda (the Helm chart's `ingress.http.tls` and `ingress.grpc.tls`). Every client, and every compute node, connects through it and verifies the server certificate. For local development and Docker Compose, plaintext on localhost or a private Docker network is acceptable; never expose a plaintext port beyond it.
- **Keep credentials on the server.** The trusted key's private key, client secrets, user assertions and cyoda tokens never reach a browser or a mobile app.
- **Authorize before you act.** cyoda records the user you assert and enforces only the client's roles. Like a database, it cannot protect data from an application that is itself compromised.
- **Rate-limit at the edge.** The operator puts a per-source rate limit in front of `/api/oauth/token` (see `auth.tokens`).

## CLUSTER NOTES

`CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` and `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` are read by each node at startup, and the node that takes a request decides by its own value. Behind a load balancer, give every node the same value, or management calls fail at random with `404 FEATURE_DISABLED`. Neither flag affects tokens: an exchange works with every trusted key registered while registration was on, on every node, whatever that node's flag; an admin client keeps `ROLE_ADMIN` after its flag is turned off. Clients and trusted keys live in the shared store: a create, reset, delete, registration or invalidation is in force on every node when the call returns.

## RUNNING IT LOCALLY

**Mock mode** (`CYODA_IAM_MODE=mock`, the default) accepts every request, with or without a token, as one fixed principal: user id `mock-user-001`, tenant `mock-tenant`, kind `CYODA_IAM_MOCK_KIND` (default `service`), roles `CYODA_IAM_MOCK_ROLES` (default `ROLE_ADMIN,ROLE_M2M`). There is no executor apart from that principal, so callouts carry `mock-user-001` in both `authid` and `authexecid`, with the configured kind in both `authtype` and `authexectype`:

- `service` — like a client's own request. A compute node opens a stream with no token, and `authctx.Require` passes for the mock roles.
- `user` — `authctx.Require` never passes, and a compute-node stream is refused (`PermissionDenied`).
- `system` — the same refusals as `user`.

Mock mode has no client store: `POST /oauth/token` issues no token and answers `501 NOT_IMPLEMENTED`, as do the client and trusted-key endpoints (trusted keys answer `404 FEATURE_DISABLED` while their flag is off). **On-behalf-of access cannot be exercised in mock mode.** Use jwt mode for it.

**On-behalf-of end to end, in jwt mode, on one machine** (needs `openssl`, `xxd`, `jq` and `curl`):

```bash
openssl genrsa -out signing.pem 2048
export CYODA_IAM_MODE=jwt CYODA_JWT_SIGNING_KEY_FILE=$PWD/signing.pem \
  CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=true
cyoda &    # Docker Compose: set the same variables on the service
B=http://localhost:8080/api

# Operator token for tenant acme; with ROLE_M2M it can also reach data.
ADMIN_TOKEN=$(cyoda token --tenant acme --roles ROLE_ADMIN,ROLE_M2M)

# The backend's OBO client.
curl -s -X POST "$B/clients?onBehalfOf=true" \
  -H @- <<<"Authorization: Bearer $ADMIN_TOKEN" > obo.json
OBO_ID=$(jq -r .client_id obo.json); OBO_SECRET=$(jq -r .client_secret obo.json)

# The application's key, registered as a trusted key.
openssl genrsa -out app-key.pem 2048
N=$(openssl rsa -in app-key.pem -noout -modulus | cut -d= -f2 \
    | xxd -r -p | openssl base64 -A | tr '+/' '-_' | tr -d '=')
jq -n --arg n "$N" '{keyId:"app-key-1",jwk:{kty:"RSA",n:$n,e:"AQAB",alg:"RS256"}}' \
  | curl -s -X POST "$B/oauth/keys/trusted" -H 'Content-Type: application/json' \
      -d @- -H "Authorization: Bearer $ADMIN_TOKEN"

# A user assertion for alice (aud = CYODA_JWT_ISSUER, default "cyoda").
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
NOW=$(date +%s)
HDR=$(printf '{"alg":"RS256","kid":"app-key-1"}' | b64url)
PAY=$(jq -cn --argjson iat "$NOW" --argjson exp "$((NOW+300))" \
  '{sub:"alice",caas_org_id:"acme",aud:"cyoda",iat:$iat,exp:$exp}' | b64url)
SIG=$(printf '%s.%s' "$HDR" "$PAY" | openssl dgst -sha256 -sign app-key.pem -binary | b64url)

# Exchange it, then call cyoda as alice through the OBO client.
OBO_TOKEN=$(curl -s -X POST "$B/oauth/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
  -d subject_token_type=urn:ietf:params:oauth:token-type:jwt \
  -K- <<EOF | jq -r .access_token
user = "$OBO_ID:$OBO_SECRET"
data = "subject_token=$HDR.$PAY.$SIG"
EOF
)
curl -s "$B/account" -H @- <<<"Authorization: Bearer $OBO_TOKEN"   # userId "alice"
```

In a cluster, `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` goes on every node.

## MOVING FROM OIDC AND FORWARDED IDENTITY-PROVIDER TOKENS

cyoda no longer accepts identity-provider tokens and no longer has a bootstrap client. What changed, and what replaces each piece:

- **Identity-provider tokens on API calls** are refused with `401 UNAUTHORIZED`: cyoda accepts only tokens signed by its own key pairs. Stop forwarding your users' IdP tokens. Your users keep signing in to your application with your IdP; your backend becomes an OBO client (STEP 2 to STEP 4) and asserts the user's id — for example the IdP's `sub` — in a user assertion.
- **The OIDC provider endpoints** (`/oauth/oidc/providers` and everything below it) no longer exist and answer `404`. Delete your registration scripts. Provider records stored by an earlier release stay in the store, unread.
- **`CYODA_OIDC_*` variables** are ignored, with no warning. Remove them from your deployment.
- **The bootstrap client and `CYODA_BOOTSTRAP_*`** (`CYODA_BOOTSTRAP_CLIENT_ID`, `CYODA_BOOTSTRAP_CLIENT_SECRET`, `CYODA_BOOTSTRAP_CLIENT_SECRET_FILE`, `CYODA_BOOTSTRAP_TENANT_ID`, `CYODA_BOOTSTRAP_USER_ID`, `CYODA_BOOTSTRAP_ROLES`) are ignored, with no warning, and no client is created from them; a token request with the old bootstrap client answers `401 invalid_client`. The platform operator signs the first admin token with `cyoda token` instead (STEP 1).
- **One client for everything** becomes one client per part: an OBO client for the backend, a plain client for each compute node and job, an admin client for provisioning (STEP 2).
- **Roles from the IdP** no longer reach cyoda. Your application decides what a user may do; cyoda applies the client's roles.
- **The `oidc:` user-id prefix** is not reserved any more: a user id is exactly what you assert. Remove any `oidc:` handling.
- **`X-User-ID` on `POST /message`** is gone: the OBO token carries the user.
- **Compute nodes** that read `authid` as the caller read `authexecid`; `authid` is now the user the work is for, `authtype` is `user` for write-back cascades of a user's request, and `authclaims` hold the executor's roles (READING IDENTITY IN A COMPUTE NODE).
- **Trusted keys registered by an earlier release** are not read: register them again (STEP 3).
- **Compute nodes connect over TLS** (SECURITY).

## SEE ALSO

- `auth` — the access model and the role rules
- `auth.clients` — create, list, delete and reset clients
- `auth.tokens` — the token endpoint, every claim and every error description
- `auth.trusted-keys` — register, invalidate and delete trusted keys
- `cli.token` — the operator's offline admin token
- `config.auth` — server settings, the user-id rule, and the operator's incident procedures
- `grpc` — the compute-node protocol and the callout attributes
- `audit` — the identities recorded on audit events
- `errors` — every error code
