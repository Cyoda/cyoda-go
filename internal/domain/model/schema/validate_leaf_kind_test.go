package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// A field declared as a scalar has a one-element kind set. Strict validation
// must refuse a value whose JSON kind is not in it — the same way a model
// declaring a container already refuses a scalar. Before this was enforced an
// array or an object went into a STRING field unchallenged, because the value
// was classified as STRING before the type check ran, and the stored value was
// then unaddressable by any predicate the declared type permits.
func TestValidate_LeafRejectsNonScalarKinds(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("s", NewLeafNode(String))

	cases := []struct {
		name    string
		data    string
		wantMsg string
	}{
		{"array into STRING", `{"s":["A"]}`, "s: expected scalar, got array"},
		{"object into STRING", `{"s":{"k":"v"}}`, "s: expected scalar, got object"},
		{"empty array into STRING", `{"s":[]}`, "s: expected scalar, got array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate(model, decodeJSON(t, tc.data))
			if len(errs) != 1 {
				t.Fatalf("got %d errors %v, want exactly 1", len(errs), errs)
			}
			if got := errs[0].Error(); got != tc.wantMsg {
				t.Errorf("message = %q, want %q", got, tc.wantMsg)
			}
			// A kind mismatch is not a DataType incompatibility: there is no
			// actual DataType to report, so it must not be dressed as one.
			if errs[0].Kind != ErrKindGeneric {
				t.Errorf("Kind = %v, want ErrKindGeneric", errs[0].Kind)
			}
		})
	}
}

// The element declaration of an array is a declaration like any other: an
// object element written into an array of STRING is the same defect one level
// down.
func TestValidate_ArrayElementLeafRejectsNonScalarKinds(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("a", NewArrayNode(NewLeafNode(String)))

	errs := Validate(model, decodeJSON(t, `{"a":[{"k":"v"}]}`))
	if len(errs) != 1 {
		t.Fatalf("got %d errors %v, want exactly 1", len(errs), errs)
	}
	if want := "a[0]: expected scalar, got object"; errs[0].Error() != want {
		t.Errorf("message = %q, want %q", errs[0].Error(), want)
	}
}

// Values whose kind IS the declared one stay accepted, null included: null is
// the absence of a value, not a kind mismatch.
func TestValidate_LeafAcceptsScalarsAndNull(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("s", NewLeafNode(String))

	for _, doc := range []string{`{"s":"x"}`, `{"s":null}`, `{}`} {
		if errs := Validate(model, decodeJSON(t, doc)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}
}

// A genuinely polymorphic slot — a field observed as BOTH a scalar and a
// container — declares both kinds, and both must stay admissible. Enforcing
// the declared kind set is not the same as banning kind unions.
func TestValidate_KindUnionAcceptsBothBranches(t *testing.T) {
	union := Merge(NewLeafNode(String), NewArrayNode(NewLeafNode(String)))
	model := NewObjectNode()
	model.SetChild("poly", union)

	for _, doc := range []string{`{"poly":"x"}`, `{"poly":["A","B"]}`} {
		if errs := Validate(model, decodeJSON(t, doc)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}
	// A kind outside the union is still refused.
	errs := Validate(model, decodeJSON(t, `{"poly":{"k":"v"}}`))
	if len(errs) != 1 {
		t.Fatalf("got %d errors %v, want exactly 1", len(errs), errs)
	}
}

// The container-vs-scalar direction was already enforced; it names the offending
// kind in the wire vocabulary rather than in Go's.
func TestValidate_ContainerMismatchNamesJSONKinds(t *testing.T) {
	model := NewObjectNode()
	model.SetChild("o", NewObjectNode())
	model.SetChild("a", NewArrayNode(NewLeafNode(String)))

	cases := []struct{ doc, want string }{
		{`{"o":["x"]}`, "o: expected object, got array"},
		{`{"o":"x"}`, "o: expected object, got string"},
		{`{"a":{"k":"v"}}`, "a: expected array, got object"},
		{`{"a":"x"}`, "a: expected array, got string"},
		{`{"a":1}`, "a: expected array, got number"},
		{`{"a":true}`, "a: expected array, got boolean"},
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

func decodeJSON(t *testing.T, doc string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(doc))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	return v
}
