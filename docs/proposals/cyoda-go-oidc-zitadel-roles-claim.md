# Feature request: OIDC federation — support object-shaped roles claims (Zitadel)

**Repo:** cyoda-go
**Type:** enhancement — federated OIDC role extraction
**Filed by:** SIOMS team (platform consumer)
**Relates to:** `auth.oidc` (`POST /oauth/oidc/providers`, the `rolesClaim` config).

## Summary

The federated-OIDC role extraction currently expects the configured `rolesClaim` to resolve to a
**JSON string array**. **Zitadel** (and some other IdPs) emit roles as a **JSON object** keyed by
role, not an array. Extend extraction so that when the claim value is an object, the **role list is
its top-level keys**. Without this, every Zitadel-federated user resolves to *no roles*.

## Zitadel's actual claim shape (the format to support)

With project-role assertion enabled, Zitadel issues the claim **`urn:zitadel:iam:org:project:roles`**
whose value is an object: each granted role key maps to an object of `{ orgId: orgPrimaryDomain }`.

Real SIOMS example — a user granted the `admin` role in project `sioms-app`:

```json
{
  "sub": "377153257094512643",
  "iss": "http://localhost:8081",
  "urn:zitadel:iam:org:project:roles": {
    "admin": { "312...orgId": "sioms.localhost" }
  }
}
```

A multi-role user:

```json
"urn:zitadel:iam:org:project:roles": {
  "sales_rep": { "312...orgId": "sioms.localhost" },
  "warehouse": { "312...orgId": "sioms.localhost" }
}
```

The **role keys** (`admin`, `sales_rep`, `warehouse`) are the role values we need; the inner
`{ orgId: domain }` map is Zitadel routing metadata and is irrelevant for role extraction.

(There is also a per-project variant `urn:zitadel:iam:org:project:<projectId>:roles` with the same
value shape; supporting object-by-keys handles both — the operator points `rolesClaim` at whichever
claim their IdP emits.)

## Required behaviour

When resolving roles from the JWT at the configured `rolesClaim` path, accept all three shapes:

1. **String array** — `["admin","warehouse"]` → use as-is. *(current behaviour, unchanged)*
2. **Object / map** — `{ "admin": {...}, "warehouse": {...} }` → roles = the **top-level keys**
   (`["admin","warehouse"]`). Inner values are ignored regardless of type.
3. **Single string** — `"admin"` → `["admin"]`. *(nice-to-have; treat a lone string as one role.)*

Empty array / empty object / claim absent → empty role set (existing behaviour; the user is
authenticated but unprivileged). A scalar that is neither string nor array nor object (number,
bool) → empty role set (don't crash).

This should be the default extraction (no per-provider flag needed) since taking object keys is
unambiguous and backward-compatible with the string-array path.

## Authorization mapping — DECIDED

Separate from *extraction* is what the extracted roles **grant** for cyoda API access. **Decision
(SIOMS auth design, [docs/design/auth.md](../design/auth.md)):**

- A valid, tenant-bound **federated user is authorized for tenant-scoped CRUD by virtue of valid
  federation** — there is **no required/baseline cyoda role** and **no `rolePrefix`/role mapping**.
- Extracted roles are **descriptive only** (used for `sub`-based attribution and available to the
  caller); they are **not a cyoda-side authorization gate**.
- **SIOMS owns RBAC end-to-end in the BFF** (deny-by-default `authorize(role, resource, action)`,
  the role read from the user's session token). cyoda's job is authn + tenant-binding + `sub`
  attribution only.

SIOMS's app roles are `admin` / `sales_rep` / `warehouse` (no `ROLE_` prefix); cyoda neither requires
nor interprets them for access decisions.

## Acceptance criteria

- A Zitadel JWT with `urn:zitadel:iam:org:project:roles` as an object, with `rolesClaim` pointed at
  that claim, yields the role keys as the user's roles (single- and multi-role).
- String-array `rolesClaim` extraction is unchanged.
- Empty/absent/non-collection claim → empty roles, no error.
- A valid Zitadel-federated user can perform tenant-scoped CRUD and writes are attributed to `sub`
  (per the authorization-mapping decision above).
- `cyoda help auth oidc` notes the supported claim shapes.

## References

- SIOMS Zitadel config: `infra/zitadel/seed.sh` (`projectRoleAssertion:true`; roles
  `admin`/`sales_rep`/`warehouse`), `infra/zitadel/create-oidc-app.sh`
  (`idTokenRoleAssertion`/`accessTokenRoleAssertion`).
- Auth migration consuming this: `docs/superpowers/specs/2026-06-17-ioms-auth-oidc-federation-migration.md`.
