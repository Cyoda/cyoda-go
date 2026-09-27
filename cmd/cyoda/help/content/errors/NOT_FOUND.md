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

NOT_FOUND — a generic resource-not-found error code, declared but not currently returned by any endpoint.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

No current endpoint answers this code. The key-pair and trusted-key admin
endpoints, which a generic "resource not found" description might suggest use
it, in fact return their own dedicated codes — `errors.KEYPAIR_NOT_FOUND` and
`errors.TRUSTED_KEY_NOT_FOUND` — never this one. Domain-specific not-found
conditions (entity, model, transition, workflow, search-job) likewise have
their own dedicated codes — see SEE ALSO.

Not retryable, should it come to be used; the resource would need to be
created or registered before the request could succeed.

## SEE ALSO

- errors
- errors.ENTITY_NOT_FOUND
- errors.MODEL_NOT_FOUND
- errors.KEYPAIR_NOT_FOUND
- errors.TRUSTED_KEY_NOT_FOUND
