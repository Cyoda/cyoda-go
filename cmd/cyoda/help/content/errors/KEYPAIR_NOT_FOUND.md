---
topic: errors.KEYPAIR_NOT_FOUND
title: "KEYPAIR_NOT_FOUND — signing keypair not found"
stability: stable
see_also:
  - errors
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
- `GET /oauth/keys/keypair/current` — no active signing key.

Also returned when the keyId names:

- A key pair owned by another bootstrap key. It is retired after
  `CYODA_JWT_SIGNING_KEY` was replaced, until that key is restored.
- The bootstrap key after it was deleted through the API. This is permanent
  for that key.
- Another bootstrap key's state record, for example its revocation record.
  Only a node configured with that bootstrap key can change it.
- A record that cannot be decoded, on invalidate or reactivate. Delete it
  instead. At this node's bootstrap key id, a delete permanently deletes the
  bootstrap key.

Verify the keyId, or check whether any key pair is active and inside its window.

## SEE ALSO

- errors
