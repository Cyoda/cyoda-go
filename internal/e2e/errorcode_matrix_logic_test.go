package e2e_test

import (
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

func TestProducibleGaps(t *testing.T) {
	m := map[string][]codeCell{
		"getOneEntity": {{404, "ENTITY_NOT_FOUND"}, {400, "BAD_REQUEST"}},
	}
	observed := []openapivalidator.ErrorTriple{
		{Operation: "getOneEntity", Status: 404, ErrorCode: "ENTITY_NOT_FOUND"},
	}
	gaps := producibleGaps(m, observed)
	if len(gaps) != 1 { // 400/BAD_REQUEST declared but never observed
		t.Fatalf("expected 1 producible gap, got %d: %v", len(gaps), gaps)
	}
}

func TestDeclaredGaps(t *testing.T) {
	m := map[string][]codeCell{
		"getOneEntity": {{404, "ENTITY_NOT_FOUND"}},
	}
	observed := []openapivalidator.ErrorTriple{
		{Operation: "getOneEntity", Status: 404, ErrorCode: "ENTITY_NOT_FOUND"}, // declared → OK
		{Operation: "getOneEntity", Status: 400, ErrorCode: "BAD_REQUEST"},      // undeclared → gap
		{Operation: "searchEntities", Status: 400, ErrorCode: "WHATEVER"},       // op not in matrix → ignored
	}
	gaps := declaredGaps(m, observed)
	if len(gaps) != 1 {
		t.Fatalf("expected 1 declared gap, got %d: %v", len(gaps), gaps)
	}
}

// TestDeclaredGaps_ConflictExempt verifies that CONFLICT triples on in-scope
// operations do not produce a declared gap. CONFLICT is a retryable
// serialization abort that any write op may emit non-deterministically under
// concurrency, so it is exempt from the per-endpoint declared check.
func TestDeclaredGaps_ConflictExempt(t *testing.T) {
	m := map[string][]codeCell{
		"create": {{409, "UNIQUE_VIOLATION"}},
	}
	observed := []openapivalidator.ErrorTriple{
		{Operation: "create", Status: 409, ErrorCode: "UNIQUE_VIOLATION"}, // declared → OK
		{Operation: "create", Status: 409, ErrorCode: "CONFLICT"},         // universal concurrency code → exempt
	}
	gaps := declaredGaps(m, observed)
	if len(gaps) != 0 {
		t.Fatalf("expected 0 declared gaps (CONFLICT is exempt), got %d: %v", len(gaps), gaps)
	}
}
