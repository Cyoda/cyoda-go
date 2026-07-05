package e2e_test

import (
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/google/uuid"
)

// TestSearchEntities_UnknownModel_404 asserts that POST /api/search/direct/{model}/{version}
// returns 404 MODEL_NOT_FOUND when the model has never been registered.
func TestSearchEntities_UnknownModel_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	model := "never-registered-" + uuid.NewString()
	resp := doAuth(t, http.MethodPost, "/api/search/direct/"+model+"/1",
		`{"type":"group","operator":"AND","conditions":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}

// TestSubmitAsyncSearchJob_UnknownModel_404 asserts that POST /api/search/async/{model}/{version}
// returns 404 MODEL_NOT_FOUND immediately at submit time when the model has never been registered.
func TestSubmitAsyncSearchJob_UnknownModel_404(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	model := "never-registered-" + uuid.NewString()
	resp := doAuth(t, http.MethodPost, "/api/search/async/"+model+"/1",
		`{"type":"group","operator":"AND","conditions":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	commontest.ExpectErrorCode(t, resp, "MODEL_NOT_FOUND")
}
