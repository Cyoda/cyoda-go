package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_Livez_Returns200(t *testing.T) {
	h := NewHandler(Options{
		Readiness: func() error { return nil },
	})
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("livez: got %d, want 200", w.Code)
	}
}

func TestHandler_Readyz_Unready(t *testing.T) {
	h := NewHandler(Options{
		Readiness: func() error { return errors.New("not ready") },
	})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: got %d, want 503", w.Code)
	}
}

// TestHandler_Readyz_Unready_DoesNotLeakInternalDetails guards #68 item 14
// for the admin /readyz path. A readiness probe returning an error must not
// reflect that error's text into the HTTP body — readiness checks may surface
// connection details, secrets, or stack traces from the underlying probe.
func TestHandler_Readyz_Unready_DoesNotLeakInternalDetails(t *testing.T) {
	h := NewHandler(Options{
		Readiness: func() error {
			return errors.New("postgres dial tcp 10.0.0.5:5432: i/o timeout (secret-token-abc)")
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: got %d, want 503", w.Code)
	}
	body := w.Body.String()
	for _, leaked := range []string{"postgres", "10.0.0.5", "5432", "secret-token-abc"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response body leaked %q: %q", leaked, body)
		}
	}
}

func TestHandler_Readyz_Ready(t *testing.T) {
	h := NewHandler(Options{Readiness: func() error { return nil }})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("readyz: got %d, want 200", w.Code)
	}
}

func TestHandler_Metrics_ReturnsPrometheusFormat(t *testing.T) {
	h := NewHandler(Options{Readiness: func() error { return nil }})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: got %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") && !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Fatalf("metrics: Content-Type %q is not a Prometheus exposition format", ct)
	}
}

func TestHandler_Metrics_UsesInjectedHandler(t *testing.T) {
	sentinel := "injected_metrics_sentinel 1"
	h := NewHandler(Options{
		Readiness: func() error { return nil },
		MetricsHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(sentinel))
		}),
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), sentinel) {
		t.Fatalf("expected injected handler output, got %q", rec.Body.String())
	}
}

func TestHandler_Metrics_InjectedHandlerStillBearerGated(t *testing.T) {
	h := NewHandler(Options{
		Readiness:          func() error { return nil },
		MetricsBearerToken: "secret",
		MetricsHandler:     http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil) // no bearer
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", rec.Code)
	}
}
