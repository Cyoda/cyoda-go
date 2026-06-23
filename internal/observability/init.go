package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/cyoda-platform/cyoda-go"

var (
	tp         *sdktrace.TracerProvider
	mp         *sdkmetric.MeterProvider
	initOnce   sync.Once
	initErr    error
	shutdownFn func(context.Context) error

	// activeRegistry holds the Prometheus registry served by MetricsHandler.
	// Always non-nil (seeded below) so the handler works before/around Init,
	// and an atomic.Pointer so ResetInit-vs-scrape is data-race-free.
	activeRegistry atomic.Pointer[prometheus.Registry]
)

func init() {
	activeRegistry.Store(prometheus.NewRegistry())
}

// Init initializes OpenTelemetry. The Prometheus scrape pipeline is ALWAYS set
// up (so /metrics carries app metrics with no collector); OTLP push + tracing
// are created only when otelEnabled. Guarded by sync.Once.
func Init(ctx context.Context, serviceName, nodeID string, otelEnabled bool) (func(context.Context) error, error) {
	initOnce.Do(func() {
		res, err := resource.New(ctx,
			resource.WithAttributes(
				semconv.ServiceName(serviceName),
				semconv.ServiceInstanceID(nodeID),
			),
		)
		if err != nil {
			initErr = fmt.Errorf("create OTel resource: %w", err)
			return
		}

		// --- Always-on scrape pipeline (best-effort; never fatal) ---
		var readerOpts []sdkmetric.Option
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		promExp, err := otelprom.New(
			otelprom.WithRegisterer(reg),
			otelprom.WithoutScopeInfo(),
			otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		)
		if err != nil {
			slog.Error("failed to create Prometheus metric exporter; app metrics disabled at /metrics",
				"pkg", "observability", "error", err)
		} else {
			readerOpts = append(readerOpts, sdkmetric.WithReader(promExp))
		}

		// --- Gated OTLP push + tracing (fatal on error, BEFORE provider publish) ---
		if otelEnabled {
			metricExporter, err := otlpmetrichttp.New(ctx)
			if err != nil {
				initErr = fmt.Errorf("create metric exporter: %w", err)
				return
			}
			readerOpts = append(readerOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)))

			traceExporter, err := otlptracehttp.New(ctx)
			if err != nil {
				initErr = fmt.Errorf("create trace exporter: %w", err)
				return
			}

			initialCfg := SamplerConfigFromEnv()
			if err := Sampler.SetSampler(initialCfg); err != nil {
				slog.Error("failed to set initial trace sampler, using default",
					"pkg", "observability", "error", err)
				_ = Sampler.SetSampler(SamplerConfig{Sampler: "always", ParentBased: true})
			}
			tp = sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(traceExporter),
				sdktrace.WithResource(res),
				sdktrace.WithSampler(Sampler),
			)
			otel.SetTracerProvider(tp)
			otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagation.Baggage{},
			))
		}

		// --- Meter provider: always built (0..2 readers); always non-nil ---
		mp = sdkmetric.NewMeterProvider(append(readerOpts, sdkmetric.WithResource(res))...)
		otel.SetMeterProvider(mp)
		activeRegistry.Store(reg)

		shutdownFn = func(ctx context.Context) error {
			if tp != nil {
				if err := tp.Shutdown(ctx); err != nil {
					slog.Warn("OTel trace provider shutdown error", "pkg", "observability", "err", err)
				}
			}
			if err := mp.Shutdown(ctx); err != nil {
				slog.Warn("OTel meter provider shutdown error", "pkg", "observability", "err", err)
			}
			return nil
		}
	})
	if initErr != nil {
		return nil, initErr
	}
	return shutdownFn, nil
}

// ResetInit resets the init guard so Init can be called again. Test-only.
func ResetInit() {
	initOnce = sync.Once{}
	initErr = nil
	shutdownFn = nil
	tp = nil
	mp = nil
	activeRegistry.Store(prometheus.NewRegistry())
}

// MetricsHandler serves the current Prometheus registry, resolved per request
// so a handler wired once tracks the active registry across ResetInit.
func MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg := activeRegistry.Load()
		promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}

// Tracer returns the application tracer.
func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

// Meter returns the application meter.
func Meter() metric.Meter {
	return otel.Meter(instrumentationName)
}
