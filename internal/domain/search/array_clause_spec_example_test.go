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
	if g.Operator != "AND" {
		t.Errorf("spec example's positions combine with %q, want AND", g.Operator)
	}
	for i, want := range []struct{ path, value string }{
		{"$.scores[0]", "50"},
		{"$.scores[2]", "70"},
	} {
		leaf, ok := g.Conditions[i].(*predicate.SimpleCondition)
		if !ok {
			t.Errorf("leaf %d is %T, want *predicate.SimpleCondition", i, g.Conditions[i])
			continue
		}
		n, ok := leaf.Value.(json.Number)
		if !ok {
			t.Errorf("leaf %d value is %T, want json.Number", i, leaf.Value)
			continue
		}
		if leaf.JsonPath != want.path || leaf.OperatorType != "GREATER_THAN" || n.String() != want.value {
			t.Errorf("leaf %d = %+v, want %s GREATER_THAN %s", i, leaf, want.path, want.value)
		}
	}
}
