package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type fakeDispatcher struct {
	processorCalled bool
	criteriaCalled  bool
	functionCalled  bool
	returnErr       error
}

func (f *fakeDispatcher) DispatchProcessor(
	ctx context.Context, entity *spi.Entity, processor spi.ProcessorDefinition,
	workflowName, transitionName, txID string,
) (*spi.Entity, error) {
	f.processorCalled = true
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return entity, nil
}

func (f *fakeDispatcher) DispatchCriteria(
	ctx context.Context, entity *spi.Entity, criterion json.RawMessage,
	target, workflowName, transitionName, processorName, txID string,
) (bool, string, error) {
	f.criteriaCalled = true
	if f.returnErr != nil {
		return false, "", f.returnErr
	}
	return true, "", nil
}

func (f *fakeDispatcher) DispatchFunction(
	ctx context.Context, entity *spi.Entity, fn spi.ScheduleFunction,
	workflowName, transitionName, txID string,
) (contract.FunctionResult, error) {
	f.functionCalled = true
	if f.returnErr != nil {
		return contract.FunctionResult{}, f.returnErr
	}
	return contract.FunctionResult{Kind: "Schedule", Value: json.RawMessage(`{}`)}, nil
}

func TestTracingExternalProcessingService_DispatchProcessor_DelegatesToInner(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeDispatcher{}
	traced := observability.NewTracingExternalProcessingService(inner, observability.Meter())

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-1"}}
	processor := spi.ProcessorDefinition{
		Name:          "myProcessor",
		ExecutionMode: "async",
		Config: spi.ProcessorConfig{
			CalculationNodesTags: "tag1",
		},
	}

	result, err := traced.DispatchProcessor(context.Background(), entity, processor, "wf1", "t1", "tx-1")
	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
	if result != entity {
		t.Error("expected inner entity to be returned")
	}
	if !inner.processorCalled {
		t.Error("inner.DispatchProcessor not called")
	}
}

func TestTracingExternalProcessingService_DispatchCriteria_DelegatesToInner(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeDispatcher{}
	traced := observability.NewTracingExternalProcessingService(inner, observability.Meter())

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-2"}}
	criterion := json.RawMessage(`{"op":"eq","field":"status","value":"active"}`)

	matches, _, err := traced.DispatchCriteria(context.Background(), entity, criterion, "target1", "wf1", "t1", "proc1", "tx-1")
	if err != nil {
		t.Fatalf("DispatchCriteria: %v", err)
	}
	if !matches {
		t.Error("expected matches=true from inner")
	}
	if !inner.criteriaCalled {
		t.Error("inner.DispatchCriteria not called")
	}
}

func TestTracingExternalProcessingService_DispatchProcessor_PropagatesError(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	wantErr := errors.New("dispatch failed")
	inner := &fakeDispatcher{returnErr: wantErr}
	traced := observability.NewTracingExternalProcessingService(inner, observability.Meter())

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-3"}}
	processor := spi.ProcessorDefinition{Name: "failProc"}

	_, err := traced.DispatchProcessor(context.Background(), entity, processor, "wf1", "t1", "tx-1")
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestTracingExternalProcessingService_DispatchCriteria_PropagatesError(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	wantErr := errors.New("criteria failed")
	inner := &fakeDispatcher{returnErr: wantErr}
	traced := observability.NewTracingExternalProcessingService(inner, observability.Meter())

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-4"}}
	criterion := json.RawMessage(`{}`)

	_, _, err := traced.DispatchCriteria(context.Background(), entity, criterion, "target1", "wf1", "t1", "proc1", "tx-1")
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

// singleDispatchTypeAttr drives exactly one dispatch (via dispatch) through a fresh
// ManualReader and returns the sole "type" attribute value recorded on
// cyoda.dispatch.count. Isolating each call to its own reader lets the caller
// correlate a specific dispatch method to the exact kind label it emitted — a
// swapped label (DispatchProcessor emitting "criteria", etc.) is then caught,
// which an aggregate set-equality assertion across both calls would miss.
func singleDispatchTypeAttr(t *testing.T, dispatch func(traced *observability.TracingExternalProcessingService)) string {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	traced := observability.NewTracingExternalProcessingService(&fakeDispatcher{}, mp.Meter("test"))

	dispatch(traced)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "cyoda.dispatch.count" {
				continue
			}
			sum := md.Data.(metricdata.Sum[int64])
			if len(sum.DataPoints) != 1 {
				t.Fatalf("expected exactly 1 cyoda.dispatch.count data point, got %d", len(sum.DataPoints))
			}
			v, ok := sum.DataPoints[0].Attributes.Value(observability.AttrDispatchType)
			if !ok {
				t.Fatalf("missing %q attribute on data point %+v", observability.AttrDispatchType, sum.DataPoints[0])
			}
			return v.AsString()
		}
	}
	t.Fatal("cyoda.dispatch.count not found")
	return ""
}

func TestTracingDispatch_RecordsKindLabeledAttributes(t *testing.T) {
	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-1"}}

	gotProcessor := singleDispatchTypeAttr(t, func(traced *observability.TracingExternalProcessingService) {
		_, _ = traced.DispatchProcessor(context.Background(), entity, spi.ProcessorDefinition{Name: "p"}, "wf", "tr", "tx")
	})
	if gotProcessor != "processor" {
		t.Errorf("DispatchProcessor recorded type=%q, want %q", gotProcessor, "processor")
	}

	gotCriteria := singleDispatchTypeAttr(t, func(traced *observability.TracingExternalProcessingService) {
		_, _, _ = traced.DispatchCriteria(context.Background(), entity, json.RawMessage(`{}`), "target1", "wf", "tr", "p", "tx")
	})
	if gotCriteria != "criteria" {
		t.Errorf("DispatchCriteria recorded type=%q, want %q", gotCriteria, "criteria")
	}
}

func TestTracingDispatch_DurationHasExplicitBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	traced := observability.NewTracingExternalProcessingService(&fakeDispatcher{}, mp.Meter("test"))

	entity := &spi.Entity{Meta: spi.EntityMeta{ID: "ent-1"}}
	_, _ = traced.DispatchProcessor(context.Background(), entity, spi.ProcessorDefinition{Name: "p"}, "wf", "tr", "tx")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "cyoda.dispatch.duration" {
				continue
			}
			h := md.Data.(metricdata.Histogram[float64])
			got := h.DataPoints[0].Bounds
			if len(got) != len(want) {
				t.Fatalf("bounds len=%d want %d (%v)", len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("bound[%d]=%v want %v", i, got[i], want[i])
				}
			}
			return
		}
	}
	t.Fatal("cyoda.dispatch.duration not found")
}
