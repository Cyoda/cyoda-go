---
topic: errors.SERVER_BUSY
title: "SERVER_BUSY — the node has no capacity left for this request right now"
stability: stable
see_also:
  - errors
  - auth.clients
  - config.auth
---

# errors.SERVER_BUSY

## NAME

SERVER_BUSY — the node could not start the bounded work the request needs within the wait. Nothing was written.

## SYNOPSIS

HTTP: `503` `Service Unavailable`. Retryable: `yes`. Carries `Retry-After: 1`.

## DESCRIPTION

Raised by `POST /clients` and `PUT /clients/{clientId}/secret` when the new client secret cannot be hashed. Hashing a secret is bcrypt work, and each node runs at most `CYODA_IAM_TOKEN_MAX_CONCURRENT_SECRET_CHECKS` bcrypt operations at once: the secret checks of `POST /oauth/token` and these hashes share the same slots. A request that gets no slot within 1 second is refused before anything is written. No client is created, and a reset leaves the old secret in force.

Retryable. Send the request again after the `Retry-After` delay. Repeated occurrences mean the node is saturated with token requests. Each refusal is counted in `cyoda.auth.secret_checks.refused` (see `cyoda help telemetry`). Raise the setting, or add nodes, if the load is legitimate.

The token endpoint reports the same condition in its own OAuth shape: `503 temporarily_unavailable` (see `cyoda help auth tokens`).

## SEE ALSO

- errors
- auth.clients
- config.auth
