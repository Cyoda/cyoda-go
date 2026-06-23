# OIDC subsystem metrics — primitive infrastructure (issue #313)

**Status:** design approved, pre-implementation
**Date:** 2026-06-23
**Refs:** #284 (OIDC providers), D22; follow-up #313

## 1. Problem

Issue #284 committed (spec D22) to a metric set for the OIDC subsystem and shipped a
thin `Metrics` interface (`internal/auth/oidc/observability.go`) wired at every call
site, with `NopMetrics` as the only implementation. No metrics are emitted in
production today. This change lands the metric-primitive infrastructure and a real
`Metrics` implementation.

### 1.1 The latent split this change resolves

The codebase has **two disconnected metric pipelines**:

1. **OTel meters → OTLP push.** The initial import wired an OTel `MeterProvider` with a
   `PeriodicReader` → `otlpmetrichttp` exporter (`internal/observability/init.go`).
   `observability.Meter()` returns a meter; `tx_tracing.go` and `dispatch_tracing.go`
   already create instruments against it (`cyoda.tx.*`, `cyoda.dispatch.*`). These are
   pushed via OTLP only.
2. **Prometheus scrape `/metrics`.** Three days after the initial import, the admin
   probe surface (`internal/admin/admin.go`) added `/metrics` via `promhttp.Handler()`
   on the **global default Prometheus registry**. It was never bridged to the OTel
   meter provider, so it exposes only the Go runtime/process collectors that
   `client_golang` auto-registers. **No application metric appears at `/metrics` today.**

A `grep` confirms nothing registers application metrics into the Prometheus registry
(`promauto` / `MustRegister` absent outside the admin import). The scrape endpoint is an
empty-of-app-metrics surface sitting next to the real (OTLP-push) pipeline.

## 2. Decision & rationale (SOTA review)

**Use the OTel metrics API for instrumentation; add the OTel Prometheus exporter as a
second `MetricReader` so `/metrics` serves all OTel metrics.**

The governing principle (OpenTelemetry's own compatibility guidance): instrumentation
code should depend only on the metrics API, with SDK/exporter configuration kept
separate. Prometheus `client_golang` fuses metric definition with backend/registry
binding; OTel splits them — where a metric goes (OTLP push, Prometheus scrape, both) is
a startup-only decision. A single OTel `MeterProvider` accepts multiple readers, each
exporting independently, so we get push **and** scrape from one instrumentation surface.
`go.opentelemetry.io/otel/exporters/prometheus` is a `metric.Reader` that registers as a
`prometheus.Collector`; its default translation (`UnderscoreEscapingWithSuffixes`) maps
dotted instrument names to the exact D22 Prometheus names.

This choice also:
- **Matches existing precedent** — `tx_tracing.go` / `dispatch_tracing.go` already use OTel
  meters. A second instrumentation style (Prometheus client direct) would fracture the
  codebase.
- **Resolves the §1.1 split** — one wiring change makes every OTel metric both scrapeable
  and pushable. The `cyoda.tx.*` / `cyoda.dispatch.*` metrics become visible at `/metrics`
  as a side effect.
- **Is smoke-testable without a collector** — the acceptance criterion ("metric names appear
  in the export endpoint") becomes a `GET /metrics` assertion, which matters for
  local/desktop deploys that have no OTLP collector.

### 2.1 Alternatives rejected

- **Prometheus client direct (`promauto` on default registry).** Introduces a second
  instrumentation API in application code, re-fusing definition and backend — the exact
  coupling OTel removes. Two parallel pipelines and naming conventions forever,
  inconsistent with tx/dispatch, awkward to also OTLP-push. Rejected.
- **OTLP-push only (no `/metrics` change).** Consistent style, but `/metrics` stays blind to
  app metrics; on any deploy without a collector the metrics evaporate, and the smoke
  test can't read them without standing up a collector. Leaves the split unfixed
  (fails Gate 6). Rejected.

### 2.2 Primitive shape

No bespoke wrapper types. "Shared primitives" = OTel instruments created via
`internal/observability.Meter()`, exactly the tx/dispatch precedent. A cyoda-owned
`Counter`/`Gauge`/`Histogram` abstraction over the OTel API would re-introduce the
coupling OTel removes and diverge from the established pattern.

## 3. Architecture

Single OTel `MeterProvider`, **two readers**:

```
                         ┌─ PeriodicReader → otlpmetrichttp  (push, existing)
MeterProvider ──────────-┤
                         └─ otelprom.Exporter (Reader)        (pull, NEW)
                                  │ registers as prometheus.Collector
                                  ▼
                         dedicated *prometheus.Registry  (observability-owned)
                                  │  + Go + process collectors
                                  ▼
                         observability.MetricsHandler() http.Handler
                                  │
                                  ▼
                         admin /metrics  (optional Bearer auth, unchanged)
```

### 3.1 Registry ownership

`internal/observability` owns a **dedicated `prometheus.Registry`** (OTel docs explicitly
recommend a custom registry over the global default to avoid global state):

- In `Init`, create `reg := prometheus.NewRegistry()`, register
  `collectors.NewGoCollector()` + `collectors.NewProcessCollector(...)` into it (preserving
  the runtime/process metrics the default registry gave us), and construct the exporter
  with `otelprom.New(otelprom.WithRegisterer(reg))`.
- Expose `observability.MetricsHandler() http.Handler` returning
  `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})`.
- `Init` already sync.Once-guards; `ResetInit` (test-only) discards the registry so a fresh
  one is built on re-init — no `AlreadyRegisteredError` (the lifecycle hazard that rules
  out the global default registry).

Exporter options: keep defaults except set `WithoutScopeInfo()` to suppress per-scope
`otel_scope_*` labels (noise on a single-service binary); `target_info` is retained.

### 3.2 Admin wiring

`admin.Options` gains an optional `MetricsHandler http.Handler`. When nil, `NewHandler`
falls back to `promhttp.Handler()` (current behavior preserved for any caller that does not
set it — e.g. existing admin tests). `cmd/cyoda/run.go` / `app` pass
`observability.MetricsHandler()` so production `/metrics` carries app metrics. The Bearer-auth
wrapping (`requireBearer`) is unchanged and wraps whichever handler is in effect.

## 4. OIDC metrics implementation

New file `internal/auth/oidc/metrics_otel.go`: `otelMetrics` struct implementing the
existing `Metrics` interface, constructed via `NewOTelMetrics(meter metric.Meter)
(Metrics, error)`. Taking a `metric.Meter` (not `internal/observability` directly) keeps
`oidc` decoupled and unit-testable with a `manualReader`. `NopMetrics` stays for tests.

Instrument creation errors are returned (not swallowed). Bounded-enum label options
(`outcome`, `reason`) are pre-built once at constructor time as cached
`metric.MeasurementOption` values, never per call (OTel hot-path allocation mitigation).

### 4.1 Metric mapping

Translation strategy: exporter default (`UnderscoreEscapingWithSuffixes`). Names authored
in dotted OTel form render to the exact D22 Prometheus names.

| Interface method | OTel instrument (kind, unit) | Rendered `/metrics` name |
|---|---|---|
| `IncKidCacheHit` | `oidc.kid.cache.hit` (Int64Counter) | `oidc_kid_cache_hit_total` |
| `IncKidCacheMiss` | `oidc.kid.cache.miss` (Int64Counter) | `oidc_kid_cache_miss_total` |
| `IncKidCacheEvict` | `oidc.kid.cache.evict` (Int64Counter) | `oidc_kid_cache_evict_total` |
| `IncJWKSFetchError(outcome)` | `oidc.jwks.fetch.error` (Int64Counter) + `outcome` | `oidc_jwks_fetch_error_total{outcome}` |
| `IncBroadcastPanic` | `oidc.broadcast.panic` (Int64Counter) | `oidc_broadcast_panic_total` |
| `IncBroadcastDrop(reason)` ⚠ | `oidc.broadcast.drop` (Int64Counter) + `reason` | `oidc_broadcast_drop_total{reason}` |
| `IncUnknownProviderBroadcast` | `oidc.unknown_provider.broadcast` (Int64Counter) | `oidc_unknown_provider_broadcast_total` |
| `ObserveBroadcastReceive(s)` | `oidc.broadcast.receive` (Float64Histogram, unit `s`) | `oidc_broadcast_receive_seconds` |
| `SetRegistryProviders(n)` | `oidc.registry.providers` (Int64Gauge) | `oidc_registry_providers` |

- Gauge uses synchronous `Int64Gauge` (present in pinned otel v1.43.0), mapping 1:1 to
  `SetRegistryProviders(n int)` — no observable callback.
- `oidc_registry_providers` carries **no tenant label** (D22 / rev.4 I3).
- Label cardinality is bounded: `outcome` and `reason` are closed enums (`reason` ∈
  {`malformed_envelope`, `oversized_op`, `oversized_tenantid`, `oversized_uri`} per
  `broadcast.go`).

### 4.2 D22-vs-interface reconciliation (the ⚠ row)

D22's enumerated list in the #284 spec names **8** metrics and omits
`oidc_broadcast_drop_total`. The shipped `Metrics` interface, however, has **9**
methods — `IncBroadcastDrop(reason)` is wired at two production call sites
(`broadcast.go:54,76`) and covered by `broadcast_test.go`. The interface is the executable
contract; leaving `IncBroadcastDrop` a no-op while it is wired and tested is exactly the
half-connected instrument Gate 6 forbids. **This change implements all 9.** The D22 prose
was simply not back-ported when the drop counter was added during #284 implementation;
this spec is the record of that reconciliation. (The #284 spec is a historical design
record and is not edited.)

### 4.3 Startup wiring

`app/app.go` (~line 274): replace `oidc.NopMetrics{}` with
`oidc.NewOTelMetrics(observability.Meter())`, handling the returned error at startup
(fail fast, consistent with other startup wiring).

## 5. Adjacent cleanup (folded in — Gate 6)

Enabling the Prometheus reader exposes the existing `cyoda.tx.*` / `cyoda.dispatch.*`
instruments at `/metrics` for the first time, so their hygiene now matters. Audit results:

- **No cardinality risk.** Metric-level labels are bounded enums only (`tx.op` ∈
  {begin, commit, rollback}; `dispatch.type` ∈ {processor, criteria}). High-cardinality
  attributes (processor name, tags, workflow/transition/tx IDs) live on **spans only**,
  never on the metric instruments.
- **Naming/units render cleanly.** Histograms carry `WithUnit("s")`
  (`cyoda_tx_duration_seconds`, `cyoda_dispatch_duration_seconds`); counters → `_total`;
  the UpDownCounter renders as a gauge. No renames needed.

Two same-package debt items are fixed as part of this change, each TDD'd:

- **(a) Swallowed instrument-creation errors.** `tx_tracing.go` / `dispatch_tracing.go`
  currently do `x, _ := meter.Float64Histogram(...)`. Change to log-on-error (no
  constructor signature change).
- **(b) Per-call attribute allocation.** Both files build `metric.WithAttributes(...)` on
  every `Record`/`Add`. Hoist the bounded-enum attribute options to constructor-time
  cached `metric.MeasurementOption` fields, matching the OIDC impl pattern and serving the
  performance goal.

## 6. Performance & memory

- Counter/gauge adds are atomic ops keyed by attribute set; the one hot-path cost in
  OTel-Go is `attribute.Set` allocation, eliminated here by pre-building label options at
  constructor time (§4, §5b).
- Memory ∝ distinct `(instrument × attribute-set)` series held in the SDK. OIDC label sets
  are tiny closed enums and the gauge is aggregate (no tenant label) → single-digit series.
- The Prometheus reader is pull-based: zero steady-state cost; series materialize only on
  `/metrics` scrape.

## 7. Testing (TDD — RED first)

- **observability (unit):** `Init` registers the Prometheus reader and Go/process
  collectors; scraping `MetricsHandler()` yields a known test instrument's series.
  `ResetInit` + re-`Init` does not panic / re-register.
- **oidc (unit):** `otelMetrics` against a `sdkmetric.NewManualReader()` — each interface
  method produces the expected instrument + attribute set; `oidc_registry_providers` has no
  tenant attribute; `NewOTelMetrics` surfaces instrument-creation errors.
- **tx/dispatch (unit):** instrument-creation error path logs (a); attribute options are
  built once and reused, asserted via recorded series labels (b).
- **E2E smoke (`internal/e2e`):** exercise an OIDC path, `GET /metrics`, assert all **9**
  D22 metric names appear with correct labels/cardinality (and `oidc_registry_providers`
  has no tenant label). This is the acceptance smoke test.
- **Gate 5:** `go test ./... -v` green (incl. E2E); `go vet ./...`. **Race** (`make race`)
  once before PR.

## 8. Documentation (Gate 4)

- `WorkflowConfigurationDto` import surface untouched → no schema-version work.
- `/metrics` now carries application metrics (behavior change): update `README.md`
  observability/config section and the relevant `cmd/cyoda/help/content/config/*.md` topic
  to state that application metrics (OIDC, tx, dispatch) are exposed at `/metrics` in
  addition to OTLP push.
- No env-var changes (the OTLP and metrics-auth vars are unchanged), so `DefaultConfig()`
  is untouched.

## 9. Acceptance (from #313, with reconciliation)

- [ ] OTel Prometheus exporter wired as a second reader; `/metrics` serves OTel metrics.
- [ ] `oidc.Metrics` implemented against OTel instruments (`NewOTelMetrics`).
- [ ] All **9** interface metrics emitted with correct labels (8 D22 + reconciled
      `oidc_broadcast_drop_total{reason}`).
- [ ] `oidc_registry_providers` has no tenant label.
- [ ] `NopMetrics` remains for tests.
- [ ] tx/dispatch cleanups (a)+(b) landed with tests.
- [ ] Docs updated (README + help topic).
