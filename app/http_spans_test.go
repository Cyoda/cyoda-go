package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/cyoda-platform/cyoda-go/app"
)

// With OpenTelemetry on, every HTTP request gets a server span — the
// hand-registered routes (admin, token, JWKS, grouped statistics) as much as
// the generated API routes.
func TestHTTPServerSpans_CoverHandRegisteredRoutes(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	cfg := app.DefaultConfig()
	cfg.ContextPath = ""
	cfg.OTelEnabled = true
	a := app.New(cfg)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/admin/log-level")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			return
		}
	}
	t.Fatalf("no server span for GET /admin/log-level; spans: %d", len(rec.Ended()))
}
