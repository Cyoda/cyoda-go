# Signing key-pair window — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

## Rule

A signing key pair (`/oauth/keys/keypair`) signs and verifies tokens only
inside its window, from `validFrom` (inclusive) to `validTo` (exclusive):

- New tokens are signed by the newest active key pair of the audience inside
  its window; on a tie in `validFrom`, the greater key id.
- A key pair issued with a future `validFrom` is published in JWKS at once,
  but does not sign until its window opens.
- A token whose key pair is outside its window, or invalidated, is rejected
  with the uniform `401`. The invalidate grace period only keeps the public
  key in JWKS, for external verifiers that cache it.

`POST /oauth/keys/keypair` refuses, with `400 BAD_REQUEST`:

- `invalidateCurrent: true` together with a `validFrom` in the future — it
  would leave the audience without a signing key until the new window opens;
- a `validTo` that is not in the future — the key pair could never sign.

`POST /oauth/keys/keypair/{keyId}/reactivate` refuses a `validFrom` in the
future (`400 BAD_REQUEST`): it would put the key pair outside its own window
at once. Invalidating the only key pair that can sign is allowed — revocation
must always work — and leaves the audience without a signing key until a new
one is issued.

The bootstrap signing key (from configuration) has no window: it lasts as long
as the configuration supplies it. Reactivating it through the API gives it the
window the request names, on the node that serves the request.

## Cloud action

Confirm that Cloud signs only with a key inside its window, rejects tokens
whose key is outside it, and refuses the three request shapes above — or record
where it differs.
