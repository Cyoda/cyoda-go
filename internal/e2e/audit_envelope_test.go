package e2e_test

import (
	"net/http"
	"testing"
)

// TestAudit_Search_NotFound_ProblemDetail asserts that searchEntityAuditEvents
// emits application/problem+json (RFC-9457 ProblemDetail) with ENTITY_NOT_FOUND
// when the requested entity does not exist. This validates the real server
// behaviour that the converted spec now documents.
//
// entityId is a well-formed UUID so the request reaches the handler (not the
// binding layer); the entity simply does not exist in the store.
func TestAudit_Search_NotFound_ProblemDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}

	// Valid UUID for an entity that does not exist → handler emits 404 ENTITY_NOT_FOUND.
	resp := doAuth(t, http.MethodGet, "/api/audit/entity/00000000-0000-0000-0000-000000000099", "")
	// assertProblemJSON is defined in oidc_reconciliation_test.go (same package).
	assertProblemJSON(t, resp, http.StatusNotFound, "ENTITY_NOT_FOUND")
}
