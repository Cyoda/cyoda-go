package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
)

// samplesDir holds the wire samples that the client-generation check
// (api/clientprobe) round-trips through the generated Kotlin and Java
// clients. Each file is one sample.
const samplesDir = "clientprobe/samples"

// sample is one wire sample. It names either a component schema (schema,
// with class, the simple name of the concrete class a client reads it as)
// or an operation's JSON request body (body, "<METHOD> <path>"), which the
// generated clients take as an untyped value and so have no class for.
// valid defaults to true. A future sample is a response a later version of
// the spec could send; it is not validated.
type sample struct {
	file   string
	Schema string          `json:"schema"`
	Body   string          `json:"body"`
	Class  string          `json:"class"`
	Valid  *bool           `json:"valid"`
	Future bool            `json:"future"`
	Value  json.RawMessage `json:"value"`

	value any // Value decoded
}

func (s sample) valid() bool { return s.Valid == nil || *s.Valid }

func loadSamples(t *testing.T) []sample {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(samplesDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []sample
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		s := sample{file: filepath.Base(f)}
		if err := dec.Decode(&s); err != nil {
			t.Errorf("%s: %v", s.file, err)
			continue
		}
		if len(s.Value) == 0 {
			t.Errorf("%s: no value", s.file)
			continue
		}
		if err := json.Unmarshal(s.Value, &s.value); err != nil {
			t.Errorf("%s: value: %v", s.file, err)
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		t.Fatalf("no samples under %s", samplesDir)
	}
	return out
}

// sampleSchema returns the schema s is checked against, in the view: a use of
// its component schema, or its operation's JSON request body.
func sampleSchema(doc *openapi3.T, s sample) (*openapi3.Schema, error) {
	switch {
	case s.Schema != "" && s.Body != "":
		return nil, fmt.Errorf("names both a schema and a body")
	case s.Schema != "":
		return openapivalidator.ComponentUse(doc, s.Schema)
	case s.Body != "":
		method, path, ok := strings.Cut(s.Body, " ")
		item := doc.Paths.Find(path)
		if !ok || item == nil || item.GetOperation(method) == nil || item.GetOperation(method).RequestBody == nil {
			return nil, fmt.Errorf("body %q is not an operation with a request body", s.Body)
		}
		mt := item.GetOperation(method).RequestBody.Value.Content.Get("application/json")
		if mt == nil || mt.Schema == nil {
			return nil, fmt.Errorf("body %q has no JSON schema", s.Body)
		}
		return mt.Schema.Value, nil
	}
	return nil, fmt.Errorf("names neither a schema nor a body")
}

// sampleOptions returns the kin options a sample is validated with: a
// request body in the request direction, and every other sample with
// ValidateValue's default, the e2e response validator's options.
func sampleOptions(s sample) []openapi3.SchemaValidationOption {
	if s.Body == "" {
		return nil
	}
	return []openapi3.SchemaValidationOption{
		openapi3.MultiErrors(), openapi3.EnableJSONSchema2020(), openapi3.VisitAsRequest(),
	}
}

// A valid sample passes its schema, and the schema of its class, which is the
// member its discriminator selects; an invalid one fails. A future sample is
// not validated: by construction it is outside the current mapping.
func TestSamples(t *testing.T) {
	doc := viewedSpec(t)
	for _, s := range loadSamples(t) {
		schema, err := sampleSchema(doc, s)
		if err != nil {
			t.Errorf("%s: %v", s.file, err)
			continue
		}
		switch {
		case s.Body != "" && s.Class != "":
			t.Errorf("%s: a body sample has no class", s.file)
		case s.Schema != "" && s.Class == "" && s.valid():
			t.Errorf("%s: no class", s.file)
		case s.Class != "" && doc.Components.Schemas[s.Class] == nil:
			t.Errorf("%s: class %s is not a component schema", s.file, s.Class)
		case s.Future && !s.valid():
			t.Errorf("%s: a future sample is a valid response of a later spec", s.file)
		}
		if s.Future {
			continue
		}
		err = openapivalidator.ValidateValue(doc, schema, s.value, sampleOptions(s)...)
		if s.valid() && err != nil {
			t.Errorf("%s: valid sample refused: %v", s.file, err)
		}
		if !s.valid() && err == nil {
			t.Errorf("%s: invalid sample accepted", s.file)
		}
		if !s.valid() || s.Class == "" {
			continue
		}
		// The class is the member the discriminator selects, or the schema
		// itself when it has no discriminator.
		want := s.Schema
		if d := schema.Discriminator; d != nil {
			obj, _ := s.value.(map[string]any)
			key, _ := obj[d.PropertyName].(string)
			want = strings.TrimPrefix(d.Mapping[key].Ref, schemaRefPrefix)
		}
		if s.Class != want {
			t.Errorf("%s: class %s, but a client reads it as %q", s.file, s.Class, want)
		}
		if s.Class != s.Schema {
			class, err := openapivalidator.ComponentUse(doc, s.Class)
			if err == nil {
				err = openapivalidator.ValidateValue(doc, class, s.value)
			}
			if err != nil {
				t.Errorf("%s: not a %s: %v", s.file, s.Class, err)
			}
		}
	}
}

// coverage is what the samples hold, for the checks of TestSampleCoverage.
type coverage struct {
	mapped    map[string]bool         // "<base>/<key>" with a valid sample read through the base
	forms     map[string]bool         // "<position> <valid|invalid> <form>"
	preChecks map[string]bool         // present, null, absent
	bodies    map[string]bool         // "<body> object" and "<body> array"
	auditPage bool                    // one valid page holding every audit member and FINISH with a stop reason
	groups    map[string]map[int]bool // invalid sample name -> depths of its groups holding a non-object entry
	future    map[string]bool         // unknown audit kind, unknown event type, added field
}

func (c *coverage) form(position string, valid bool, value any) {
	verdict := "invalid"
	if valid {
		verdict = "valid"
	}
	c.forms[position+" "+verdict+" "+valueForm(value)] = true
}

// valueForm names the form of a value as section 3.4 lists them.
func valueForm(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		switch len(x) {
		case 2:
			return "pair"
		case 3:
			return "triple"
		}
		return fmt.Sprintf("array of %d", len(x))
	}
	return fmt.Sprintf("%T", v)
}

// groupDepths records the depth (1 for the top-level group) of each group in
// v that holds a non-object entry in its conditions.
func groupDepths(v any, depth int, out map[int]bool) {
	obj, ok := v.(map[string]any)
	if !ok || obj["type"] != "group" {
		return
	}
	conditions, _ := obj["conditions"].([]any)
	for _, c := range conditions {
		if _, isObj := c.(map[string]any); !isObj {
			out[depth] = true
		}
		groupDepths(c, depth+1, out)
	}
}

// memberProperties returns the property names a member schema declares, its
// allOf entries included.
func memberProperties(s *openapi3.Schema) map[string]bool {
	out := map[string]bool{}
	for name := range s.Properties {
		out[name] = true
	}
	for _, part := range s.AllOf {
		if part.Value != nil {
			maps.Copy(out, memberProperties(part.Value))
		}
	}
	return out
}

const (
	createBody = "POST /entity/{format}/{entityName}/{modelVersion}"
	importBody = "POST /model/import/{dataFormat}/{converter}/{entityName}/{modelVersion}"
)

func collectCoverage(doc *openapi3.T, samples []sample) coverage {
	c := coverage{
		mapped: map[string]bool{}, forms: map[string]bool{}, preChecks: map[string]bool{},
		bodies: map[string]bool{}, groups: map[string]map[int]bool{}, future: map[string]bool{},
	}
	auditKinds := doc.Components.Schemas["AuditEventDto"].Value.Discriminator.Mapping
	eventTypes := doc.Components.Schemas["StateMachineEventDto"].Value.Discriminator.Mapping
	for _, s := range samples {
		obj, _ := s.value.(map[string]any)

		if s.Future {
			page, _ := obj["items"].([]any)
			for _, item := range page {
				event, _ := item.(map[string]any)
				kind, _ := event["auditEventType"].(string)
				if _, known := auditKinds[kind]; !known {
					c.future["unknown audit kind"] = true
					continue
				}
				if kind != "StateMachine" {
					continue
				}
				eventType, _ := event["eventType"].(string)
				member, known := eventTypes[eventType]
				if !known {
					c.future["unknown event type"] = true
					continue
				}
				data, _ := event["data"].(map[string]any)
				props := memberProperties(doc.Components.Schemas[strings.TrimPrefix(member.Ref, schemaRefPrefix)].Value)
				for k := range data {
					if !props[k] {
						c.future["added field"] = true
					}
				}
			}
			continue
		}

		if s.Body != "" {
			form := valueForm(s.value)
			if _, isArray := s.value.([]any); isArray {
				form = "array"
			}
			if s.valid() {
				c.bodies[s.Body+" "+form] = true
			}
			continue
		}

		if base := doc.Components.Schemas[s.Schema]; s.valid() && base != nil && base.Value.Discriminator != nil {
			d := base.Value.Discriminator
			key, _ := obj[d.PropertyName].(string)
			if m, ok := d.Mapping[key]; ok && m.Ref == schemaRefPrefix+s.Class {
				c.mapped[s.Schema+"/"+key] = true
			}
		}

		switch {
		case s.Schema == "QueryConditionDto" && obj["type"] == "simple":
			if v, ok := obj["value"]; ok {
				c.form("SimpleConditionDto.value", s.valid(), v)
			}
		case s.Schema == "QueryConditionDto" && obj["type"] == "lifecycle":
			if v, ok := obj["value"]; ok {
				c.form("LifecycleConditionDto.value", s.valid(), v)
			}
		case s.Schema == "QueryConditionDto" && obj["type"] == "array":
			values, _ := obj["values"].([]any)
			for _, v := range values {
				c.form("ArrayConditionDto.values[]", s.valid(), v)
			}
		case s.Schema == "QueryConditionDto" && obj["type"] == "function" && s.valid():
			fn, _ := obj["function"].(map[string]any)
			switch criterion, present := fn["criterion"]; {
			case !present:
				c.preChecks["absent"] = true
			case criterion == nil:
				c.preChecks["null"] = true
			default:
				c.preChecks["present"] = true
			}
		case s.Schema == "QueryConditionDto" && obj["type"] == "group" && !s.valid():
			depths := map[int]bool{}
			groupDepths(obj, 1, depths)
			c.groups[strings.TrimSuffix(s.file, ".json")] = depths
		case s.Schema == "GroupKeyEntry":
			if v, ok := obj["value"]; ok {
				c.form("GroupKeyEntry.value", s.valid(), v)
			}
		case s.Schema == "EntityAuditEventsResponseDto" && s.valid():
			kinds := map[string]bool{}
			finish := false
			items, _ := obj["items"].([]any)
			for _, item := range items {
				event, _ := item.(map[string]any)
				kind, _ := event["auditEventType"].(string)
				kinds[kind] = true
				data, _ := event["data"].(map[string]any)
				if kind == "StateMachine" && data["eventType"] == "STATE_MACHINE_FINISH" && data["stopReason"] != nil {
					finish = true
				}
			}
			every := true
			for kind := range auditKinds {
				every = every && kinds[kind]
			}
			if every && finish {
				c.auditPage = true
			}
		}
	}
	return c
}

// The samples cover what section 13.2 of the design lists.
func TestSampleCoverage(t *testing.T) {
	raw, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	c := collectCoverage(raw, loadSamples(t))

	for _, name := range slices.Sorted(maps.Keys(raw.Components.Schemas)) {
		d := raw.Components.Schemas[name].Value.Discriminator
		if d == nil {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(d.Mapping)) {
			if !c.mapped[name+"/"+key] {
				t.Errorf("no valid sample of %s with %s %q read as %s", name, d.PropertyName, key,
					strings.TrimPrefix(d.Mapping[key].Ref, schemaRefPrefix))
			}
		}
	}

	scalarOrPair := []string{"string", "number", "boolean", "null", "pair"}
	want := map[string][2][]string{ // position -> valid forms, invalid forms
		"SimpleConditionDto.value":    {scalarOrPair, {"object", "triple"}},
		"LifecycleConditionDto.value": {scalarOrPair, {"object", "triple"}},
		"ArrayConditionDto.values[]":  {scalarOrPair, {"object", "triple"}},
		// Never an object or an array.
		"GroupKeyEntry.value": {{"string", "number", "boolean", "null"}, {"object", "pair"}},
	}
	for _, position := range slices.Sorted(maps.Keys(want)) {
		for i, verdict := range []string{"valid", "invalid"} {
			for _, form := range want[position][i] {
				if !c.forms[position+" "+verdict+" "+form] {
					t.Errorf("no %s sample with %s as %s", verdict, form, position)
				}
			}
		}
	}

	for _, p := range []string{"present", "null", "absent"} {
		if !c.preChecks[p] {
			t.Errorf("no valid function condition whose pre-check is %s", p)
		}
	}
	for _, b := range []string{createBody, importBody} {
		for _, form := range []string{"object", "array"} {
			if !c.bodies[b+" "+form] {
				t.Errorf("no valid %s sample of %s", form, b)
			}
		}
	}
	if !c.auditPage {
		t.Error("no valid audit page holding every audit member and a STATE_MACHINE_FINISH with a stop reason")
	}
	// kin accepts any value where a schema recurs; these samples show that
	// ValidateValue's walk refuses one in a group and in a group in a group.
	for name, depth := range map[string]int{
		"condition-group-holding-null":          1,
		"condition-group-holding-number":        1,
		"condition-group-holding-string":        1,
		"condition-group-in-group-holding-null": 2,
	} {
		if !c.groups[name][depth] {
			t.Errorf("no invalid sample %s.json whose group at depth %d holds a non-object condition", name, depth)
		}
	}
	for _, f := range []string{"unknown audit kind", "unknown event type", "added field"} {
		if !c.future[f] {
			t.Errorf("no future sample with an %s", f)
		}
	}
}
