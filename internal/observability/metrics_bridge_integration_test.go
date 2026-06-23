package observability_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/admin"
	"github.com/cyoda-platform/cyoda-go/internal/auth/oidc"
	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

func TestMetricsBridge_OIDCNamesAtAdminMetrics(t *testing.T) {
	observability.ResetInit()
	shutdown, err := observability.Init(context.Background(), "test", "node", false)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer shutdown(context.Background()) //nolint:errcheck

	m, err := oidc.NewOTelMetrics(observability.Meter())
	if err != nil {
		t.Fatalf("NewOTelMetrics: %v", err)
	}
	m.IncKidCacheHit()
	m.IncKidCacheMiss()
	m.IncKidCacheEvict()
	m.IncJWKSFetchError("discovery")
	m.IncBroadcastPanic()
	m.IncBroadcastDrop("malformed_envelope")
	m.IncUnknownProviderBroadcast()
	m.ObserveBroadcastReceive(0.003)
	m.SetRegistryProviders(4)

	h := admin.NewHandler(admin.Options{
		Readiness:      func() error { return nil },
		MetricsHandler: observability.MetricsHandler(),
	})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, name := range []string{
		"oidc_kid_cache_hit_total",
		"oidc_kid_cache_miss_total",
		"oidc_kid_cache_evict_total",
		"oidc_jwks_fetch_error_total",
		"oidc_broadcast_panic_total",
		"oidc_broadcast_drop_total",
		"oidc_unknown_provider_broadcast_total",
		"oidc_broadcast_receive_seconds",
		"oidc_registry_providers",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("missing %q in /metrics output:\n%s", name, body)
		}
	}
	if !strings.Contains(body, `oidc_jwks_fetch_error_total{outcome="discovery"}`) {
		t.Errorf("expected outcome label on jwks fetch error")
	}
	if !strings.Contains(body, `oidc_broadcast_drop_total{reason="malformed_envelope"}`) {
		t.Errorf("expected reason label on broadcast drop")
	}
	// no-tenant / no-label invariant on the gauge
	if strings.Contains(body, "oidc_registry_providers{") {
		t.Errorf("oidc_registry_providers must have no labels:\n%s", body)
	}
}
