# Canonical Docker provisioning for cyoda

This directory is the canonical Docker source:

- `Dockerfile` — the source of `ghcr.io/cyoda/cyoda:<version>`
  and `:latest`, published by GoReleaser on every non-prerelease tag.
- `compose.yaml` — a reference single-service compose file that runs the
  published image with sqlite persistence.

## Quick start

```bash
cd deploy/docker
docker compose up
```

Then:

```bash
curl http://127.0.0.1:8080/api/health    # API on port 8080
curl http://127.0.0.1:9091/livez         # admin liveness
curl http://127.0.0.1:9091/metrics       # Prometheus scrape
```

Data persists in the named volume `cyoda-data`. `docker compose down` stops
cyoda; `docker compose down -v` wipes the data.

## What this compose demonstrates

The primary use case for this file is **reference documentation**: an
application developer embedding cyoda in their own compose stack reads
this file to learn what env vars, ports, and volume mounts cyoda needs,
then cribs the fragment into their own `docker-compose.yml`.

Key elements to copy:

- `CYODA_STORAGE_BACKEND: sqlite` + `CYODA_SQLITE_PATH: /var/lib/cyoda/cyoda.db` — the default storage path.
- `CYODA_ADMIN_BIND_ADDRESS: 0.0.0.0` — required inside the container because Docker port mapping forwards to the container's eth0, not its loopback.
- Named volume mount at `/var/lib/cyoda`.
- Compose-level `healthcheck` invoking `cyoda health`.

## Security posture

- **Admin port (9091) is unauthenticated by design.** It exposes
  `/livez`, `/readyz`, `/metrics`. The compose binds it to
  `127.0.0.1:9091` — loopback-only — which is safe for dev and
  single-host deployments. Flip to `0.0.0.0:9091` ONLY if your
  deployment has authentication upstream of cyoda (ingress, sidecar,
  service mesh).
  Note: `CYODA_ADMIN_BIND_ADDRESS=0.0.0.0` inside the container means
  the admin port is reachable from any other service on the same
  compose network. The loopback-only guarantee is the host-port
  mapping (`127.0.0.1:9091:9091`), not the container-internal
  binding. If you crib this fragment into a multi-service compose
  stack, assume any sidecar can hit `/metrics` and `/readyz`
  without auth.
- **Mock auth is the startup default.** `CYODA_IAM_MODE=mock` accepts
  all requests. For production, set `CYODA_REQUIRE_JWT=true` AND
  provide `CYODA_JWT_SIGNING_KEY` (multi-line PEM:
  `export CYODA_JWT_SIGNING_KEY="$(cat key.pem)"` before
  `docker compose up`). A startup banner warns when running in mock
  mode; `CYODA_SUPPRESS_BANNER=true` silences it (CI only — not
  production).

## For Kubernetes / production

**Use the Helm chart, not this compose.** This compose is for local
development and evaluation. Production orchestration (HA, TLS, network
policy, PodDisruptionBudget, migrations as a Job, service mesh
integration) is the Helm chart's territory per
`docs/superpowers/specs/2026-04-16-provisioning-shared-design.md`.

## For Postgres and observability

`examples/compose-with-observability/compose.yaml` runs cyoda + Postgres
+ Grafana/Prometheus/Tempo. Use that file if you want to experiment
against a Postgres backend or view cyoda's metrics and traces locally.

## For contributors iterating on cyoda itself

`scripts/dev/run-docker-dev.sh` builds a local cyoda binary from source,
packages it into the canonical Dockerfile with a `:dev` tag, and runs
this compose against that local image.
