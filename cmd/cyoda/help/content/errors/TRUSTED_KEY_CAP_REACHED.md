---
topic: errors.TRUSTED_KEY_CAP_REACHED
title: "TRUSTED_KEY_CAP_REACHED — tenant trusted-key cap reached"
stability: stable
see_also:
  - errors
  - config.auth
---

# errors.TRUSTED_KEY_CAP_REACHED

## NAME

TRUSTED_KEY_CAP_REACHED — the tenant has reached the maximum registered trusted keys.

## SYNOPSIS

HTTP: `400` `Bad Request`. Retryable: `no`.

## DESCRIPTION

`POST /oauth/keys/trusted` and `POST /oauth/keys/trusted/{keyId}/reactivate` enforce a per-tenant cap (default 10, configurable via `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`). The cap counts every key that can verify: an active key whose `validTo` has not passed. Trusted keys have no grace period, so an invalidated key frees its slot at once. A registration with `invalidatePrevious` ends every other key of the tenant and is never refused by the cap. Delete or invalidate an older key, or raise the cap.

## SEE ALSO

- errors
- config.auth
