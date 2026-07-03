package openapivalidator

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Guards the typed-but-open policy (ADR 0003): an additive/unknown member
// validates against an open schema and is rejected by additionalProperties:false.
func TestTypedButOpen_AdditionalPropertiesSemantics(t *testing.T) {
	body := map[string]any{"id": "x", "extra": "y"} // "extra" = additive/unknown

	open := openapi3.NewObjectSchema().WithProperty("id", openapi3.NewStringSchema())
	if err := open.VisitJSON(body); err != nil {
		t.Fatalf("open schema must ACCEPT an additive field: %v", err)
	}

	sealed := openapi3.NewObjectSchema().WithProperty("id", openapi3.NewStringSchema())
	sealed.AdditionalProperties = openapi3.AdditionalProperties{Has: openapi3.BoolPtr(false)}
	if err := sealed.VisitJSON(body); err == nil {
		t.Fatal("additionalProperties:false must REJECT an additive field")
	}
}
