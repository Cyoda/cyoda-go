package registry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// tagMetrics are the membership instruments an operator alerts on: sends that
// failed, and peers whose list this pnode is still waiting for. A lasting
// non-zero lists_outstanding means callouts are not being handed to a pnode
// that could take them.
type tagMetrics struct {
	sendFailures metric.Int64Counter
	registration metric.Registration
}

func newTagMetrics(meter metric.Meter, outstanding func() int64) (*tagMetrics, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	failures, err := meter.Int64Counter("cyoda.cluster.tags.send_failures",
		metric.WithDescription("Reliable tag-list messages to a peer that failed to send"))
	if err != nil {
		return nil, fmt.Errorf("instrument send_failures: %w", err)
	}
	gauge, err := meter.Int64ObservableGauge("cyoda.cluster.tags.lists_outstanding",
		metric.WithDescription("Alive peers whose announced tag list is not the one held"))
	if err != nil {
		return nil, fmt.Errorf("instrument lists_outstanding: %w", err)
	}
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(gauge, outstanding())
		return nil
	}, gauge)
	if err != nil {
		return nil, fmt.Errorf("register lists_outstanding callback: %w", err)
	}
	return &tagMetrics{sendFailures: failures, registration: reg}, nil
}

func (m *tagMetrics) sendFailed(kind string) {
	m.sendFailures.Add(context.Background(), 1, metric.WithAttributes(attribute.String("msg", kind)))
}

func (m *tagMetrics) close() {
	_ = m.registration.Unregister()
}
