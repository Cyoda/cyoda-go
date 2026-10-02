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

`cyoda token` is how an operator gets the first token that can call the admin API: for example to create the M2M clients that applications and compute nodes use (`POST /clients`). It signs the token itself with the signing key from `CYODA_JWT_SIGNING_KEY` (or `CYODA_JWT_SIGNING_KEY_FILE`), which the key-pair API and `cyoda help config auth` call the bootstrap key. It opens no store and makes no network call.

Whoever holds the signing key can already sign any token cyoda-go accepts, so the command adds no capability. Protect the signing key accordingly.

The token names a person: `sub` and `caas_user_id` are `--user`, `caas_org_id` is `--tenant`, the roles are in `user_roles`. It carries `iss` from `CYODA_JWT_ISSUER` and, when `CYODA_JWT_AUDIENCE` is set, `aud`.

The token serves HTTP calls and unary gRPC calls. It never opens a compute-node stream, whatever its roles: a stream opens only with an M2M client's own `client_credentials` token. A compute node uses an M2M client, which can fetch new tokens itself.

The token carries exactly the roles it is signed with. The default, `ROLE_ADMIN`, reaches `GET /account` and the admin operations: clients, trusted keys and, in `PLATFORM`, key pairs and `/admin/*`. Every other operation — entities, models, search, messages, audit, scheduled tasks, every gRPC call — requires `ROLE_M2M` and answers `403 FORBIDDEN` (gRPC `PermissionDenied`) without it. To reach data, sign with `--roles ROLE_ADMIN,ROLE_M2M`.

The key-pair endpoints and the runtime controls (`/admin/log-level`, `/admin/trace-sampler`) need a platform operator: `ROLE_ADMIN` in the tenant `PLATFORM`. Use `cyoda token --tenant PLATFORM` for them.

## WHEN THE TOKEN IS REFUSED

A token from `cyoda token` verifies while the signing key verifies on the cluster:

- Rotating key pairs (`POST /oauth/keys/keypair` with `invalidateCurrent`) does not affect it.
- Invalidating or deleting the signing key by its key id does. This is how the root key is revoked: `cyoda token` then stops granting access, after the grace period if one was given. The command cannot tell offline. The key-pair endpoints that do this (`/oauth/keys/keypair/*`) require a platform operator (`--tenant PLATFORM`).
- Reactivating the signing key gives it a window that ends at the reactivation's `validTo`; tokens from `cyoda token` are refused from that time.
- Reactivating the signing key also sets its `validFrom`, which defaults to now. From then on it signs `POST /oauth/token` tokens before every issued key pair with an earlier `validFrom`. To keep the issued key pairs signing, pass an early `validFrom`, for example `1970-01-01T00:00:00Z` (see `cyoda help config auth`).
- After that, the platform operator's ways back to the key-pair endpoints are a token from `cyoda token --tenant PLATFORM` while the bootstrap key verifies (including its grace period); an admin M2M client in `PLATFORM`, created before the bootstrap key is revoked, while an issued key pair signs its tokens (creating an admin M2M client needs `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true`); or a new `CYODA_JWT_SIGNING_KEY` on every node, which retires every issued key pair and every token cyoda-go signed. Create an admin M2M client in `PLATFORM` before revoking the signing key. Store that client's secret like `CYODA_JWT_SIGNING_KEY`. If it may have leaked, follow *A leaked platform admin-client secret* in `cyoda help config auth`.

## OPTIONS

- `--tenant <tenantId>` — required. The tenant the token acts in.
- `--user <userId>` — the user id recorded for calls made with the token. Default `operator`. It must pass the user-identifier rule in `config.auth`; `system` is reserved and refused (exit 2). Use a distinctive user id: the value is recorded as the caller in audit, and another principal can carry the same id (for example the user of a token exchange).
- `--roles <r1,r2>` — comma-separated roles. Default `ROLE_ADMIN`, which reaches only the admin operations; `--roles ROLE_ADMIN,ROLE_M2M` also reaches data.
- `--ttl <duration>` — lifetime, at least `1s` and at most `CYODA_JWT_EXPIRY_SECONDS` (default 300 s); a value outside that range is a flag error (exit 2). Default `15m`, or `CYODA_JWT_EXPIRY_SECONDS` when that is shorter.

## ENVIRONMENT VARIABLES

- `CYODA_JWT_SIGNING_KEY` / `CYODA_JWT_SIGNING_KEY_FILE` — the signing key (PEM, or base64-encoded PEM).
- `CYODA_JWT_ISSUER` — `iss` (default `cyoda` when unset). An empty value makes the command exit 1.
- `CYODA_JWT_AUDIENCE` — `aud`, when set.
- `CYODA_JWT_EXPIRY_SECONDS` — the upper bound of `--ttl`. Unset or empty
  means 300. Otherwise it must be a whole number of seconds from 1 to
  3600; any other value makes the command exit 1.

## OUTPUT

The token and a newline on stdout, nothing else, so `TOKEN=$(cyoda token …)` captures only the token. Errors go to stderr and never contain the token or key material. Informational log lines may also appear on stderr, for example which env files were loaded (see `cyoda help config`).

## EXIT CODES

- `0` — token printed, or `-h` / `--help` (the usage goes to stderr).
- `1` — the signing key or configuration is missing, unreadable or unusable.
- `2` — flag error.

## EXAMPLES

```
# Local
TOKEN=$(cyoda token --tenant acme)
# -H @- reads the header from stdin: a command line is visible to other
# local users, stdin is not.
curl -H @- -X POST http://localhost:8080/api/clients <<<"Authorization: Bearer $TOKEN"

# Data: entities, models, search (ROLE_M2M is required)
TOKEN=$(cyoda token --tenant acme --roles ROLE_ADMIN,ROLE_M2M)
curl -H @- http://localhost:8080/api/model/ <<<"Authorization: Bearer $TOKEN"

# Platform operator (key-pair, /admin/* endpoints)
TOKEN=$(cyoda token --tenant PLATFORM)
curl -H @- 'http://localhost:8080/api/oauth/keys/keypair/current' \
  <<<"Authorization: Bearer $TOKEN"

# Create the recommended PLATFORM admin M2M client
# (server started with CYODA_IAM_M2M_ADMIN_ROLE_ENABLED=true)
TOKEN=$(cyoda token --tenant PLATFORM)
# A private directory: no other user can plant a symlink at the file name.
if dir=$(mktemp -d) && cd "$dir"; then
  # --fail: an error answer is not saved as if it were the credential.
  (umask 077; curl --fail -H @- -X POST \
    'http://localhost:8080/api/clients?withAdminRole=true' -o platform-client.json \
    <<<"Authorization: Bearer $TOKEN")
fi
# Move client_id and client_secret into a secret store kept like
# CYODA_JWT_SIGNING_KEY (the secret is shown only once), then delete the file.
# Consider setting CYODA_IAM_M2M_ADMIN_ROLE_ENABLED back to false: it applies
# to every tenant, and turning it off does not affect existing clients.

# Kubernetes (the image's binary is /cyoda; the pod already holds the key)
TOKEN=$(kubectl exec <pod> -- /cyoda token --tenant acme)

# Docker Compose
TOKEN=$(docker compose exec -T <service> /cyoda token --tenant acme)
```
