---
topic: auth
title: "auth — authenticate client applications against cyoda"
stability: evolving
version_added: 0.8.0
see_also:
  - auth.integration
  - auth.tokens
  - auth.clients
  - auth.trusted-keys
  - cli.token
  - config.auth
  - openapi
  - errors.UNAUTHORIZED
  - errors.FORBIDDEN
---

# auth

## NAME

auth — authenticate client applications against cyoda.

## GOAL

Every cyoda API call needs an `Authorization: Bearer <jwt>` header carrying a token cyoda signed. This page helps you decide how to get that token.

**Building an application?** Start with `auth.integration` (`cyoda help auth integration`): the step-by-step guide for a backend acting for its users, background jobs, compute nodes and the tenant admin, with the exact requests, error handling, rotation, incidents, a local end-to-end recipe, and the move from forwarded identity-provider tokens.

## ACCESS MODEL

Only M2M clients connect to cyoda. Users never call cyoda directly: an application signs its own users in, decides what each user may do, and calls cyoda for them.

- A **plain client** (`ROLE_M2M`) or an **admin client** (`ROLE_M2M`, `ROLE_ADMIN`) gets a token of its own with `client_credentials`. Its changes are recorded as the client's.
- An **on-behalf-of client** (`ROLE_M2M`, created with `?onBehalfOf=true`) acts for the application's users. The application signs a short user assertion with a **trusted key** it registered, and the client exchanges the assertion for a token of that user (token exchange). The token carries the client's roles, never the user's: cyoda has no per-user permissions. cyoda records the user as the one the change is for and the client as its executor, and passes both to compute nodes.

cyoda records what the client states about the user; it does not verify the user. Like a database, cyoda cannot protect data from an application that is itself compromised.

The one exception is the platform operator's offline token from `cyoda token`, signed with `CYODA_JWT_SIGNING_KEY`.

## WHICH PATH DO I NEED?

- **You are integrating an application** — read `auth.integration` first; it links every reference below.
- **You operate the deployment and need the first admin token** — `cyoda token` signs one offline with `CYODA_JWT_SIGNING_KEY`; use it to create M2M clients. Read `cli.token`.
- **A service or a compute node calls cyoda as itself** — create a plain M2M client and get tokens with `client_credentials`. Read `auth.clients` then `auth.tokens`.
- **Your application calls cyoda for its signed-in users** — create an on-behalf-of client, register a trusted key, and exchange user assertions for tokens. Read `auth.trusted-keys`, then the token-exchange section of `auth.tokens`.

**Looking for OBO?** The token-exchange (on-behalf-of) grant is documented as a section of `auth.tokens` — there is no separate `auth.obo` page. Run `cyoda help auth tokens` and read the token-exchange section.

**Looking for env vars?** All `CYODA_IAM_*`, `CYODA_JWT_*` and `CYODA_HMAC_*` knobs live in `config.auth`. Run `cyoda help config auth`.

## TOKEN PRESENTATION

All cyoda APIs accept the JWT via `Authorization: Bearer <token>`. The token claim shape — `sub`, `iss`, `caas_org_id`, `caas_user_id`, `user_roles` or `scopes`, `caas_tier`, `exp`, `iat`, `jti`, optionally `aud` and `act` — is documented in `auth.tokens`.

## ROLES

`POST /api/tenants/{tenant}/oauth/token` and `GET /api/.well-known/jwks.json` take no bearer token at all (the token endpoint authenticates the client with HTTP Basic). Every other HTTP operation and every gRPC call requires `ROLE_M2M` in the token's roles, except:

- `GET /account`;
- the client and trusted-key operations (`/clients*`, `/oauth/keys/trusted*`), which require `ROLE_ADMIN`;
- the key-pair operations (`/oauth/keys/keypair*`) and `/admin/*`, which require a platform operator (`ROLE_ADMIN` in the tenant `PLATFORM`).

A token without `ROLE_M2M` gets `403 FORBIDDEN` ("this operation requires ROLE_M2M") or gRPC `PermissionDenied` before the operation runs. Every M2M client holds `ROLE_M2M`, so its tokens reach data. There is no finer split: a client's own token with `ROLE_M2M` reaches every data operation of its tenant, model and workflow import included, and can open a compute-node stream with any tags; the only other distinctions are `ROLE_ADMIN` and the on-behalf-of permission. A token from the token exchange never administers: the client, trusted-key, key-pair and `/admin/*` operations answer it `403 FORBIDDEN` whatever its roles. A token from `cyoda token` carries the roles it was signed with: sign it with `--roles ROLE_ADMIN,ROLE_M2M` to reach data (see `cli.token`). In mock mode the principal's roles are `CYODA_IAM_MOCK_ROLES`, `ROLE_ADMIN,ROLE_M2M` by default.

## SEE ALSO

- `auth.integration` — the application integration guide, step by step
- `cli.token` — sign the first admin token with the signing key
- `config.auth` — env-var reference for every auth knob
- `openapi` — run `cyoda help openapi tags` for spec by tag, including the OAuth key and client operations
- `errors.UNAUTHORIZED`, `errors.FORBIDDEN` — universal auth-failure codes
