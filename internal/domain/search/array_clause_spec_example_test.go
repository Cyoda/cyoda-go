package search_test

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"

	"github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// The spec's whole-clause example for the array clause must parse, validate
// and test the positions it appears to test.
func TestArrayConditionDtoExampleParsesAndValidates(t *testing.T) {
	doc, err := api.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	ex := doc.Components.Schemas["ArrayConditionDto"].Value.Example
	if ex == nil {
		t.Fatal("ArrayConditionDto has no whole-clause example")
	}
	body, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	cond, err := predicate.ParseCondition(body)
	if err != nil {
		t.Fatalf("spec example does not parse: %v (%s)", err, body)
	}
	if err := search.ValidateCondition(cond); err != nil {
		t.Fatalf("spec example does not validate: %v (%s)", err, body)
	}
	d, err := spi.DesugarCondition(cond)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := d.(*predicate.GroupCondition)
	if !ok || len(g.Conditions) != 2 {
		t.Fatalf("spec example must test two positions, got %#v", d)
	}
	for i, wantPath := range []string{"$.scores[0]", "$.scores[2]"} {
		leaf := g.Conditions[i].(*predicate.SimpleCondition)
		if leaf.JsonPath != wantPath || leaf.OperatorType != "GREATER_THAN" {
			t.Errorf("leaf %d = %+v, want %s GREATER_THAN", i, leaf, wantPath)
		}
	}
}
