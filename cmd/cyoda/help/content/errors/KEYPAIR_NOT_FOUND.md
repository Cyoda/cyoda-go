---
topic: errors.KEYPAIR_NOT_FOUND
title: "KEYPAIR_NOT_FOUND — signing keypair not found"
stability: stable
see_also:
  - errors
  - errors.NOT_FOUND
---

# errors.KEYPAIR_NOT_FOUND

## NAME

KEYPAIR_NOT_FOUND — the requested JWT signing keypair is not present.

## SYNOPSIS

HTTP: `404` `Not Found`. Retryable: `no`.

## DESCRIPTION

Returned by:

- `DELETE /oauth/keys/keypair/{keyId}` — keyId not present.
- `POST /oauth/keys/keypair/{keyId}/invalidate` — keyId not present.
- `POST /oauth/keys/keypair/{keyId}/reactivate` — keyId not present.
- `GET /oauth/keys/keypair/current?audience=X` — no active key for audience X.

Verify the keyId, or check the bootstrap-key audience configuration via `CYODA_JWT_BOOTSTRAP_AUDIENCE`.

## SEE ALSO

- errors
- errors.NOT_FOUND
