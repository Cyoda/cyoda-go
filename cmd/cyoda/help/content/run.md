---
topic: run
title: "run — runtime modes and operational semantics"
stability: stable
see_also:
  - cli
  - cli.serve
  - cli.init
  - cli.health
  - quickstart
  - helm
  - config
  - config.database
  - config.auth
  - telemetry
---

# run

## NAME

run — supported ways to start cyoda-go: binary, Docker, Docker Compose, Kubernetes (Helm), and development scripts.

## SYNOPSIS

```
# Binary (default mode — no flags)
cyoda

# Docker
docker run --rm -p 127.0.0.1:8080:8080 -p 127.0.0.1:9090:9090 -p 127.0.0.1:9091:9091 \
  ghcr.io/cyoda/cyoda:latest

# Docker Compose (bundled compose.yaml)
docker compose -f deploy/docker/compose.yaml up

# Helm (Kubernetes)
helm install cyoda ./deploy/helm/cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt

# Dev script (in-memory, mock auth)
./scripts/dev/run-local.sh
```

## DESCRIPTION

cyoda-go is a single-process, multi-tenant REST and gRPC API server. It starts in serving mode when invoked with no subcommand. All configuration is via environment variables with a `CYODA_` prefix. The binary, Docker image, and Helm chart run the same binary; only the environment configuration differs across run modes.

The process binds three TCP listeners concurrently: the REST API (default port 8080), gRPC (default port 9090), and an admin server (default port 9091). The admin server hosts health probes and the Prometheus metrics endpoint. On receiving `SIGINT` or `SIGTERM`, the server drains in-flight HTTP and admin requests within a 10-second deadline, then closes the storage backend and exits.

No systemd unit files ship in the repository. Process supervision (systemd, runit, s6, etc.) is the operator's responsibility when running the binary directly outside of Docker or Kubernetes.

## RUN MODES

### Binary

The prebuilt binary is the canonical artifact. Build from source or download from the GitHub release page.

**Build from source:**

```
go build -o bin/cyoda ./cmd/cyoda
```

**Run (default — in-memory storage, mock auth):**

```
./bin/cyoda
```

**Run with SQLite after init:**

```
cyoda init
cyoda
```

`cyoda init` writes `~/.config/cyoda/cyoda.env` with `CYODA_STORAGE_BACKEND=sqlite`. The binary loads that file via `app.LoadEnvFiles()` at startup.

**Required env vars for postgres + JWT mode:**

```
export CYODA_STORAGE_BACKEND=postgres
export CYODA_POSTGRES_URL=postgres://user:pass@host:5432/dbname
export CYODA_IAM_MODE=jwt
export CYODA_REQUIRE_JWT=true
export CYODA_JWT_SIGNING_KEY_FILE=/run/secrets/signing.pem
cyoda
```

The binary accepts env vars from the process environment, from `.env` files loaded by `CYODA_PROFILES`, and from the user config written by `cyoda init`. The `CYODA_PROFILES` variable selects which `.env` profile files to load from the **current working directory**. For example, `CYODA_PROFILES=postgres,jwt` loads `.env.postgres` then `.env.jwt` from the working directory. The user config at `~/.config/cyoda/cyoda.env` (written by `cyoda init`) is always loaded automatically as a separate step — it is not a profile file.

### Docker

**Image:** `ghcr.io/cyoda/cyoda:latest`

The image uses `gcr.io/distroless/static` as its base. The binary is placed at `/cyoda`. The container runs as UID/GID 65532:65532 (non-root). `/var/lib/cyoda` is pre-staged with ownership 65532:65532 and is the intended mount point for persistent SQLite data.

Exposed ports: `8080` (HTTP), `9090` (gRPC), `9091` (admin).

The entrypoint is `/cyoda` with no default arguments. Subcommands (`init`, `health`, `migrate`) are passed as Docker CMD arguments.

**Minimal run (in-memory + mock auth):**

```
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -p 127.0.0.1:9091:9091 \
  -e CYODA_ADMIN_BIND_ADDRESS=0.0.0.0 \
  ghcr.io/cyoda/cyoda:latest
```

`CYODA_ADMIN_BIND_ADDRESS=0.0.0.0` is required when running in Docker so the health probes on port 9091 are reachable from outside the container. Without it, the admin server binds to loopback (127.0.0.1) inside the container and `/livez` and `/readyz` are unreachable.

**SQLite with persistent volume:**

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

**Postgres + JWT (production-shaped):**

```
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -p 127.0.0.1:9091:9091 \
  -e CYODA_STORAGE_BACKEND=postgres \
  -e CYODA_POSTGRES_URL=postgres://cyoda:secret@db:5432/cyoda \
  -e CYODA_IAM_MODE=jwt \
  -e CYODA_REQUIRE_JWT=true \
  -e CYODA_JWT_SIGNING_KEY_FILE=/run/secrets/signing.pem \
  -v /path/to/signing.pem:/run/secrets/signing.pem:ro \
  -e CYODA_ADMIN_BIND_ADDRESS=0.0.0.0 \
  ghcr.io/cyoda/cyoda:latest
```

### Docker Compose

The repository ships a bundled compose file at `deploy/docker/compose.yaml`.

**Default compose configuration:**

```yaml
services:
  cyoda:
    image: ghcr.io/cyoda/cyoda:latest
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:9090:9090"
      - "127.0.0.1:9091:9091"
    environment:
      CYODA_STORAGE_BACKEND: sqlite
      CYODA_SQLITE_PATH: /var/lib/cyoda/cyoda.db
      CYODA_ADMIN_BIND_ADDRESS: 0.0.0.0
    volumes:
      - cyoda-data:/var/lib/cyoda
    healthcheck:
      test: ["CMD", "/cyoda", "health"]
      interval: 10s
      timeout: 3s
      start_period: 30s
      retries: 3
```

The bundled compose file uses SQLite + mock auth by default. The compose healthcheck calls `cyoda health`, which GETs `/readyz` on the admin port with a 2-second client timeout (see `cmd/cyoda/health.go`).

**Run the bundled compose file:**

```
docker compose -f deploy/docker/compose.yaml up
```

**Enable JWT auth before starting compose (file-mount approach):**

**Do not** pass `CYODA_JWT_SIGNING_KEY` as an inline environment variable through docker compose — multi-line PEM content does not survive shell interpolation or YAML env-var parsing reliably. Always use `CYODA_JWT_SIGNING_KEY_FILE` with a volume mount.

```
# 1. Place the signing key on the host (outside the compose dir if gitignored):
#    ./secrets/signing.pem
# 2. docker-compose.yaml snippet (add to the cyoda service):
#      volumes:
#        - ./secrets/signing.pem:/run/secrets/signing.pem:ro
#      environment:
#        CYODA_IAM_MODE: jwt
#        CYODA_REQUIRE_JWT: "true"
#        CYODA_JWT_SIGNING_KEY_FILE: /run/secrets/signing.pem
#        CYODA_JWT_ISSUER: https://auth.example.com
#        CYODA_JWT_AUDIENCE: cyoda-api
# 3. Launch:
docker compose up
```

**Use a custom image (e.g. a local dev build):**

```
CYODA_IMAGE=ghcr.io/cyoda/cyoda:dev \
  docker compose -f deploy/docker/compose.yaml up
```

The compose file reads `${CYODA_IMAGE:-ghcr.io/cyoda/cyoda:latest}` for the image name.

**Adjust start_period for slow environments** (Postgres migrations, cluster mode) by editing the `healthcheck.start_period` field in `deploy/docker/compose.yaml`.

### Kubernetes (Helm)

cyoda-go ships a Helm chart at `deploy/helm/cyoda/`. The chart renders a StatefulSet (with Parallel pod management policy), ClusterIP Service, headless Service for gossip, ConfigMap for non-sensitive env vars, projected-volume Secrets for credentials, and optional Gateway API routes, Ingress, HorizontalPodAutoscaler, PodDisruptionBudget, NetworkPolicy, ServiceAccount, ServiceMonitor, and a pre-upgrade migration Job.

The chart is not yet published to a Helm repository. Install directly from the local path or from a cloned repository. See `helm` for the complete Helm reference.

**Minimal install:**

```
helm install cyoda ./deploy/helm/cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt
```

**Upgrade:**

```
helm upgrade --install cyoda ./deploy/helm/cyoda \
  --set postgres.existingSecret=cyoda-pg \
  --set jwt.existingSecret=cyoda-jwt
```

### Development Scripts

Developer convenience scripts live under `scripts/dev/`. These are not canonical provisioning artifacts. Canonical artifacts are in `deploy/`.

- `scripts/dev/run-local.sh` — runs `cyoda-go` via `go run ./cmd/cyoda` using the `local` profile (in-memory storage, mock auth). Override with `CYODA_PROFILES=postgres,otel ./scripts/dev/run-local.sh`.
- `scripts/dev/run-docker-dev.sh` — builds the binary from source for the host platform (`linux/amd64` or `linux/arm64`), builds a local Docker image tagged `ghcr.io/cyoda/cyoda:dev`, and runs it via `docker compose -f deploy/docker/compose.yaml up`. Generates a fresh JWT signing key and randomized bootstrap client secret per run. Intended for contributors testing local changes in a container before they land.

**Run with in-memory storage and mock auth (go run):**

```
./scripts/dev/run-local.sh
```

**Run with a custom profile set:**

```
CYODA_PROFILES=postgres,otel ./scripts/dev/run-local.sh
```

**Build and run a local Docker image:**

```
./scripts/dev/run-docker-dev.sh
```

## SIGNALS

- `SIGINT` (Ctrl+C) — triggers graceful shutdown. HTTP and admin servers drain in-flight requests within a 10-second deadline. The storage backend is closed. The process exits with code 0.
- `SIGTERM` — same behavior as `SIGINT`. Kubernetes sends `SIGTERM` when a pod is evicted or deleted.
- `SIGPIPE` — ignored. When the binary is piped through `tee` (e.g. `./bin/cyoda | tee log`) and Ctrl+C kills `tee` first, the broken pipe would cause the binary to exit immediately before the `SIGINT` handler runs. Ignoring `SIGPIPE` lets the write fail silently while the graceful shutdown proceeds. (Source: `cmd/cyoda/main.go`, `signal.Ignore(syscall.SIGPIPE)`.)

Signal handling is established in `main()` before the listeners start. The signal channel has buffer size 1.

## HEALTH PROBES

Both probes are served on `CYODA_ADMIN_PORT` (default `9091`) at `CYODA_ADMIN_BIND_ADDRESS` (default `127.0.0.1`). Both endpoints are unauthenticated — authentication is not applied to `/livez` or `/readyz` regardless of `CYODA_METRICS_BEARER` or `CYODA_METRICS_REQUIRE_AUTH`.

- `GET /livez` — liveness probe. Returns `200 OK` with body `ok` when the admin server is accepting connections. No business logic check is performed.
- `GET /readyz` — readiness probe. Returns `200 OK` with body `ok` when the server has completed startup and is ready to serve requests. Returns a non-200 status while the storage backend is initializing or migrations are pending.

The `cyoda health` subcommand calls `/readyz` on the admin port with a 2-second HTTP client timeout and exits 0 on `200 OK`, 1 otherwise. This is the implementation behind Docker's `HEALTHCHECK: CMD /cyoda health` and is valid as a readiness check for any init system.

**Admin bind address in container environments:** set `CYODA_ADMIN_BIND_ADDRESS=0.0.0.0` to make health probes reachable from outside the container. The Kubernetes Helm chart sets this value in its ConfigMap. Without it, `/livez` and `/readyz` are inaccessible from the kubelet or Docker healthcheck daemon.

## SHUTDOWN TIMING

The graceful shutdown deadline is **10 seconds**, applied separately to the HTTP server and the admin server. This value is hardcoded in `cmd/cyoda/main.go`:

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
```

After both HTTP servers shut down, `app.Close()` is called to release backend resources (database connection pools, cluster membership). There is no separate configurable timeout for `app.Close()` — it runs to completion after the HTTP deadline.

In Kubernetes, the pod `terminationGracePeriodSeconds` (default 30s) must be greater than 10s to allow the HTTP drain to complete before the kubelet sends `SIGKILL`.

## PORT LAYOUT

All ports are configurable via environment variables. The defaults:

- **HTTP REST API** — port `8080`, bind address `0.0.0.0` (all interfaces). Controlled by `CYODA_HTTP_PORT`. All entity, model, workflow, search, and auth endpoints. Context path prefix: `CYODA_CONTEXT_PATH` (default `/api`).
- **gRPC** — port `9090`, bind address `0.0.0.0` (all interfaces). Controlled by `CYODA_GRPC_PORT`. Externalized-processor streaming (processor and criteria dispatch). The bind expression is `fmt.Sprintf(":%d", cfg.GRPC.Port)` — all interfaces, not loopback.
- **Admin** — port `9091`, bind address `127.0.0.1` (loopback) by default. Controlled by `CYODA_ADMIN_PORT` and `CYODA_ADMIN_BIND_ADDRESS`. Hosts `/livez`, `/readyz`, and `/metrics`. Set `CYODA_ADMIN_BIND_ADDRESS=0.0.0.0` in Docker/Kubernetes to make probes reachable.
- **Gossip (cluster mode only)** — port `7946` TCP+UDP. Controlled by `CYODA_GOSSIP_ADDR` (default `:7946`). Used by the memberlist gossip protocol for cluster membership and SWIM health checking. Active only when `CYODA_CLUSTER_ENABLED=true`.

## EXAMPLES

**Binary — postgres + JWT, production-shaped:**

```
export CYODA_STORAGE_BACKEND=postgres
export CYODA_POSTGRES_URL_FILE=/run/secrets/postgres-url
export CYODA_IAM_MODE=jwt
export CYODA_REQUIRE_JWT=true
export CYODA_JWT_SIGNING_KEY_FILE=/run/secrets/signing.pem
export CYODA_JWT_ISSUER=https://auth.example.com
export CYODA_JWT_AUDIENCE=cyoda-api
./bin/cyoda
```

**Docker — in-memory + OTel tracing:**

```
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -p 127.0.0.1:9091:9091 \
  -e CYODA_ADMIN_BIND_ADDRESS=0.0.0.0 \
  -e CYODA_OTEL_ENABLED=true \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://host.docker.internal:4318 \
  -e OTEL_SERVICE_NAME=cyoda \
  ghcr.io/cyoda/cyoda:latest
```

**Docker — suppress banner (CI):**

```
docker run --rm \
  -e CYODA_SUPPRESS_BANNER=true \
  -e CYODA_ADMIN_BIND_ADDRESS=0.0.0.0 \
  ghcr.io/cyoda/cyoda:latest
```

**Check health from outside the container:**

```
curl -s http://localhost:9091/readyz
```

**Binary — check readiness via subcommand:**

```
./bin/cyoda health
echo $?   # 0 = ready, 1 = not ready or error
```

**Docker Compose — production JWT (file-mount):**

```
# Mount the PEM file and reference it via CYODA_JWT_SIGNING_KEY_FILE.
# Do not export CYODA_JWT_SIGNING_KEY inline — multi-line PEM does not
# survive shell → docker-compose env interpolation reliably.
# See the Docker Compose section above for the full snippet.
docker compose up
```

## SEE ALSO

- cli
- cli.serve
- cli.init
- cli.health
- quickstart
- helm
- config
- config.database
- config.auth
- telemetry
