# Signing key-pair window — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

## Rule

A signing key pair (`/oauth/keys/keypair`) signs and verifies tokens only
inside its window, from `validFrom` (inclusive) to `validTo` (exclusive):

- New tokens are signed by the newest active key pair of the audience inside
  its window; on a tie in `validFrom`, the greater key id.
- A key pair issued with a future `validFrom` is published in JWKS at once,
  but does not sign until its window opens.
- A token whose key pair is outside its window is rejected with the uniform
  `401`.
- An invalidated key pair never signs again unless reactivated. Its tokens
  keep verifying until the end of its grace period: invalidating sets
  `validTo` to now plus the grace period, never later than its current
  `validTo`. A grace period of 0, the default (`gracePeriodSec` /
  `invalidateGracePeriodSec` omitted), ends verification at once;
  invalidating again with 0, or `DELETE`, cuts a running grace period short.
  JWKS publishes a key pair until it can no longer verify.

`POST /oauth/keys/keypair` refuses, with `400 BAD_REQUEST`:

- `invalidateCurrent: true` together with a `validFrom` in the future — it
  can leave the audience without a signing key until the new window opens
  (for example an audience other than the bootstrap key's, or once the
  bootstrap key is revoked);
- a `validTo` that is not in the future — the key pair could never sign.

`POST /oauth/keys/keypair/{keyId}/reactivate` refuses a `validFrom` in the
future (`400 BAD_REQUEST`): it would put the key pair outside its own window
at once. Invalidating the only key pair that can sign is allowed — revocation
must always work.

The bootstrap signing key (from configuration) has no window unless the
key-pair API gave it one: reactivating it through the API gives it the window
the request names (`validFrom` defaults to now), invalidating it ends it as a
signer and gives it a grace period like any key pair, and both states are
stored and shared by the cluster (see `signing-key-pairs.md`). It takes part
in signer selection like any key pair, with a zero `validFrom` until a
reactivation sets one. Until then it signs whenever no issued key pair of its
audience is active and inside its window: invalidating the last issued key
pair of the bootstrap key's audience makes the bootstrap key sign again. After
a reactivation its `validFrom` ranks it like any key pair: with the default
(now) it signs before every issued key pair of its audience with an earlier
`validFrom`; an early `validFrom`, such as `1970-01-01T00:00:00Z`, keeps it
behind them. A rotation
(`invalidateCurrent`) never ends it; only an invalidate or `DELETE` that names
its key id does. To end a leaked token, the operator revokes the key pair
named by the `kid` in its header; a rotation does not end tokens the bootstrap
key signed.

## Cloud action

Confirm that Cloud signs only with a key inside its window, rejects tokens
whose key is outside it, and refuses the three request shapes above — or record
where it differs.

Grace periods: Cloud's verification already follows the window alone
(`StoredJWKPublicKeyProvider.kt:39`, `JWKEntity.isValidKey`,
`StoredJWKService.kt:710-713`), and its invalidation sets `validTo` to now plus
the grace period (`StoredJWKService.kt:607-624`), so an invalidated key
verifies through its grace period on both tiers. Two differences remain for
Cloud to adopt:

1. **Never sign with a key in its grace period.** Cloud selects a signer by
   window alone (`isValidSigningKey`, `StoredJWKService.kt:409-412`), so a key
   in its grace period can keep signing. In cyoda-go an invalidated key pair
   never signs again unless reactivated.
2. **Never lengthen `validTo`.** Cloud sets `validTo` to now plus the grace
   period unconditionally; cyoda-go never moves it past the key pair's
   current `validTo`.

The default grace period differs, by design of each surface: cyoda-go's is 0
(revocation takes effect at once unless a grace period is asked for); Cloud's
rotation default is 3600 s (`IAMProperties.keyPairInvalidateGracePeriodSec`).
