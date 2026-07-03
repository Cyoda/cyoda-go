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
