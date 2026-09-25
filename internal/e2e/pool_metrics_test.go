package e2e_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/observability"
)

// Pool statistics are exported on the metrics endpoint with the rendered
// Prometheus names, labelled backend="postgres"; the connections gauge also
// carries the pool: main, scheduler or heartbeat.
func TestMetrics_PostgresPoolSeriesAreExported(t *testing.T) {
	srv := httptest.NewServer(observability.MetricsHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`cyoda_storage_pool_connections{backend="postgres",pool="main",state="acquired"}`,
		`cyoda_storage_pool_connections{backend="postgres",pool="main",state="idle"}`,
		`cyoda_storage_pool_connections{backend="postgres",pool="scheduler",state="idle"}`,
		`cyoda_storage_pool_connections{backend="postgres",pool="heartbeat",state="idle"}`,
		`cyoda_storage_pool_max_connections{backend="postgres"} 5`,
		`cyoda_storage_pool_acquires_total{backend="postgres"}`,
		`cyoda_storage_pool_empty_acquires_total{backend="postgres"}`,
		`cyoda_storage_pool_canceled_acquires_total{backend="postgres"}`,
		`cyoda_storage_pool_acquire_duration_seconds_total{backend="postgres"}`,
		`cyoda_storage_pool_empty_acquire_wait_seconds_total{backend="postgres"}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output lacks %q", want)
		}
	}
}
