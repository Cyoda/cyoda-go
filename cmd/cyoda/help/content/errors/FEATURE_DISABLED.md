---
topic: errors.FEATURE_DISABLED
title: "FEATURE_DISABLED — feature is not enabled"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.FEATURE_DISABLED

## NAME

FEATURE_DISABLED — the operation belongs to an optional feature not enabled in this deployment.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by trusted-key endpoints when `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED=false` (the default):

- `GET /oauth/keys/trusted`
- `POST /oauth/keys/trusted`
- `DELETE /oauth/keys/trusted/{keyId}`
- `POST /oauth/keys/trusted/{keyId}/invalidate`
- `POST /oauth/keys/trusted/{keyId}/reactivate`

Enable by setting the env var and restarting. Keypair endpoints (`/oauth/keys/keypair/*`) are unaffected.

## SEE ALSO

- errors
- config.auth
