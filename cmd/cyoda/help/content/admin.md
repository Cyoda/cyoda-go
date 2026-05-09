---
topic: admin
title: "admin — runtime-switchable admin endpoints"
stability: stable
see_also:
  - run
  - telemetry
  - config.auth
---

# admin

## NAME

admin — runtime-switchable controls exposed on the main API listener.

## SYNOPSIS

```
GET  /api/admin/log-level
POST /api/admin/log-level
GET  /api/admin/trace-sampler
POST /api/admin/trace-sampler
```

## DESCRIPTION

Both endpoint families require `ROLE_ADMIN` on the JWT and update process-local state atomically. State is **not** propagated across nodes; multi-node deployments must hit each node's endpoint separately.

## ENDPOINTS

### log-level

`GET /api/admin/log-level` returns the current effective log level as JSON:

```
{"level": "info"}
```

`POST /api/admin/log-level` changes the level atomically. Request body: `{"level": "<level>"}`. Response: `{"level": "<new>", "previous": "<old>"}`. Valid values: `debug`, `info`, `warn`, `error`.

### trace-sampler

`GET /api/admin/trace-sampler` returns the current OpenTelemetry sampler configuration:

```
{"sampler": "ratio", "ratio": 0.1, "parent_based": true}
```

`POST /api/admin/trace-sampler` changes the sampler atomically. Body shape mirrors the GET response. Valid `sampler` values: `always`, `never`, `ratio`. When `sampler` is `ratio`, `ratio` must be a float in `(0, 1]`. Use `sampler: never` for zero sampling — `ratio: 0` is rejected.

## EXAMPLES

```
# Read current log level
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/admin/log-level

# Switch to debug
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"level":"debug"}' \
  http://localhost:8080/api/admin/log-level

# Sample 10% of traces
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"ratio","ratio":0.1}' \
  http://localhost:8080/api/admin/trace-sampler

# Force 100% sampling on this node regardless of upstream
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"always","parent_based":false}' \
  http://localhost:8080/api/admin/trace-sampler

# Disable local sampling (still honors upstream-sampled traceparent; set parent_based:false to override)
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"never"}' \
  http://localhost:8080/api/admin/trace-sampler
```

## NOTES

- `parent_based` defaults to `true` and respects upstream sampling decisions in the `traceparent` header. With `parent_based: true`, an upstream "do not sample" overrides this node's `sampler: always`. This is standard OpenTelemetry `ParentBased` semantics and is usually correct for distributed-trace integrity. Set `parent_based: false` to override.
- The initial sampler at process start is seeded from the standard OpenTelemetry env vars `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`. Supported values are the six standard combinations from the OTel spec (`always_on`, `always_off`, `traceidratio`, and their `parentbased_` variants). The admin endpoint is a runtime override, not a replacement.
- Sampler and log level are process-local. Each node has its own state; multi-node deployments need to hit each node's admin endpoint separately.

## SEE ALSO

- `run` — server lifecycle
- `telemetry` — OpenTelemetry exporters and metrics
- `config.auth` — JWT and `ROLE_ADMIN`
