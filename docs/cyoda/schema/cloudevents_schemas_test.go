package cyodaschemas

import (
	"encoding/json"
	"testing"
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
// criterion decides a transition. A successful response without it is refused
// (internal/grpc: the decode keeps the absence, the read reports an answer that
// could not be read), so this tree — the single source of truth an SDK is
// generated from — has to say it is required, and has to say it only of a
// successful response: a `success: false` answer carries no verdict.
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

	// Required when the wire says success. The `if` must require `success`
	// itself, or an answer that omits it satisfies the clause vacuously and
	// `matches` is demanded of a response the server reads as a failure.
	cond, ok := schema["if"].(map[string]any)
	if !ok {
		t.Fatalf("%s states no condition under which `matches` is required", path)
	}
	condProps, _ := cond["properties"].(map[string]any)
	success, _ := condProps["success"].(map[string]any)
	if success["const"] != true {
		t.Errorf("the condition tests success = %v; want the const true", success["const"])
	}
	if !contains(toStrings(cond["required"]), "success") {
		t.Error("the condition does not require `success` itself, so a response omitting it would demand a verdict")
	}
	then, ok := schema["then"].(map[string]any)
	if !ok {
		t.Fatalf("%s has an `if` with no `then`", path)
	}
	if !contains(toStrings(then["required"]), "matches") {
		t.Errorf("a successful response is not required to carry `matches`: then.required = %v", then["required"])
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
