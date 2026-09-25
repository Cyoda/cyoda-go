package scheduler

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetrics_EveryInstrumentWithItsAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := newMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	m.runStarted()
	m.runEnded(outcomeSelfCancelled, 2*time.Second)
	m.claimed(claimOwnerLost)
	m.heartbeatFailed()
	m.bookkeepingRetried()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			found[md.Name] = md.Data
		}
	}

	sumOf := func(name string, want int64, key, value string) {
		t.Helper()
		sum, ok := found[name].(metricdata.Sum[int64])
		if !ok || len(sum.DataPoints) != 1 {
			t.Fatalf("%s: got %T with %v", name, found[name], found[name])
		}
		dp := sum.DataPoints[0]
		if dp.Value != want {
			t.Errorf("%s = %d, want %d", name, dp.Value, want)
		}
		wantAttrs := attribute.NewSet()
		if key != "" {
			wantAttrs = attribute.NewSet(attribute.String(key, value))
		}
		if !dp.Attributes.Equals(&wantAttrs) {
			t.Errorf("%s attributes = %v, want %v", name, dp.Attributes.ToSlice(), wantAttrs.ToSlice())
		}
	}
	sumOf("cyoda.scheduler.runs", 1, "outcome", "self_cancelled")
	sumOf("cyoda.scheduler.runs.in_progress", 0, "", "")
	sumOf("cyoda.scheduler.claims", 1, "reason", "owner_lost")
	sumOf("cyoda.scheduler.heartbeat.failures", 1, "", "")
	sumOf("cyoda.scheduler.bookkeeping.retries", 1, "", "")

	hist, ok := found["cyoda.scheduler.run.duration"].(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("cyoda.scheduler.run.duration: got %T", found["cyoda.scheduler.run.duration"])
	}
	if hp := hist.DataPoints[0]; hp.Count != 1 || hp.Sum != 2 {
		t.Errorf("run.duration count=%d sum=%v, want 1 and 2 seconds", hp.Count, hp.Sum)
	}
}

func TestMetrics_NilMeterIsNoop(t *testing.T) {
	m, err := newMetrics(nil)
	if err != nil {
		t.Fatalf("newMetrics(nil): %v", err)
	}
	m.runStarted()
	m.runEnded(outcomeFailed, time.Millisecond)
}
