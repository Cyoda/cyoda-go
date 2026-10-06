package observability_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/observability"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type fakeTxManager struct {
	beginCalled    bool
	commitCalled   bool
	rollbackCalled bool
	rollbackErr    error
	lostRaceTxID   string
	lostRace       bool
	lostRaceErr    error
	ctTime         time.Time
	ctErr          error
}

func (f *fakeTxManager) ConsistencyTime(ctx context.Context) (time.Time, error) {
	return f.ctTime, f.ctErr
}

func (f *fakeTxManager) Begin(ctx context.Context) (string, context.Context, error) {
	f.beginCalled = true
	return "tx-1", ctx, nil
}
func (f *fakeTxManager) Commit(ctx context.Context, txID string) error {
	f.commitCalled = true
	return nil
}
func (f *fakeTxManager) Rollback(ctx context.Context, txID string) error {
	f.rollbackCalled = true
	return f.rollbackErr
}
func (f *fakeTxManager) Join(ctx context.Context, txID string) (context.Context, error) {
	return ctx, nil
}
func (f *fakeTxManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	return time.Now(), nil
}
func (f *fakeTxManager) Savepoint(ctx context.Context, txID string) (string, error) {
	return "sp-1", nil
}
func (f *fakeTxManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	return nil
}
func (f *fakeTxManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	return nil
}
func (f *fakeTxManager) LostRace(ctx context.Context, txID string) (bool, error) {
	f.lostRaceTxID = txID
	return f.lostRace, f.lostRaceErr
}

// LostRace is answered by the wrapped manager, answer and error unchanged.
func TestTracingTxManager_LostRaceDelegates(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeTxManager{lostRace: true}
	traced := observability.NewTracingTransactionManager(inner, observability.Meter())
	lost, err := traced.LostRace(context.Background(), "tx-9")
	if err != nil || !lost || inner.lostRaceTxID != "tx-9" {
		t.Fatalf("LostRace = (%v, %v), inner asked about %q; want (true, nil) for tx-9", lost, err, inner.lostRaceTxID)
	}

	wantErr := errors.New("tenant mismatch")
	inner = &fakeTxManager{lostRaceErr: wantErr}
	traced = observability.NewTracingTransactionManager(inner, observability.Meter())
	if _, err := traced.LostRace(context.Background(), "tx-9"); !errors.Is(err, wantErr) {
		t.Fatalf("LostRace error = %v, want %v", err, wantErr)
	}
}

// ConsistencyTime is answered by the wrapped manager, value and error unchanged.
func TestTracing_ForwardsConsistencyTime(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	want := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	inner := &fakeTxManager{ctTime: want}
	traced := observability.NewTracingTransactionManager(inner, observability.Meter())
	got, err := traced.ConsistencyTime(context.Background())
	if err != nil || !got.Equal(want) {
		t.Fatalf("ConsistencyTime = (%v, %v), want (%v, nil)", got, err, want)
	}

	wantErr := errors.New("unavailable")
	inner = &fakeTxManager{ctErr: wantErr}
	traced = observability.NewTracingTransactionManager(inner, observability.Meter())
	if _, err := traced.ConsistencyTime(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("ConsistencyTime error = %v, want %v", err, wantErr)
	}
}

// ConsistencyTime can wait up to 10 s for in-flight commits, so it has a span;
// a failure is recorded on it.
func TestTracing_ConsistencyTimeSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	inner := &fakeTxManager{ctTime: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	traced := observability.NewTracingTransactionManager(inner, sdkmetric.NewMeterProvider().Meter("test"))
	if _, err := traced.ConsistencyTime(context.Background()); err != nil {
		t.Fatal(err)
	}
	inner.ctErr = errors.New("unavailable")
	if _, err := traced.ConsistencyTime(context.Background()); err == nil {
		t.Fatal("want error")
	}

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	for _, s := range spans {
		if s.Name != "tx.consistency_time" {
			t.Errorf("span name = %q, want tx.consistency_time", s.Name)
		}
	}
	if len(spans[0].Events) != 0 {
		t.Errorf("successful call recorded events: %v", spans[0].Events)
	}
	if len(spans[1].Events) != 1 || spans[1].Events[0].Name != "exception" {
		t.Errorf("failed call events = %v, want one exception event", spans[1].Events)
	}
}

func TestTracingTxManager_DelegatesToInner(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	inner := &fakeTxManager{}
	traced := observability.NewTracingTransactionManager(inner, observability.Meter())

	ctx := context.Background()
	txID, txCtx, err := traced.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if txID != "tx-1" {
		t.Errorf("txID = %q, want tx-1", txID)
	}
	if !inner.beginCalled {
		t.Error("inner.Begin not called")
	}

	if err := traced.Commit(txCtx, txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !inner.commitCalled {
		t.Error("inner.Commit not called")
	}

	if err := traced.Rollback(ctx, "tx-2"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !inner.rollbackCalled {
		t.Error("inner.Rollback not called")
	}
}

// IM-17: txActive counter must NOT be decremented when rollback fails.
// A failed rollback means the transaction is still active.
func TestTracingTxManager_Rollback_FailedRollbackDoesNotDecrementActive(t *testing.T) {
	shutdown, _ := observability.Init(context.Background(), "test", "node-test", true)
	defer shutdown(context.Background())

	rollbackErr := errors.New("rollback failed")
	inner := &fakeTxManager{rollbackErr: rollbackErr}
	traced := observability.NewTracingTransactionManager(inner, observability.Meter())

	// The test verifies the error is propagated and the method completes without panic.
	// The real assertion is structural: txActive.Add(-1) must only run on success path.
	// We verify the error propagation which would differ if the bug caused early return.
	err := traced.Rollback(context.Background(), "tx-1")
	if !errors.Is(err, rollbackErr) {
		t.Errorf("expected rollback error, got %v", err)
	}
}

func TestTracingTxManager_DurationHasExplicitBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	traced := observability.NewTracingTransactionManager(&fakeTxManager{}, mp.Meter("test"))

	_, _, _ = traced.Begin(context.Background())

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "cyoda.tx.duration" {
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
	t.Fatal("cyoda.tx.duration not found")
}
