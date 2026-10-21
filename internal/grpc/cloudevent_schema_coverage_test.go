package grpc

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"

	cyodaschemas "github.com/cyoda-platform/cyoda-go/docs/cyoda/schema"
)

// registeredEventTypes lists every CloudEvent type declared in
// cloudevent_types.go, read from the source so that a type added there is
// covered without anyone remembering to list it here.
func registeredEventTypes(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "cloudevent_types.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cloudevent_types.go: %v", err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, v := range spec.(*ast.ValueSpec).Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("cloudevent_types.go declares a constant that is not a string literal: %#v", v)
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, name)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("cloudevent_types.go declares no event types")
	}
	return out
}

// schemaTitles maps each schema's title to its path in the published tree.
func schemaTitles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(cyodaschemas.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".json" {
			return err
		}
		raw, err := cyodaschemas.FS.ReadFile(p)
		if err != nil {
			return err
		}
		var doc struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s is not valid JSON: %v", p, err)
		}
		out[doc.Title] = p
		return nil
	})
	if err != nil {
		t.Fatalf("walk the schema tree: %v", err)
	}
	return out
}

// cloudEventTypeEnum reads the published CloudEventType enum.
func cloudEventTypeEnum(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := cyodaschemas.FS.ReadFile("common/CloudEventType.json")
	if err != nil {
		t.Fatalf("read CloudEventType.json: %v", err)
	}
	var doc struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("CloudEventType.json is not valid JSON: %v", err)
	}
	out := map[string]bool{}
	for _, v := range doc.Enum {
		out[v] = true
	}
	return out
}

// The published schema tree is the contract for CloudEvent payloads: a client
// generates its event types from it. An event type the server accepts or sends
// with no schema there is one such a client cannot spell, so every registered
// type has a schema of its own name and is a value of the CloudEventType enum.
func TestEveryRegisteredEventTypeIsPublished(t *testing.T) {
	titles := schemaTitles(t)
	enum := cloudEventTypeEnum(t)
	for _, name := range registeredEventTypes(t) {
		p, ok := titles[name]
		if !ok {
			t.Errorf("%s has no schema in the published tree", name)
		} else if path.Base(p) != name+".json" {
			t.Errorf("%s is published as %s; want a file named %s.json", name, p, name)
		}
		if !enum[name] {
			t.Errorf("%s is not a value of common/CloudEventType.json", name)
		}
	}
}

// The other direction: the enum names only event types the server knows, so a
// value there is not a promise nothing keeps. BaseEvent is the one exception —
// it names the shape every event extends, not an event a client sends.
func TestCloudEventTypeEnumNamesOnlyRegisteredTypes(t *testing.T) {
	registered := map[string]bool{"BaseEvent": true}
	for _, name := range registeredEventTypes(t) {
		registered[name] = true
	}
	var unknown []string
	for name := range cloudEventTypeEnum(t) {
		if !registered[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		t.Errorf("common/CloudEventType.json names types cloudevent_types.go does not declare: %s", strings.Join(unknown, ", "))
	}
}
