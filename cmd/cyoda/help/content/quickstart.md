---
topic: quickstart
title: "cyoda quickstart — minimum invocations"
stability: stable
see_also:
  - cli
  - cli.init
  - config
  - config.database
  - config.auth
  - run
---

# quickstart

## NAME

quickstart — minimum commands to run a cyoda-go server.

## SYNOPSIS

```
cyoda init && cyoda
```

## DESCRIPTION

cyoda-go is a single-process, multi-tenant REST and gRPC API server backed by a pluggable embedded database management system. Storage backends are `memory`, `sqlite`, and `postgres`; authentication modes are `mock` and `jwt`. All configuration is via environment variables with a `CYODA_` prefix.

The binary starts with no required configuration. Default mode: sqlite storage (enabled by `cyoda init`), mock IAM, REST on port 8080, gRPC on port 9090, admin on port 9091.

## DEFAULTS

Without any environment variables, after `cyoda init`:

- `CYODA_STORAGE_BACKEND` = `memory` (before `cyoda init`; `sqlite` after init writes the user config)
- `CYODA_SQLITE_PATH` = `~/.local/share/cyoda/cyoda.db` (Linux/macOS XDG; `%LocalAppData%\cyoda\cyoda.db` on Windows)
- `CYODA_SQLITE_AUTO_MIGRATE` = `true`
- `CYODA_SQLITE_BUSY_TIMEOUT` = `5s`
- `CYODA_SQLITE_CACHE_SIZE` = `64000` (KiB)
- `CYODA_SQLITE_SEARCH_SCAN_LIMIT` = `100000`
- `CYODA_IAM_MODE` = `mock` (all requests accepted without authentication)
- `CYODA_IAM_MOCK_ROLES` = `ROLE_ADMIN,ROLE_M2M`
- `CYODA_HTTP_PORT` = `8080`
- `CYODA_GRPC_PORT` = `9090`
- `CYODA_ADMIN_PORT` = `9091`
- `CYODA_ADMIN_BIND_ADDRESS` = `127.0.0.1`
- `CYODA_CONTEXT_PATH` = `/api`
- `CYODA_LOG_LEVEL` = `info`
- `CYODA_REQUIRE_JWT` = `false`
- `CYODA_OTEL_ENABLED` = `false`
- `CYODA_CLUSTER_ENABLED` = `false`
- `CYODA_SUPPRESS_BANNER` = `false`

The admin listener binds to `127.0.0.1` by default. `/livez` and `/readyz` are unauthenticated.

**Warning:** mock auth accepts all requests without a token. An instance in mock mode must not be exposed to untrusted networks.

## PRODUCTION CHECKLIST

Env vars required to move from defaults to a production-shaped deployment.

**Storage — switch from sqlite to postgres:**

- `CYODA_STORAGE_BACKEND` = `postgres` (required)
- `CYODA_POSTGRES_URL` = `postgres://user:pass@host:5432/dbname` (required when backend is postgres)
- `CYODA_POSTGRES_URL_FILE` — file path for `CYODA_POSTGRES_URL`; takes precedence over the plain var
- `CYODA_POSTGRES_MAX_CONNS` = `25` (default)
- `CYODA_POSTGRES_MIN_CONNS` = `5` (default)
- `CYODA_POSTGRES_MAX_CONN_IDLE_TIME` = `5m` (default)
- `CYODA_POSTGRES_AUTO_MIGRATE` = `true` (default)

**Auth — switch from mock to JWT:**

- `CYODA_IAM_MODE` = `jwt` (required)
- `CYODA_REQUIRE_JWT` = `true` — refuse startup unless jwt mode and signing key are set
- `CYODA_JWT_SIGNING_KEY` — RSA private key, PEM-encoded (required in jwt mode)
- `CYODA_JWT_SIGNING_KEY_FILE` — file path for `CYODA_JWT_SIGNING_KEY`; takes precedence
- `CYODA_JWT_ISSUER` = `cyoda` (default; set to your issuer URI)
- `CYODA_JWT_AUDIENCE` = `` (default empty; set to require audience claim validation)
- `CYODA_JWT_EXPIRY_SECONDS` = `3600` (default)

**Inter-node dispatch auth (cluster mode):**

- `CYODA_HMAC_SECRET` — hex-encoded HMAC secret for inter-node dispatch authentication
- `CYODA_HMAC_SECRET_FILE` — file path for `CYODA_HMAC_SECRET`; takes precedence

**Bootstrap M2M client (optional):**

- `CYODA_BOOTSTRAP_CLIENT_ID` — M2M client ID to provision at startup
- `CYODA_BOOTSTRAP_CLIENT_SECRET` — M2M client secret (required when client ID is set)
- `CYODA_BOOTSTRAP_CLIENT_SECRET_FILE` — file path for `CYODA_BOOTSTRAP_CLIENT_SECRET`; takes precedence
- `CYODA_BOOTSTRAP_TENANT_ID` = `default-tenant` (default)
- `CYODA_BOOTSTRAP_USER_ID` = `admin` (default)
- `CYODA_BOOTSTRAP_ROLES` = `ROLE_ADMIN,ROLE_M2M` (default)

**Admin metrics auth (optional):**

- `CYODA_METRICS_BEARER` — static bearer token required on `GET /metrics`
- `CYODA_METRICS_REQUIRE_AUTH` = `false` (default; set `true` to enforce metrics auth at startup)

### Generating secrets

**JWT signing key** — RSA private key, PEM-encoded. The binary accepts PKCS#8 (`BEGIN PRIVATE KEY`) and PKCS#1 (`BEGIN RSA PRIVATE KEY`) formats. Only RSA keys are accepted; the signature algorithm is always RS256. Minimum recommended size: 2048 bits.

Generate a PKCS#8 RSA-2048 key (preferred):

```
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out signing.pem
```

Generate a PKCS#1 RSA-2048 key (also accepted):

```
openssl genrsa -out signing.pem 2048
```

Pass to the binary via file reference (recommended):

```
export CYODA_JWT_SIGNING_KEY_FILE=/run/secrets/signing.pem
```

**HMAC secret** — hex-encoded shared secret for inter-node dispatch authentication. The binary decodes the value from hex to raw bytes. Minimum: 32 bytes of entropy (64 hex characters).

Generate a 32-byte secret (64 hex chars):

```
openssl rand -hex 32
```

Pass to the binary via file reference (recommended):

```
# Write the hex string to a file, then:
export CYODA_HMAC_SECRET_FILE=/run/secrets/hmac-secret
```

Or pass inline (dev only — not suitable for production):

```
export CYODA_HMAC_SECRET="$(openssl rand -hex 32)"
```

## EXAMPLES

**First-run default (sqlite + mock auth):**

```
cyoda init
cyoda
```

`cyoda init` writes `~/.config/cyoda/cyoda.env` with `CYODA_STORAGE_BACKEND=sqlite`. Running `cyoda` then loads that file and starts the server on port 8080. No other configuration required.

**Postgres + mock auth:**

```
export CYODA_STORAGE_BACKEND=postgres
export CYODA_POSTGRES_URL=postgres://cyoda:secret@localhost:5432/cyoda
cyoda
```

**Postgres + JWT required (production-shaped):**

```
export CYODA_STORAGE_BACKEND=postgres
export CYODA_POSTGRES_URL_FILE=/run/secrets/postgres-url
export CYODA_IAM_MODE=jwt
export CYODA_REQUIRE_JWT=true
export CYODA_JWT_SIGNING_KEY_FILE=/run/secrets/signing.pem
export CYODA_JWT_ISSUER=https://auth.example.com
export CYODA_JWT_AUDIENCE=cyoda-api
cyoda
```

**Docker (sqlite + mock auth):**

```
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -p 127.0.0.1:9091:9091 \
  -e CYODA_STORAGE_BACKEND=sqlite \
  -e CYODA_SQLITE_PATH=/var/lib/cyoda/cyoda.db \
  -e CYODA_ADMIN_BIND_ADDRESS=0.0.0.0 \
  -v cyoda-data:/var/lib/cyoda \
  ghcr.io/cyoda/cyoda:latest
```

The image pre-stages `/var/lib/cyoda` owned by UID 65532. The container runs as non-root (65532:65532). `/livez` and `/readyz` are served on port 9091. Set `CYODA_ADMIN_BIND_ADDRESS=0.0.0.0` so the health endpoint is reachable from outside the container.

**Docker Compose (sqlite + mock auth):**

```
curl -O https://raw.githubusercontent.com/cyoda/cyoda-go/main/deploy/docker/compose.yaml
docker compose up
```

The bundled `compose.yaml` at `deploy/docker/compose.yaml` uses `CYODA_STORAGE_BACKEND=sqlite` and a named volume `cyoda-data` for persistence. For production JWT auth, mount the signing key file and set `CYODA_JWT_SIGNING_KEY_FILE` — do not pass `CYODA_JWT_SIGNING_KEY` inline; multi-line PEM does not survive shell or YAML env-var interpolation. See `run` for the full docker compose JWT configuration.

## SEE ALSO

- cli
- cli.init
- config
- config.database
- config.auth
- run
