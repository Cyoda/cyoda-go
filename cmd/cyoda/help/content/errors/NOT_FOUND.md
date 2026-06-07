---
topic: errors.NOT_FOUND
title: "NOT_FOUND — generic resource not found"
stability: stable
see_also:
  - errors
  - errors.ENTITY_NOT_FOUND
  - errors.MODEL_NOT_FOUND
  - errors.KEYPAIR_NOT_FOUND
---

# errors.NOT_FOUND

## NAME

NOT_FOUND — the requested resource (key pair, trusted key, or other admin-managed object) does not exist.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by administrative endpoints (key pair lifecycle, trusted-key lifecycle) when the supplied identifier does not match any registered resource. The submitted identifier is never echoed in the response body — only a generic descriptor — so attackers cannot use the response as a reflection oracle. The identifier is logged server-side at INFO for operator correlation.

Domain-specific not-found conditions (entity, model, transition, workflow, search-job) have their own dedicated codes — see SEE ALSO.

Not retryable; the resource must be created or registered before the request can succeed.

## SEE ALSO

- errors
- errors.ENTITY_NOT_FOUND
- errors.MODEL_NOT_FOUND
- errors.KEYPAIR_NOT_FOUND
