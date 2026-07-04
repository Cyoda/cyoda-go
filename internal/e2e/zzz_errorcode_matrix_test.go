package e2e_test

import (
	"flag"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// codeCell is one documented (status, errorCode) combination for an operation.
type codeCell struct {
	Status int
	Code   string
}

// EntityErrorCodeMatrix declares, per in-scope entity operationId, the
// (status, errorCode) combinations the spec's per-endpoint error tables
// promise (design §7). The suite-end checks assert bidirectional agreement
// with what the run actually produced. Out-of-scope operationIds are absent
// and therefore exempt — the marker-aware coverage gate governs their
// coverage. Rows are added by later tasks as each endpoint gains codes.
var EntityErrorCodeMatrix = map[string][]codeCell{
	// Seeded row: getOneEntity's error surface, pinned by
	// TestErrCodeMatrix_GetOneEntityNotFound below and existing lifecycle tests.
	"getOneEntity": {
		{Status: 404, Code: "ENTITY_NOT_FOUND"},
		{Status: 400, Code: "BAD_REQUEST"}, // conflicting pointInTime+transactionId
	},
	"deleteEntities": {
		{Status: 400, Code: "INVALID_CONDITION"},
		{Status: 404, Code: "MODEL_NOT_FOUND"},
	},
	// Entity write operations — full deterministic error surface (E5).
	// CONFLICT (409) is exempt from all rows: it is a retryable serialization
	// abort emitted non-deterministically by any write op under concurrency and
	// is therefore not a per-endpoint documented code (see universalConcurrencyCodes).
	"create": {
		{Status: 400, Code: "BAD_REQUEST"},       // invalid payload, transactionWindow out of range
		{Status: 400, Code: "INCOMPATIBLE_TYPE"},  // payload type mismatches the model
		{Status: 400, Code: "WORKFLOW_FAILED"},    // workflow processor rejected the entity
		{Status: 404, Code: "MODEL_NOT_FOUND"},    // model not registered
		{Status: 409, Code: "UNIQUE_VIOLATION"},   // TestUniqueKeys_CreateDuplicate et al.
		{Status: 422, Code: "INVALID_UNIQUE_KEY"}, // TestUniqueKeys_PartialKeyCreate, TestUniqueKeys_OverBoundNumeric
	},
	"createCollection": {
		{Status: 400, Code: "BAD_REQUEST"},      // invalid JSON array or parameter
		{Status: 404, Code: "MODEL_NOT_FOUND"},  // one or more models not registered
		{Status: 409, Code: "UNIQUE_VIOLATION"}, // TestUniqueKeys_CollectionIntraBatchDuplicate, TestUniqueKeys_MixedModelBatch
	},
	"updateSingleWithLoopback": {
		{Status: 409, Code: "UNIQUE_VIOLATION"}, // TestUniqueKeys_UpdateMovesKey
		// 409 CONFLICT is exempt (universalConcurrencyCodes)
	},
	"updateSingle": {
		{Status: 400, Code: "TRANSITION_NOT_FOUND"}, // named transition absent from the model
		{Status: 400, Code: "WORKFLOW_FAILED"},       // workflow processor rejected the update
		{Status: 404, Code: "ENTITY_NOT_FOUND"},      // entity UUID not found
		{Status: 409, Code: "UNIQUE_VIOLATION"},      // TestUniqueKeys_ProcessorRewrite_IfMatchUpdate_409
		{Status: 412, Code: "ENTITY_MODIFIED"},       // If-Match mismatch
	},
	"patchSingleWithLoopback": {
		{Status: 412, Code: "ENTITY_MODIFIED"},        // If-Match transactionId no longer matches
		{Status: 415, Code: "UNSUPPORTED_MEDIA_TYPE"}, // non-JSON format or unrecognised Content-Type
		{Status: 422, Code: "INVALID_UNIQUE_KEY"},     // TestUniqueKeys_PatchNullsKeyField
		{Status: 428, Code: "PRECONDITION_REQUIRED"},  // If-Match header absent
		{Status: 501, Code: "NOT_IMPLEMENTED"},        // application/json-patch+json not yet implemented
	},
}

func hasTriple(observed []openapivalidator.ErrorTriple, op string, c codeCell) bool {
	for _, tr := range observed {
		if tr.Operation == op && tr.Status == c.Status && tr.ErrorCode == c.Code {
			return true
		}
	}
	return false
}

// producibleGaps returns "op status code" strings for every declared cell that
// was never observed (fictional / unexercised documented codes).
func producibleGaps(matrix map[string][]codeCell, observed []openapivalidator.ErrorTriple) []string {
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

// universalConcurrencyCodes is the set of error codes that any write operation
// may emit non-deterministically under concurrency. They are retryable,
// cross-cutting serialization outcomes — not part of any endpoint's per-code
// documented contract — and are exempt from the declared check.
// CONFLICT (409) is a retryable SERIALIZABLE serialization abort: whichever
// concurrent writer loses the optimistic-lock race emits it, so it can appear
// on any write endpoint depending on timing and is not pin-able to a specific op.
var universalConcurrencyCodes = map[string]bool{
	"CONFLICT": true,
}

// declaredGaps returns "op status code" strings for every observed error triple
// whose operation is IN the matrix but whose (status, code) is undocumented.
// Triples whose Code is in universalConcurrencyCodes are exempt: they are
// cross-cutting, non-deterministic concurrency outcomes, not per-endpoint codes.
func declaredGaps(matrix map[string][]codeCell, observed []openapivalidator.ErrorTriple) []string {
	var gaps []string
	for _, tr := range observed {
		if universalConcurrencyCodes[tr.ErrorCode] {
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
	if gaps := producibleGaps(EntityErrorCodeMatrix, observed); len(gaps) > 0 {
		t.Errorf("documented error codes never produced by any E2E (fictional?): %v", gaps)
	}
	if gaps := declaredGaps(EntityErrorCodeMatrix, observed); len(gaps) > 0 {
		t.Errorf("error codes produced but undocumented in EntityErrorCodeMatrix (add the cell + its §7 table entry): %v", gaps)
	}
}
