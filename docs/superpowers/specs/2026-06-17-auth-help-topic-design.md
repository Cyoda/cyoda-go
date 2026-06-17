# Top-level `auth` help topic + v0.8.0 OIDC polish — design

| Field | Value |
|---|---|
| Issue | TBD — single umbrella issue covering all work in this spec, filed before PR open |
| Target milestone | v0.8.0 (cyoda-go) |
| Spec date | 2026-06-17 |
| Review iterations | 1 (interactive brainstorming) |
| Status | Draft, pending review |
| Related repos | `cyoda-go` only |
| Worktree / branch | `.worktrees/feat-auth-help-topic` / `feat/auth-help-topic` off `release/v0.8.0` |

## 1. Background

The OIDC providers subsystem (#284, PR #314) landed on `release/v0.8.0` as the third "OIDC is done" iteration. A pre-spec audit during this brainstorm surfaced gaps that contradict that claim, in particular a wire-fidelity break against cyoda-cloud's OpenAPI contract. Separately, an information-architecture gap in `cyoda help` means that a developer (or an LLM helping a developer) writing a client application against cyoda has no discoverable narrative entry point for authentication: env vars live in `config/auth`, error semantics in `errors/OIDC_*`, and the OpenAPI spec describes shape but not flow — nowhere does the help tree answer "how does my client get a JWT?".

This change closes both gaps in one PR:

1. Bugfix the OIDC response DTO so registered fields round-trip per spec.
2. Add a top-level `auth` help topic with four subtopics (`clients`, `tokens`, `oidc`, `trusted-keys`) optimised for an LLM-implementing-a-client audience.
3. Clean up `docs/cyoda/` and `cloud-divergences.md` so the stub-tracking matches the v0.8.0 conformance reality (10 keys + 7 OIDC + clients now conformant).
4. Update `cmd/cyoda/help/content/openapi.md` and `COMPATIBILITY.md` to stop claiming OIDC is 501.

## 2. Decisions log

| # | Decision | Rationale |
|---|---|---|
| D1 | Audience for new topics is **LLMs helping developers write client code**, not human operators. Page structure optimises for predictable section anchors. | Per user direction; reframes the IA from operator-centric to integrator-centric. |
| D2 | Subtopic set: `auth/clients`, `auth/tokens`, `auth/oidc`, `auth/trusted-keys`. OBO becomes a section of `auth/tokens`, not a peer. | Maps 1:1 to "how does my client get a JWT?" decision points. Original three-topic proposal (`oidc`/`trusted-keys`/`obo`) missed `client_credentials` — the 80% case. |
| D3 | `config/auth.md` stays as the flat env-var reference. No content moves; `auth/*` cross-links into it. | One home per concern; matches the existing `config/*` convention. |
| D4 | All new topics marked `stability: evolving`, `version_added: 0.8.0`. | Stability is documentation-only per `help.go:70-75` — no runtime gating. Safe for applications to depend on. Promote to `stable` post-v0.8.0 after one release of API stability. |
| D5 | Rigid 7-section page template (`NAME`, `GOAL`, `PREREQUISITES`, `REQUEST FLOW`, `TOKEN`, `ERRORS`, `SEE ALSO`). Subtopics do not deviate. | LLM consumers can pattern-match section anchors without prose parsing. Per user confirmation. |
| D6 | No new topic-actions for `auth/*` pages. Machine-readable spec stays under `cyoda help openapi {json,yaml,tags}`. | Topic-actions are for spec-export artefacts; `auth/*` is narrative. |
| D7 | OIDC response DTO bug (`expectedAudiences`, `rolesClaim` dropped) fixed via red→green TDD on parity tests, in the same PR as the new `auth/oidc.md` topic. | The narrative page would lie if shipped before the bug is fixed. |
| D8 | `cloud-divergences.md` "22 IAM/OAuth/OIDC/account stub endpoints" row is rewritten with a current, named list of remaining stubs (not a count). | The count went stale across PRs #312, #314, and the keys-conformance work; a count without names is uncheckable. |
| D9 | `docs/cyoda/openapi.yml` (the 316KB resolved-bundle root) is removed, not resynced. The split files under `docs/cyoda/api/` remain as the upstream-cloud parity reference. | No consumer in cyoda-go reads the bundled form; bundling is reproducible from the split files on demand. |
| D10 | `docs/cyoda/README.md` is added stating that `docs/cyoda/api/*.yml` is a read-only mirror of `Cyoda-platform/cyoda` `develop` `client/src/main/resources/api/`, with sync procedure. | Eliminates the layering ambiguity that a future maintainer would have to re-discover. |
| D11 | Ambiguous-tenant routing (MINOR-8 from audit) stays opaque at the wire (401 generic) but gets a `Diagnostics` subsection in `auth/oidc.md` explaining the symptom-to-cause mapping. | Don't leak provider-topology info via a precise error; do help operators debug. |
| D12 | One umbrella GitHub issue and one PR for all work in this spec. No follow-up issues filed at PR-open time; deferred work in §7 stays explicitly out of scope until it lands. | Per user direction; per memory `feedback_courtesy_pr_scope` no drive-by fixes. |
| D13 | No local `go test -race ./...` gate. Race detector runs in CI per `.github/workflows/ci.yml`. | Per user direction; this PR has no concurrency surface changes. |

## 3. Topic tree

New files under `cmd/cyoda/help/content/`:

```
auth.md                       → cyoda help auth          (landing / decision tree)
auth/clients.md               → cyoda help auth clients
auth/tokens.md                → cyoda help auth tokens
auth/oidc.md                  → cyoda help auth oidc
auth/trusted-keys.md          → cyoda help auth trusted-keys
```

Filesystem-derived path per `cmd/cyoda/help/help.go:142-174`; the renderer auto-promotes children into the parent's TOPICS list. Pattern is proven by existing `cli.md` + `cli/*.md` and `config.md` + `config/*.md` trees.

After this lands, `cyoda help` (top-level summary) shows `auth` under the **Evolving** group, alphabetically first.

### 3.1 `auth.md` landing-page body shape

```markdown
## NAME
auth — authenticate client applications against cyoda.

## GOAL
Every cyoda API call needs a `Authorization: Bearer <jwt>` header. This page
helps you decide how to get that JWT.

## WHICH PATH DO I NEED?

| You have | Use | Subtopic |
|---|---|---|
| client_id/secret you'll manage in cyoda | M2M client + token endpoint | clients + tokens |
| an existing IdP (Cognito, Keycloak, Auth0, ...) | federated OIDC | oidc |
| a key you sign tokens with yourself, no IdP | trusted-keys | trusted-keys |
| an M2M client acting on behalf of a user | token-exchange grant | tokens (OBO section) |

## TOKEN PRESENTATION
All cyoda APIs accept the JWT via `Authorization: Bearer <token>`. The token
shape is documented in `auth.tokens`.

## TOPICS
(auto-rendered from children)

## SEE ALSO
- config.auth — env-var reference
- openapi (action: `cyoda help openapi tags`) — spec by tag, including OAuth/OIDC
```

### 3.2 Per-subtopic page template

```markdown
---
topic: auth.<name>
title: "auth.<name> — <one-line summary>"
stability: evolving
version_added: 0.8.0
see_also:
  - config.auth
  - errors.<relevant codes>
  - openapi
---

## NAME
auth.<name> — <one-line summary>

## GOAL
What this auth path achieves. When to choose it. When NOT to choose it.

## PREREQUISITES
- **Admin (cyoda operator) sets up:** <env vars, registrations>
- **Client (you) needs:** <secrets, keys, signing material>

## REQUEST FLOW
Concrete HTTP example (curl form). Request headers, body, response.

## TOKEN
Claims emitted, audience binding, lifetime, `Authorization: Bearer ...` shape.

## ERRORS
The 4-6 error codes a client will actually hit, one-liner each, linked.

## SEE ALSO
```

### 3.3 Subtopic content scope

| Subtopic | Key content |
|---|---|
| `auth.clients` | `POST /clients` (provision M2M), `GET /clients`, `DELETE /clients/{id}`. Roles (`ROLE_M2M`, `ROLE_ADMIN` gated by `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`). Tenant scoping. The bootstrap M2M client. Cross-link to `config.auth` for `CYODA_BOOTSTRAP_*`. |
| `auth.tokens` | `POST /oauth/token` endpoint. **client_credentials** grant (most common, front-loaded). **token-exchange** grant (OBO; documents the `act.sub` actor claim per RFC 8693, the tenant-boundary check from `internal/auth/token.go:198-208`, and the `user_roles` carry-over). Token shape (claims: `sub`, `iss`, `caas_org_id`, `caas_user_id`, `user_roles`, `caas_tier`, `exp`, `iat`, `jti`, `act`). Audience binding (`CYODA_JWT_BOOTSTRAP_AUDIENCE`). |
| `auth.oidc` | All 7 endpoints under `/oauth/oidc/providers/*` (real in v0.8.0 — no 501 stubs). Register flow with SSRF guard semantics. JWKS lifecycle (warmup, refresh, cold-start window). Per-provider `rolesClaim` override and global `CYODA_OIDC_ROLES_CLAIM` default. `expectedAudiences` binding. `Diagnostics` subsection covering ambiguous-tenant routing (D11). |
| `auth.trusted-keys` | All 5 endpoints under `/oauth/keys/trusted/*`. Feature flag `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (off by default — pages must lead with this). Per-tenant cap. Lifecycle (active → invalidated → reactivated; delete). Offline JWT signing shape, KID binding. Cross-link to `errors.TRUSTED_KEY_*` and `errors.KEY_OWNED_BY_DIFFERENT_TENANT`. |

## 4. Content boundaries

| Content | Lives in | Why |
|---|---|---|
| Env-var reference (`CYODA_OIDC_*`, `CYODA_IAM_*`, `CYODA_JWT_*`, `CYODA_HMAC_*`, `CYODA_BOOTSTRAP_*`) | `config/auth.md` (existing, unchanged) | Flat reference; one home; `auth/*` cross-links in. |
| Per-error semantics, HTTP status, retryability | `errors/<code>.md` (existing, unchanged) | One-error-per-file convention. `auth/*` references via §ERRORS one-liners. |
| OpenAPI schemas (request/response DTOs, paths) | `api/openapi.yaml` (existing) | Generated. `auth/*` references via `cyoda help openapi tags` action. |
| Conceptual flow, decision tree | **NEW** `auth/*` | This is the missing layer. |
| Concrete HTTP request examples (curl) | **NEW** `auth/*` (§REQUEST FLOW) | Doesn't belong in env vars, error pages, or spec. |
| Token claim semantics | **NEW** `auth/tokens.md` claims table | Currently nowhere; LLM writing a server-side JWT validator needs this. |
| OIDC validation pipeline (kid → provider → tenant) | **NEW** `auth/oidc.md` | Currently scattered in `internal/auth/oidc/registry.go` comments. |
| Ambiguous-tenant routing → 401 (D11) | **NEW** `auth/oidc.md` `Diagnostics` subsection | Per audit MINOR-8. |

## 5. Bundled fixes

This PR is one change, six commits, one issue. Commits split by concern for revertability:

1. **`fix(iam): OIDC response DTO emits expectedAudiences + rolesClaim`** — `internal/domain/account/oidc_adapter.go:402-415`. Red: parity-test assertion in `e2e/parity/oidc.go` that register / list / update / reactivate responses carry the two fields. Green: populate them in `toOidcProviderResponseDto`. Remove the stale "Task 8.1" comment (lines 400-401) in the same commit.
2. **`docs(help): rewrite openapi.md OIDC status as v0.8.0 conformant`** — `cmd/cyoda/help/content/openapi.md:96`. New text: "As of v0.8.0, the 10 `/oauth/keys/*` operations, the 7 `/oauth/oidc/providers/*` operations, and `/clients` are conformant. Remaining stubbed surface is tracked in `docs/cyoda/cloud-divergences.md`."
3. **`docs(cyoda): add README documenting layering; remove unused bundled openapi.yml`** — new `docs/cyoda/README.md`; delete `docs/cyoda/openapi.yml`. README documents the split-file mirror of upstream `Cyoda-platform/cyoda` `develop`, the sync procedure, and the fact that this directory is read-only reference.
4. **`docs(cyoda): rewrite cloud-divergences IAM-stubs row`** — `docs/cyoda/cloud-divergences.md`. Replace the "22 stubs" row with a current, named list of remaining genuine 501 endpoints across `/account/*` and any leftover `/oauth/*` corners. Audit-driven (enumerate during implementation by grepping `api/openapi.yaml` paths against `internal/api/unimplemented.go` route registrations).
5. **`docs(compat): COMPATIBILITY.md OIDC v0.8.0 parity row`** — `COMPATIBILITY.md`. Row stating cyoda-cloud parity surface = matches upstream `develop` IAM spec; known divergences = none post-fix-#1; Helm chart support = `CYODA_OIDC_*` env vars wired through values.yaml; v0.8.0 first-class.
6. **`feat(help): top-level auth topic with clients/tokens/oidc/trusted-keys subtopics`** — the 5 new help-content files per §3.

## 6. Help-subsystem mechanics

Front-matter contract from `help.go:33-77`:

```yaml
---
topic: auth.<name>             # MUST match filesystem dotted path
title: "auth.<name> — <summary>"
stability: evolving             # documentation-only; no runtime gating
version_added: 0.8.0
see_also:
  - config.auth
  - errors.<code>
  - openapi
---
```

Front-matter validation runs at tree-load time (`DefaultTree` panics on load if malformed) — typos caught at startup, not at query time. No new mechanics needed; parent/child rendering is well-trodden ground.

## 7. Testing

| Surface | Test | Assertion |
|---|---|---|
| Help tree loads with new content | Existing `cmd/cyoda/help/help_test.go` via `DefaultTree` panic-on-bad-frontmatter | Front-matter / stability errors caught at boot. |
| Topic lookups resolve | New cases in `help_test.go` | `Find(["auth"])`, `Find(["auth","oidc"])`, etc. return non-nil with expected title and a `## GOAL` section. |
| BLOCKING-1 red→green | `e2e/parity/oidc.go` register + list + update + reactivate scenarios | Response DTOs carry `expectedAudiences` and `rolesClaim` after round-trip. Picked up by every backend via the parity registry. |
| Tag conformance count | Existing `cmd/cyoda/help/openapi_tags_test.go` | If it asserts an exact stub-count, update during implementation; if it tags by operation no change needed. |
| Renderer text mode | Existing `cmd/cyoda/help/renderer/...` | Already exercises parent-with-children pages; covers `auth.md` listing its 4 children. |

`make test-all` + `go vet ./...` at pre-PR verification (D13: no local `-race` run — CI gates).

## 8. Rollout

Branch: `feat/auth-help-topic` off `release/v0.8.0` (already set up). Six commits per §5. One PR to `release/v0.8.0`. PR body references the umbrella issue (`Closes #N`).

Per memory `feedback_release_milestone_invariant`, the umbrella issue gets v0.8.0 milestone applied at merge time so the release notes pick it up.

## 9. Explicit non-scope

These items were discussed and explicitly deferred. Listing them so the user knows they're real and tracked elsewhere, not silently dropped:

- Renaming or restructuring `config/auth.md` itself.
- Adding a dedicated public error code for ambiguous-tenant routing (D11: opacity is deliberate; documented diagnostically instead).
- Adding `cyoda` CLI subcommands for OIDC management (REST is sufficient; UX-debt PR if ever needed).
- E2E multi-node cluster broadcast tests for OIDC reload (documented limitation per audit MAJOR-7).
- Promoting `auth/*` topics from `evolving` → `stable` (post-v0.8.0 decision).
- Per-language client SDK examples on `auth/*` pages (curl only in v1; SDK examples deferred).
- Folding `auth/*` content into a generated artefact (none planned; topic tree is the artefact).
