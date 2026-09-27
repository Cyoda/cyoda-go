# Signing key pairs — Cloud twin-alignment spec

cyoda-go **defines** the contract; Cyoda Cloud aligns to it. See also
`signing-key-window.md` for the window a signing key pair must be inside to
sign or verify — this document covers what happens to a key pair across nodes,
across a restart, and when its issuing bootstrap key is replaced.

## What cyoda-go does

- **Key pairs are shared and persisted.** Issuing, invalidating, reactivating
  or deleting a key pair (`/oauth/keys/keypair*`) on one node of a cluster
  takes effect on every other node, and the change survives a restart on a
  persistent backend (sqlite, postgres, or a commercial backend). The memory
  backend persists nothing, by design. A node applies the change locally before
  answering; other nodes apply it when the change reaches them (normally under
  a second) or, if it is missed, at the next periodic reconcile.
- **The bootstrap key's revocation is persisted too.** Invalidating,
  reactivating or deleting the bootstrap signing key
  (`CYODA_JWT_SIGNING_KEY`) through the API is stored and cluster-wide, and it
  survives a restart — a restart no longer restores a revoked bootstrap key.
  Deleting it is terminal for that key: no API call reactivates a deleted
  bootstrap key. Recovery is replacing `CYODA_JWT_SIGNING_KEY`, which mints a
  fresh KID with no stored state.
- **Replacing the bootstrap key retires every key pair it owned.** A key pair
  is sealed under the bootstrap key active when it was issued. Once
  `CYODA_JWT_SIGNING_KEY` is replaced, those key pairs stop signing, verifying
  and appearing in JWKS — this is the deliberate response to the bootstrap
  key's PEM being exposed, not a side effect to work around. Restoring the old
  bootstrap key brings them back. **No Cloud equivalent**: Cloud's configured
  signing key is not stored, so it has nothing to retire when it is replaced.
- **A retired key pair answers 404, not 200 with stale data.**
  `DELETE`/`invalidate`/`reactivate` on a retired key pair, and `current` when
  the selected key pair would be one, all answer `404 KEYPAIR_NOT_FOUND`; a
  retired key pair is excluded from JWKS and from signer selection. It becomes
  usable again only if the bootstrap key that owns it is restored.
- **A storage failure is 503, never a stale or wrong answer.** All five
  `/oauth/keys/keypair*` endpoints and `GET /.well-known/jwks.json` answer
  `503 STORAGE_UNAVAILABLE` (JWKS: with `Retry-After`) when the node cannot
  reach its storage, or when its copy of the store has gone stale (no
  successful reconcile for 10 reconcile intervals). This is retryable and is
  never downgraded to a `404` or to an empty/partial answer: an unavailable
  store fails the request rather than serving a guess.
- **At-rest sealing is an implementation property, not part of the contract.**
  cyoda-go seals an issued key pair's private key at rest (AES-256-GCM under a
  key derived from the bootstrap key's RSA primes) so a copy of the store
  alone — a backup, a replica, a read-only injection — does not expose private
  key material. Cloud stores the PKCS#8 form of an issued key pair
  unencrypted. What both tiers owe callers is identical (a private key that
  never leaves the server in a signing or verification response, and a key
  pair's lifecycle behaving as above); how it is protected at rest is each
  tier's own storage decision, not something Cloud must match.

## Cloud action

Confirm, or record where Cloud differs:

1. A key pair issued, invalidated, reactivated or deleted on one node is
   visible and effective on every node, and survives a restart.
2. Invalidating, reactivating or deleting the bootstrap signing key is stored
   cluster-wide and survives a restart; deleting it is terminal until the
   configured key is replaced.
3. Replacing the configured signing key does not carry old issued key pairs
   forward as still-usable — if Cloud has no notion of "retiring" a key pair
   because it does not store the configured key, record that as the
   difference rather than as a gap.
4. A retired, deleted-bootstrap or otherwise unusable key pair answers `404`
   on every lifecycle endpoint, never a stale `200`.
5. A storage or availability failure on a key-pair endpoint or JWKS answers a
   retryable `5xx`, never a `404` and never an empty or partial JWKS set.

## CaaS ticket to file

**Title:** `[CaaS] Signing key pairs: persisted bootstrap revocation, 503 on key-pair endpoints and JWKS, 404 for retired key pairs`

**Description:**

cyoda-go now shares signing key pairs across every node of a cluster and
persists them (and the bootstrap key's invalidate/reactivate/delete state)
across a restart. Confirm Cloud's equivalent behaviour, or record the
divergence, against these points (full contract:
`docs/cloud-parity/signing-key-pairs.md` in cyoda-go):

- A key pair change (issue/invalidate/reactivate/delete) and a bootstrap-key
  revocation change are effective everywhere Cloud's request could land, and
  persist across a restart.
- Deleting the bootstrap key is terminal until the configured key changes; no
  reactivate call restores a deleted bootstrap key.
- A key pair that cannot be used because its owning key was replaced answers
  `404`, not a stale `200`.
- A storage or availability failure on a key-pair endpoint, or on the JWKS
  endpoint, answers a retryable `5xx`, never `404` and never an empty or
  partial key set.
- At-rest sealing of a private key is an implementation detail, not part of
  the contract — Cloud need not match cyoda-go's AES-256-GCM sealing scheme,
  only the visible behaviour above.
