# OIDC subsystem metrics — primitive infrastructure (issue #313)

**Status:** design approved (rev. 2, post fresh-context review), pre-implementation
**Date:** 2026-06-23
**Refs:** #284 (OIDC providers), D22; follow-up #313

## 0. Revision note

rev. 2 incorporates two independent fresh-context design reviews (correctness +
proportionality). Material changes from rev. 1: enablement model decoupled from
`CYODA_OTEL_ENABLED` (§2.3 — the always-on scrape decision); exporter pinned to
`v0.65.0` (§2.4); `MetricsHandler()` resolves the live registry per-call (§3.1); tx/dispatch
constructors take an injected `metric.Meter` for testability (§5); cleanup (b) re-justified
on consistency, not performance (§5/§6); test plan reworked — the acceptance smoke test is
an admin-handler HTTP integration test, not the main e2e harness (§7); documentation
targets expanded to the help `telemetry` topic, `ARCHITECTURE.md`, `PRD.md` (§8).

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
   pushed via OTLP only — **and the entire pipeline is gated behind `CYODA_OTEL_ENABLED`,
   which defaults to `false`** (`main.go:104`, `config.go:198`). On a default deploy
   `Init` never runs, `Meter()` returns a no-op, and nothing is emitted.
2. **Prometheus scrape `/metrics`.** Three days after the initial import, the admin
   probe surface (`internal/admin/admin.go`) added `/metrics` via `promhttp.Handler()`
   on the **global default Prometheus registry**, *always on*. It was never bridged to the
   OTel meter provider, so it exposes only the Go runtime/process collectors that
   `client_golang` auto-registers. **No application metric appears at `/metrics` today.**
   The current `telemetry` help topic states this explicitly: "OTel metrics and Prometheus
   metrics are separate emission paths."

A `grep` confirms nothing registers application metrics into the Prometheus registry
(`promauto` / `MustRegister` absent outside the admin import). The scrape endpoint is an
empty-of-app-metrics surface sitting next to the no-op-by-default OTLP-push pipeline.

## 2. Decision & rationale (SOTA review)

**Use the OTel metrics API for instrumentation; add the OTel Prometheus exporter as a
second `MetricReader` so `/metrics` serves all OTel metrics; make the scrape pipeline
always-on, with OTLP push remaining gated behind `CYODA_OTEL_ENABLED`.**

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
- **Resolves the §1.1 split** — one wiring change makes OTel metrics scrapeable. The
  `cyoda.tx.*` / `cyoda.dispatch.*` metrics become visible at `/metrics` whenever their
  decorators are active (i.e. when `CYODA_OTEL_ENABLED=true` — see §2.3 asymmetry note).
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

No bespoke wrapper types. "Shared primitives" = OTel instruments created via an injected
`metric.Meter` sourced from `internal/observability.Meter()`, consistent with the
tx/dispatch precedent. A cyoda-owned `Counter`/`Gauge`/`Histogram` abstraction over the
OTel API would re-introduce the coupling OTel removes and diverge from the established
pattern.

### 2.3 Enablement model — always-on scrape, gated push (the B1 decision)

Today `CYODA_OTEL_ENABLED` (default `false`) gates the *entire* OTel pipeline, so a default
deploy emits nothing and `/metrics` is app-metric-blind — which would defeat this change's
premise. Resolution:

- **The metric-scrape pipeline is always on.** The `MeterProvider` + Prometheus exporter
  reader are always created, so `Meter()` always returns real instruments and `/metrics`
  always carries application metrics. The scrape path has **no external dependency**
  (pull-based; no collector required), so always-on is safe on desktop/docker/local.
- **OTLP push stays gated behind `CYODA_OTEL_ENABLED`.** When the flag is off, the OTLP
  metric `PeriodicReader` and the OTLP trace exporter are **not** created (no connection
  attempts to a nonexistent collector). When on, they are added as today, alongside the
  always-present Prometheus reader.
- **Metrics-init failure is log-and-continue, not fatal.** Metrics are not critical to
  serving requests; a failure to build the meter provider/exporter must not crash startup.
- **No new env var.** The scrape pipeline is unconditionally on; `/metrics` was already
  always served by the admin listener. `CYODA_OTEL_ENABLED` retains its meaning (OTLP
  push + tracing decorators).

**Asymmetry (accepted, documented).** The tracing decorators that create the `cyoda.tx.*` /
`cyoda.dispatch.*` instruments (`app.go:204,484`) remain gated behind `CYODA_OTEL_ENABLED`
— ungating them would impose always-on span-creation overhead, which is out of scope and
undesirable. Consequence:
- Default deploy (OTEL off): `/metrics` carries OIDC metrics + Go/process metrics.
- `CYODA_OTEL_ENABLED=true`: `/metrics` additionally carries tx/dispatch metrics, and
  everything also pushes via OTLP.

OIDC metrics are always-on because the OIDC registry is wired unconditionally
(`app.go:274`); they are the auth-critical, always-relevant set. tx/dispatch are deeper
diagnostics enabled with full observability. This is recorded in the help topic and
`ARCHITECTURE.md` (§8).

### 2.4 Dependency pin (the B3 decision)

Pin `go.opentelemetry.io/otel/exporters/prometheus v0.65.0` — it requires `otel v1.43.0`
and `client_golang v1.23.2`, both exact matches to the current `go.mod`. **Do not** let
`go get` pull `v0.66.0`, which requires `otel v1.44.0` and would drag the whole OTel stack
to a new minor across all four `go.mod` files — an unwanted bump during the v0.8.0 SPI pin
window. The exporter is added to the root module only.

## 3. Architecture

Single OTel `MeterProvider`, **one or two readers** depending on `CYODA_OTEL_ENABLED`:

```
                         ┌─ otelprom.Exporter (Reader)        (pull, ALWAYS)
MeterProvider ──────────-┤
                         └─ PeriodicReader → otlpmetrichttp   (push, only if OTEL enabled)

      otelprom.Exporter registers as prometheus.Collector
                  │
                  ▼
      dedicated *prometheus.Registry  (observability-owned)
                  │  + Go + process collectors
                  ▼
      observability.MetricsHandler() http.Handler  (resolves live registry per request)
                  │
                  ▼
      admin /metrics  (optional Bearer auth, unchanged)
```

### 3.1 Registry ownership & handler lifecycle

`internal/observability` owns a **dedicated `prometheus.Registry`** (OTel docs explicitly
recommend a custom registry over the global default to avoid global state):

- On metrics init, create `reg := prometheus.NewRegistry()`, register
  `collectors.NewGoCollector()` + `collectors.NewProcessCollector(...)` into it (preserving
  the runtime/process metrics the default registry gave us), and construct the exporter
  with `otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo())`. (Scope
  info is suppressed — single instrumentation scope, so `otel_scope_*` labels add only
  noise; `target_info` retained. Reversible one-liner if multiple scopes are added later.)
- Expose `observability.MetricsHandler() http.Handler`. **The handler resolves the *current*
  registry at request time** (closure over a package-level accessor / sync-guarded var),
  not a registry captured at wiring time. This is required so that after `ResetInit` builds
  a fresh registry, a previously-wired handler scrapes the live one, not a dead one.
  Before metrics init has run, `MetricsHandler()` serves an empty (but valid) registry.
- `ResetInit` (test-only) discards the registry/handler state so a fresh registry is built
  on re-init — no `AlreadyRegisteredError` (the lifecycle hazard that rules out the global
  default registry). `ResetInit` must nil the new registry package var in addition to the
  existing `tp`/`mp`.

### 3.2 Admin wiring

`admin.Options` gains an optional `MetricsHandler http.Handler`. When nil, `NewHandler`
falls back to `promhttp.Handler()` (current behavior preserved for existing admin unit
tests, which construct `Options` without a metrics handler). `cmd/cyoda/run.go` / `app`
pass `observability.MetricsHandler()` so production `/metrics` carries app metrics. The
Bearer-auth wrapping (`requireBearer`) is unchanged and wraps whichever handler is in
effect.

**Known caveat (accepted):** the nil-fallback exposes the *global default* registry
(Go/process only), while production exposes the *dedicated* registry (app metrics). If
production wiring ever omits `MetricsHandler`, `/metrics` silently reverts to the empty-of-
app-metrics state. Mitigation: the §7 integration smoke test stands up admin with
`observability.MetricsHandler()` and asserts the OIDC names render, exercising the real
bridge. The field is kept optional (not required) to avoid churning the admin unit tests
for marginal gain; the regression vector is covered by test.

## 4. OIDC metrics implementation

New file `internal/auth/oidc/metrics_otel.go`: `otelMetrics` struct implementing the
existing `Metrics` interface, constructed via `NewOTelMetrics(meter metric.Meter)
(Metrics, error)`. Taking a `metric.Meter` (not `internal/observability` directly) keeps
`oidc` decoupled and unit-testable with an SDK `ManualReader`. `NopMetrics` stays for tests.

Instrument creation errors are returned (not swallowed); `app.go` fails fast on them
(names are constants, so this only fires on a programming error). Bounded-enum label
options (`outcome`, `reason`) are pre-built once at constructor time as cached
`metric.MeasurementOption` values, never per call (OTel hot-path allocation mitigation —
the OIDC kid-cache path is per-token-verify, so this one is genuinely hot-path relevant).

### 4.1 Metric mapping

Translation strategy: exporter default (`UnderscoreEscapingWithSuffixes`). Names authored
in dotted OTel form render to the exact D22 Prometheus names. (All nine renderings were
verified against `otlptranslator` as bundled with `exporters/prometheus v0.65.0`.)

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

- Gauge uses synchronous `Int64Gauge` (verified present in pinned otel/metric v1.43.0),
  mapping 1:1 to `SetRegistryProviders(n int)` — no observable callback.
- `oidc_registry_providers` carries **no tenant label** (D22 / rev.4 I3).
- Label cardinality is bounded: `outcome` and `reason` are closed enums (`reason` ∈
  {`malformed_envelope`, `oversized_op`, `oversized_tenantid`, `oversized_uri`} per
  `broadcast.go`). No tenant identifier or secret reaches any label (Gate 3 — verified:
  `broadcast.go` already logs only field lengths; `reason` comes from a fixed switch).

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
(fail fast). Because the scrape pipeline is always on (§2.3), `Meter()` returns a real
meter here regardless of `CYODA_OTEL_ENABLED`.

## 5. Adjacent cleanup (folded in — Gate 6)

Enabling the Prometheus reader exposes the existing `cyoda.tx.*` / `cyoda.dispatch.*`
instruments at `/metrics` (when OTEL is enabled), so their hygiene now matters. Audit:

- **No cardinality risk.** Metric-level labels are bounded enums only (`tx.op` ∈
  {begin, commit, rollback}; `dispatch.type` ∈ {processor, criteria}). High-cardinality
  attributes (processor name, tags, workflow/transition/tx IDs) live on **spans only**.
- **Naming/units render cleanly** (`cyoda_tx_duration_seconds`, `cyoda_dispatch_count_total`,
  etc. — verified). No renames needed.

Three same-package changes, each TDD'd:

- **Meter injection (enabler).** `NewTracingTransactionManager` and
  `NewTracingExternalProcessingService` currently call `observability.Meter()` internally,
  which makes their metric output untestable. Change both constructors to accept a
  `metric.Meter` parameter (call sites in `app.go:205,485` pass `observability.Meter()`).
  This mirrors the OIDC impl and is the prerequisite for unit-testing (a) and (b).
- **(a) Log instrument-creation errors.** Replace `x, _ := meter.Float64Histogram(...)`
  with logged error handling (no behavioral change beyond a diagnostic). Once these
  instruments are externally visible, a silently-dropped creation error is a silently-
  missing metric.
- **(b) Hoist per-call attribute allocation.** Both files build `metric.WithAttributes(...)`
  on every `Record`/`Add`. Hoist the bounded-enum options to constructor-time cached
  `metric.MeasurementOption` fields, matching the OIDC pattern. **Justification: codebase
  consistency (one pattern), not performance** — the tx/dispatch paths fire once per
  transaction / dispatch and are dominated by I/O, so the allocation saving is immaterial
  there. (Contrast §4, where the OIDC kid-cache path is genuinely hot.)

## 6. Performance & memory

- Counter/gauge adds are atomic ops keyed by attribute set. The OIDC kid-cache path is
  per-token-verify (hot); pre-building label options at constructor time (§4) removes the
  per-call `attribute.Set` allocation there. For tx/dispatch the same hoist (§5b) is a
  consistency change, not a measurable speedup.
- Memory ∝ distinct `(instrument × attribute-set)` series held in the SDK. OIDC label sets
  are tiny closed enums and the gauge is aggregate (no tenant label) → single-digit series,
  a few KB. Always-on scrape adds this fixed, negligible footprint even when unscraped.
- The Prometheus reader is pull-based: zero steady-state cost; series materialize only on
  `/metrics` scrape. No OTLP connection attempts when `CYODA_OTEL_ENABLED=false`.

## 7. Testing (TDD — RED first)

- **observability (unit):** metrics init registers the Prometheus reader + Go/process
  collectors; scraping `MetricsHandler()` yields a known test instrument's series.
  `ResetInit` + re-init does not panic / double-register, and `MetricsHandler()` obtained
  before re-init scrapes the *new* registry (live-resolution assertion).
- **oidc (unit):** `otelMetrics` against `sdkmetric.NewManualReader()` — each interface
  method produces the expected instrument + attribute set; `oidc_registry_providers` has no
  tenant attribute; `NewOTelMetrics` surfaces instrument-creation errors.
- **tx/dispatch (unit):** with an injected meter backed by a `ManualReader`, assert the
  emitted series carry the correct bounded-enum labels after the (b) hoist (label
  correctness — *not* an unobservable "built once" claim). Cleanup (a)'s error path is
  exercised by injecting a meter stub whose instrument constructor returns an error and
  asserting it is logged, not swallowed. (An optional `testing.AllocsPerRun` micro-check
  may document the hoist, but is not the primary assertion.)
- **Acceptance smoke (HTTP integration):** stand up `admin.NewHandler(admin.Options{
  MetricsHandler: observability.MetricsHandler()})` behind an `httptest.Server`, record via
  a real `oidc.NewOTelMetrics(observability.Meter())`, `GET /metrics`, and assert all **9**
  D22 metric names render with correct labels and that `oidc_registry_providers` has no
  tenant label. This exercises the real exporter translation + dedicated registry + admin
  bridge end-to-end. (It lives as a focused integration test — `internal/admin` or a
  dedicated metrics integration test — **not** the main `internal/e2e` harness, which
  mounts only the API router and has no `/metrics` surface. The main API is unchanged by
  this work, so Gate 2's E2E surface is unaffected.)
- **Gate 5:** `go test ./... -v` green; `go vet ./...`. **Race** (`make race`) once before
  PR.

## 8. Documentation (Gate 4)

`WorkflowConfigurationDto` import surface untouched → no schema-version work. No env-var
changes → `DefaultConfig()` untouched. The behavior change (always-on scrape; `/metrics`
now carries app metrics) must be reflected in:

- **`cmd/cyoda/help/content/telemetry.md`** (primary). Rewrite the DESCRIPTION: the meter
  provider + Prometheus scrape reader are always initialized; `/metrics` always exposes
  application metrics (OIDC always; tx/dispatch when `CYODA_OTEL_ENABLED=true`); OTLP push
  remains gated. Remove the now-false "no metrics are emitted" / "separate emission paths"
  language.
- **`cmd/cyoda/help/content/admin.md`.** Note `/metrics` carries application metrics (not
  just runtime), and the OIDC metric family is always present.
- **`docs/ARCHITECTURE.md`** §11 Observability (≈L1486–1502): document the dual-reader
  `MeterProvider`, the always-on Prometheus scrape path vs. gated OTLP push, the dedicated
  registry, and the OIDC metric set. Update the admin/metrics section (≈L1169–1176) and the
  env table note on `CYODA_OTEL_ENABLED` (≈L1316) to clarify it gates OTLP push + tracing,
  not the scrape pipeline.
- **`docs/PRD.md`** observability paragraph (≈L821): add that metrics are also exposed via a
  Prometheus scrape endpoint (always-on), including the OIDC subsystem set — currently it
  mentions only OTLP HTTP exporters.
- **`README.md`:** if the observability/config section enumerates what `/metrics` exposes,
  update it; otherwise no change (it currently only links the compose example).

`docs/FEATURES.md` has no observability section — no update needed.

## 9. Acceptance (from #313, with rev. 2 additions)

- [ ] OTel Prometheus exporter (`v0.65.0`) wired as an always-on reader; `/metrics` serves
      OTel metrics regardless of `CYODA_OTEL_ENABLED`; OTLP push stays gated.
- [ ] `MeterProvider` always initialized (scrape); metrics-init failure is log-and-continue.
- [ ] `MetricsHandler()` resolves the live registry per-request (survives `ResetInit`).
- [ ] `oidc.Metrics` implemented against OTel instruments (`NewOTelMetrics`).
- [ ] All **9** interface metrics emitted with correct labels (8 D22 + reconciled
      `oidc_broadcast_drop_total{reason}`).
- [ ] `oidc_registry_providers` has no tenant label.
- [ ] `NopMetrics` remains for tests.
- [ ] tx/dispatch: meter injected; cleanups (a)+(b) landed with tests.
- [ ] Acceptance smoke test (admin `/metrics` HTTP integration) green.
- [ ] Docs updated: `telemetry.md`, `admin.md`, `ARCHITECTURE.md`, `PRD.md` (+ README if
      applicable).
