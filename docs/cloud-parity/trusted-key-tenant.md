# Trusted keys belong to one tenant — Cloud twin-alignment spec

cyoda-go defines the contract; Cyoda Cloud aligns to it.

## Rule

A trusted key belongs to the tenant that registered it. It verifies a token for
that tenant only. The tenant is taken from the key's registration, never from a
claim in the token it signs: the key holder writes those claims.

In cyoda-go a trusted key verifies one thing: the subject token of the
token-exchange grant (`POST /oauth/token`,
`urn:ietf:params:oauth:grant-type:token-exchange`). The key is looked up in the
exchanging M2M client's tenant (`TrustedKeyStore.GetForVerification`). A `kid`
registered by another tenant is not found: `400 invalid_grant`, "unknown
trusted key" — the same answer as a `kid` that does not exist, so the grant does
not reveal another tenant's keys. The subject's `caas_org_id` must still equal
the client's tenant (`403 access_denied` otherwise).

cyoda-go does not accept a trusted-key-signed JWT as a bearer token on API
calls. Only cyoda's own signing keys and registered OIDC providers verify bearer
tokens.

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

When a token is verified with a stored trusted key, require the key's owning
legal entity to equal the token's `caas_org_id`, on every path that verifies
with it (bearer authentication and token exchange).
