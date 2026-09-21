package observability_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// reportingDispatcher stands where the owner's loop stands: it fills the
// CalloutStats the decorator asked for on the context.
type reportingDispatcher struct {
	report contract.CalloutStats
}

func (d *reportingDispatcher) fill(ctx context.Context) {
	if stats := contract.CalloutStatsFrom(ctx); stats != nil {
		*stats = d.report
	}
}

func (d *reportingDispatcher) DispatchProcessor(ctx context.Context, entity *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	d.fill(ctx)
	return entity, nil
}

func (d *reportingDispatcher) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	d.fill(ctx)
	return true, "", nil
}

func (d *reportingDispatcher) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, _ string) (contract.FunctionResult, error) {
	d.fill(ctx)
	return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{}`)}, nil
}

// sumByAttrs collects an Int64 counter into "k=v,k=v" → value.
func sumByAttrs(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			for _, dp := range md.Data.(metricdata.Sum[int64]).DataPoints {
				out[dp.Attributes.Encoded(attribute.DefaultEncoder())] += dp.Value
			}
		}
	}
	return out
}

// hasMetric reports whether the collected metrics carry an instrument of that name.
func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == name {
				return true
			}
		}
	}
	return false
}

func TestTracingDispatch_CountsTriesByOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inner := &reportingDispatcher{report: contract.CalloutStats{
		Tries:     []string{"no_answer", "no_answer", contract.CalloutOutcomeOK},
		HandOvers: []string{contract.CalloutOutcomeUnreachable, contract.CalloutOutcomeOK},
		Waited:    1500 * time.Millisecond,
	}}
	traced := observability.NewTracingExternalProcessingService(inner, mp.Meter("test"))

	if _, err := traced.DispatchFunction(context.Background(), &spi.Entity{}, spi.ScheduleFunction{Name: "f"}, "wf", "tr", "tx"); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	tries := sumByAttrs(t, rm, "cyoda.callout.tries")
	if tries["outcome=no_answer,type=function"] != 2 || tries["outcome=ok,type=function"] != 1 || len(tries) != 2 {
		t.Errorf("cyoda.callout.tries = %v", tries)
	}
	var waits uint64
	var waitSum float64
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "cyoda.callout.wait.duration" {
				for _, dp := range md.Data.(metricdata.Histogram[float64]).DataPoints {
					waits += dp.Count
					waitSum += dp.Sum
				}
			}
		}
	}
	if waits != 1 || waitSum != 1.5 {
		t.Errorf("cyoda.callout.wait.duration count = %d sum = %v, want one callout that waited 1.5 s", waits, waitSum)
	}
}

// There is one hand-over counter, and it is the peer router's: it sees every
// hand-over, including those of a peer that was never asked. A second one here
// would double-count the same event under a second set of outcomes.
func TestTracingDispatch_DoesNotCountHandOversItself(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inner := &reportingDispatcher{report: contract.CalloutStats{
		Tries:     []string{contract.CalloutOutcomeOK},
		HandOvers: []string{contract.CalloutOutcomeUnreachable, contract.CalloutOutcomeOK},
	}}
	traced := observability.NewTracingExternalProcessingService(inner, mp.Meter("test"))

	if _, err := traced.DispatchFunction(context.Background(), &spi.Entity{}, spi.ScheduleFunction{Name: "f"}, "wf", "tr", "tx"); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if hasMetric(rm, "cyoda.callout.handovers") {
		t.Error("the tracing decorator registered cyoda.callout.handovers; the peer router already records it")
	}
}

// A callout that did not wait is not in the wait histogram, so its count is
// the number of callouts that waited.
func TestTracingDispatch_NoWaitNoWaitSample(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inner := &reportingDispatcher{report: contract.CalloutStats{Tries: []string{contract.CalloutOutcomeOK}}}
	traced := observability.NewTracingExternalProcessingService(inner, mp.Meter("test"))

	if _, err := traced.DispatchProcessor(context.Background(), &spi.Entity{}, spi.ProcessorDefinition{Name: "p"}, "wf", "tr", "tx"); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if hasMetric(rm, "cyoda.callout.wait.duration") {
		t.Error("a callout that did not wait was recorded in cyoda.callout.wait.duration")
	}
}

func TestTracingDispatch_SpanRecordsTriesAndHandOver(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	inner := &reportingDispatcher{report: contract.CalloutStats{
		Tries:     []string{"no_handoff", contract.CalloutOutcomeOK},
		HandOvers: []string{contract.CalloutOutcomeOK},
		Waited:    250 * time.Millisecond,
	}}
	traced := observability.NewTracingExternalProcessingService(inner, sdkmetric.NewMeterProvider().Meter("test"))
	if _, _, err := traced.DispatchCriteria(context.Background(), &spi.Entity{}, json.RawMessage(`{}`), "TRANSITION", "wf", "tr", "", "tx"); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	got := map[attribute.Key]attribute.Value{}
	for _, kv := range spans[0].Attributes {
		got[kv.Key] = kv.Value
	}
	if got[observability.AttrCalloutTries].AsInt64() != 2 {
		t.Errorf("%s = %v, want 2", observability.AttrCalloutTries, got[observability.AttrCalloutTries].AsInt64())
	}
	if !got[observability.AttrCalloutHandOver].AsBool() {
		t.Errorf("%s = false, want true", observability.AttrCalloutHandOver)
	}
	if got[observability.AttrCalloutWaitedMs].AsInt64() != 250 {
		t.Errorf("%s = %v, want 250", observability.AttrCalloutWaitedMs, got[observability.AttrCalloutWaitedMs].AsInt64())
	}
}
