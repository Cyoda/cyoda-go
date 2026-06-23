// Package admin provides the admin HTTP listener for /livez, /readyz,
// and /metrics. The listener is a narrow probe-and-metrics surface —
// /livez and /readyz are unauthenticated by design (kubelet probes carry
// no bearer), while /metrics can optionally require a static Bearer
// token (see Options.MetricsBearerToken) for shared-cluster deployments
// where the listener is reachable by any pod.
//
// Bind address still controls the outer exposure: callers are
// responsible for choosing CYODA_ADMIN_BIND_ADDRESS. Defense in depth —
// auth on /metrics + NetworkPolicy + loopback bind — is the intended
// posture on the Helm target.
package admin

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Options struct {
	// Readiness returns nil when the instance is ready to serve, or a
	// non-nil error describing why it isn't. Called synchronously on
	// every /readyz probe — keep it cheap.
	Readiness func() error

	// MetricsBearerToken, when non-empty, gates /metrics behind a
	// static Bearer token (constant-time compare). /livez and /readyz
	// stay unauthenticated regardless — kubelet probes have no way to
	// present a bearer. Empty leaves /metrics unauthenticated (the
	// desktop/docker default, where the listener is loopback-only).
	MetricsBearerToken string

	// MetricsHandler, when non-nil, serves GET /metrics. When nil, falls back
	// to promhttp.Handler() (global default registry — runtime metrics only).
	// Production passes observability.MetricsHandler(); admin unit tests may
	// leave it nil.
	MetricsHandler http.Handler
}

func NewHandler(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := opts.Readiness(); err != nil {
			// Generic client message; full detail logged server-side (#68 item 14).
			// Readiness probe errors may surface connection details, secrets, or
			// stack traces from the underlying probe — never reflect them.
			slog.Warn("readiness probe failed", "err", err.Error())
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	var metricsHandler http.Handler = promhttp.Handler()
	if opts.MetricsHandler != nil {
		metricsHandler = opts.MetricsHandler
	}
	if opts.MetricsBearerToken != "" {
		metricsHandler = requireBearer(opts.MetricsBearerToken, metricsHandler)
	}
	mux.Handle("/metrics", metricsHandler)
	return mux
}
