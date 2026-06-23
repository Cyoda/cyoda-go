---
topic: telemetry
title: "telemetry — OpenTelemetry emission"
stability: evolving
see_also:
  - config
  - cli.serve
  - cli.health
  - config.auth
---

# telemetry

## NAME

telemetry — OpenTelemetry trace, metric, and log emission configuration.

## SYNOPSIS

```
CYODA_OTEL_ENABLED=true \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_SERVICE_NAME=cyoda \
cyoda
```

## DESCRIPTION

cyoda-go integrates the OpenTelemetry Go SDK (`go.opentelemetry.io/otel`). The
**metric scrape pipeline is always on**: at startup the binary creates a meter
provider with an OpenTelemetry → Prometheus exporter and serves it at
`:9091/metrics`, regardless of `CYODA_OTEL_ENABLED`. No collector is required to
scrape metrics.

`CYODA_OTEL_ENABLED` (default `false`) additionally enables **OTLP push** — the
OTLP metric and trace exporters and the tracer provider — for environments with
an OTel collector. When it is `false`, tracing is off and nothing is pushed, but
application metrics are still exposed at `/metrics`.

Which application metrics appear:
- **OIDC subsystem metrics** (`oidc_*`) — always exposed when IAM runs in `jwt`
  mode (the OIDC subsystem is active).
- **Transaction and dispatch metrics** (`cyoda_tx_*`, `cyoda_dispatch_*`) —
  exposed when `CYODA_OTEL_ENABLED=true` (their instrumentation decorators are
  enabled with full observability).
- Go runtime and process metrics are always present.

The instrumentation name is `github.com/cyoda-platform/cyoda-go`.

## ENV VARS

**cyoda-specific:**

- `CYODA_OTEL_ENABLED` — `true` to enable OTLP push (the OTLP metric and trace exporters) and the otelhttp/gRPC tracing middleware; `false` (default) leaves tracing off and OTLP disabled, but the Prometheus scrape pipeline and `/metrics` remain active. All standard `OTEL_*` env vars are only read when this is `true`.
- `CYODA_METRICS_BEARER` — static Bearer token required on `GET :9091/metrics`. When empty (default), `/metrics` is unauthenticated. Supports `_FILE` suffix: `CYODA_METRICS_BEARER_FILE=<path>` takes precedence over the plain var.
- `CYODA_METRICS_REQUIRE_AUTH` — `true` to refuse startup when `CYODA_METRICS_BEARER` is unset. Default `false`. The Helm chart sets this to `true` for shared-cluster deployments.

**Standard OTel env vars read by cyoda-go when `CYODA_OTEL_ENABLED=true`:**

- `OTEL_EXPORTER_OTLP_ENDPOINT` — base URL of the OTLP HTTP collector (e.g. `http://localhost:4318`). Applies to both trace and metric exporters unless overridden by signal-specific vars.
- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` — OTLP HTTP endpoint for traces. Overrides `OTEL_EXPORTER_OTLP_ENDPOINT` for the trace exporter.
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` — OTLP HTTP endpoint for metrics. Overrides `OTEL_EXPORTER_OTLP_ENDPOINT` for the metric exporter.
- `OTEL_EXPORTER_OTLP_HEADERS` — comma-separated `key=value` headers sent with OTLP requests (e.g. for API key authentication).
- `OTEL_EXPORTER_OTLP_TRACES_HEADERS` — per-signal override of `OTEL_EXPORTER_OTLP_HEADERS` for traces.
- `OTEL_EXPORTER_OTLP_METRICS_HEADERS` — per-signal override of `OTEL_EXPORTER_OTLP_HEADERS` for metrics.
- `OTEL_SERVICE_NAME` — `service.name` resource attribute. Identifies the service in traces and metrics. Default value from the OTel SDK (`unknown_service`) when unset.
- `OTEL_TRACES_SAMPLER` — sampler selection. Supported values: (unset) → `ParentBased(AlwaysSample)` (default); `always_on` → `AlwaysSample` root; `always_off` → `NeverSample` root; `traceidratio` → `TraceIDRatioBased(OTEL_TRACES_SAMPLER_ARG)` root; `parentbased_always_on` → `ParentBased(AlwaysSample)`; `parentbased_always_off` → `ParentBased(NeverSample)`; `parentbased_traceidratio` → `ParentBased(TraceIDRatioBased(OTEL_TRACES_SAMPLER_ARG))`; unknown values → logged as WARN, fallback to `parentbased_always_on`.
- `OTEL_TRACES_SAMPLER_ARG` — ratio for `traceidratio` and `parentbased_traceidratio` samplers. Float in `(0, 1]`. Invalid or out-of-range values: logged as WARN, fallback to `1.0`.

## SIGNALS

**Traces**

Traces are exported via `otlptracehttp`. Spans are created by:

- `otelhttp.NewMiddleware("cyoda")` — wraps the HTTP API handler when `CYODA_OTEL_ENABLED=true`; creates one span per inbound HTTP request.
- `otelgrpc.NewServerHandler()` — installed as a gRPC stats handler when `CYODA_OTEL_ENABLED=true`; creates one span per inbound gRPC RPC.
- `observability.TracingTransactionManager` — decorator around the storage `TransactionManager`; creates spans for `tx.begin`, `tx.commit`, `tx.rollback`, `tx.savepoint`, `tx.rollback_to_savepoint`, `tx.release_savepoint`.
- `observability.TracingExternalProcessingService` — decorator around the processor dispatcher; creates spans for `dispatch.processor` and `dispatch.criteria`.

**Metrics**

Metrics are exported via `otlpmetrichttp` with a periodic reader. The following instruments are registered when `CYODA_OTEL_ENABLED=true` (their instrumentation decorators are active only when OTLP push is enabled):

- `cyoda.tx.duration` — `Float64Histogram`, unit `s` — transaction operation duration; labeled by `op` (`begin`, `commit`, `rollback`)
- `cyoda.tx.active` — `Int64UpDownCounter` — count of active (begun but not committed/rolled-back) transactions
- `cyoda.tx.conflicts` — `Int64Counter` — count of transaction serialization conflicts (commit returning `spi.ErrConflict`)
- `cyoda.dispatch.duration` — `Float64Histogram`, unit `s` — processor/criteria dispatch duration; labeled by `type` (`processor`, `criteria`)
- `cyoda.dispatch.count` — `Int64Counter` — total processor/criteria dispatch calls; labeled by `type` (`processor`, `criteria`)

OIDC subsystem metrics (`oidc_*`) are exposed at `/metrics` whenever IAM runs in `jwt` mode, regardless of `CYODA_OTEL_ENABLED`.

**Logs**

cyoda-go uses `log/slog` for structured logging. OTel log emission (OTLP log exporter) is not currently wired. Logs are written to stderr only.

## ATTRIBUTE VOCABULARY

Cyoda-specific span attribute keys defined in `internal/observability/attrs.go`:

- `entity.id` — UUID of the entity being processed
- `entity.model` — model name of the entity
- `entity.state` — current workflow state of the entity
- `tx.id` — transaction UUID
- `op` — operation name within a transaction (`begin`, `commit`, `rollback`, etc.)
- `workflow.name` — name of the workflow definition
- `transition.name` — name of the transition being executed
- `state.from` — workflow state before a transition
- `state.to` — workflow state after a transition
- `cascade.depth` — current depth in the automated-transition cascade loop
- `processor.name` — name of the processor being dispatched
- `processor.execution_mode` — execution mode of the processor (`SYNC`, `ASYNC_SAME_TX`, `ASYNC_NEW_TX`, or `COMMIT_BEFORE_DISPATCH`)
- `processor.tags` — comma-separated `calculationNodesTags` used for member routing
- `criterion.target` — criteria target type (`TRANSITION`, `WORKFLOW`)
- `criteria.matches` — boolean result of a criteria evaluation
- `type` — dispatch type label for `cyoda.dispatch.duration` and `cyoda.dispatch.count` (`processor` or `criteria`)
- `entity.count` — count of entities in a batch operation
- `cql.name` — CQL statement name (Cassandra plugin)
- `cql.op` — CQL operation type (Cassandra plugin)
- `batch.size` — size of a batch operation
- `batch.type` — type of batch
- `version_check.reason` — reason for a version check (cluster protocol)
- `tx.conflict` — boolean; set `true` on `tx.commit` spans when a serialization conflict is recorded
- `tx.savepoint_id` — savepoint identifier on savepoint-related spans

Standard OTel semantic convention attributes set on the resource:

- `service.name` — from `OTEL_SERVICE_NAME`
- `service.instance.id` — from `CYODA_NODE_ID` (set to the gossip node ID in cluster mode; empty string in single-node mode)

## TRACE CONTEXT PROPAGATION

cyoda-go uses **W3C Trace Context** (`traceparent`, `tracestate`) and **W3C Baggage** for context propagation, via `propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})` set as the global text map propagator.

**HTTP**: `otelhttp.NewMiddleware` extracts `traceparent` and `tracestate` from inbound HTTP request headers automatically. Outbound requests (inter-node cluster dispatch via `HTTPForwarder`) do not currently inject trace context.

**gRPC**: `otelgrpc.NewServerHandler()` extracts trace context from inbound gRPC metadata automatically.

**Messaging / internal**: `observability.InjectTraceContext(ctx, headers)` writes `traceparent` and `tracestate` into a `map[string]string` carrier. `observability.ExtractTraceContext(baseCtx, headers)` restores the remote span context. Both use the global text map propagator.

## METRICS ENDPOINT

**GET :9091/metrics**

Serves Prometheus-format metrics (text exposition format). The handler is `promhttp.Handler()` from `github.com/prometheus/client_golang`. Port is `CYODA_ADMIN_PORT` (default `9091`); bind address is `CYODA_ADMIN_BIND_ADDRESS` (default `127.0.0.1`).

The handler is backed by a dedicated `prometheus.Registry` that collects from the
OTel SDK meter provider (via the OpenTelemetry → Prometheus bridge), so
application metrics registered through the OTel API appear here automatically.
Go runtime metrics (GC, goroutine count, memory) and process metrics (CPU, open
FDs) are always present. OIDC subsystem metrics (`oidc_*`) appear whenever IAM
runs in `jwt` mode. Transaction and dispatch metrics (`cyoda_tx_*`,
`cyoda_dispatch_*`) appear when `CYODA_OTEL_ENABLED=true`.

## AUTHENTICATION

When `CYODA_METRICS_BEARER` is non-empty, `GET :9091/metrics` requires:

```
Authorization: Bearer <CYODA_METRICS_BEARER value>
```

The comparison is constant-time to prevent timing attacks. `GET :9091/livez` and `GET :9091/readyz` remain unauthenticated regardless of `CYODA_METRICS_BEARER`.

When `CYODA_METRICS_BEARER` is empty (default), `/metrics` is unauthenticated. This is the expected posture when the admin listener is bound to loopback (`CYODA_ADMIN_BIND_ADDRESS=127.0.0.1`) and access is controlled at the network level.

### Bind-address modes

`CYODA_ADMIN_BIND_ADDRESS` is the outer boundary of the admin listener. Pair it with the bearer-token settings according to the deployment shape:

- **Desktop**: leave `CYODA_ADMIN_BIND_ADDRESS` at its default `127.0.0.1` (loopback only); no bearer needed.
- **Kubernetes**: bind to `0.0.0.0` so kubelet probes and Prometheus reach the pod-facing interface, and enable `CYODA_METRICS_BEARER` (+ `CYODA_METRICS_REQUIRE_AUTH=true`). The Helm chart does both, mounts the token via `_FILE`, and points the `ServiceMonitor` at it via `bearerTokenSecret`.
- **Docker Compose**: keep the default loopback bind inside the container and publish the port as `127.0.0.1:9091:9091` so it is only reachable from the host; no bearer needed.

The `/livez` and `/readyz` probe endpoints stay unauthenticated regardless of bind address, since kubelet probes carry no bearer.

## SAMPLER

The trace sampler is runtime-configurable via:

- `POST :8080/api/admin/trace-sampler` — replaces the sampler atomically; requires `Authorization: Bearer <token>`
- `GET :8080/api/admin/trace-sampler` — returns the current sampler config

Request and response body:

```json
{
  "sampler": "ratio",
  "ratio": 0.1,
  "parent_based": true
}
```

- `sampler` — `"always"`, `"never"`, or `"ratio"`
- `ratio` — float in `(0, 1]`; required and only valid when `sampler="ratio"`
- `parent_based` — boolean; when `true`, the sampler is wrapped with `ParentBased()`

The sampler is backed by `atomic.Pointer[samplerState]`. Reads on the sampling hot path pay no mutex cost. The runtime-replaced sampler takes effect immediately for all new spans.

On startup, `SamplerConfigFromEnv()` is called to seed the sampler from `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` before the `TracerProvider` is constructed.

## EXAMPLES

**Start with OTel enabled, exporting to a local OTLP collector:**

```
CYODA_OTEL_ENABLED=true \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
OTEL_SERVICE_NAME=cyoda-dev \
OTEL_TRACES_SAMPLER=parentbased_traceidratio \
OTEL_TRACES_SAMPLER_ARG=0.1 \
cyoda
```

**Start with OTel enabled via Docker, exporting to Jaeger:**

```
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -p 127.0.0.1:9091:9091 \
  -e CYODA_STORAGE_BACKEND=memory \
  -e CYODA_OTEL_ENABLED=true \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://host.docker.internal:4318 \
  -e OTEL_SERVICE_NAME=cyoda \
  ghcr.io/cyoda/cyoda:latest
```

**Scrape Prometheus metrics from the admin port:**

```
curl -s http://localhost:9091/metrics | grep "^go_"
```

**Scrape metrics with bearer auth:**

```
curl -s \
  -H "Authorization: Bearer $CYODA_METRICS_BEARER" \
  http://localhost:9091/metrics
```

**Get current sampler configuration:**

```
curl -s \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/admin/trace-sampler
```

**Set sampler to 10% ratio sampling:**

```
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sampler":"ratio","ratio":0.1,"parent_based":true}' \
  http://localhost:8080/api/admin/trace-sampler
```

## SEE ALSO

- config
- cli.serve
- cli.health
- config.auth
