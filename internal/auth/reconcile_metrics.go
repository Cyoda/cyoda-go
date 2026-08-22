package auth

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// ReconcileMetrics receives auth-cache reconcile health signals so
// operators can alert on staleness well before the fail-closed bound.
type ReconcileMetrics interface {
	SetReconcileConsecutiveFailures(n int)
	SetReconcileStalenessSeconds(sec float64)
}

// NopReconcileMetrics is the default no-op implementation.
type NopReconcileMetrics struct{}

func (NopReconcileMetrics) SetReconcileConsecutiveFailures(int)  {}
func (NopReconcileMetrics) SetReconcileStalenessSeconds(float64) {}

var _ ReconcileMetrics = NopReconcileMetrics{}

// otelReconcileMetrics implements ReconcileMetrics over OTel gauges.
type otelReconcileMetrics struct {
	consecutiveFailures metric.Int64Gauge
	stalenessSeconds    metric.Float64Gauge
}

// NewOTelReconcileMetrics builds the OTel-backed ReconcileMetrics for the
// trusted-key cache.
func NewOTelReconcileMetrics(meter metric.Meter) (ReconcileMetrics, error) {
	m := &otelReconcileMetrics{}
	var err error
	if m.consecutiveFailures, err = meter.Int64Gauge("auth.trustedkeys.reconcile_consecutive_failures"); err != nil {
		return nil, err
	}
	if m.stalenessSeconds, err = meter.Float64Gauge("auth.trustedkeys.reconcile_staleness_seconds"); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *otelReconcileMetrics) SetReconcileConsecutiveFailures(n int) {
	m.consecutiveFailures.Record(context.Background(), int64(n))
}

func (m *otelReconcileMetrics) SetReconcileStalenessSeconds(sec float64) {
	m.stalenessSeconds.Record(context.Background(), sec)
}

var _ ReconcileMetrics = (*otelReconcileMetrics)(nil)
