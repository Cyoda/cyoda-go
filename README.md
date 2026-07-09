[![CI](https://github.com/cyoda/cyoda-go/actions/workflows/ci.yml/badge.svg)](https://github.com/cyoda/cyoda-go/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/cyoda/cyoda-go)](https://github.com/cyoda/cyoda-go/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/cyoda-platform/cyoda-go.svg)](https://pkg.go.dev/github.com/cyoda-platform/cyoda-go)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

# cyoda-go

**One transactional runtime for the entity lifecycle.**

cyoda-go is an EDBMS (Entity Database Management System) — state machine, processors, and full revision history live inside the record, committed atomically. Minimizes the need for sagas, CDC pipelines, and external orchestration.

## Four storage engines, one application contract

Same application code, four operational shapes:

| Engine     | Where it fits                                 | Availability       |
|------------|-----------------------------------------------|--------------------|
| memory     | Local dev, unit tests, digital-twin scenarios | open source        |
| sqlite     | Edge, single-node self-host, persistent dev   | open source        |
| postgres   | Production transactional workloads, HA        | open source        |
| cassandra  | Distributed scale, high write throughput      | commercial (Cyoda) |

Switch by setting `CYODA_STORAGE_BACKEND` — no code changes. The cassandra engine is offered as a commercial backend by Cyoda for workloads that outgrow a single PostgreSQL primary; contact information is on the [cyoda.com](https://cyoda.com) website.

## Try it in 30 seconds

```bash
brew install cyoda/cyoda-go/cyoda
cyoda init && cyoda &
curl http://localhost:8080/api/health
# {"status":"UP"}
```

`cyoda init` writes a sqlite-backed user config (default path `~/.local/share/cyoda/cyoda.db`); `cyoda` then starts the server with that config and mock auth. See **Install** for non-Homebrew options and **First real call** for jwt + a real authenticated request.

## Install

### Homebrew (macOS / Linux)

```bash
brew install cyoda/cyoda-go/cyoda
```

### curl (any Unix)

```bash
curl -fsSL https://github.com/cyoda/cyoda-go/releases/latest/download/install.sh | sh
```

Installs to `~/.local/bin/cyoda` and runs `cyoda init`. Pin a version with `CYODA_VERSION=v0.7.1 curl ... | sh`. The installer SHA256-verifies the archive and, if [`cosign`](https://docs.sigstore.dev/cosign/installation/) is on `PATH`, also verifies a Sigstore keyless signature from the cyoda-go release workflow.

### Debian / Ubuntu / Fedora / RHEL

```bash
# Debian / Ubuntu
wget https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb

# Fedora / RHEL
wget https://github.com/cyoda/cyoda-go/releases/latest/download/cyoda_linux_amd64.rpm
sudo rpm -i cyoda_linux_amd64.rpm
```

Replace `amd64` with `arm64` for ARM hosts. Both packages drop `/usr/bin/cyoda` and `/etc/cyoda/cyoda.env` (sqlite as the system-wide default, preserved across upgrades).

### From source

Requires **Go 1.26+**.

```bash
go install github.com/cyoda-platform/cyoda-go/cmd/cyoda@latest
```

This binary uses the in-memory backend by default. Run `cyoda init` for sqlite persistence, or set `CYODA_STORAGE_BACKEND` directly.

## First real call

The 30-second example uses mock auth. To exercise the real auth chain end-to-end with sqlite + jwt — without leaking the bootstrap secret into your shell history or `ps` output — use the project's profile pattern:

```bash
# Generate a JWT signing key (openssl writes it 0600 by default; make it explicit)
openssl genrsa -out /tmp/jwt.key 2048
chmod 600 /tmp/jwt.key

# Write a local profile with sqlite + jwt + bootstrap creds. .env.local is
# gitignored; chmod 600 keeps the secret off other users' eyes on shared boxes.
cat > .env.local <<'EOF'
CYODA_STORAGE_BACKEND=sqlite
CYODA_IAM_MODE=jwt
CYODA_JWT_SIGNING_KEY_FILE=/tmp/jwt.key
CYODA_BOOTSTRAP_CLIENT_ID=demo
CYODA_BOOTSTRAP_CLIENT_SECRET=demo-secret
EOF
chmod 600 .env.local

# Start cyoda with the local profile (loads .env.local automatically)
CYODA_PROFILES=local cyoda &

# Read the secret from the file at the moment we need it — never `export` it
SECRET=$(grep '^CYODA_BOOTSTRAP_CLIENT_SECRET=' .env.local | cut -d= -f2-)

# Get an OAuth 2.0 token via client_credentials
TOKEN=$(curl -sX POST http://localhost:8080/api/oauth/token \
  -u "demo:$SECRET" \
  -d "grant_type=client_credentials" | jq -r .access_token)

# Make an authenticated call
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/account
```

The `/api/account` response confirms the bootstrap client's tenant and roles. From here, follow the **Build an app** link below to register an entity model and start creating entities.

**Optional IAM feature flags** (all default `false`):

| Env var | Default | Effect |
|---------|---------|--------|
| `CYODA_IAM_TRUSTED_KEY_REGISTRATION_ENABLED` | `false` | When `true`, enables the 5 `/oauth/keys/trusted/*` admin endpoints. When `false`, those endpoints return `404 FEATURE_DISABLED`. |
| `CYODA_IAM_M2M_ADMIN_ROLE_ENABLED` | `false` | When `true`, `POST /clients?withAdminRole=true` may grant `ROLE_ADMIN` to created M2M clients. When `false` (default), that request shape returns `404 FEATURE_DISABLED`. |

### Federated OIDC providers

cyoda-go can accept JWTs issued by external OIDC providers — Auth0, Cognito, Keycloak, or any spec-compliant issuer — alongside its own first-party tokens. Each tenant registers its own providers; tokens are validated against the provider's JWKS endpoint.

**Validation order:** cyoda-go tries the built-in JWKSValidator first (trusted keys registered via `/oauth/keys/trusted/*`), then the OIDCValidator. The first validator that recognises the issuer wins.

**Management endpoints** (JWT mode, `CYODA_IAM_MODE=jwt`):

| Method | Path | Auth |
|--------|------|------|
| `POST` | `/oauth/oidc/providers` | `ROLE_ADMIN` |
| `GET` | `/oauth/oidc/providers` | any authenticated tenant member |
| `PATCH` | `/oauth/oidc/providers/{id}` | `ROLE_ADMIN` |
| `POST` | `/oauth/oidc/providers/{id}/invalidate` | `ROLE_ADMIN` |
| `POST` | `/oauth/oidc/providers/{id}/reactivate` | `ROLE_ADMIN` |
| `DELETE` | `/oauth/oidc/providers/{id}` | `ROLE_ADMIN` |
| `POST` | `/oauth/oidc/providers/reload` | `ROLE_ADMIN` |

The `reload` endpoint flushes the in-memory JWKS cache and re-fetches keys from every active provider for the tenant — useful after a key rotation at the IdP.

**Register a provider:**

```bash
curl -sX POST http://localhost:8080/api/oauth/oidc/providers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-auth0",
    "wellKnownConfigUri": "https://example.auth0.com/.well-known/openid-configuration",
    "audienceClaim": "https://api.example.com",
    "rolesClaim": "https://example.com/roles"
  }'
```

**Configuration:** see `cyoda help config.auth` (the "Federated OIDC providers" section) for the six `CYODA_OIDC_*` env vars that control HTTPS enforcement, SSRF blocking, default roles claim, and HTTP timeouts for discovery and JWKS fetches.

## Composite unique keys

An entity model can declare one or more **composite unique keys** — each key is a set of scalar field paths that must be unique across all live entities of that model within a tenant.

**Declaring keys** requires the model to be `UNLOCKED`:

```
PUT /api/model/{entityName}/{modelVersion}/unique-keys
```

Request body:

```json
{
  "uniqueKeys": [
    { "id": "by-email", "fields": ["$.email"] },
    { "id": "by-org-and-handle", "fields": ["$.org", "$.handle"] }
  ]
}
```

This call is idempotent — it replaces the model's entire key list. The keys are validated immediately (field paths must be known scalar leaves in the inferred schema). After locking the model, no entity create or update can produce a duplicate value-set for any declared key.

**Key semantics:**

- **Scope:** per `(tenant, model name, model version)`, live entities only. Soft-deleting an entity frees its key value-set.
- **Null rule (all-or-nothing):** if all fields in a key are absent or null, the entity is exempt. If some but not all fields are present, the write is rejected with `422 INVALID_UNIQUE_KEY`. If all fields are present, uniqueness is enforced.
- **String comparison is byte-exact:** case-sensitive, no Unicode normalization, no whitespace trimming — the bytes the application wrote are what is compared. Applications that want case-insensitive matching must normalize before writing.
- **Enforced on create and update.** Moving a key value to a free slot is allowed; moving it to a slot already taken by another entity returns `409 UNIQUE_VIOLATION`.
- **Supported backends:** memory, sqlite, postgres. The commercial backend returns `422 COMPOSITE_KEY_UNSUPPORTED` until its own support lands.

**Multi-node note:** see the `cluster` help topic — *Composite unique key staleness* — for a bounded operational limitation when changing a key on a live multi-node postgres deployment.

## Search result sorting

Search endpoints accept one or more `sort` query parameters to order results by scalar data or meta fields:

```
GET /api/entity/{entityName}/{modelVersion}/search?sort=price:asc&sort=@creationDate:desc
```

Grammar: `[@]path[:asc|desc]` — a bare dotted path sorts by a scalar entity-data field; the `@` prefix sorts by a meta field. Direction defaults to `asc`. Repetition order is sort precedence; `entity_id` is always the final tiebreaker. Absent/null values sort last.

**Sortable meta fields:** `state`, `creationDate`, `lastUpdateTime`, `transitionForLatestSave`, `transactionId`, `id`.

**Error:** unsortable, unknown, or non-scalar paths return `400 INVALID_FIELD_PATH`.

**Key cap:** `CYODA_SEARCH_MAX_SORT_KEYS` (default `16`) — see `cyoda help config` (Search and transaction internals).

## Where to go next

Online docs at [docs.cyoda.net](https://docs.cyoda.net) mirror the `cyoda help` topic tree — the same content is available offline via `cyoda help <topic>`.

Run `cyoda help config all` for the complete env-var reference (add `--format=json` for machine-readable output); `cyoda help config cluster` covers multi-node/dispatch vars.

| Goal                          | Link                                              |
|-------------------------------|---------------------------------------------------|
| Build an app fast (Claude Code) | [github.com/cyoda/cyoda-skills](https://github.com/cyoda/cyoda-skills) — install the cyoda-skills plugin and use `/cyoda:app` to scaffold |
| Build an app                  | [docs.cyoda.net/help/quickstart](https://docs.cyoda.net/help/quickstart) |
| Configure                     | [docs.cyoda.net/help/config](https://docs.cyoda.net/help/config)       |
| Error reference               | [docs.cyoda.net/help/errors](https://docs.cyoda.net/help/errors)       |
| Deploy with Helm              | [docs.cyoda.net/help/helm](https://docs.cyoda.net/help/helm)           |
| Deploy with Docker Compose    | [examples/compose-with-observability/](examples/compose-with-observability/) |
| Architecture                  | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)      |
| Application examples          | [docs/PRD.md#target-applications](docs/PRD.md#target-applications) |
| Product overview              | [docs/PRD.md](docs/PRD.md)                        |
| Feature & API inventory       | [docs/FEATURES.md](docs/FEATURES.md)              |
| Multi-node cluster            | [docs.cyoda.net/help/cluster](https://docs.cyoda.net/help/cluster) |
| Admin endpoints (log/trace)   | [docs.cyoda.net/help/admin](https://docs.cyoda.net/help/admin)         |
| Write a storage plugin        | [docs/plugins.md](docs/plugins.md)                |
| Contribute                    | [CONTRIBUTING.md](CONTRIBUTING.md)                |
| Security disclosures          | [SECURITY.md](SECURITY.md)                        |

## Related projects

Sibling repositories under [github.com/cyoda](https://github.com/cyoda) that complement cyoda-go:

- **[cyoda-skills](https://github.com/cyoda/cyoda-skills)** — Claude Code skills (`/cyoda:app`, `/cyoda:design`, `/cyoda:build`, `/cyoda:test`, ...) for AI-assisted Cyoda app development against a local cyoda-go or Cyoda Cloud instance.
- **[cyoda-cloud-cli](https://github.com/cyoda/cyoda-cloud-cli)** — Command-line client for Cyoda Cloud with OAuth 2.0 authentication and Cloud-side API operations.
- **[cyoda-docs](https://github.com/cyoda/cyoda-docs)** — Source for [docs.cyoda.net](https://docs.cyoda.net) — developer guides, onboarding, and the rendered `cyoda help` topic tree.
- **[cyoda-workflow-editor](https://github.com/cyoda/cyoda-workflow-editor)** — TypeScript components for parsing, rendering, and editing Cyoda workflow JSON definitions.

## Versioning

The Cyoda-Go ecosystem follows Semantic Versioning with a leading `v`, under the pre-1.0 convention where **the minor component is the breaking-change signal**:

- **`0.MINOR.0`** — a backward-*incompatible* change to that module's public contract (the HTTP/wire API for the `cyoda-go` binary; the Go interface surface for `cyoda-go-spi`).
- **`0.x.PATCH`** — any backward-*compatible* change, **including new features**: additive API parameters, new endpoints, new optional SPI fields, and bug fixes all ship as patches.

This is the "leftmost non-zero component is the de-facto major" convention (as used by Cargo and npm's `^0.x` ranges). It keeps the minor counter meaningful — a minor bump means "something under you may have broken" — rather than a feature odometer. The discipline it rests on: **a breaking change never ships in a patch.**

**Each module versions on its own axis.** `cyoda-go-spi`, the `cyoda-go` binary, the in-tree plugins, and the Helm chart are **not** required to share a version number. Because the SPI surface is strictly additive today, its minor stays put while the binary iterates — a new SPI release is a *patch* unless it breaks an interface. The compatible combinations are recorded in [`COMPATIBILITY.md`](COMPATIBILITY.md); that matrix, not a shared digit, is the source of truth for what works with what.

See [`CHANGELOG.md`](CHANGELOG.md) for breaking changes and [`MAINTAINING.md`](MAINTAINING.md#maintenance-of-older-release-lines) for the policy on older release lines.

## License

Apache-2.0 — see [LICENSE](LICENSE).
