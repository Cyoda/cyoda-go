package exporter_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/exporter"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// exportSimpleView renders node and returns the path-keyed "model" map.
func exportSimpleView(t *testing.T, node *schema.ModelNode) map[string]any {
	t.Helper()
	data, err := exporter.NewSimpleViewExporter("LOCKED").Export(node)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var sv map[string]any
	if err := json.Unmarshal(data, &sv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	model, ok := sv["model"].(map[string]any)
	if !ok {
		t.Fatalf("no model in %s", data)
	}
	return model
}

func bucket(t *testing.T, model map[string]any, path string) map[string]any {
	t.Helper()
	b, ok := model[path].(map[string]any)
	if !ok {
		t.Fatalf("no %q bucket in %v", path, model)
	}
	return b
}

func derive(t *testing.T, doc string) *schema.ModelNode {
	t.Helper()
	node, err := importer.NewSampleDataImporter().Import(strings.NewReader(doc), "JSON")
	if err != nil {
		t.Fatalf("import %s: %v", doc, err)
	}
	return node
}

// An array of arrays is described one wildcard hop per level, the same spelling
// the field paths and the search surface use ($.m[*][*]). Rendering it as
// `.m[*]: NULL` said the elements have no type at all, when what has no type of
// its own is the intermediate array.
//
// The "(T x N)" width decoration these tests see comes from ArrayInfo, which is
// carried on the in-memory tree only — the schema codec does not persist it —
// so an export served from the store shows the bare type. The e2e tests assert
// that form; the width assertions here pin the descriptor's composition, not a
// guarantee to callers.
func TestSimpleView_NestedArrayOfPrimitives(t *testing.T) {
	root := exportSimpleView(t, derive(t, `{"m":[["A"],["B","C"],["D"]]}`))
	rootBucket := bucket(t, root, "$")

	if _, stale := rootBucket[".m[*]"]; stale {
		t.Errorf(".m[*] must not describe an array of arrays: %v", rootBucket)
	}
	if got, want := rootBucket[".m[*][*]"], "(STRING x 2)"; got != want {
		t.Errorf(".m[*][*] = %v, want %v", got, want)
	}
}

// The innermost element is what carries a structure, so an array of arrays of
// objects gets its own bucket at the fully-hopped path.
func TestSimpleView_NestedArrayOfObjects(t *testing.T) {
	model := exportSimpleView(t, derive(t, `{"m":[[{"sku":"A"}]]}`))

	if got := bucket(t, model, "$")["#.m"]; got != "OBJECT" {
		t.Errorf(`$["#.m"] = %v, want OBJECT`, got)
	}
	elem := bucket(t, model, "$.m[*][*]")
	if got := elem["#"]; got != "ARRAY_ELEMENT" {
		t.Errorf("element marker = %v, want ARRAY_ELEMENT", got)
	}
	if got := elem[".sku"]; got != "STRING" {
		t.Errorf(".sku = %v, want STRING", got)
	}
}

// A nested array reached through an array-of-objects element renders the same
// way — the rule is about the shape, not about where it sits.
func TestSimpleView_NestedArrayInsideArrayElement(t *testing.T) {
	model := exportSimpleView(t, derive(t, `{"items":[{"m":[["A"]]}]}`))
	elem := bucket(t, model, "$.items[*]")
	if got, want := elem[".m[*][*]"], "(STRING x 1)"; got != want {
		t.Errorf(".m[*][*] = %v, want %v", got, want)
	}
}

// A field observed as both a scalar and an array declares BOTH kinds and
// enforces both, so the export has to show both. Rendering only the array
// branch made two models that enforce differently render identically, and told
// an operator inspecting the export something false.
func TestSimpleView_KindUnionShowsBothBranches(t *testing.T) {
	union := schema.Merge(derive(t, `{"poly":"x"}`), derive(t, `{"poly":["A","B"]}`))
	rootBucket := bucket(t, exportSimpleView(t, union), "$")

	if got, want := rootBucket[".poly"], "STRING"; got != want {
		t.Errorf(".poly = %v, want %v (the scalar branch)", got, want)
	}
	if got, want := rootBucket[".poly[*]"], "(STRING x 2)"; got != want {
		t.Errorf(".poly[*] = %v, want %v (the array branch)", got, want)
	}
}

func TestSimpleView_KindUnionScalarAndObjectShowsBothBranches(t *testing.T) {
	union := schema.Merge(derive(t, `{"o":"x"}`), derive(t, `{"o":{"k":"v"}}`))
	model := exportSimpleView(t, union)
	rootBucket := bucket(t, model, "$")

	if got, want := rootBucket[".o"], "STRING"; got != want {
		t.Errorf(".o = %v, want %v (the scalar branch)", got, want)
	}
	if got := rootBucket["#.o"]; got != "OBJECT" {
		t.Errorf(`$["#.o"] = %v, want OBJECT (the object branch)`, got)
	}
	if got := bucket(t, model, "$.o")[".k"]; got != "STRING" {
		t.Errorf("$.o.k = %v, want STRING", got)
	}
}

// A field that is only ever an array must NOT grow a scalar branch: the
// nullable marker is not a scalar observation.
func TestSimpleView_ArrayOnlyFieldHasNoScalarBranch(t *testing.T) {
	rootBucket := bucket(t, exportSimpleView(t, derive(t, `{"poly":["A","B"]}`)), "$")
	if got, present := rootBucket[".poly"]; present {
		t.Errorf(".poly = %v, want no scalar branch", got)
	}

	// null then array: the null is absence, not a scalar observation.
	nullable := schema.Merge(derive(t, `{"poly":null}`), derive(t, `{"poly":["A"]}`))
	rootBucket = bucket(t, exportSimpleView(t, nullable), "$")
	if got, present := rootBucket[".poly"]; present {
		t.Errorf("nullable array .poly = %v, want no scalar branch", got)
	}
}

// An array whose elements were never observed is still a declared array —
// validation enforces it — so the export names it rather than omitting the
// field entirely. The same shape one level down already rendered this way.
func TestSimpleView_UnobservedElementArrayIsNamed(t *testing.T) {
	node := schema.NewObjectNode()
	node.SetChild("a", schema.NewArrayNode(nil))

	rootBucket := bucket(t, exportSimpleView(t, node), "$")
	if got, want := rootBucket[".a[*]"], "NULL"; got != want {
		t.Errorf(".a[*] = %v, want %v; bucket: %v", got, want, rootBucket)
	}
}

// Elements observed in more than one kind are described by every branch they
// carry, exactly as a named field is — the rule does not stop at an array hop.
func TestSimpleView_ArrayElementUnionShowsBothBranches(t *testing.T) {
	union := schema.Merge(derive(t, `{"m":[{"k":"v"}]}`), derive(t, `{"m":["A"]}`))
	model := exportSimpleView(t, union)
	rootBucket := bucket(t, model, "$")

	if got, want := rootBucket[".m[*]"], "(STRING x 1)"; got != want {
		t.Errorf(".m[*] = %v, want %v (the scalar-element branch)", got, want)
	}
	if got := rootBucket["#.m"]; got != "OBJECT" {
		t.Errorf(`$["#.m"] = %v, want OBJECT (the object-element branch)`, got)
	}
	if got := bucket(t, model, "$.m[*]")[".k"]; got != "STRING" {
		t.Errorf("$.m[*].k = %v, want STRING", got)
	}
}

// The JSON Schema rendering owes the same faithfulness: a union of kinds is a
// an anyOf over them.
func TestJSONSchema_KindUnionShowsBothBranches(t *testing.T) {
	union := schema.Merge(derive(t, `{"poly":"x"}`), derive(t, `{"poly":["A","B"]}`))
	data, err := exporter.NewJSONSchemaExporter("LOCKED").Export(union)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var doc struct {
		Model struct {
			Properties struct {
				Poly struct {
					AnyOf []map[string]any `json:"anyOf"`
				} `json:"poly"`
			} `json:"properties"`
		} `json:"model"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	branches := doc.Model.Properties.Poly.AnyOf
	if len(branches) != 2 {
		t.Fatalf("poly branches = %v, want an array branch and a string branch; body: %s", branches, data)
	}
	kinds := map[string]bool{}
	for _, b := range branches {
		kinds[b["type"].(string)] = true
	}
	if !kinds["array"] || !kinds["string"] {
		t.Errorf("poly branches = %v, want both array and string; body: %s", branches, data)
	}
}
