---
topic: cli.token
title: "cyoda token — sign an admin token with the signing key"
stability: stable
see_also:
  - cli
  - auth
  - auth.clients
  - config.auth
---

# cli.token

## NAME

cli.token — sign a short-lived token with `CYODA_JWT_SIGNING_KEY` and print it.

## SYNOPSIS

`cyoda token --tenant <tenantId> [--user <userId>] [--roles <r1,r2>] [--ttl <duration>]`

## DESCRIPTION

`cyoda token` is how an operator gets the first token that can call the admin API: for example to create the M2M clients that applications and compute nodes use (`POST /clients`). It signs the token itself with the signing key from `CYODA_JWT_SIGNING_KEY` (or `CYODA_JWT_SIGNING_KEY_FILE`). It opens no store and makes no network call.

Whoever holds the signing key can already sign any token cyoda-go accepts, so the command adds no capability. Protect the signing key accordingly.

The token names a person: `sub` and `caas_user_id` are `--user`, `caas_org_id` is `--tenant`, the roles are in `user_roles`. It carries `iss` from `CYODA_JWT_ISSUER` and, when `CYODA_JWT_AUDIENCE` is set, `aud`.

The token serves HTTP calls and unary gRPC calls. It does not open a compute-node stream: that needs `ROLE_M2M`, and a compute node should use an M2M client, which can fetch new tokens itself.

## WHEN THE TOKEN IS REFUSED

A token from `cyoda token` verifies while the signing key verifies on the cluster:

- Rotating key pairs (`POST /oauth/keys/keypair` with `invalidateCurrent`) does not affect it.
- Invalidating or deleting the signing key by its key id does. This is how the root key is revoked: `cyoda token` then stops granting access, after the grace period if one was given. The command cannot tell offline.
- After that, admin access comes from an OIDC admin, or from an admin M2M client created beforehand. With neither, the recovery is a new `CYODA_JWT_SIGNING_KEY` on every node, which retires every issued key pair and every token in use.

## OPTIONS

- `--tenant <tenantId>` — required. The tenant the token acts in.
- `--user <userId>` — the user id recorded for calls made with the token. Default `operator`.
- `--roles <r1,r2>` — comma-separated roles. Default `ROLE_ADMIN`.
- `--ttl <duration>` — lifetime, greater than 0 and at most `CYODA_JWT_EXPIRY_SECONDS` (default 3600 s). Default `15m`.

## ENVIRONMENT VARIABLES

- `CYODA_JWT_SIGNING_KEY` / `CYODA_JWT_SIGNING_KEY_FILE` — the signing key (PEM, or base64-encoded PEM).
- `CYODA_JWT_ISSUER` — `iss` (default `cyoda`).
- `CYODA_JWT_AUDIENCE` — `aud`, when set.
- `CYODA_JWT_EXPIRY_SECONDS` — the upper bound of `--ttl` (default 3600).

## OUTPUT

The token and a newline on stdout, nothing else. Errors go to stderr and never contain the token or key material.

## EXIT CODES

- `0` — token printed.
- `1` — the signing key or configuration is missing, unreadable or unusable.
- `2` — flag error.

## EXAMPLES

```
# Local
TOKEN=$(cyoda token --tenant acme)
curl -H "Authorization: Bearer $TOKEN" -X POST http://localhost:8080/api/clients

# Kubernetes (the image's binary is /cyoda; the pod already holds the key)
TOKEN=$(kubectl exec <pod> -- /cyoda token --tenant acme)

# Docker Compose
TOKEN=$(docker compose exec <service> /cyoda token --tenant acme)
```
