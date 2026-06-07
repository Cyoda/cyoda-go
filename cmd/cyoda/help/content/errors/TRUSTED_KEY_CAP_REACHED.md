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

`POST /oauth/keys/trusted` enforces a per-tenant cap (default 10, configurable via `CYODA_IAM_TRUSTED_KEY_MAX_PER_TENANT`). The cap counts only currently-valid keys (Active and not past `validTo`). Delete or invalidate older keys, or raise the cap.

## SEE ALSO

- errors
- config.auth
