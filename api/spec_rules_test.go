package api

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/getkin/kin-openapi/openapi3"
)

const schemaRefPrefix = "#/components/schemas/"

// specSchema is one schema of the spec as a walk reaches it.
type specSchema struct {
	where string
	s     *openapi3.Schema
	// exempt is true for a schema under the `items` or `not` of an untyped
	// leaf, where a `type` list states a rule and generates nothing.
	exempt bool
}

// walkSpec returns every schema of the spec once: the component schemas by
// name, then every inline schema below them and below the operations. A $ref
// to a component schema is not followed; the component is listed by name.
func walkSpec(t *testing.T, doc *openapi3.T) []specSchema {
	t.Helper()
	var out []specSchema
	seen := map[*openapi3.Schema]bool{}
	var walk func(where string, ref *openapi3.SchemaRef, exempt bool)
	walk = func(where string, ref *openapi3.SchemaRef, exempt bool) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		if ref.Ref != "" && where != strings.TrimPrefix(ref.Ref, schemaRefPrefix) {
			return
		}
		s := ref.Value
		seen[s] = true
		out = append(out, specSchema{where: where, s: s, exempt: exempt})
		leaf := isUntypedLeaf(s)
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			walk(where+"."+name, s.Properties[name], exempt)
		}
		walk(where+"[]", s.Items, exempt || leaf)
		walk(where+"!", s.Not, exempt || leaf)
		walk(where+"{}", s.AdditionalProperties.Schema, exempt)
		for i, group := range []openapi3.SchemaRefs{s.AllOf, s.OneOf, s.AnyOf} {
			for j, b := range group {
				walk(where+"/"+[]string{"allOf", "oneOf", "anyOf"}[i]+"/"+strconv.Itoa(j), b, exempt)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		walk(name, &openapi3.SchemaRef{Ref: schemaRefPrefix + name, Value: doc.Components.Schemas[name].Value}, false)
	}
	content := func(where string, c openapi3.Content) {
		for _, mt := range slices.Sorted(maps.Keys(c)) {
			walk(where+" "+mt, c[mt].Schema, false)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(doc.Paths.Map())) {
		item := doc.Paths.Value(path)
		for method, op := range item.Operations() {
			where := method + " " + path
			for _, p := range op.Parameters {
				if p.Value != nil {
					walk(where+" param "+p.Value.Name, p.Value.Schema, false)
				}
			}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				content(where+" body", op.RequestBody.Value.Content)
			}
			for code, r := range op.Responses.Map() {
				if r.Value != nil {
					content(where+" "+code, r.Value.Content)
				}
			}
		}
	}
	for name, p := range doc.Components.Parameters {
		if p.Value != nil {
			walk("parameter "+name, p.Value.Schema, false)
		}
	}
	for name, r := range doc.Components.Responses {
		if r.Value != nil {
			content("response "+name, r.Value.Content)
		}
	}
	return out
}

func isUntypedLeaf(s *openapi3.Schema) bool {
	return (s.Type == nil || len(*s.Type) == 0) && len(s.AllOf) == 0 && len(s.OneOf) == 0 && len(s.AnyOf) == 0
}

// bases returns the component schemas that carry a discriminator outside a
// union, by name.
func bases(doc *openapi3.T) map[string]*openapi3.Schema {
	out := map[string]*openapi3.Schema{}
	for name, ref := range doc.Components.Schemas {
		s := ref.Value
		if s.Discriminator != nil && len(s.OneOf) == 0 && len(s.AnyOf) == 0 {
			out[name] = s
		}
	}
	return out
}

// includers returns, for each base, the component schemas whose allOf
// includes it.
func includers(doc *openapi3.T, bs map[string]*openapi3.Schema) map[string][]string {
	out := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		for _, part := range doc.Components.Schemas[name].Value.AllOf {
			if b := strings.TrimPrefix(part.Ref, schemaRefPrefix); part.Ref != "" && bs[b] != nil {
				out[b] = append(out[b], name)
			}
		}
	}
	return out
}

func mappingTargets(s *openapi3.Schema) []string {
	var out []string
	for _, m := range s.Discriminator.Mapping {
		out = append(out, strings.TrimPrefix(m.Ref, schemaRefPrefix))
	}
	sort.Strings(out)
	return out
}

// ownProperty returns the property a schema declares itself, directly or in
// an inline allOf part, or nil.
func ownProperty(s *openapi3.Schema, name string) *openapi3.SchemaRef {
	if p := s.Properties[name]; p != nil {
		return p
	}
	for _, part := range s.AllOf {
		if part.Ref == "" && part.Value != nil {
			if p := part.Value.Properties[name]; p != nil {
				return p
			}
		}
	}
	return nil
}

// Rule 1: polymorphism has one form — a base with a discriminator and a
// mapping, whose members include it with allOf and are exactly the mapping's
// targets. A union never carries a discriminator, and no schema includes two
// bases.
func TestRule1_EveryDiscriminatorIsOnABase(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, w := range walkSpec(t, doc) {
		d := w.s.Discriminator
		if d == nil {
			continue
		}
		if len(d.Mapping) == 0 {
			t.Errorf("%s: discriminator %q has no mapping", w.where, d.PropertyName)
		}
		if len(w.s.OneOf) > 0 || len(w.s.AnyOf) > 0 {
			t.Errorf("%s: a oneOf/anyOf carries a discriminator; refer to a base with $ref", w.where)
		}
	}
	bs := bases(doc)
	inc := includers(doc, bs)
	for _, name := range slices.Sorted(maps.Keys(bs)) {
		if got, want := inc[name], mappingTargets(bs[name]); !slices.Equal(got, want) {
			t.Errorf("%s: schemas that include it %v, mapping targets %v", name, got, want)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		n := 0
		for _, part := range doc.Components.Schemas[name].Value.AllOf {
			if bs[strings.TrimPrefix(part.Ref, schemaRefPrefix)] != nil {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s includes %d bases", name, n)
		}
	}
}

// Rule 5: the client generator reads every shape — no union of scalar types,
// no union other than a nullable reference, no inline enum on a property of a
// base, and no member that declares its base's discriminator property.
func TestRule5_GeneratorReadableShapes(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, w := range walkSpec(t, doc) {
		if w.s.Type != nil && !w.exempt {
			nonNull := 0
			for _, ty := range *w.s.Type {
				if ty != openapi3.TypeNull {
					nonNull++
				}
			}
			if nonNull > 1 {
				t.Errorf("%s: type list %v holds more than one non-null type", w.where, *w.s.Type)
			}
		}
		for kw, union := range map[string]openapi3.SchemaRefs{"oneOf": w.s.OneOf, "anyOf": w.s.AnyOf} {
			if len(union) == 0 {
				continue
			}
			if !isNullableRef(union) {
				t.Errorf("%s: %s is not [{type: null}, {$ref}]", w.where, kw)
			}
		}
	}
	bs := bases(doc)
	inc := includers(doc, bs)
	for _, name := range slices.Sorted(maps.Keys(bs)) {
		b := bs[name]
		for _, prop := range slices.Sorted(maps.Keys(b.Properties)) {
			if p := b.Properties[prop]; p.Ref == "" && len(p.Value.Enum) > 0 {
				t.Errorf("%s.%s: inline enum on a property of a base; name it", name, prop)
			}
		}
		for _, member := range inc[name] {
			if ownProperty(doc.Components.Schemas[member].Value, b.Discriminator.PropertyName) != nil {
				t.Errorf("%s declares %s, the discriminator of its base %s", member, b.Discriminator.PropertyName, name)
			}
		}
	}
}

func isNullableRef(union openapi3.SchemaRefs) bool {
	if len(union) != 2 {
		return false
	}
	var null, ref bool
	for _, b := range union {
		switch {
		case b.Ref != "":
			ref = true
		case b.Value != nil && b.Value.Type != nil && b.Value.Type.Is(openapi3.TypeNull):
			null = true
		}
	}
	return null && ref
}

// Rule 6: a value set that grows is a plain string. Every discriminator
// property is one, and so is the outer state-machine eventType.
func TestRule6_ValueSetsThatGrowAreStrings(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	bs := bases(doc)
	for _, name := range slices.Sorted(maps.Keys(bs)) {
		prop := bs[name].Discriminator.PropertyName
		p := bs[name].Properties[prop]
		if p == nil || p.Value == nil {
			t.Errorf("%s does not declare its discriminator %s", name, prop)
			continue
		}
		if p.Ref != "" || !p.Value.Type.Is(openapi3.TypeString) || len(p.Value.Enum) > 0 {
			t.Errorf("%s.%s: a discriminator is a plain string with no enum", name, prop)
		}
	}
	et := ownProperty(doc.Components.Schemas["StateMachineAuditEventDto"].Value, "eventType")
	if et == nil || et.Value == nil {
		t.Fatal("StateMachineAuditEventDto.eventType missing")
	}
	if et.Ref != "" || !et.Value.Type.Is(openapi3.TypeString) || len(et.Value.Enum) > 0 {
		t.Error("StateMachineAuditEventDto.eventType: a plain string with no enum; its known values are data's mapping keys")
	}
}

// Rule 6: a use of a base in a response is never a required property. A
// client reads an object of a member it does not know as empty, and
// kotlin-spring makes a required property non-null, so a required use would
// fail the whole response.
func TestRule6_UsesOfABaseInAResponseAreNotRequired(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, where := range requiredBaseUses(doc) {
		t.Errorf("%s: a use of a base in a response is required", where)
	}
}

// requiredBaseUses finds a required use of a base wherever a response
// reaches it, and nowhere else.
func TestRule6_RequiredBaseUsesAreFound(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData([]byte(`
openapi: 3.1.0
info: {title: fixture, version: "1"}
paths:
  /pets:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [pet, owner]
                properties:
                  pet: {$ref: "#/components/schemas/Pet"}
                  owner: {$ref: "#/components/schemas/Owner"}
                  shelters:
                    type: array
                    items: {$ref: "#/components/schemas/Shelter"}
    post:
      requestBody:
        content:
          application/json:
            schema: {$ref: "#/components/schemas/Adoption"}
      responses:
        "204": {description: ok}
components:
  schemas:
    Pet:
      type: object
      required: [kind]
      properties:
        kind: {type: string}
      discriminator:
        propertyName: kind
        mapping:
          cat: "#/components/schemas/Cat"
    Cat:
      allOf:
        - $ref: "#/components/schemas/Pet"
        - type: object
          required: [toy]
          properties:
            toy: {$ref: "#/components/schemas/Pet"}
    Owner:
      type: object
      properties:
        pet: {$ref: "#/components/schemas/Pet"}
        home:
          type: object
          required: [pet]
          properties:
            pet: {$ref: "#/components/schemas/Pet"}
    Shelter:
      allOf:
        - type: object
          properties:
            pet: {$ref: "#/components/schemas/Pet"}
      required: [pet]
    Adoption:
      type: object
      required: [pet]
      properties:
        pet: {$ref: "#/components/schemas/Pet"}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{
		"Cat.toy",                            // required in a member's allOf entry; reached through the base's mapping
		"GET /pets 200 application/json.pet", // inline in a response
		"Owner.home.pet",                     // inline in a component
		"Shelter.pet",                        // required by a component, declared in its allOf
	}
	if got := requiredBaseUses(doc); !slices.Equal(got, want) {
		t.Errorf("required uses %v, want %v", got, want)
	}
}

// responseSchema is one schema a response reaches. part is true for an
// inline allOf entry, whose required list and properties are checked with
// the schema that holds it.
type responseSchema struct {
	where string
	s     *openapi3.Schema
	part  bool
}

// requiredBaseUses returns, for every schema a response reaches, inline ones
// included, each property in its required list that is a $ref to a base.
func requiredBaseUses(doc *openapi3.T) []string {
	bs := bases(doc)
	var out []string
	for _, w := range responseSchemas(doc) {
		if w.part {
			continue
		}
		required := slices.Clone(w.s.Required)
		for _, part := range w.s.AllOf {
			if part.Ref == "" && part.Value != nil {
				required = append(required, part.Value.Required...)
			}
		}
		for _, prop := range required {
			if p := ownProperty(w.s, prop); p != nil && bs[strings.TrimPrefix(p.Ref, schemaRefPrefix)] != nil {
				out = append(out, w.where+"."+prop)
			}
		}
	}
	sort.Strings(out)
	return out
}

// responseSchemas returns every schema a response reaches, once: the inline
// schemas below the response content, and the component schemas a $ref or a
// discriminator mapping leads to, with the inline schemas below them.
func responseSchemas(doc *openapi3.T) []responseSchema {
	var out []responseSchema
	seen := map[*openapi3.Schema]bool{}
	var walk func(where string, ref *openapi3.SchemaRef, part bool)
	walk = func(where string, ref *openapi3.SchemaRef, part bool) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		if ref.Ref != "" {
			where, part = strings.TrimPrefix(ref.Ref, schemaRefPrefix), false
		}
		s := ref.Value
		seen[s] = true
		out = append(out, responseSchema{where: where, s: s, part: part})
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			walk(where+"."+name, s.Properties[name], false)
		}
		walk(where+"[]", s.Items, false)
		walk(where+"!", s.Not, false)
		walk(where+"{}", s.AdditionalProperties.Schema, false)
		for i, b := range s.AllOf {
			walk(where+"/allOf/"+strconv.Itoa(i), b, true)
		}
		for i, group := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf} {
			for j, b := range group {
				walk(where+"/"+[]string{"oneOf", "anyOf"}[i]+"/"+strconv.Itoa(j), b, false)
			}
		}
		if s.Discriminator != nil {
			for _, key := range slices.Sorted(maps.Keys(s.Discriminator.Mapping)) {
				m := s.Discriminator.Mapping[key]
				if target := doc.Components.Schemas[strings.TrimPrefix(m.Ref, schemaRefPrefix)]; target != nil {
					walk("", &openapi3.SchemaRef{Ref: m.Ref, Value: target.Value}, false)
				}
			}
		}
	}
	content := func(where string, c openapi3.Content) {
		for _, mt := range slices.Sorted(maps.Keys(c)) {
			walk(where+" "+mt, c[mt].Schema, false)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(doc.Paths.Map())) {
		ops := doc.Paths.Value(path).Operations()
		for _, method := range slices.Sorted(maps.Keys(ops)) {
			responses := ops[method].Responses.Map()
			for _, code := range slices.Sorted(maps.Keys(responses)) {
				if r := responses[code]; r.Value != nil {
					content(method+" "+path+" "+code, r.Value.Content)
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Responses)) {
		if r := doc.Components.Responses[name]; r.Value != nil {
			content("response "+name, r.Value.Content)
		}
	}
	return out
}

// Each base maps the wire value of its discriminator to its member.
func TestBaseMappings(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	want := map[string]map[string]string{
		"QueryConditionDto": {
			"array":     "ArrayConditionDto",
			"function":  "FunctionConditionDto",
			"group":     "GroupConditionDto",
			"lifecycle": "LifecycleConditionDto",
			"simple":    "SimpleConditionDto",
		},
		"AuditEventDto": {
			"EntityChange": "EntityChangeAuditEventDto",
			"StateMachine": "StateMachineAuditEventDto",
			"System":       "SystemAuditEventDto",
		},
		"ProcessorDefinitionDto": {
			"externalized": "ExternalizedProcessorDefinitionDto",
		},
		"StateMachineEventDto": {
			string(spi.SMEventStarted):                      "StateMachineStartedDto",
			string(spi.SMEventFinished):                     "StateMachineFinishedDto",
			string(spi.SMEventCancelled):                    "StateMachineCancelledDto",
			string(spi.SMEventForcedSuccess):                "StateMachineForcedSuccessDto",
			string(spi.SMEventWorkflowFound):                "StateMachineWorkflowFoundDto",
			string(spi.SMEventWorkflowNotFound):             "StateMachineWorkflowNotFoundDto",
			string(spi.SMEventWorkflowSkipped):              "StateMachineWorkflowSkippedDto",
			string(spi.SMEventTransitionMade):               "StateMachineTransitionMadeDto",
			string(spi.SMEventTransitionNotFound):           "StateMachineTransitionNotFoundDto",
			string(spi.SMEventTransitionCriterionNoMatch):   "StateMachineTransitionCriterionNotMatchedDto",
			string(spi.SMEventProcessCriterionNoMatch):      "StateMachineProcessCriterionNotMatchedDto",
			string(spi.SMEventProcessingPaused):             "StateMachineProcessingPausedDto",
			string(spi.SMEventStateProcessResult):           "StateMachineStateProcessResultDto",
			string(spi.SMEventScheduledTransitionArmed):     "StateMachineScheduledTransitionArmedDto",
			string(spi.SMEventScheduledTransitionFired):     "StateMachineScheduledTransitionFiredDto",
			string(spi.SMEventScheduledTransitionExpired):   "StateMachineScheduledTransitionExpiredDto",
			string(spi.SMEventScheduledTransitionCancelled): "StateMachineScheduledTransitionCancelledDto",
			string(spi.SMEventScheduledTransitionFailed):    "StateMachineScheduledTransitionFailedDto",
			// cyoda-go's own event type, declared in internal/domain/workflow.
			"TRANSITION_ABORTED": "StateMachineTransitionAbortedDto",
		},
	}
	bs := bases(doc)
	if got := slices.Sorted(maps.Keys(bs)); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
		t.Errorf("bases %v, want %v", got, slices.Sorted(maps.Keys(want)))
	}
	for name, w := range want {
		b := bs[name]
		if b == nil {
			continue
		}
		got := map[string]string{}
		for k, m := range b.Discriminator.Mapping {
			got[k] = strings.TrimPrefix(m.Ref, schemaRefPrefix)
		}
		if !maps.Equal(got, w) {
			t.Errorf("%s mapping %v, want %v", name, got, w)
		}
	}
}

// Every condition position is a use of QueryConditionDto. Where `function`
// is valid is said by each description and enforced by the server, not by
// the schema.
func TestConditionsAreUsesOfTheBase(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	const base = schemaRefPrefix + "QueryConditionDto"
	schema := func(name string) *openapi3.Schema { return doc.Components.Schemas[name].Value }
	body := func(method, path string) *openapi3.SchemaRef {
		item := doc.Paths.Find(path)
		if item == nil || item.GetOperation(method) == nil || item.GetOperation(method).RequestBody == nil {
			t.Fatalf("%s %s has no request body", method, path)
		}
		return item.GetOperation(method).RequestBody.Value.Content.Get("application/json").Schema
	}
	prop := func(s *openapi3.Schema, name string) *openapi3.SchemaRef {
		p := ownProperty(s, name)
		if p == nil {
			t.Fatalf("property %s missing", name)
		}
		return p
	}
	uses := map[string]*openapi3.SchemaRef{
		"POST /search/direct body":       body("POST", "/search/direct/{entityName}/{modelVersion}"),
		"POST /search/async body":        body("POST", "/search/async/{entityName}/{modelVersion}"),
		"DELETE /entity body":            body("DELETE", "/entity/{entityName}/{modelVersion}"),
		"GroupedStatsRequest.condition":  prop(schema("GroupedStatsRequest"), "condition"),
		"WorkflowConfigurationDto.crit":  prop(schema("WorkflowConfigurationDto"), "criterion"),
		"TransitionDefinitionDto.crit":   prop(schema("TransitionDefinitionDto"), "criterion"),
		"GroupConditionDto.conditions[]": prop(schema("GroupConditionDto"), "conditions").Value.Items,
	}
	for where, ref := range uses {
		if ref == nil || ref.Ref != base {
			t.Errorf("%s is not a $ref to QueryConditionDto", where)
		}
	}
	pre := prop(schema("ExternalizedFunctionDto"), "criterion").Value
	if !isNullableRef(pre.OneOf) || pre.Discriminator != nil {
		t.Error("ExternalizedFunctionDto.criterion is not oneOf [{type: null}, {$ref}] without a discriminator")
	}
	for _, b := range pre.OneOf {
		if b.Ref != "" && b.Ref != base {
			t.Errorf("ExternalizedFunctionDto.criterion refers to %s, want QueryConditionDto", b.Ref)
		}
	}
	for _, gone := range []string{"AbstractConditionDto", "CriterionGroupConditionDto"} {
		if doc.Components.Schemas[gone] != nil {
			t.Errorf("%s is still declared", gone)
		}
	}
	if op := schema("GroupConditionDto"); prop(op, "operator").Ref != schemaRefPrefix+"GroupOperator" {
		t.Error("GroupConditionDto.operator is not a $ref to GroupOperator")
	}
}

// No live text names a removed condition schema: every condition is a
// QueryConditionDto, and a group is a GroupConditionDto. The files that keep
// history are exempt. The walk covers what the repository holds — tracked
// files and new ones not yet added — and not what .gitignore keeps out of it
// (build output, local scratch).
func TestNoLiveMentionOfRemovedConditionSchemas(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	history := func(rel string) bool {
		switch rel {
		case "CHANGELOG.md", ".github/oasdiff-err-ignore.txt", "docs/cyoda/openapi.yml", "api/spec_rules_test.go":
			return true
		}
		return strings.HasPrefix(rel, "docs/release-notes/") || strings.HasPrefix(rel, "docs/superpowers/")
	}
	files := 0
	var offenders []string
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" || history(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) { // deleted in the working tree, not yet staged
				continue
			}
			t.Fatalf("read %s: %v", rel, err)
		}
		files++
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "AbstractConditionDto") || strings.Contains(line, "CriterionGroupConditionDto") {
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1))
			}
		}
	}
	if files == 0 {
		t.Fatal("no file was scanned: the walk looked in the wrong place")
	}
	for _, o := range offenders {
		t.Errorf("%s names a removed condition schema", o)
	}
}
