package api

import (
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func operationByID(t *testing.T, doc *openapi3.T, id string) *openapi3.Operation {
	t.Helper()
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.OperationID == id {
				return op
			}
		}
	}
	t.Fatalf("operation %s not found", id)
	return nil
}

// TestConflictCells asserts the 409 cells of spec §8.1: each operation that
// can lose a task-row race declares a 409 problem response, and says it is
// retryable and what it races.
func TestConflictCells(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, id := range []string{"deleteSingleEntity"} {
		t.Run(id, func(t *testing.T) {
			ref := operationByID(t, doc, id).Responses.Status(409)
			if ref == nil || ref.Value == nil {
				t.Fatal("no 409 response declared")
			}
			if ref.Value.Content.Get("application/problem+json") == nil {
				t.Error("409 has no application/problem+json content")
			}
			desc := ""
			if ref.Value.Description != nil {
				desc = *ref.Value.Description
			}
			for _, want := range []string{"CONFLICT", "etryable", "scheduler"} {
				if !strings.Contains(desc, want) {
					t.Errorf("409 description %q does not mention %q", desc, want)
				}
			}
		})
	}
}
