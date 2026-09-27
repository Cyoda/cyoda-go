---
topic: errors.NOT_FOUND
title: "NOT_FOUND — generic resource not found"
stability: stable
see_also:
  - errors
  - errors.ENTITY_NOT_FOUND
  - errors.MODEL_NOT_FOUND
  - errors.KEYPAIR_NOT_FOUND
  - errors.TRUSTED_KEY_NOT_FOUND
---

# errors.NOT_FOUND

## NAME

NOT_FOUND — the requested resource does not exist.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by administrative endpoints when the supplied identifier does not match any registered resource. The submitted identifier is never echoed in the response body — only a generic descriptor — so attackers cannot use the response as a reflection oracle. The identifier is logged server-side at INFO for operator correlation.

Domain-specific not-found conditions (entity, model, transition, workflow, search-job) have their own dedicated codes — see SEE ALSO. The key-pair and trusted-key admin endpoints also have their own codes, `errors.KEYPAIR_NOT_FOUND` and `errors.TRUSTED_KEY_NOT_FOUND`, rather than this one.

Not retryable; the resource must be created or registered before the request can succeed.

## SEE ALSO

- errors
- errors.ENTITY_NOT_FOUND
- errors.MODEL_NOT_FOUND
- errors.KEYPAIR_NOT_FOUND
- errors.TRUSTED_KEY_NOT_FOUND
