package oidc

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// otelMetrics implements Metrics against OpenTelemetry instruments. Dotted
// instrument names render to the D22 Prometheus names via the exporter's
// UnderscoreEscapingWithSuffixes strategy (see observability.Init).
type otelMetrics struct {
	kidHit            metric.Int64Counter
	kidMiss           metric.Int64Counter
	kidEvict          metric.Int64Counter
	jwksFetchErr      metric.Int64Counter
	broadcastPanic    metric.Int64Counter
	broadcastDrop     metric.Int64Counter
	unknownProvider   metric.Int64Counter
	broadcastReceive  metric.Float64Histogram
	registryProviders metric.Int64Gauge

	// Pre-built bounded-enum label options (cold paths; values are closed sets).
	jwksOutcomeOpts map[string]metric.MeasurementOption
	dropReasonOpts  map[string]metric.MeasurementOption
}

// NewOTelMetrics builds an OIDC Metrics implementation on the given meter.
func NewOTelMetrics(meter metric.Meter) (Metrics, error) {
	m := &otelMetrics{}
	var err error
	if m.kidHit, err = meter.Int64Counter("oidc.kid.cache.hit"); err != nil {
		return nil, fmt.Errorf("oidc metrics: kid.cache.hit: %w", err)
	}
	if m.kidMiss, err = meter.Int64Counter("oidc.kid.cache.miss"); err != nil {
		return nil, fmt.Errorf("oidc metrics: kid.cache.miss: %w", err)
	}
	if m.kidEvict, err = meter.Int64Counter("oidc.kid.cache.evict"); err != nil {
		return nil, fmt.Errorf("oidc metrics: kid.cache.evict: %w", err)
	}
	if m.jwksFetchErr, err = meter.Int64Counter("oidc.jwks.fetch.error"); err != nil {
		return nil, fmt.Errorf("oidc metrics: jwks.fetch.error: %w", err)
	}
	if m.broadcastPanic, err = meter.Int64Counter("oidc.broadcast.panic"); err != nil {
		return nil, fmt.Errorf("oidc metrics: broadcast.panic: %w", err)
	}
	if m.broadcastDrop, err = meter.Int64Counter("oidc.broadcast.drop"); err != nil {
		return nil, fmt.Errorf("oidc metrics: broadcast.drop: %w", err)
	}
	if m.unknownProvider, err = meter.Int64Counter("oidc.unknown_provider.broadcast"); err != nil {
		return nil, fmt.Errorf("oidc metrics: unknown_provider.broadcast: %w", err)
	}
	if m.broadcastReceive, err = meter.Float64Histogram(
		"oidc.broadcast.receive",
		metric.WithUnit("s"),
		metric.WithDescription("OIDC broadcast receive-handling latency"),
		metric.WithExplicitBucketBoundaries(0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5),
	); err != nil {
		return nil, fmt.Errorf("oidc metrics: broadcast.receive: %w", err)
	}
	if m.registryProviders, err = meter.Int64Gauge("oidc.registry.providers"); err != nil {
		return nil, fmt.Errorf("oidc metrics: registry.providers: %w", err)
	}

	m.jwksOutcomeOpts = map[string]metric.MeasurementOption{
		"discovery":           metric.WithAttributes(attribute.String("outcome", "discovery")),
		"issuer_pin_mismatch": metric.WithAttributes(attribute.String("outcome", "issuer_pin_mismatch")),
	}
	m.dropReasonOpts = map[string]metric.MeasurementOption{
		"malformed_envelope": metric.WithAttributes(attribute.String("reason", "malformed_envelope")),
		"oversized_op":       metric.WithAttributes(attribute.String("reason", "oversized_op")),
		"oversized_tenantid": metric.WithAttributes(attribute.String("reason", "oversized_tenantid")),
		"oversized_uri":      metric.WithAttributes(attribute.String("reason", "oversized_uri")),
	}
	return m, nil
}

func (m *otelMetrics) IncKidCacheHit()   { m.kidHit.Add(context.Background(), 1) }
func (m *otelMetrics) IncKidCacheMiss()  { m.kidMiss.Add(context.Background(), 1) }
func (m *otelMetrics) IncKidCacheEvict() { m.kidEvict.Add(context.Background(), 1) }

func (m *otelMetrics) IncJWKSFetchError(outcome string) {
	opt, ok := m.jwksOutcomeOpts[outcome]
	if !ok {
		opt = metric.WithAttributes(attribute.String("outcome", outcome))
	}
	m.jwksFetchErr.Add(context.Background(), 1, opt)
}

func (m *otelMetrics) IncBroadcastPanic() { m.broadcastPanic.Add(context.Background(), 1) }

func (m *otelMetrics) IncBroadcastDrop(reason string) {
	opt, ok := m.dropReasonOpts[reason]
	if !ok {
		opt = metric.WithAttributes(attribute.String("reason", reason))
	}
	m.broadcastDrop.Add(context.Background(), 1, opt)
}

func (m *otelMetrics) IncUnknownProviderBroadcast() { m.unknownProvider.Add(context.Background(), 1) }

func (m *otelMetrics) ObserveBroadcastReceive(seconds float64) {
	m.broadcastReceive.Record(context.Background(), seconds)
}

func (m *otelMetrics) SetRegistryProviders(n int) {
	m.registryProviders.Record(context.Background(), int64(n))
}

var _ Metrics = (*otelMetrics)(nil)
