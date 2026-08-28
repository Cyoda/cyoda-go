package schema

import "testing"

// objectOrArray builds the union Merge records for a field observed as both an
// object and an array: a KindObject node that kept its element.
func objectOrArray() *ModelNode {
	obj := NewObjectNode()
	obj.SetChild("k", NewLeafNode(String))
	return Merge(obj, NewArrayNode(NewLeafNode(Integer)))
}

// A field observed as both an object and an array declares both kinds, so a
// write may take either branch. Validation dispatched on the node's dominant
// Kind alone, which for this union is OBJECT — so the array branch, which the
// field walk and the export both name, was refused on write. The three surfaces
// have to agree: what is declared is what is admitted.
func TestValidate_ObjectOrArrayUnionAcceptsBothBranches(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("both", objectOrArray())

	for _, doc := range []string{`{"both":{"k":"v"}}`, `{"both":[1]}`, `{"both":[]}`} {
		if errs := Validate(model, decodeJSON(t, doc)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}

	// A kind outside the declared set is still refused, and the rejection names
	// every kind the field does declare.
	errs := Validate(model, decodeJSON(t, `{"both":"x"}`))
	if len(errs) != 1 {
		t.Fatalf("got %d errors %v, want exactly 1", len(errs), errs)
	}
	if want := "both: expected object or array, got string"; errs[0].Error() != want {
		t.Errorf("message = %q, want %q", errs[0].Error(), want)
	}
}

// Each branch is validated on its own terms once selected.
func TestValidate_ObjectOrArrayUnionValidatesTheSelectedBranch(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("both", objectOrArray())

	cases := []struct{ doc, want string }{
		{`{"both":["x"]}`, "both[0]: value of type STRING is not compatible with [INTEGER]"},
		{`{"both":{"k":1}}`, "both.k: value of type INTEGER is not compatible with [STRING]"},
		{`{"both":{"nope":"v"}}`, "both.nope: unexpected field not present in model"},
	}
	for _, tc := range cases {
		errs := Validate(model, decodeJSON(t, tc.doc))
		if len(errs) != 1 {
			t.Fatalf("Validate(%s): got %d errors %v, want exactly 1", tc.doc, len(errs), errs)
		}
		if errs[0].Error() != tc.want {
			t.Errorf("Validate(%s) message = %q, want %q", tc.doc, errs[0].Error(), tc.want)
		}
	}
}

// Persistence must not lose a branch. The model is stored as its wire form and
// read back on every write, so a branch the codec drops is a branch the model
// stops declaring — and the model then refuses a document it was derived from.
func TestCodec_RoundTripPreservesEveryBranch(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("both", objectOrArray())
	model.SetChild("poly", Merge(NewLeafNode(String), NewArrayNode(NewLeafNode(String))))

	blob, err := Marshal(model)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(blob)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, want := range []string{"$.both.k", "$.both[*]", "$.poly", "$.poly[*]"} {
		if _, ok := back.FieldsMap()[want]; !ok {
			t.Errorf("round-trip lost %s; fields = %v", want, back.FieldsMap())
		}
	}
	for _, doc := range []string{`{"both":{"k":"v"}}`, `{"both":[1]}`, `{"poly":"x"}`, `{"poly":["A"]}`} {
		if errs := Validate(back, decodeJSON(t, doc)); len(errs) != 0 {
			t.Errorf("after round-trip, Validate(%s) = %v, want no errors", doc, errs)
		}
	}
}

// Null is admitted where the model observed one, and refused where it did not —
// unchanged by the branch dispatch. A container that was never seen holding null
// does not silently start accepting it.
func TestValidate_NullFollowsTheDeclaration(t *testing.T) {
	model := NewObjectNode()
	pureObj := NewObjectNode()
	pureObj.SetChild("k", NewLeafNode(String))
	model.SetChild("o", pureObj)
	model.SetChild("a", NewArrayNode(NewLeafNode(String)))

	nullableObj := NewObjectNode()
	nullableObj.SetChild("k", NewLeafNode(String))
	nullableObj.Types().Add(Null)
	model.SetChild("no", nullableObj)

	model.SetChild("s", NewLeafNode(String))
	model.SetChild("poly", Merge(NewLeafNode(String), NewArrayNode(NewLeafNode(String))))

	accepted := []string{`{"s":null}`, `{"no":null}`, `{"poly":null}`, `{"a":[null]}`}
	for _, doc := range accepted {
		if errs := Validate(model, decodeJSON(t, doc)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}
	refused := map[string]string{
		`{"o":null}`: "o: expected object, got null",
		`{"a":null}`: "a: expected array, got null",
	}
	for doc, want := range refused {
		errs := Validate(model, decodeJSON(t, doc))
		if len(errs) != 1 {
			t.Fatalf("Validate(%s): got %d errors %v, want exactly 1", doc, len(errs), errs)
		}
		if errs[0].Error() != want {
			t.Errorf("Validate(%s) message = %q, want %q", doc, errs[0].Error(), want)
		}
	}
}

// An ARRAY node whose element was never observed — the empty-array seed the
// codec preserves — still declares "array" and nothing else.
func TestValidate_UnobservedElementArrayStillDeclaresArray(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("a", NewArrayNode(nil))

	if errs := Validate(model, decodeJSON(t, `{"a":["x"]}`)); len(errs) != 0 {
		t.Errorf("Validate({\"a\":[\"x\"]}) = %v, want no errors", errs)
	}
	errs := Validate(model, decodeJSON(t, `{"a":"x"}`))
	if len(errs) != 1 || errs[0].Error() != "a: expected array, got string" {
		t.Errorf("got %v, want a single \"a: expected array, got string\"", errs)
	}
}
