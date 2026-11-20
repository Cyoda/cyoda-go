package schema_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"

	"github.com/cyoda/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

// num builds a json.Number the way the HTTP decoder does.
func num(s string) any { return json.Number(s) }

// TestAdmit_Leaf is the spec's case table. "Held" means Admit returns no
// change: the field holds the value and the model does not move.
func TestAdmit_Leaf(t *testing.T) {
	cases := []struct {
		name     string
		declared []schema.DataType
		value    any
		held     bool
	}{
		{"double holds 1000", []schema.DataType{schema.Double}, num("1000"), true},
		{"double holds 1000.0", []schema.DataType{schema.Double}, num("1000.0"), true},
		{"double holds 1e3", []schema.DataType{schema.Double}, num("1e3"), true},
		{"double holds a whole past 2^31", []schema.DataType{schema.Double}, num("2147483648"), true},
		{"double holds the ceiling", []schema.DataType{schema.Double}, num("9.99999999999999e292"), true},
		{"double refuses above the ceiling", []schema.DataType{schema.Double}, num("9.99999999999999e300"), false},
		{"double refuses 16 significant digits", []schema.DataType{schema.Double}, num("9007199254740993"), false},
		{"integer holds its ceiling", []schema.DataType{schema.Integer}, num("2147483647"), true},
		{"integer refuses past its ceiling", []schema.DataType{schema.Integer}, num("2147483648"), false},
		{"integer refuses a fraction", []schema.DataType{schema.Integer}, num("13.111"), false},
		{"unbound integer refuses a fraction", []schema.DataType{schema.UnboundInteger}, num("1.5"), false},
		{"big decimal holds a high scale", []schema.DataType{schema.BigDecimal},
			num("1.23456789012345678901234567890"), true},

		{"string holds a date-shaped string", []schema.DataType{schema.String}, "2026-03-01", true},
		{"string holds hello", []schema.DataType{schema.String}, "hello", true},
		{"string refuses a number", []schema.DataType{schema.String}, num("5"), false},
		{"string refuses a boolean", []schema.DataType{schema.String}, true, false},
		{"boolean refuses a string", []schema.DataType{schema.Boolean}, "true", false},
		{"integer refuses a string", []schema.DataType{schema.Integer}, "2024", false},

		{"zoned holds a timestamp", []schema.DataType{schema.ZonedDateTime}, "2026-03-01T10:00:00Z", true},
		{"zoned refuses a year", []schema.DataType{schema.ZonedDateTime}, "2026", false},
		{"local date time refuses an offset", []schema.DataType{schema.LocalDateTime},
			"2026-03-01T10:00:00+05:00", false},

		{"a scalar leaf admits null", []schema.DataType{schema.String}, nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf := schema.NewLeafNode(tc.declared[0])
			for _, dt := range tc.declared[1:] {
				leaf.AddScalarTypes(dt)
			}
			overlay, changes, err := schema.Admit(leaf, tc.value)
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			held := len(changes) == 0
			if held != tc.held {
				t.Errorf("held = %v, want %v (changes: %+v)", held, tc.held, changes)
			}
			if held && overlay != nil {
				t.Errorf("a held value must produce no overlay, got %v", overlay.DeclaredTypes())
			}
			if !held && overlay == nil {
				t.Error("a change must produce an overlay describing it")
			}
		})
	}
}

// A held value must leave the declared set byte-identical.
func TestAdmit_HeldValueDoesNotMoveTheModel(t *testing.T) {
	leaf := schema.NewLeafNode(schema.Double)
	before := append([]schema.DataType(nil), leaf.DeclaredTypes()...)

	if _, changes, err := schema.Admit(leaf, num("2147483648")); err != nil || len(changes) != 0 {
		t.Fatalf("expected the value to be held, got changes=%+v err=%v", changes, err)
	}

	after := leaf.DeclaredTypes()
	if len(before) != len(after) {
		t.Fatalf("declared types changed: %v -> %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("declared types changed: %v -> %v", before, after)
		}
	}
}

// A change records the level it costs and the context the error needs.
func TestAdmit_LeafChangeCarriesLevelAndProps(t *testing.T) {
	leaf := schema.NewLeafNode(schema.Integer)
	_, changes, err := schema.Admit(leaf, num("2147483648"))
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	c := changes[0]
	if c.Reason != schema.ReasonLeafType {
		t.Errorf("Reason = %v, want ReasonLeafType", c.Reason)
	}
	if c.Required != spi.ChangeLevelType {
		t.Errorf("Required = %v, want TYPE", c.Required)
	}
	if c.Observed != schema.Long {
		t.Errorf("Observed = %v, want LONG", c.Observed)
	}
	if len(c.Declared) != 1 || c.Declared[0] != schema.Integer {
		t.Errorf("Declared = %v, want [INTEGER]", c.Declared)
	}
}

// A leaf never admits a container, whatever its declared types.
func TestAdmit_LeafRefusesAContainer(t *testing.T) {
	leaf := schema.NewLeafNode(schema.String)
	for _, v := range []any{map[string]any{"k": "v"}, []any{"a"}} {
		_, changes, err := schema.Admit(leaf, v)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) == 0 {
			t.Errorf("a STRING leaf must not admit %T", v)
		}
		if changes[0].Reason != schema.ReasonNewKind {
			t.Errorf("Reason = %v, want ReasonNewKind", changes[0].Reason)
		}
	}
}

// TestAdmit_NullablePromotion pins the nullable-marker promotion: a node
// with no scalar declaration records ReasonNullable at scalarLevel and
// overlays a bare nullable marker; a node that already declares a scalar
// admits null silently.
func TestAdmit_NullablePromotion(t *testing.T) {
	t.Run("promotes a node with no scalar declaration", func(t *testing.T) {
		model := schema.NewObjectNode()
		overlay, changes, err := schema.Admit(model, nil)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
		}
		c := changes[0]
		if c.Reason != schema.ReasonNullable {
			t.Errorf("Reason = %v, want ReasonNullable", c.Reason)
		}
		if c.Required != spi.ChangeLevelType {
			t.Errorf("Required = %v, want TYPE (the default scalarLevel)", c.Required)
		}
		if c.Observed != schema.Null {
			t.Errorf("Observed = %v, want NULL", c.Observed)
		}
		if overlay == nil {
			t.Fatal("want an overlay describing the promotion")
		}
		if !overlay.Nullable() {
			t.Error("overlay must be nullable")
		}
		if len(overlay.Kinds()) != 0 {
			t.Errorf("overlay.Kinds() = %v, want none", overlay.Kinds())
		}
	})

	t.Run("a node that already declares a scalar admits null silently", func(t *testing.T) {
		leaf := schema.NewLeafNode(schema.String)
		overlay, changes, err := schema.Admit(leaf, nil)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 0 {
			t.Fatalf("want no changes, got %+v", changes)
		}
		if overlay != nil {
			t.Fatalf("want nil overlay, got %v", overlay.DeclaredTypes())
		}
	})
}

// TestAdmit_NewKindLevelSplit pins the STRUCTURAL-vs-scalarLevel split a new
// kind costs: adding a kind beside one already declared is STRUCTURAL: more
// fundamental than a new field. Establishing the first kind on a node that
// declares none is the nullable-marker promotion instead, at scalarLevel.
func TestAdmit_NewKindLevelSplit(t *testing.T) {
	t.Run("a declared scalar gaining a container kind costs STRUCTURAL", func(t *testing.T) {
		leaf := schema.NewLeafNode(schema.String)
		overlay, changes, err := schema.Admit(leaf, map[string]any{"k": "v"})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
		}
		c := changes[0]
		if c.Required != spi.ChangeLevelStructural {
			t.Errorf("Required = %v, want STRUCTURAL", c.Required)
		}
		if c.Reason != schema.ReasonNewKind {
			t.Errorf("Reason = %v, want ReasonNewKind", c.Reason)
		}
		if overlay == nil {
			t.Fatal("want an overlay")
		}
	})

	t.Run("a node with no declared kinds learning a scalar costs the nullable-promotion level", func(t *testing.T) {
		model := spi.NewEmptyNode()
		overlay, changes, err := schema.Admit(model, "x")
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
		}
		c := changes[0]
		if c.Required != spi.ChangeLevelType {
			t.Errorf("Required = %v, want TYPE", c.Required)
		}
		if c.Reason != schema.ReasonNewKind {
			t.Errorf("Reason = %v, want ReasonNewKind", c.Reason)
		}
		if c.Observed != schema.String {
			t.Errorf("Observed = %v, want STRING", c.Observed)
		}
		if overlay == nil {
			t.Fatal("want an overlay")
		}
	})
}

// TestAdmit_ChangeDeclaredKindsAndValue pins DeclaredKinds (rendered via the
// same declaredKindNames wording Validate has always used) and Value on
// every change kind this task produces.
func TestAdmit_ChangeDeclaredKindsAndValue(t *testing.T) {
	t.Run("a leaf type change carries the value and scalar DeclaredKinds", func(t *testing.T) {
		leaf := schema.NewLeafNode(schema.String)
		_, changes, err := schema.Admit(leaf, num("5"))
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d", len(changes))
		}
		c := changes[0]
		if c.DeclaredKinds != "scalar" {
			t.Errorf("DeclaredKinds = %q, want %q", c.DeclaredKinds, "scalar")
		}
		if c.Value != num("5") {
			t.Errorf("Value = %v, want json.Number(\"5\")", c.Value)
		}
	})

	t.Run("a container against a scalar leaf carries scalar DeclaredKinds", func(t *testing.T) {
		leaf := schema.NewLeafNode(schema.String)
		_, changes, err := schema.Admit(leaf, map[string]any{"k": "v"})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d", len(changes))
		}
		if changes[0].DeclaredKinds != "scalar" {
			t.Errorf("DeclaredKinds = %q, want %q", changes[0].DeclaredKinds, "scalar")
		}
	})

	t.Run("a string against an object node carries object DeclaredKinds", func(t *testing.T) {
		model := schema.NewObjectNode()
		_, changes, err := schema.Admit(model, "x")
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d", len(changes))
		}
		if changes[0].DeclaredKinds != "object" {
			t.Errorf("DeclaredKinds = %q, want %q", changes[0].DeclaredKinds, "object")
		}
	})
}

// TestAdmit_OverlayDeclaresExactlyObserved pins the overlay's CONTENT for a
// leaf type change: it declares the observed type alone, not the union of
// the observed and previously-declared types — Merge, not this traversal, is
// where widening happens.
func TestAdmit_OverlayDeclaresExactlyObserved(t *testing.T) {
	leaf := schema.NewLeafNode(schema.Integer)
	overlay, _, err := schema.Admit(leaf, num("2147483648"))
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if overlay == nil {
		t.Fatal("want an overlay")
	}
	got := overlay.DeclaredTypes()
	if len(got) != 1 || got[0] != schema.Long {
		t.Errorf("overlay.DeclaredTypes() = %v, want [LONG]", got)
	}
}

// The defect Extend had: Merge ran over the whole walked document whenever
// any node changed, so an unrelated field widened on a verdict about another.
func TestAdmit_OverlayTouchesOnlyTheChangedPath(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("x", schema.NewLeafNode(schema.Double))
	model.SetChild("y", schema.NewLeafNode(schema.Integer))

	doc := map[string]any{"x": num("2147483648"), "y": num("1.5")}

	overlay, changes, err := schema.Admit(model, doc)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want exactly 1 change (y), got %d: %+v", len(changes), changes)
	}
	if changes[0].Path != ".y" {
		t.Errorf("changed path = %q, want \".y\"", changes[0].Path)
	}
	if overlay.Object().Child("x") != nil {
		t.Error("x is held; the overlay must not mention it")
	}

	merged := schema.Merge(model, overlay)
	got := merged.Object().Child("x").DeclaredTypes()
	if len(got) != 1 || got[0] != schema.Double {
		t.Errorf("x must stay [DOUBLE], got %v", got)
	}
}

// Each array element is judged on its own; nothing is fused beforehand.
func TestAdmit_MixedKindArrayJudgedElementByElement(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("tags", schema.NewArrayNode(schema.NewLeafNode(schema.Double)))

	doc := map[string]any{"tags": []any{num("2147483648"), "hello"}}

	overlay, changes, err := schema.Admit(model, doc)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// 2147483648 is held by DOUBLE; only "hello" forces a change.
	if len(changes) != 1 {
		t.Fatalf("want exactly 1 change, got %d: %+v", len(changes), changes)
	}
	if changes[0].Required != spi.ChangeLevelArrayElements {
		t.Errorf("Required = %v, want ARRAY_ELEMENTS", changes[0].Required)
	}

	merged := schema.Merge(model, overlay)
	got := merged.Object().Child("tags").Array().Element().DeclaredTypes()
	if len(got) != 2 {
		t.Fatalf("element must declare DOUBLE and STRING, got %v", got)
	}
}

func TestAdmit_ContainerRules(t *testing.T) {
	cases := []struct {
		name         string
		model        func() *schema.ModelNode
		doc          any
		wantChanges  int
		wantRequired spi.ChangeLevel
	}{
		{
			name: "a new object field is STRUCTURAL",
			model: func() *schema.ModelNode {
				m := schema.NewObjectNode()
				m.SetChild("a", schema.NewLeafNode(schema.String))
				return m
			},
			doc:          map[string]any{"a": "x", "b": "y"},
			wantChanges:  1,
			wantRequired: spi.ChangeLevelStructural,
		},
		{
			// An array's length is not part of the model; the element type
			// matches (String), so this document is fully admitted.
			name: "a longer array is not a change",
			model: func() *schema.ModelNode {
				m := schema.NewObjectNode()
				m.SetChild("a", schema.NewArrayNode(schema.NewLeafNode(schema.String)))
				return m
			},
			doc:         map[string]any{"a": []any{"x", "y", "z"}},
			wantChanges: 0,
		},
		{
			name: "an element type change is ARRAY_ELEMENTS",
			model: func() *schema.ModelNode {
				m := schema.NewObjectNode()
				arr := schema.NewArrayNode(schema.NewLeafNode(schema.String))
				m.SetChild("a", arr)
				return m
			},
			doc:          map[string]any{"a": []any{"x", num("5")}},
			wantChanges:  1,
			wantRequired: spi.ChangeLevelArrayElements,
		},
		{
			name: "an object inside an array resets to TYPE",
			model: func() *schema.ModelNode {
				inner := schema.NewObjectNode()
				inner.SetChild("k", schema.NewLeafNode(schema.String))
				arr := schema.NewArrayNode(inner)
				m := schema.NewObjectNode()
				m.SetChild("a", arr)
				return m
			},
			doc:          map[string]any{"a": []any{map[string]any{"k": num("5")}}},
			wantChanges:  1,
			wantRequired: spi.ChangeLevelType,
		},
		{
			name: "a document the model fully admits produces nothing",
			model: func() *schema.ModelNode {
				m := schema.NewObjectNode()
				m.SetChild("a", schema.NewLeafNode(schema.Double))
				return m
			},
			doc:         map[string]any{"a": num("2147483648")},
			wantChanges: 0,
		},
		{
			name: "a missing field is not a change",
			model: func() *schema.ModelNode {
				m := schema.NewObjectNode()
				m.SetChild("a", schema.NewLeafNode(schema.String))
				m.SetChild("b", schema.NewLeafNode(schema.String))
				return m
			},
			doc:         map[string]any{"a": "x"},
			wantChanges: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, changes, err := schema.Admit(tc.model(), tc.doc)
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if len(changes) != tc.wantChanges {
				t.Fatalf("want %d changes, got %d: %+v", tc.wantChanges, len(changes), changes)
			}
			if tc.wantChanges > 0 && changes[0].Required != tc.wantRequired {
				t.Errorf("Required = %v, want %v", changes[0].Required, tc.wantRequired)
			}
		})
	}
}

// Against an empty model everything is a change and the overlay is the whole
// document's description — which is what registration needs.
func TestAdmit_AgainstEmptyModelDescribesTheWholeDocument(t *testing.T) {
	doc := map[string]any{
		"note":  "hello",
		"when":  "2026-03-01",
		"count": num("5"),
		"tags":  []any{"a", "b"},
	}
	overlay, changes, err := schema.Admit(schema.NewObjectNode(), doc)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("an empty model admits nothing; every field must be a change")
	}
	for _, f := range []string{"note", "when", "count", "tags"} {
		if overlay.Object().Child(f) == nil {
			t.Errorf("overlay is missing %q", f)
		}
	}
	if got := overlay.Object().Child("when").DeclaredTypes(); len(got) != 1 || got[0] != schema.LocalDate {
		t.Errorf("when = %v, want [LOCAL_DATE] — registration discovers temporal types", got)
	}
}

// Handoff from Task 6 review: wrongKind's container return must describe the
// value's real shape, not an empty container that would admit nothing on
// merge. A [STRING] leaf against a mixed-kind array must produce an overlay
// whose element declares both kinds actually observed.
func TestAdmit_WrongKindContainerDescribesTheValue(t *testing.T) {
	leaf := schema.NewLeafNode(schema.String)
	overlay, changes, err := schema.Admit(leaf, []any{"a", num("5")})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
	}
	if overlay == nil || overlay.Array() == nil {
		t.Fatal("want an array overlay")
	}
	elem := overlay.Array().Element()
	if elem == nil {
		t.Fatal("want the overlay's element to describe the observed values")
	}
	got := elem.DeclaredTypes()
	if len(got) != 2 {
		t.Fatalf("element must declare STRING and INTEGER, got %v", got)
	}
	var hasString, hasInteger bool
	for _, dt := range got {
		if dt == schema.String {
			hasString = true
		}
		if dt == schema.Integer {
			hasInteger = true
		}
	}
	if !hasString || !hasInteger {
		t.Errorf("element declared types = %v, want STRING and INTEGER", got)
	}
}

// An empty container is still an observation of its kind. A derived model
// has always recorded an empty object/array as declaring that kind with no
// content (a bare NewObjectNode(), a NewArrayNode(NewLeafNode(Null)));
// Describe must produce the identical shape, or a brand-new field whose
// only observed value is {} or [] would
// either vanish from the derived model or persist as bytes that differ from
// what importer.Walk would have written for the same document — breaking the
// parity oracle's byte-identity and dropping the $.tags[*] descriptor
// (Types: [NULL], IsArray: true) that {"tags":[]} produces today.
func TestAdmit_EmptyContainerFieldStillDeclaresItsKind(t *testing.T) {
	t.Run("empty object", func(t *testing.T) {
		overlay, changes, err := schema.Admit(schema.NewObjectNode(), map[string]any{"a": map[string]any{}})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
		}
		child := overlay.Object().Child("a")
		if child == nil || child.Object() == nil {
			t.Fatalf("field %q must declare an (empty) object branch, got %v", "a", child)
		}
	})

	t.Run("empty array", func(t *testing.T) {
		overlay, changes, err := schema.Admit(schema.NewObjectNode(), map[string]any{"a": []any{}})
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if len(changes) != 1 {
			t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
		}
		child := overlay.Object().Child("a")
		if child == nil || child.Array() == nil {
			t.Fatalf("field %q must declare an (empty) array branch, got %v", "a", child)
		}
		// Not just "an array branch" — the SAME element Describe gives an
		// empty array: a non-nil, Nullable leaf declaring exactly [NULL].
		// NewArrayNode(nil) (a declared-but-unobserved element) is a
		// different wire shape and drops the $.a[*] field descriptor.
		elem := child.Array().Element()
		if elem == nil {
			t.Fatal("empty array's element must be NewLeafNode(Null), not nil")
		}
		if !elem.Nullable() {
			t.Error("empty array's element must be Nullable")
		}
		if got := elem.DeclaredTypes(); len(got) != 1 || got[0] != schema.Null {
			t.Errorf("empty array's element DeclaredTypes = %v, want [NULL]", got)
		}
	})
}

// The array traversal Admit itself runs (not Describe's fresh-field
// shortcut) must charge the same promotion checkBranch always did: incoming
// arrays from importer.Walk always carry a non-nil element — Describe gives
// even [] a NewLeafNode(Null) element — so an array observed but never with
// content (a nil element) always learns SOMETHING at ARRAY_ELEMENTS when the
// document holds an array at that path, whether or not that array is empty.
func TestAdmit_EmptyArrayAgainstUnlearnedElementChargesArrayElements(t *testing.T) {
	model := schema.NewObjectNode()
	model.SetChild("tags", schema.NewArrayNode(nil))

	overlay, changes, err := schema.Admit(model, map[string]any{"tags": []any{}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
	}
	if changes[0].Reason != schema.ReasonArrayElement {
		t.Errorf("Reason = %v, want ReasonArrayElement", changes[0].Reason)
	}
	if changes[0].Required != spi.ChangeLevelArrayElements {
		t.Errorf("Required = %v, want ARRAY_ELEMENTS", changes[0].Required)
	}

	elem := overlay.Object().Child("tags").Array().Element()
	if elem == nil {
		t.Fatal("the learned element must be NewLeafNode(Null), not nil")
	}
	if !elem.Nullable() {
		t.Error("the learned element must be Nullable")
	}
	if got := elem.DeclaredTypes(); len(got) != 1 || got[0] != schema.Null {
		t.Errorf("learned element DeclaredTypes = %v, want [NULL]", got)
	}
}

// Describe's own recursion (a brand-new field's contents are ALL new, so
// object/array re-enter Describe for every level) must be bounded by the same
// MaxValidationDepth guard node enforces, or a document nested deeper than
// the limit under a brand-new field would recurse unbounded instead of
// failing closed with a clean error.
func TestAdmit_DescribeRespectsMaxValidationDepth(t *testing.T) {
	var doc any = "leaf"
	for i := 0; i < schema.MaxValidationDepth+5; i++ {
		doc = map[string]any{"nested": doc}
	}
	root := map[string]any{"a": doc}

	_, _, err := schema.Admit(schema.NewObjectNode(), root)
	if err == nil {
		t.Fatal("want a validation-depth-exceeded error")
	}
	if !strings.Contains(err.Error(), "validation depth exceeded") {
		t.Errorf("error = %v, want a validation-depth-exceeded error", err)
	}
}

// A brand-new field's own name must pass the same jsonPath-segment check as
// any other field name — ValidateFieldName is not skipped just because the
// field is new.
func TestAdmit_NewFieldInvalidNameIsRejected(t *testing.T) {
	_, _, err := schema.Admit(schema.NewObjectNode(), map[string]any{"bad name": num("1")})
	if err == nil {
		t.Fatal("want an error for an unaddressable field name")
	}
	if !errors.Is(err, schema.ErrInvalidFieldName) {
		t.Errorf("error = %v, want errors.Is(err, schema.ErrInvalidFieldName)", err)
	}
}

// Ruling 22 (final review I2): the two doors that establish a model's field
// set — Admit (an entity write) and importer.Walk (sample-data import) —
// must spell the diagnostic's parent identically, or a client sees a
// different string for the same root-level bad key depending on which door
// rejected it. Admit's own path convention names the root "" and Walk's
// names it "$" (docs/cloud-parity/model-field-name-grammar.md); the message
// normalises both onto "$".
func TestValidateFieldName_BothDoorsSpellRootParentTheSame(t *testing.T) {
	_, _, admitErr := schema.Admit(schema.NewObjectNode(), map[string]any{"bad name": num("1")})
	if admitErr == nil || !errors.Is(admitErr, schema.ErrInvalidFieldName) {
		t.Fatalf("Admit error = %v, want errors.Is(err, ErrInvalidFieldName)", admitErr)
	}

	_, walkErr := importer.Walk(map[string]any{"bad name": num("1")})
	if walkErr == nil || !errors.Is(walkErr, schema.ErrInvalidFieldName) {
		t.Fatalf("Walk error = %v, want errors.Is(err, ErrInvalidFieldName)", walkErr)
	}

	if admitErr.Error() != walkErr.Error() {
		t.Errorf("Admit and Walk render different messages for the same root-level bad key:\nAdmit: %s\nWalk:  %s",
			admitErr.Error(), walkErr.Error())
	}
	if !strings.Contains(admitErr.Error(), `at "$"`) {
		t.Errorf("Admit error = %q, want it to say at \"$\"", admitErr.Error())
	}
}

// The same check must fire from inside Describe when it is reached through
// wrongKind's container branch — a container value against a node lacking
// that branch, itself holding a field name the query grammar cannot
// address — so the error has to propagate out of wrongKind's new error
// return, not just out of object's direct callers.
func TestAdmit_NestedInvalidFieldNamePropagatesThroughWrongKind(t *testing.T) {
	leaf := schema.NewLeafNode(schema.String)
	_, _, err := schema.Admit(leaf, []any{map[string]any{"bad name": num("1")}})
	if err == nil {
		t.Fatal("want an error for an unaddressable field name nested inside a container mismatch")
	}
	if !errors.Is(err, schema.ErrInvalidFieldName) {
		t.Errorf("error = %v, want errors.Is(err, schema.ErrInvalidFieldName)", err)
	}
}

// An array's length is not part of the model. The array declares its
// element, and a homogeneous list of any length is held by that element —
// so a longer array proposes no change, at any level, even against the
// very tree Describe derived from a shorter one. The three doors agree:
// Admit records nothing, Validate reports nothing, and Extend at the
// strictest level returns the model unchanged.
func TestAdmit_ArrayLengthIsNotASchemaChange(t *testing.T) {
	model, err := schema.Describe(map[string]any{"a": []any{"x", "y"}}, "")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	doc := map[string]any{"a": []any{"x", "y", "z"}}

	overlay, changes, err := schema.Admit(model, doc)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("a longer array is not a change; got %+v", changes)
	}
	if overlay != nil {
		t.Errorf("no change means no overlay; got %v", overlay)
	}
	if errs := schema.Validate(model, doc); len(errs) != 0 {
		t.Errorf("Validate must hold a longer array; got %v", errs)
	}

	before, err := spi.MarshalModelNode(model)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	extended, err := schema.Extend(model, doc, spi.ChangeLevelArrayLength)
	if err != nil {
		t.Fatalf("Extend at ARRAY_LENGTH: %v", err)
	}
	after, err := spi.MarshalModelNode(extended)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("Extend must leave the model byte-identical:\n before %s\n after  %s", before, after)
	}
}
