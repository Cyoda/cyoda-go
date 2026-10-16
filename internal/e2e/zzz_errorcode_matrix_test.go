package e2e_test

import (
	"flag"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/errorcodematrix"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

func hasTriple(observed []openapivalidator.ErrorTriple, op string, c errorcodematrix.Cell) bool {
	for _, tr := range observed {
		if tr.Operation == op && tr.Status == c.Status && tr.ErrorCode == c.Code {
			return true
		}
	}
	return false
}

// producibleGaps returns "op status code" strings for every declared cell that
// was never observed (fictional / unexercised documented codes).
func producibleGaps(matrix map[string][]errorcodematrix.Cell, observed []openapivalidator.ErrorTriple) []string {
	var gaps []string
	for op, cells := range matrix {
		for _, c := range cells {
			if !hasTriple(observed, op, c) {
				gaps = append(gaps, fmt.Sprintf("%s %d %s", op, c.Status, c.Code))
			}
		}
	}
	sort.Strings(gaps)
	return gaps
}

// universalCrossCuttingCodes is the set of error codes that are not part of any
// endpoint's per-code documented contract — they arise from layers that sit in
// front of, or underneath, every operation — and are exempt from the declared
// check.
//
// CONFLICT (409) is a retryable SERIALIZABLE serialization abort: whichever
// concurrent writer loses the optimistic-lock race emits it, so it can appear
// on any write endpoint depending on timing and is not pin-able to a specific op.
//
// UNAUTHORIZED (401) is emitted by the auth middleware before the request ever
// reaches a handler, so it is producible on every authenticated route and
// belongs to the middleware's contract rather than any one endpoint's. Its
// coverage is pinned across representative routes by
// TestAuth_MissingOrInvalidCredentials_401 (auth_failures_test.go).
//
// SERVER_ERROR (500) is the sanitized fallback for an internal fault (a
// storage outage, a processor's context cancellation, any unclassified
// error) — per error-handling.md, 5xx is deliberately generic (message +
// correlation ticket, no domain detail) rather than a per-endpoint
// documented contract the way 4xx is. Any operation can suffer an infra
// fault, so — like CONFLICT and UNAUTHORIZED above — it is not pin-able to a
// specific op's error table. Its coverage is exercised by the dedicated
// storage-failure/fault-injection suites (lookup_storage_failure_e2e_test.go,
// entity_read_storage_failure_e2e_test.go, storage_ceilings_e2e_test.go,
// workflow_failure_test.go's TestWorkflowFailure_ProcessorContextCancelled).
var universalCrossCuttingCodes = map[string]bool{
	"CONFLICT":     true,
	"UNAUTHORIZED": true,
	"SERVER_ERROR": true,
}

// declaredGaps returns "op status code" strings for every observed error triple
// whose operation is IN the matrix but whose (status, code) is undocumented.
// Triples whose Code is in universalCrossCuttingCodes are exempt: they belong
// to the auth middleware or the concurrency layer, not to any one endpoint.
func declaredGaps(matrix map[string][]errorcodematrix.Cell, observed []openapivalidator.ErrorTriple) []string {
	var gaps []string
	for _, tr := range observed {
		if universalCrossCuttingCodes[tr.ErrorCode] {
			continue // cross-cutting concurrency code; not endpoint-specific
		}
		cells, inScope := matrix[tr.Operation]
		if !inScope {
			continue // out-of-scope op — exempt
		}
		found := false
		for _, c := range cells {
			if c.Status == tr.Status && c.Code == tr.ErrorCode {
				found = true
				break
			}
		}
		if !found {
			gaps = append(gaps, fmt.Sprintf("%s %d %s", tr.Operation, tr.Status, tr.ErrorCode))
		}
	}
	sort.Strings(gaps)
	return gaps
}

// TestErrCodeMatrix_GetOneEntity makes both seeded getOneEntity cells producible:
// 404 ENTITY_NOT_FOUND (unknown id) and 400 BAD_REQUEST (conflicting
// pointInTime+transactionId). This test is declared BEFORE TestZZZErrorCodeMatrix
// so that both triples are recorded before the suite-end matrix check runs (both
// are in the same zzz_ file, so declaration order determines execution order).
// ExpectErrorCode re-buffers the body, so it is called on the live resp (no readBody first).
func TestErrCodeMatrix_GetOneEntity(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	// 404 ENTITY_NOT_FOUND — random unknown id.
	nf := doAuth(t, http.MethodGet, "/api/entity/"+uuid.NewString(), "")
	if nf.StatusCode != http.StatusNotFound {
		t.Fatalf("getOneEntity unknown id: expected 404, got %d", nf.StatusCode)
	}
	commontest.ExpectErrorCode(t, nf, "ENTITY_NOT_FOUND")

	// 400 BAD_REQUEST — pointInTime and transactionId are mutually exclusive
	// (handler.go:434, common.ErrCodeBadRequest). The params check fires before
	// any entity existence check, so a random id is safe here.
	id := uuid.NewString()
	pit := "2035-01-01T12:00:00Z"
	tx := uuid.NewString()
	br := doAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s?pointInTime=%s&transactionId=%s", id, pit, tx), "")
	if br.StatusCode != http.StatusBadRequest {
		t.Fatalf("getOneEntity conflicting params: expected 400, got %d", br.StatusCode)
	}
	commontest.ExpectErrorCode(t, br, "BAD_REQUEST")
}

// TestZZZErrorCodeMatrix runs at suite end (zzz_ prefix orders it last, after
// all endpoint tests have recorded their error triples) and asserts the
// entity-scope error-code matrix is neither over- nor under-declared.
// Within this file, TestErrCodeMatrix_GetOneEntity is declared first so its
// triples are recorded before this check reads them.
func TestZZZErrorCodeMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires the full running-backend suite")
	}
	// Bail under -shuffle: the zzz_ ordering that guarantees all endpoint tests
	// have recorded their triples first does not hold when execution is
	// shuffled (same idiom as the sibling TestOpenAPIConformanceReport guard).
	if v := flag.Lookup("test.shuffle"); v != nil && v.Value.String() != "off" {
		t.Skip("error-code matrix depends on suite ordering; skipped under -shuffle")
	}
	observed := openapivalidator.ObservedErrorTriples()
	if gaps := producibleGaps(errorcodematrix.EntityErrorCodeMatrix, observed); len(gaps) > 0 {
		t.Errorf("documented error codes never produced by any E2E (fictional?): %v", gaps)
	}
	if gaps := declaredGaps(errorcodematrix.EntityErrorCodeMatrix, observed); len(gaps) > 0 {
		t.Errorf("error codes produced but undocumented in EntityErrorCodeMatrix (add the cell to internal/e2e/errorcodematrix and name the code in the operation's response description in api/openapi.yaml): %v", gaps)
	}
}
