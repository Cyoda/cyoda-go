package errorcodematrix

import (
	"regexp"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/cyoda-platform/cyoda-go/api"
)

// TestEveryCellIsNamedInTheSpec checks that for every cell of
// EntityErrorCodeMatrix, the operation declares a response for that status
// and the response's description names the code. The matrix's other check,
// in internal/e2e, proves each cell is produced; this one proves each is
// documented.
func TestEveryCellIsNamedInTheSpec(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	ops := map[string]*openapi3.Operation{}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			ops[op.OperationID] = op
		}
	}
	for opID, cells := range EntityErrorCodeMatrix {
		op, ok := ops[opID]
		if !ok {
			t.Errorf("%s: no such operationId in api/openapi.yaml", opID)
			continue
		}
		for _, c := range cells {
			ref := op.Responses.Status(c.Status)
			if ref == nil || ref.Value == nil {
				t.Errorf("%s %d %s: the operation declares no %d response", opID, c.Status, c.Code, c.Status)
				continue
			}
			desc := ""
			if ref.Value.Description != nil {
				desc = *ref.Value.Description
			}
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(c.Code) + `\b`).MatchString(desc) {
				t.Errorf("%s %d %s: the %d response description does not name the code: %q",
					opID, c.Status, c.Code, c.Status, desc)
			}
		}
	}
}
