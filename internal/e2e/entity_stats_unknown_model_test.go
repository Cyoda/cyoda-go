package e2e_test

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/google/uuid"
)

// TestGetStatisticsForModel_UnknownModel_404 asserts that GET /api/entity/stats/{model}/{version}
// returns 404 MODEL_NOT_FOUND when the model has never been registered.
func TestGetStatisticsForModel_UnknownModel_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuth(t, http.MethodGet, "/api/entity/stats/never-registered-"+uuid.NewString()+"/1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}

// TestGetStatisticsByStateForModel_UnknownModel_404 asserts that
// GET /api/entity/stats/states/{model}/{version} returns 404 MODEL_NOT_FOUND
// when the model has never been registered.
func TestGetStatisticsByStateForModel_UnknownModel_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	resp := doAuth(t, http.MethodGet, "/api/entity/stats/states/never-registered-"+uuid.NewString()+"/1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
