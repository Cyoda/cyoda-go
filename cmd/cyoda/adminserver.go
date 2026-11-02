package main

import (
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/admin"
	"github.com/cyoda-platform/cyoda-go/internal/api/middleware"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// newAdminHandler assembles the admin surface (/livez, /readyz, /metrics)
// under the same panic recovery the API surface has: a panic is contained by
// ticket, logged with its stack, and answered as a sanitized 500.
//
// It passes no health latch, so a panic here does not latch the node
// unhealthy. Probes and scrapes do no engine or store work on the
// application's behalf — /livez writes a constant, /readyz reads two latches,
// /metrics gathers collectors — so a panic in one says nothing about whether
// this node's state is still correct, and taking a healthy node out of
// service over a broken probe would be the wrong trade. That is the same
// criterion the per-member gRPC goroutines follow.
func newAdminHandler(readiness func() error, metricsBearer string) http.Handler {
	h := admin.NewHandler(admin.Options{
		Readiness:          readiness,
		MetricsBearerToken: metricsBearer,
		MetricsHandler:     observability.MetricsHandler(),
	})
	return middleware.Recovery(nil)(h)
}
