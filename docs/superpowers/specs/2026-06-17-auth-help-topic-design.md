# Top-level `auth` help topic + v0.8.0 OIDC polish — design

| Field | Value |
|---|---|
| Issue | TBD — single umbrella issue covering all work in this spec, filed before PR open |
| Target milestone | v0.8.0 (cyoda-go) |
| Spec date | 2026-06-17 |
| Review iterations | 2 (interactive brainstorming · fresh-context spec review) |
| Status | Draft, pending review |
| Related repos | `cyoda-go` only |
| Worktree / branch | `.worktrees/feat-auth-help-topic` / `feat/auth-help-topic` off `release/v0.8.0` |

## 1. Background

The OIDC providers subsystem (#284, PR #314) landed on `release/v0.8.0` as the third "OIDC is done" iteration. A pre-spec audit during this brainstorm surfaced gaps that contradict that claim, in particular a wire-fidelity break against cyoda-cloud's OpenAPI contract. Separately, an information-architecture gap in `cyoda help` means that a developer (or an LLM helping a developer) writing a client application against cyoda has no discoverable narrative entry point for authentication: env vars live in `config/auth`, error semantics in `errors/OIDC_*`, and the OpenAPI spec describes shape but not flow — nowhere does the help tree answer "how does my client get a JWT?".

This change closes both gaps in one PR:

1. Bugfix the OIDC response DTO so registered fields round-trip per spec.
2. Add a top-level `auth` help topic with four subtopics (`clients`, `tokens`, `oidc`, `trusted-keys`) optimised for an LLM-implementing-a-client audience.
3. Clean up `docs/cyoda/` and `cloud-divergences.md` so the stub-tracking matches the v0.8.0 conformance reality (10 keys + 7 OIDC + clients now conformant).
4. Update `cmd/cyoda/help/content/openapi.md` to stop claiming OIDC is 501.

## 2. Decisions log

| # | Decision | Rationale |
|---|---|---|
| D1 | Audience for new topics is **LLMs helping developers write client code**, not human operators. Page structure optimises for predictable section anchors. | Per user direction; reframes the IA from operator-centric to integrator-centric. |
| D2 | Subtopic set: `auth/clients`, `auth/tokens`, `auth/oidc`, `auth/trusted-keys`. OBO becomes a section of `auth/tokens`, not a peer. | Maps 1:1 to "how does my client get a JWT?" decision points. Original three-topic proposal (`oidc`/`trusted-keys`/`obo`) missed `client_credentials` — the 80% case. |
| D3 | `config/auth.md` stays as the flat env-var reference. No content moves; `auth/*` cross-links into it. | One home per concern; matches the existing `config/*` convention. |
| D4 | All new topics marked `stability: evolving`, `version_added: 0.8.0`. | Stability is documentation-only per the `switch fm.Stability` validator in `parseFrontMatter` — no runtime gating. Safe for applications to depend on. Promote to `stable` post-v0.8.0 after one release of API stability. |
| D5 | 7-section page template (`NAME`, `GOAL`, `PREREQUISITES`, `REQUEST FLOW`, `TOKEN`, `ERRORS`, `SEE ALSO`). Subtopics do not deviate at the H2 level. **Carve-out:** when a page covers multiple flows (notably `auth.tokens` with `client_credentials` + token-exchange), `## REQUEST FLOW` may contain one `### <grant>` subheading per flow. | LLM consumers can pattern-match section anchors without prose parsing. The carve-out prevents the template from collapsing two genuinely distinct flows into a misleading single example. |
| D6 | No new topic-actions for `auth/*` pages. Machine-readable spec stays under `cyoda help openapi {json,yaml,tags}`. | Topic-actions are for spec-export artefacts; `auth/*` is narrative. |
| D7 | The OIDC response mapper `toOidcProviderResponseDto` silently drops `expectedAudiences` and `rolesClaim` despite the generated DTO (`api/generated.go`, `OidcProviderResponseDto`) declaring them and the OpenAPI schema (`api/openapi.yaml`, lines around 8521 / 8530) requiring them. Fixed via red→green TDD on parity tests, in the same PR as the new `auth/oidc.md` topic. | This is silent data loss at a public response boundary, not deferred work — the comment in the mapper calls it "Task 8.1 not done", but the DTO and spec are both already there. Same severity tier as a 5xx leak in terms of client trust. The narrative `auth/oidc.md` page would also lie if shipped before the fix. |
| D8 | `cloud-divergences.md` "22 IAM/OAuth/OIDC/account stub endpoints" row is rewritten as both (a) a current snapshot of remaining genuine 501 endpoints and (b) a re-derivation shell snippet a future maintainer can re-run to refresh the snapshot. | The count went stale across PRs #312, #314, and the keys-conformance work. A static list goes stale again the next time IAM moves; the snippet lets the row be re-derived rather than re-edited. |
| D9 | `docs/cyoda/openapi.yml` (the resolved-bundle root) is **kept**, not deleted. It is the canonical line-number anchor for ~20 `Canonical: docs/cyoda/openapi.yml:NNN` citations in `e2e/parity/client/http.go` (18 sites), `e2e/parity/client/types.go`, and `internal/domain/entity/handler_create_collection_chunking_test.go`. | The original draft proposed deletion; the fresh-context review caught that those citations would all break. Split-file line numbers differ from the bundled form, so a delete-and-rewrite alternative would be a non-trivial scope expansion. Keeping the bundle and documenting both layers in the new README (D10) is the lower-risk path. |
| D10 | `docs/cyoda/README.md` is added explaining the **two coexisting layers**: `docs/cyoda/api/*.yml` is the upstream-split mirror of `Cyoda-platform/cyoda` `develop` `client/src/main/resources/api/` (parity reference); `docs/cyoda/openapi.yml` is the resolved-bundle citation anchor. Names the upstream source and the sync procedure; marks the directory as read-only reference. | Eliminates the layering ambiguity. A future maintainer sees why both forms exist and how to refresh each. |
| D11 | One umbrella GitHub issue and one PR for all work in this spec. No follow-up issues filed at PR-open time; deferred work in §9 stays explicitly out of scope until it lands. | Per user direction; per memory `feedback_courtesy_pr_scope` no drive-by fixes. |
| D12 | `auth.tokens` is the **single home** for the JWT claim contract (claims emitted, audience binding, lifetime). `auth.oidc` and `auth.trusted-keys` cross-reference rather than duplicate. | Claim shape is universal; per-path pages otherwise drift. Reviewer-surfaced overlap risk (N3). |
| D13 | `auth.md` landing page must explicitly call out OBO discoverability: a row in the WHICH-PATH table points `cyoda help auth obo` seekers to `auth.tokens` (token-exchange section). No `auth/obo.md` redirect stub. | Folding OBO under tokens (D2) creates a discoverability cost; the explicit pointer pays it once, in the landing page. A stub file adds maintenance noise. |
| D14 | Gate-4 standing rule for OIDC: adding or changing an `/oauth/oidc/providers/*` operation requires updating `auth/oidc.md` in the same PR. Recorded here so the rule survives the brainstorm session. | The spec body lists "all 7 endpoints" verbatim; without a standing rule, this page goes stale next time IAM moves. Matches the existing Gate-4 pattern from CLAUDE.md. |

## 3. Topic tree

New files under `cmd/cyoda/help/content/`:

```
auth.md                       → cyoda help auth          (landing / decision tree)
auth/clients.md               → cyoda help auth clients
auth/tokens.md                → cyoda help auth tokens
auth/oidc.md                  → cyoda help auth oidc
auth/trusted-keys.md          → cyoda help auth trusted-keys
```

Filesystem-derived path per `help.go` `loadInto` / `insertOrMerge`; the renderer auto-promotes children into the parent's TOPICS list. Pattern is proven by existing `cli.md` + `cli/*.md` and `config.md` + `config/*.md` trees.

After this lands, `cyoda help` (top-level summary) shows `auth` under the **Evolving** group alongside `analytics` and `telemetry`.

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

> **Looking for OBO?** The token-exchange (on-behalf-of) grant is documented
> as a section of `auth.tokens` — there is no separate `auth.obo` page.
> Run `cyoda help auth tokens` and jump to the token-exchange section.

> **Looking for env vars?** All `CYODA_OIDC_*`, `CYODA_IAM_*`, `CYODA_JWT_*`,
> `CYODA_HMAC_*`, and `CYODA_BOOTSTRAP_*` knobs live in `config.auth`.

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
(Per D5 carve-out: when the page covers multiple flows — e.g.
`auth.tokens` covering both `client_credentials` and token-exchange —
this section uses one `### <grant>` subheading per flow.)

## TOKEN
Claims emitted, audience binding, lifetime, `Authorization: Bearer ...` shape.
(Per D12: this section is fully populated only in `auth.tokens`. Other
pages reference it.)

## ERRORS
The 4-6 error codes a client will actually hit, one-liner each, linked.

## SEE ALSO
```

### 3.3 Subtopic content scope

| Subtopic | Key content |
|---|---|
| `auth.clients` | `POST /clients` (provision M2M), `GET /clients`, `DELETE /clients/{id}`. Roles (`ROLE_M2M`, `ROLE_ADMIN` gated by `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED`). Tenant scoping. The bootstrap M2M client. Cross-link to `config.auth` for `CYODA_BOOTSTRAP_*`. |
| `auth.tokens` | `POST /oauth/token` endpoint. **client_credentials** grant (most common, front-loaded). **token-exchange** grant (OBO; documents the `act.sub` actor claim per RFC 8693, the tenant-boundary check in the OBO handler in `internal/auth/token.go`, and the `user_roles` carry-over). Token shape (claims: `sub`, `iss`, `caas_org_id`, `caas_user_id`, `user_roles`, `caas_tier`, `exp`, `iat`, `jti`, `act`). Audience binding (`CYODA_JWT_BOOTSTRAP_AUDIENCE`). Per D12, this is the single home for the JWT claim contract — `auth.oidc` and `auth.trusted-keys` link here for claim shape. |
| `auth.oidc` | All 7 endpoints under `/oauth/oidc/providers/*` (real in v0.8.0 — no 501 stubs, post-fix). Register flow with SSRF guard semantics. JWKS lifecycle (warmup, refresh, cold-start window). Per-provider `rolesClaim` override and global `CYODA_OIDC_ROLES_CLAIM` default. `expectedAudiences` binding. **Diagnostics subsection:** ambiguous-tenant routing — two tenants registering the same IdP with overlapping or empty `expectedAudiences` resolve to a generic 401 (`ErrAmbiguousProvider` wrapped in `ErrUnknownKID` in `internal/auth/oidc/registry.go`); this is deliberate opacity, not a missing precise error code, and the diagnostic path is "check provider routing in logs" rather than "match the error code". |
| `auth.trusted-keys` | All 5 endpoints under `/oauth/keys/trusted/*`. Feature flag `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` (off by default — pages must lead with this). Per-tenant cap. Lifecycle (active → invalidated → reactivated; delete). Offline JWT signing shape (KID binding, supported `kty`/`alg`). Per D12, the **JWT claim contract** (sub/aud/iss claims, audience binding, lifetime) is defined in `auth.tokens` and cross-referenced here — this page covers signing-side ops only. Cross-link to `errors.TRUSTED_KEY_*` and `errors.KEY_OWNED_BY_DIFFERENT_TENANT`. |

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
| Ambiguous-tenant routing → 401 | **NEW** `auth/oidc.md` `Diagnostics` subsection | Per audit MINOR-8. |

## 5. Bundled fixes

This PR is one change, five commits, one issue. Commits split by concern for revertability:

1. **`fix(iam): OIDC response mapper emits expectedAudiences + rolesClaim`** — `internal/domain/account/oidc_adapter.go`, function `toOidcProviderResponseDto`. The mapper today drops both fields silently despite the generated DTO declaring them (`api/generated.go`, `OidcProviderResponseDto`); the comment in the source frames this as deferred "Task 8.1" work, but the spec and DTO are both already there — this is silent data loss in a public response. **Red:** parity-test assertions in `e2e/parity/oidc.go` covering every endpoint that returns the DTO — `POST /oauth/oidc/providers` (register), `GET /oauth/oidc/providers` (list), `PATCH /oauth/oidc/providers/{id}` (update), `POST /oauth/oidc/providers/{id}/reactivate` (reactivate) — assert that requests carrying non-empty `expectedAudiences` and `rolesClaim` see those fields in the response body. There is no `GET /oauth/oidc/providers/{id}` endpoint in the OpenAPI spec (verbs on that path are `PATCH` + `DELETE` only); single-provider read happens implicitly through list + filter. **Green:** populate them in `toOidcProviderResponseDto`. **Cleanup (Gate 6):** remove the stale "Task 8.1" comment block above the function in the same commit.
2. **`docs(help): rewrite openapi.md OIDC status as v0.8.0 conformant`** — `cmd/cyoda/help/content/openapi.md`. New text: "As of v0.8.0, the 10 `/oauth/keys/*` operations, the 7 `/oauth/oidc/providers/*` operations, and `/clients` are conformant. Remaining stubbed surface is tracked in `docs/cyoda/cloud-divergences.md`."
3. **`docs(cyoda): add README documenting both spec layers`** — new `docs/cyoda/README.md`. Documents (a) `docs/cyoda/api/*.yml` as the upstream-split mirror of `Cyoda-platform/cyoda` `develop` `client/src/main/resources/api/` used as the parity reference, (b) `docs/cyoda/openapi.yml` as the resolved bundle that line-number citations in parity tests anchor against, (c) sync procedure for each, (d) directory is read-only reference. No file deletions — D9 keeps the bundle.
4. **`docs(cyoda): rewrite cloud-divergences IAM-stubs row with snapshot + derivation`** — `docs/cyoda/cloud-divergences.md`. Replace the static "22 stubs" row with two parts: a current snapshot enumerating the genuine 501 endpoints still in the spec (derived at implementation time from `api/openapi.yaml` paths × `internal/api/unimplemented.go` registrations), and a shell snippet a future maintainer can re-run to refresh the snapshot. Per D8, the snippet is the canonical thing; the snapshot is its current output.
5. **`feat(help): top-level auth topic with clients/tokens/oidc/trusted-keys subtopics`** — the 5 new help-content files per §3.

## 6. Help-subsystem mechanics

Front-matter contract from `help.go` (`parseFrontMatter`, `FrontMatter` struct):

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

**Standing Gate-4 rule (D14):** adding or changing an `/oauth/oidc/providers/*` operation requires updating `auth/oidc.md` in the same PR. Without this rule, `auth/oidc.md`'s "all 7 endpoints" enumeration goes stale next time IAM moves. Mirrors the existing Gate-4 pattern in `CLAUDE.md` for env-var changes.

**Naming collision note.** `cyoda help auth` (new) and `cyoda help config auth` (existing) are distinct dotted-path topics (`auth` vs `config.auth`) — no resolver collision. The new `auth.md` landing page prominently links to `config.auth` in its `SEE ALSO` and in the `WHICH PATH DO I NEED?` table footer so users searching for env vars are routed correctly.

## 7. Testing

| Surface | Test | Assertion |
|---|---|---|
| Help tree loads with new content | Existing `cmd/cyoda/help/help_test.go` via `DefaultTree` panic-on-bad-frontmatter | Front-matter / stability errors caught at boot. |
| Topic lookups resolve | New cases in `help_test.go` | `Find(["auth"])`, `Find(["auth","oidc"])`, etc. return non-nil with expected title and a `## GOAL` section. |
| OIDC response-mapper bug red→green | `e2e/parity/oidc.go` register + list + update + reactivate scenarios (the four endpoints that return the DTO; no GET-by-id exists in the spec) | Response DTOs carry `expectedAudiences` and `rolesClaim` after round-trip. Picked up by every backend via the parity registry. |
| Tag conformance count | Existing `cmd/cyoda/help/openapi_tags_test.go` | If it asserts an exact stub-count, update during implementation; if it tags by operation no change needed. |
| Renderer text mode | Existing `cmd/cyoda/help/renderer/...` | Already exercises parent-with-children pages; covers `auth.md` listing its 4 children. |

`make test-all` + `go vet ./...` at pre-PR verification. Race detector runs in CI per the existing race-testing rule (`.claude/rules/race-testing.md`); no local run required for this PR (no concurrency-surface changes).

## 8. Rollout

Branch: `feat/auth-help-topic` off `release/v0.8.0` (already set up). Five commits per §5. One PR to `release/v0.8.0`. PR body references the umbrella issue (`Closes #N`).

Per memory `feedback_release_milestone_invariant`, the umbrella issue gets v0.8.0 milestone applied at merge time so the release notes pick it up.

## 9. Explicit non-scope

These items were discussed and explicitly deferred. Listing them so the user knows they're real and tracked elsewhere, not silently dropped:

- Renaming or restructuring `config/auth.md` itself.
- Adding a dedicated public error code for ambiguous-tenant routing (opacity is deliberate; documented diagnostically in `auth/oidc.md` per §3.3).
- Adding `cyoda` CLI subcommands for OIDC management (REST is sufficient; UX-debt PR if ever needed).
- E2E multi-node cluster broadcast tests for OIDC reload (documented limitation per audit MAJOR-7).
- Promoting `auth/*` topics from `evolving` → `stable` (post-v0.8.0 decision).
- Per-language client SDK examples on `auth/*` pages (curl only in v1; SDK examples deferred).
- Folding `auth/*` content into a generated artefact (none planned; topic tree is the artefact).
