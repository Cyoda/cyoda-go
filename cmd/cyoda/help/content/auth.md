---
topic: auth
title: "auth — authenticate client applications against cyoda"
stability: evolving
version_added: 0.8.0
see_also:
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

Every cyoda API call needs an `Authorization: Bearer <jwt>` header. This page helps you decide how to get that JWT.

## WHICH PATH DO I NEED?

- **You operate the deployment and need the first admin token** — `cyoda token` signs one offline with `CYODA_JWT_SIGNING_KEY`; use it to create M2M clients. Read `cli.token`.
- **You have a `client_id`/`secret` you'll manage in cyoda** — use the M2M client + token endpoint. Read `auth.clients` then `auth.tokens`.
- **You have an existing IdP (Cognito, Keycloak, Auth0, …)** — federate via OIDC. Read `auth.oidc`.
- **Your own system signs JWTs for its users, no IdP** — register a trusted key, and have an M2M client exchange those JWTs for cyoda tokens. Read `auth.trusted-keys`.
- **You have an M2M client acting on behalf of a user** — use the token-exchange grant. Read the OBO section of `auth.tokens`.

**Looking for OBO?** The token-exchange (on-behalf-of) grant is documented as a section of `auth.tokens` — there is no separate `auth.obo` page. Run `cyoda help auth tokens` and read the token-exchange section.

**Looking for env vars?** All `CYODA_OIDC_*`, `CYODA_IAM_*`, `CYODA_JWT_*` and `CYODA_HMAC_*` knobs live in `config.auth`. Run `cyoda help config auth`.

## TOKEN PRESENTATION

All cyoda APIs accept the JWT via `Authorization: Bearer <token>`. The token claim shape — `sub`, `iss`, `caas_org_id`, `caas_user_id`, `user_roles` or `scopes`, `caas_tier`, `exp`, `iat`, `jti`, optionally `aud` and `act` — is documented in `auth.tokens`.

## SEE ALSO

- `cli.token` — sign the first admin token with the signing key
- `config.auth` — env-var reference for every auth knob
- `openapi` — run `cyoda help openapi tags` for spec by tag, including OAuth and OIDC
- `errors.UNAUTHORIZED`, `errors.FORBIDDEN` — universal auth-failure codes
