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
brew install cyoda-platform/cyoda-go/cyoda
cyoda init && cyoda &
curl http://localhost:8080/api/health
# {"status":"UP"}
```

`cyoda init` writes a sqlite-backed user config (default path `~/.local/share/cyoda/cyoda.db`); `cyoda` then starts the server with that config and mock auth. See **Install** for non-Homebrew options and **First real call** for jwt + a real authenticated request.

## Install

### Homebrew (macOS / Linux)

```bash
brew install cyoda-platform/cyoda-go/cyoda
```

### curl (any Unix)

```bash
curl -fsSL https://github.com/cyoda-platform/cyoda-go/releases/latest/download/install.sh | sh
```

Installs to `~/.local/bin/cyoda` and runs `cyoda init`. Pin a version with `CYODA_VERSION=v0.7.1 curl ... | sh`. The installer SHA256-verifies the archive and, if [`cosign`](https://docs.sigstore.dev/cosign/installation/) is on `PATH`, also verifies a Sigstore keyless signature from the cyoda-go release workflow.

### Debian / Ubuntu / Fedora / RHEL

```bash
# Debian / Ubuntu
wget https://github.com/cyoda-platform/cyoda-go/releases/latest/download/cyoda_linux_amd64.deb
sudo dpkg -i cyoda_linux_amd64.deb

# Fedora / RHEL
wget https://github.com/cyoda-platform/cyoda-go/releases/latest/download/cyoda_linux_amd64.rpm
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

## Where to go next

Online docs at [docs.cyoda.net](https://docs.cyoda.net) mirror the `cyoda help` topic tree — the same content is available offline via `cyoda help <topic>`.

| Goal                          | Link                                              |
|-------------------------------|---------------------------------------------------|
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

## Versioning

cyoda-go is **pre-1.0**. Minor bumps may break wire format, configuration, or operational surface; patch bumps do not. See [`CHANGELOG.md`](CHANGELOG.md) for breaking changes and [`MAINTAINING.md`](MAINTAINING.md#maintenance-of-older-release-lines) for the policy on older release lines.

## License

Apache-2.0 — see [LICENSE](LICENSE).
