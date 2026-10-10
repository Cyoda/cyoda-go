---
topic: cli.health
title: "cyoda health — readiness probe"
stability: stable
see_also:
  - telemetry
---

# cli.health

## NAME

cli.health — probe the admin listener's `/readyz` endpoint.

## SYNOPSIS

`cyoda health`

## DESCRIPTION

`cyoda health` sends an HTTP GET to `http://127.0.0.1:<port>/readyz` and exits 0 if the server responds with HTTP 200. Any non-200 response or connection error causes exit 1.

The probe uses a hard-coded 2-second HTTP client timeout. This timeout is load-bearing: a deadlocked readiness handler looks identical to "server accepts the connection then hangs" from the client's perspective. Without a timeout, Docker's `HEALTHCHECK` would inherit the deadlock and never mark the container unhealthy.

The port is read from `CYODA_ADMIN_PORT` (default: `9091`). The admin listener always binds to `127.0.0.1` from the probe's perspective — `cyoda health` is designed to run inside the same container or on the same host as the server.

Despite the command name, this probes `/readyz` on the admin listener, not `GET /health` on the API listener. The two mirror the same underlying flag — `200`/`ready` while healthy, `503` once a panic has been recovered in engine or store work and the node's state is unverified, latched until the node is replaced (a panic in a probe or a metrics scrape is contained with a ticket but does not latch) — but `/health` is a plain JSON summary for humans and simple scripts, not the deployment probe.

Primary consumers:

- **Docker Compose** — the `healthcheck` of the bundled compose file runs `cyoda health`.

The Helm chart does not run this command: its `readinessProbe` sends an HTTP GET to `/readyz` itself.

## OPTIONS

`cyoda health` accepts no flags and no arguments; any argument is refused with exit code `2` before the probe is sent. The one exception is a lone `-h` or `--help`, which prints this topic and exits `0`.

## ENVIRONMENT VARIABLES

- `CYODA_ADMIN_PORT` — Admin listener port to probe (default: `9091`).

## EXIT CODES

- `0` — Server responded HTTP 200. Instance is ready.
- `1` — Connection failed, timed out, or server returned a non-200 status.
- `2` — An argument was given. No probe is sent.

## EXAMPLES

```
# Basic probe (uses CYODA_ADMIN_PORT or default 9091)
cyoda health

# Probe a server on a non-default admin port
CYODA_ADMIN_PORT=19091 cyoda health

# Use in a shell script
if cyoda health; then
  echo "server is ready"
else
  echo "server not ready" >&2
  exit 1
fi
```

## SEE ALSO

- telemetry
