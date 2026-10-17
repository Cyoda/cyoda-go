package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// eventTypeConstants returns the value of every constant of type
// StateMachineEventType declared in the files, read from the Go source: one
// declared with the type (`X StateMachineEventType = "…"`) or as a
// conversion (`X = spi.StateMachineEventType("…")`). A constant that names
// another constant declares no new value and is skipped.
func eventTypeConstants(t *testing.T, files []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					value, typed := vs.Values[i], vs.Type != nil && isEventType(vs.Type)
					if call, ok := value.(*ast.CallExpr); ok && isEventType(call.Fun) && len(call.Args) == 1 {
						value, typed = call.Args[0], true
					}
					if !typed {
						continue
					}
					switch v := value.(type) {
					case *ast.Ident, *ast.SelectorExpr:
						continue
					case *ast.BasicLit:
						if v.Kind == token.STRING {
							s, err := strconv.Unquote(v.Value)
							if err != nil {
								t.Fatalf("%s: %s: %v", f, name.Name, err)
							}
							out[s] = filepath.Base(f) + ":" + name.Name
							continue
						}
					}
					t.Fatalf("%s: %s is not a string literal", f, name.Name)
				}
			}
		}
	}
	return out
}

func isEventType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "StateMachineEventType"
	case *ast.SelectorExpr:
		return x.Sel.Name == "StateMachineEventType"
	}
	return false
}

// moduleGoFiles returns every non-test .go file of the root module. The
// plugin submodules, vendored and hidden trees are not part of it.
func moduleGoFiles(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var files []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); path != root &&
				(strings.HasPrefix(name, ".") || name == "plugins" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

// The server sends no event type that the mapping of StateMachineEventDto
// lacks, and the mapping lists no type that nothing declares.
func TestEventTypeConstantsEqualTheMapping(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/cyoda-platform/cyoda-go-spi").Output()
	if err != nil {
		t.Fatalf("go list the SPI module: %v", err)
	}
	files := append([]string{filepath.Join(strings.TrimSpace(string(out)), "types.go")}, moduleGoFiles(t)...)
	declared := eventTypeConstants(t, files)

	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	mapped := map[string]bool{}
	for k := range doc.Components.Schemas["StateMachineEventDto"].Value.Discriminator.Mapping {
		mapped[k] = true
	}
	for _, v := range slices.Sorted(maps.Keys(declared)) {
		if !mapped[v] {
			t.Errorf("%s (%s) is not a key of StateMachineEventDto's mapping", v, declared[v])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(mapped)) {
		if _, ok := declared[k]; !ok {
			t.Errorf("mapping key %s is declared by no StateMachineEventType constant", k)
		}
	}
	if len(declared) != 19 {
		t.Errorf("%d event types declared, want 19: %v", len(declared), slices.Sorted(maps.Keys(declared)))
	}
}

// eventProp is one property of an event schema: whether it is required, and
// its shape as shapeOf spells it.
type eventProp struct {
	required bool
	shape    string
}

// shapeOf spells a property's schema: "$Name" for a reference, "[]" and the
// items' shape for an array, and the type with its format and enum otherwise.
func shapeOf(ref *openapi3.SchemaRef) string {
	if ref.Ref != "" {
		return "$" + strings.TrimPrefix(ref.Ref, schemaRefPrefix)
	}
	s := ref.Value
	if s.Type.Is(openapi3.TypeArray) {
		return "[]" + shapeOf(s.Items)
	}
	out := strings.Join(s.Type.Slice(), ",")
	if s.Format != "" {
		out += "/" + s.Format
	}
	if len(s.Enum) > 0 {
		var vs []string
		for _, v := range s.Enum {
			vs = append(vs, v.(string))
		}
		out += " enum " + strings.Join(vs, ",")
	}
	return out
}

// ownProps returns the properties a schema declares itself, beside what an
// included base declares, with the required list of the schema and of its
// inline parts.
func ownProps(s *openapi3.Schema) map[string]eventProp {
	out := map[string]eventProp{}
	required := map[string]bool{}
	parts := []*openapi3.Schema{s}
	for _, part := range s.AllOf {
		if part.Ref == "" && part.Value != nil {
			parts = append(parts, part.Value)
		}
	}
	for _, p := range parts {
		for _, r := range p.Required {
			required[r] = true
		}
	}
	for _, p := range parts {
		for name, ref := range p.Properties {
			out[name] = eventProp{required: required[name], shape: shapeOf(ref)}
		}
	}
	for r := range required {
		if _, ok := out[r]; !ok {
			out[r] = eventProp{required: true, shape: "(undeclared)"}
		}
	}
	return out
}

// The event schemas, as spec section 4 tabulates them.
func TestEventSchemasMatchTheDataTable(t *testing.T) {
	const (
		str  = "string"
		boo  = "boolean"
		fns  = "[]$CriterionFunctionResultDto"
		mode = "string enum SYNC,ASYNC_SAME_TX,ASYNC_NEW_TX,COMMIT_BEFORE_DISPATCH"
	)
	R := func(shape string) eventProp { return eventProp{required: true, shape: shape} }
	O := func(shape string) eventProp { return eventProp{shape: shape} }
	table := map[string]map[string]eventProp{
		"StateMachineStartedDto": {"state": O(str)},
		"StateMachineFinishedDto": {
			"state": R(str), "success": R(boo), "stopReason": R("$WorkflowStopReasonDto"),
		},
		"StateMachineCancelledDto":        {"reason": R(str)},
		"StateMachineForcedSuccessDto":    {},
		"StateMachineWorkflowFoundDto":    {"workflowName": R(str), "functions": O(fns)},
		"StateMachineWorkflowSkippedDto":  {"workflowName": R(str), "reason": R(str), "functions": O(fns)},
		"StateMachineWorkflowNotFoundDto": {},
		"StateMachineTransitionMadeDto": {
			"workflowName": R(str), "transition": R(str), "newState": R(str), "functions": O(fns),
		},
		"StateMachineTransitionNotFoundDto": {"workflowName": R(str), "transition": R(str)},
		"StateMachineTransitionCriterionNotMatchedDto": {
			"workflowName": R(str), "transition": R(str), "criterion": R(str), "reason": R(str),
			"functions": O(fns),
		},
		"StateMachineTransitionAbortedDto": {
			"reason": R(str), "transition": O(str), "expectedTxId": R(str), "actualTxId": R(str),
		},
		"StateMachineProcessCriterionNotMatchedDto": {
			"workflowName": R(str), "processorClassName": R(str), "criterion": R(str), "reason": O(str),
		},
		"StateMachineProcessingPausedDto": {
			"workflowName": R(str), "transition": R(str), "processNames": R("[]string"),
		},
		"StateMachineStateProcessResultDto": {
			"processorName": R(str), "executionMode": R(mode), "success": R(boo), "failReason": O(str),
		},
		"StateMachineScheduledTransitionArmedDto": {
			"transition": R(str), "sourceState": R(str), "scheduledTime": R("integer/int64"),
		},
		"StateMachineScheduledTransitionFiredDto": {
			"transition": R(str), "sourceState": R(str), "workflowName": R(str),
		},
		"StateMachineScheduledTransitionExpiredDto": {"transition": R(str), "sourceState": R(str)},
		"StateMachineScheduledTransitionCancelledDto": {
			"transition": R(str), "sourceState": R(str), "workflowName": O(str),
		},
		"StateMachineScheduledTransitionFailedDto": {
			"transition": R(str), "sourceState": R(str), "reason": R(str),
			"attempts": R("integer/int32"), "lostOwners": R("integer/int32"),
		},
		"CriterionFunctionResultDto": {
			"path": R(str), "name": R(str), "called": R(boo), "preCheck": O("$QueryConditionDto"),
			"preCheckMatched": O(boo), "matches": O(boo), "reason": O(str),
		},
	}
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	for _, name := range slices.Sorted(maps.Keys(table)) {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Errorf("%s missing", name)
			continue
		}
		got := ownProps(ref.Value)
		if !maps.Equal(got, table[name]) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, table[name])
		}
	}
	if doc.Components.Schemas["StateProcessIdDto"] != nil {
		t.Error("StateProcessIdDto is still declared")
	}
}

// FINISH's stop reason, as spec section 5 states it; the type enum keeps
// Cloud's values.
func TestStopReasonSchema(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}
	want := map[string]map[string]eventProp{
		"WorkflowStopReasonDto": {
			"type": {required: true, shape: "string enum COMPLETED_NO_AUTO_TRANSITIONS,COMPLETED_ALL_CRITERIA_REJECTED," +
				"PAUSED_FOR_PROCESSING,CANCELED,NO_WORKFLOW_FOUND,WORKFLOW_SKIPPED,FORCE_SUCCESS," +
				"INCOMPLETE_TRANSACTION,UNKNOWN"},
			"description":         {required: true, shape: "string"},
			"rejectedTransitions": {shape: "[]$RejectedTransitionCriterionDto"},
		},
		"RejectedTransitionCriterionDto": {
			"transitionName": {required: true, shape: "string"},
			"criterion":      {required: true, shape: "string"},
			"reason":         {required: true, shape: "string"},
		},
	}
	for name, w := range want {
		ref := doc.Components.Schemas[name]
		if ref == nil || ref.Value == nil {
			t.Errorf("%s missing", name)
			continue
		}
		if got := ownProps(ref.Value); !maps.Equal(got, w) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, w)
		}
	}
}

// eventDataUse returns the viewed schema of StateMachineAuditEventDto.data,
// which must be a use of the base StateMachineEventDto: the view replaces a
// use with a oneOf of the base's members.
func eventDataUse(t *testing.T, doc *openapi3.T) *openapi3.Schema {
	t.Helper()
	data := ownProperty(doc.Components.Schemas["StateMachineAuditEventDto"].Value, "data")
	if data == nil || data.Value == nil {
		t.Fatal("StateMachineAuditEventDto.data missing")
	}
	if len(data.Value.OneOf) == 0 || data.Value.Discriminator == nil {
		t.Fatal("StateMachineAuditEventDto.data is not a use of StateMachineEventDto")
	}
	return data.Value
}

// The two event types cyoda-go never sends keep schemas a client can read.
func TestNeverSentEventsHaveValidSamples(t *testing.T) {
	doc := viewedSpec(t)
	data := eventDataUse(t, doc)
	validateCases(t, doc, []validateCase{
		{"PROCESS_NOT_MATCH_CRITERION", data, `{
			"eventType": "PROCESS_NOT_MATCH_CRITERION",
			"workflowName": "orders",
			"processorClassName": "com.example.Enrich",
			"criterion": "simple",
			"reason": "amount below threshold"
		}`, true},
		{"PROCESS_NOT_MATCH_CRITERION without criterion", data, `{
			"eventType": "PROCESS_NOT_MATCH_CRITERION",
			"workflowName": "orders",
			"processorClassName": "com.example.Enrich"
		}`, false},
		{"WORKFLOW_NOT_FOUND", data, `{"eventType": "WORKFLOW_NOT_FOUND"}`, true},
	})
}

// data is never null. It is not required: Rule 6 lets a client read an event
// of a type it does not know, and a required use of a base would refuse it.
// That the server always sends data is asserted by the engine and e2e tests,
// not by the schema.
func TestDataIsNeverNull(t *testing.T) {
	doc := viewedSpec(t)
	event := doc.Components.Schemas["StateMachineAuditEventDto"].Value
	sample := func(data string) string {
		v := map[string]any{
			"auditEventType": "StateMachine",
			"eventId":        "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b",
			"eventType":      "FORCE_SUCCESS",
			"severity":       "INFO",
			"utcTime":        "2026-10-15T10:00:00Z",
			"microsTime":     1760522400000000,
		}
		if data != "" {
			v["data"] = json.RawMessage(data)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	validateCases(t, doc, []validateCase{
		{"typed data", event, sample(`{"eventType": "FORCE_SUCCESS"}`), true},
		{"null data", event, sample(`null`), false},
		{"absent data", event, sample(""), true},
	})
}
