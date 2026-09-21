package cyodaschemas

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// readSchema parses one embedded schema.
func readSchema(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := FS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return obj
}

// The criteria response's `matches` is the verdict on a criterion, and a
// criterion decides a transition. A response that is not an explicit failure
// and carries no verdict is refused (internal/grpc: the decode keeps the
// absence, the read reports an answer that could not be read), so this tree —
// the single source of truth an SDK is generated from — has to say it is
// required, and has to say it of every answer but an explicit `success: false`
// one: only a failure carries no verdict.
//
// `success` is optional with the default `true` (common/BaseEvent.json), so the
// condition is written as "not an explicit false" rather than "an explicit
// true": an answer omitting the field is a successful one and owes a verdict
// like any other.
func TestCriteriaResponse_MatchesRequiredOnSuccess(t *testing.T) {
	const path = "processing/EntityCriteriaCalculationResponse.json"
	schema := readSchema(t, path)

	props, _ := schema["properties"].(map[string]any)
	matches, ok := props["matches"].(map[string]any)
	if !ok {
		t.Fatalf("%s declares no `matches` property", path)
	}
	// Typed boolean, which is also what refuses an explicit null: a null
	// verdict is no more a verdict than an absent one.
	if matches["type"] != "boolean" {
		t.Errorf("`matches` type = %v; want boolean, so an explicit null is not a verdict", matches["type"])
	}

	// NOT unconditionally required: an error answer has no verdict to give.
	for _, r := range toStrings(schema["required"]) {
		if r == "matches" {
			t.Errorf("%s requires `matches` unconditionally; a success:false answer carries none", path)
		}
	}

	// Required unless the wire says failure. The `if` must be the negation of
	// "success is present and false", and must require `success` inside that
	// negation — otherwise an answer omitting the field escapes the clause and
	// the one shape this exists to refuse, an answer with neither field, would
	// validate.
	cond, ok := schema["if"].(map[string]any)
	if !ok {
		t.Fatalf("%s states no condition under which `matches` is required", path)
	}
	failed, ok := cond["not"].(map[string]any)
	if !ok {
		t.Fatalf("the condition is not the negation of a failure answer: if = %v", cond)
	}
	condProps, _ := failed["properties"].(map[string]any)
	success, _ := condProps["success"].(map[string]any)
	if success["const"] != false {
		t.Errorf("the negated condition tests success = %v; want the const false", success["const"])
	}
	if !contains(toStrings(failed["required"]), "success") {
		t.Error("the negated condition does not require `success` itself, so every answer would count as a failure")
	}
	then, ok := schema["then"].(map[string]any)
	if !ok {
		t.Fatalf("%s has an `if` with no `then`", path)
	}
	if !contains(toStrings(then["required"]), "matches") {
		t.Errorf("a successful response is not required to carry `matches`: then.required = %v", then["required"])
	}
}

// The clause above, read by a validator rather than by shape: what an SDK
// generated from this tree, and a compute-member author checking a response
// against it, actually get. The structural test says how the clause is
// written; this says what it decides.
func TestCriteriaResponse_MatchesRequiredValidation(t *testing.T) {
	const path = "processing/EntityCriteriaCalculationResponse.json"
	raw, err := FS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(path, doc); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	schema, err := compiler.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}

	// Every instance carries the two unconditionally required fields, so what
	// each case turns on is the verdict clause alone.
	const ids = `"requestId":"r-1","entityId":"1b4e28ba-2fa1-11d2-883f-0016d3cca427"`
	for name, tc := range map[string]struct {
		instance string
		want     bool
	}{
		"no success, a verdict":       {`{` + ids + `,"matches":true}`, true},
		"no success, a refusal":       {`{` + ids + `,"matches":false,"reason":"too small"}`, true},
		"no success, no verdict":      {`{` + ids + `}`, false},
		"success true, a verdict":     {`{` + ids + `,"success":true,"matches":true}`, true},
		"success true, no verdict":    {`{` + ids + `,"success":true}`, false},
		"success false, no verdict":   {`{` + ids + `,"success":false,"error":{"code":"E","message":"boom"}}`, true},
		"success false, with verdict": {`{` + ids + `,"success":false,"matches":true}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.instance))
			if err != nil {
				t.Fatalf("instance is not valid JSON: %v", err)
			}
			err = schema.Validate(inst)
			if tc.want && err != nil {
				t.Errorf("%s is refused: %v", tc.instance, err)
			}
			if !tc.want && err == nil {
				t.Errorf("%s validates; a response that is not an explicit failure owes a verdict", tc.instance)
			}
		})
	}
}

// toStrings reads a JSON array of strings.
func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
