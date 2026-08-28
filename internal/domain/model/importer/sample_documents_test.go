package importer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func importJSON(t *testing.T, doc string) (*schema.ModelNode, error) {
	t.Helper()
	return importer.NewSampleDataImporter().Import(strings.NewReader(doc), "JSON")
}

// A JSON array of sample documents is what an operator reaches for when
// registering a model from several representative records, and it is the same
// shape the entity ingress already reads as "a collection of entities of the
// same type". It derives the merge of the documents — the result successive
// imports onto an UNLOCKED model produce — rather than a model describing an
// array at the root, which describes nothing usable and refuses the very
// documents it was derived from.
func TestImport_TopLevelArrayWalksAsDocumentCollection(t *testing.T) {
	node, err := importJSON(t, `[{"name":"A","tags":["A","B"]},{"name":"B","sku":1}]`)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if node.Kind() != schema.KindObject {
		t.Fatalf("root kind = %v, want OBJECT", node.Kind())
	}

	want := map[string][]schema.DataType{
		"$.name":    {schema.String},
		"$.sku":     {schema.Integer},
		"$.tags[*]": {schema.String},
	}
	fields := node.FieldsMap()
	if len(fields) != len(want) {
		t.Fatalf("fields = %v, want %d entries", fields, len(want))
	}
	for path, types := range want {
		f, ok := fields[path]
		if !ok {
			t.Errorf("missing field %s in %v", path, fields)
			continue
		}
		if len(f.Types) != len(types) || f.Types[0] != types[0] {
			t.Errorf("field %s types = %v, want %v", path, f.Types, types)
		}
	}

	// The derived model must admit the documents it was derived from.
	for _, doc := range []string{`{"name":"A","tags":["A","B"]}`, `{"name":"B","sku":1}`} {
		if errs := schema.Validate(node, decodeJSON(t, doc)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}
}

// One document in an array is the same as that document on its own.
func TestImport_SingleElementArrayEqualsBareDocument(t *testing.T) {
	fromArray, err := importJSON(t, `[{"name":"A","m":[["A"]]}]`)
	if err != nil {
		t.Fatalf("Import array: %v", err)
	}
	bare, err := importJSON(t, `{"name":"A","m":[["A"]]}`)
	if err != nil {
		t.Fatalf("Import object: %v", err)
	}
	a, err := schema.Marshal(fromArray)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b, err := schema.Marshal(bare)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("array-of-one derived a different model\n  array: %s\n  bare:  %s", a, b)
	}
}

// An empty collection carries no observations, so it derives the same empty
// model an empty document does — not an error, and not an array root.
func TestImport_EmptyArrayDerivesEmptyObjectModel(t *testing.T) {
	node, err := importJSON(t, `[]`)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if node.Kind() != schema.KindObject {
		t.Fatalf("root kind = %v, want OBJECT", node.Kind())
	}
	if fields := node.Fields(); len(fields) != 0 {
		t.Errorf("fields = %v, want none", fields)
	}
}

// Anything that is not a document, or a collection of documents, has no
// reading that yields a usable model — so it is refused at the boundary
// instead of registering one that rejects everything.
func TestImport_NonDocumentSampleDataRejected(t *testing.T) {
	cases := []struct {
		name, doc, wantIn string
	}{
		{"top-level string", `"x"`, "got a string"},
		{"top-level number", `5`, "got a number"},
		{"top-level boolean", `true`, "got a boolean"},
		{"top-level null", `null`, "got null"},
		{"array of scalars", `["A","B"]`, "element 0 is a string"},
		{"array of arrays", `[["A"]]`, "element 0 is an array"},
		{"array with a scalar after a document", `[{"a":1},"x"]`, "element 1 is a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := importJSON(t, tc.doc)
			if err == nil {
				t.Fatal("Import succeeded, want rejection")
			}
			if !errors.Is(err, importer.ErrNonDocumentSampleData) {
				t.Errorf("error = %v, want ErrNonDocumentSampleData", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantIn)
			}
		})
	}
}
