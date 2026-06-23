package observability_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

func TestInit_ReturnsShutdownFunc(t *testing.T) {
	observability.ResetInit()
	shutdown, err := observability.Init(context.Background(), "test-service", "node-test", false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	// Shutdown should not error
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// IM-22: Double-init must not leak providers. Second call returns early.
func TestInit_DoubleInitReturnsEarly(t *testing.T) {
	observability.ResetInit()
	ctx := context.Background()
	shutdown1, err := observability.Init(ctx, "test-service-1", "node-1", false)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	defer shutdown1(ctx)

	shutdown2, err := observability.Init(ctx, "test-service-2", "node-2", false)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	// Second init should return a valid (non-nil) shutdown function
	if shutdown2 == nil {
		t.Fatal("expected non-nil shutdown from second Init call")
	}
	// Calling shutdown2 should not panic or error
	if err := shutdown2(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

func TestInit_TracerAndMeterAvailable(t *testing.T) {
	observability.ResetInit()
	shutdown, err := observability.Init(context.Background(), "test-service", "node-test", false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer shutdown(context.Background()) //nolint:errcheck

	tracer := observability.Tracer()
	if tracer == nil {
		t.Fatal("expected non-nil tracer")
	}

	meter := observability.Meter()
	if meter == nil {
		t.Fatal("expected non-nil meter")
	}
}

func TestInit_MetricsHandler_ServesAppMetricsWhenOTelDisabled(t *testing.T) {
	observability.ResetInit()
	shutdown, err := observability.Init(context.Background(), "test", "node", false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer shutdown(context.Background()) //nolint:errcheck

	ctr, err := observability.Meter().Int64Counter("test.always.on.counter")
	if err != nil {
		t.Fatalf("counter: %v", err)
	}
	ctr.Add(context.Background(), 1)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	observability.MetricsHandler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "test_always_on_counter_total") {
		t.Fatalf("expected app metric at /metrics with OTEL disabled, got:\n%s", rec.Body.String())
	}
}

func TestMetricsHandler_ResolvesLiveRegistryAcrossReset(t *testing.T) {
	observability.ResetInit()
	h := observability.MetricsHandler() // wired BEFORE Init
	shutdown, err := observability.Init(context.Background(), "test", "node", false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer shutdown(context.Background()) //nolint:errcheck

	ctr, _ := observability.Meter().Int64Counter("test.live.counter")
	ctr.Add(context.Background(), 1)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "test_live_counter_total") {
		t.Fatal("handler wired before Init did not resolve the live registry")
	}
}
