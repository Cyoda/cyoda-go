# Trusted keys belong to one tenant — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

## Rule

A trusted key belongs to the tenant that registered it. It verifies a token for
that tenant only. The tenant is taken from the key's registration, never from a
claim in the token it signs: the key holder writes those claims.

In cyoda-go a trusted key verifies one thing: the user assertion presented as
the subject token of the token-exchange grant (`POST /tenants/{tenant}/oauth/token`,
`urn:ietf:params:oauth:grant-type:token-exchange`), which only an on-behalf-of
client may use (see `obo-only-user-identity.md`). The key is read from the
store, on every exchange, in the exchanging client's tenant
(`TrustedKeyStore.GetForVerification`). A `kid`
registered by another tenant is not found: `400 invalid_request`, "unknown or
inactive trusted key" — the same answer as a `kid` that does not exist, so the grant does
not reveal another tenant's keys. The subject's `caas_org_id` must still equal
the client's tenant (`403 access_denied` otherwise).

cyoda-go does not accept a trusted-key-signed JWT as a bearer token on API
calls. Only cyoda's own signing keys verify bearer tokens.

## Key ids, lifecycle and cap

- Key ids are unique within a tenant only: each tenant's keys live in their
  own namespace, keyed by kid. Another tenant may register the same `keyId`
  for an independent key; there is no cross-tenant `409`.
- Register is an upsert on `(tenant, kid)`, so a retried registration
  succeeds.
- Register accepts a public key only: a JWK that carries a private member
  (`d`, `p`, `q`, `dp`, `dq`, `qi`, `oth`) is `400 BAD_REQUEST`, and the
  detail names the member; an RSA modulus under 2048 bits is `400
  BAD_REQUEST`. The store holds, and register and list return, only `kty`,
  `kid`, `n`, `e`, and `alg` / `use` when given.
- Trusted keys have no grace period and no `audience`. Invalidating a key
  ends it at once, and the invalidate request has no body;
  `invalidatePrevious` on register ends every other key of the tenant at
  once.
- `GET /oauth/keys/trusted` reads the store and can fail: `503
  STORAGE_UNAVAILABLE` when the store reports itself unavailable, `500`
  otherwise.
- The per-tenant cap counts every active key whose `validTo` has not passed,
  and applies to reactivation as well as registration (`400
  TRUSTED_KEY_CAP_REACHED`); a registration with `invalidatePrevious` is never
  refused by it.
- Register, invalidate, reactivate and delete write INFO lines with the
  tenant, the kid, and the attributed principal and executor of the call.

## What changed

An earlier design chose not to compare the key's tenant, to match Cloud, on the
reasoning that only tenant A holds tenant A's private key. That misses the case
where tenant A's key holder also holds a credential of tenant B: with any M2M
client of B it could exchange a subject token naming B, with any user and any
roles, including `ROLE_ADMIN`.

## Cloud today

Cloud has the same gap, and a wider one: it also accepts trusted-key-signed
JWTs as bearer tokens, picks the verifying key by `kid` alone, and takes the
tenant, user and roles from the token's claims. Tracked in CP-3974.

## Cloud action

1. When a token is verified with a stored trusted key, require the key's
   owning legal entity to equal the token's `caas_org_id`. Under the access
   model of `obo-only-user-identity.md` the only such path is the token
   exchange: stop accepting trusted-key JWTs as bearer tokens.
2. Make key ids unique per tenant, register an upsert, and drop the grace
   period and `audience` of trusted keys.
3. Apply the cap to reactivation as well as registration, or record the
   difference.
4. Refuse a JWK that carries a private member (`d`, `p`, `q`, `dp`, `dq`,
   `qi`, `oth`) with `400 BAD_REQUEST` naming the member, refuse an RSA
   modulus under 2048 bits with `400 BAD_REQUEST`, and store and return only
   the public members `kty`, `kid`, `n`, `e`, `alg`, `use`.
