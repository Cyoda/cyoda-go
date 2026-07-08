package e2e_test

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestReadCyodaStatus(t *testing.T) {
	op := &openapi3.Operation{
		OperationID: "foo",
		Extensions:  map[string]any{"x-cyoda-status": "planned"},
	}
	if got := readCyodaStatus(op); got != "planned" {
		t.Errorf("got %q, want planned", got)
	}
	bare := &openapi3.Operation{OperationID: "bar"}
	if got := readCyodaStatus(bare); got != "" {
		t.Errorf("unmarked op should return empty, got %q", got)
	}
}
