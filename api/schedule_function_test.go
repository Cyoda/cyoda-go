package api

import (
	"encoding/json"
	"testing"
)

// TestScheduleFunctionDtoSchema asserts ScheduleFunctionDto is typed-but-open
// (never additionalProperties:false) with name/resultKind/calculationNodesTags
// required, and that TransitionScheduleDto.function refs it while delayMs
// stays optional (the delayMs/function XOR is enforced server-side, not by
// the schema).
func TestScheduleFunctionDtoSchema(t *testing.T) {
	doc, err := GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}

	sfd := doc.Components.Schemas["ScheduleFunctionDto"]
	if sfd == nil || sfd.Value == nil {
		t.Fatal("ScheduleFunctionDto schema missing")
	}
	schema := sfd.Value

	req := map[string]bool{}
	for _, r := range schema.Required {
		req[r] = true
	}
	for _, want := range []string{"name", "resultKind", "calculationNodesTags"} {
		if !req[want] {
			t.Errorf("ScheduleFunctionDto.required missing %q", want)
		}
	}
	for _, want := range []string{"name", "resultKind", "calculationNodesTags", "attachEntity", "context", "responseTimeoutMs"} {
		if schema.Properties[want] == nil {
			t.Errorf("ScheduleFunctionDto.properties missing %q", want)
		}
	}
	if schema.AdditionalProperties.Has != nil && !*schema.AdditionalProperties.Has {
		t.Error("ScheduleFunctionDto must be typed-but-open, never additionalProperties:false")
	}

	tsd := doc.Components.Schemas["TransitionScheduleDto"]
	if tsd == nil || tsd.Value == nil {
		t.Fatal("TransitionScheduleDto schema missing")
	}
	tsdSchema := tsd.Value
	fnRef := tsdSchema.Properties["function"]
	if fnRef == nil || fnRef.Value == nil {
		t.Fatal("TransitionScheduleDto.function missing or unresolved")
	}
	if fnRef.Ref != "#/components/schemas/ScheduleFunctionDto" {
		t.Errorf("TransitionScheduleDto.function must $ref ScheduleFunctionDto, got ref %q", fnRef.Ref)
	}
	tsdReq := map[string]bool{}
	for _, r := range tsdSchema.Required {
		tsdReq[r] = true
	}
	if tsdReq["delayMs"] {
		t.Error("TransitionScheduleDto.delayMs must not be schema-required — it's mutually exclusive with function, enforced server-side")
	}
}

// TestScheduleFunctionDtoJSONRoundTrip asserts the generated Go mirror struct
// marshals/unmarshals with the expected json tags.
func TestScheduleFunctionDtoJSONRoundTrip(t *testing.T) {
	attach := true
	timeout := int64(5000)
	fn := ScheduleFunctionDto{
		Name:                 "computeNextFireTime",
		ResultKind:           ScheduleFunctionDtoResultKind("Schedule"),
		CalculationNodesTags: "billing",
		AttachEntity:         &attach,
		Context:              strPtr("role=nightly"),
		ResponseTimeoutMs:    &timeout,
	}

	b, err := json.Marshal(fn)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"name", "resultKind", "calculationNodesTags", "attachEntity", "context", "responseTimeoutMs"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("marshaled JSON missing key %q: %s", key, b)
		}
	}

	var back ScheduleFunctionDto
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal into ScheduleFunctionDto: %v", err)
	}
	if back.Name != fn.Name || back.ResultKind != fn.ResultKind || back.CalculationNodesTags != fn.CalculationNodesTags {
		t.Errorf("round-trip mismatch: got %+v, want %+v", back, fn)
	}

	// TransitionScheduleDto.function embeds ScheduleFunctionDto; delayMs is a
	// pointer so it can be omitted when function is set.
	sched := TransitionScheduleDto{Function: &fn}
	sb, err := json.Marshal(sched)
	if err != nil {
		t.Fatalf("Marshal TransitionScheduleDto: %v", err)
	}
	var schedRaw map[string]any
	if err := json.Unmarshal(sb, &schedRaw); err != nil {
		t.Fatalf("Unmarshal TransitionScheduleDto: %v", err)
	}
	if _, ok := schedRaw["delayMs"]; ok {
		t.Errorf("delayMs must be omitted when unset: %s", sb)
	}
	if _, ok := schedRaw["function"]; !ok {
		t.Errorf("function must be present: %s", sb)
	}
}

func strPtr(s string) *string { return &s }
