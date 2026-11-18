package importer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/internal/domain/model/importer"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
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
	if node.Object() == nil {
		t.Fatalf("root kinds = %v, want the object branch", node.Kinds())
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
	if node.Object() == nil {
		t.Fatalf("root kinds = %v, want the object branch", node.Kinds())
	}
	if fields := node.Fields(); len(fields) != 0 {
		t.Errorf("fields = %v, want none", fields)
	}
}

// Registration discovers types; ingestion checks against them. These give
// different declared sets for the same value, and that is the design: a field
// registered from both a word and a date supports temporal predicates, while
// a field locked as text and then written a date stays text.
func TestImport_DiscoversTemporalTypesAcrossDocuments(t *testing.T) {
	node, err := importJSON(t, `[{"note":"hello"},{"note":"2026-03-01"}]`)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	got := node.Object().Child("note").DeclaredTypes()
	if len(got) != 2 {
		t.Fatalf("note = %v, want {STRING, LOCAL_DATE}", got)
	}
	var hasString, hasDate bool
	for _, dt := range got {
		hasString = hasString || dt == schema.String
		hasDate = hasDate || dt == schema.LocalDate
	}
	if !hasString || !hasDate {
		t.Errorf("note = %v, want both STRING and LOCAL_DATE", got)
	}
}

// The same within one document, across array elements.
func TestImport_FusesArrayElements(t *testing.T) {
	node, err := importJSON(t, `{"tags":["hello","2026-03-01"]}`)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	got := node.Object().Child("tags").Array().Element().DeclaredTypes()
	if len(got) != 2 {
		t.Fatalf("element = %v, want {STRING, LOCAL_DATE}", got)
	}
	var hasString, hasDate bool
	for _, dt := range got {
		hasString = hasString || dt == schema.String
		hasDate = hasDate || dt == schema.LocalDate
	}
	if !hasString || !hasDate {
		t.Errorf("element = %v, want both STRING and LOCAL_DATE", got)
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
