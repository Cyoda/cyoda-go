package events_test

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// TestEntityFunctionCalculationResponseJson_RoundTrip pins the wire shape
// of the Function callout response: a `result` (raw JSON object) plus a
// `resultKind` discriminator string, alongside the common
// requestId/entityId/success/warnings/error envelope shared with the
// criteria callout response.
func TestEntityFunctionCalculationResponseJson_RoundTrip(t *testing.T) {
	result := json.RawMessage(`{"fireAt":1}`)
	resultKind := "Schedule"

	resp := events.EntityFunctionCalculationResponseJson{
		ID:         "00000000-0000-0000-0000-000000000000",
		RequestID:  "r",
		EntityID:   "e",
		Success:    true,
		Result:     &result,
		ResultKind: &resultKind,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got events.EntityFunctionCalculationResponseJson
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.RequestID != "r" {
		t.Errorf("RequestID = %q, want %q", got.RequestID, "r")
	}
	if got.EntityID != "e" {
		t.Errorf("EntityID = %q, want %q", got.EntityID, "e")
	}
	if !got.Success {
		t.Errorf("Success = false, want true")
	}
	if got.Result == nil {
		t.Fatal("Result is nil, want the raw object to survive round-trip")
	}
	if !json.Valid(*got.Result) {
		t.Errorf("Result is not valid JSON: %s", *got.Result)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(*got.Result, &decoded); err != nil {
		t.Fatalf("Result did not decode as a JSON object: %v", err)
	}
	if _, ok := decoded["fireAt"]; !ok {
		t.Errorf("Result missing fireAt key: %v", decoded)
	}
	if got.ResultKind == nil || *got.ResultKind != "Schedule" {
		t.Errorf("ResultKind = %v, want %q", got.ResultKind, "Schedule")
	}
}

// TestEntityFunctionCalculationRequestJson_RoundTrip pins the wire shape of
// the Function callout request, mirroring EntityCriteriaCalculationRequestJson
// but naming its callout target functionId/functionName rather than
// criteriaId/criteriaName.
func TestEntityFunctionCalculationRequestJson_RoundTrip(t *testing.T) {
	req := events.EntityFunctionCalculationRequestJson{
		ID:           "00000000-0000-0000-0000-000000000000",
		RequestID:    "r",
		EntityID:     "e",
		FunctionID:   "fn-1",
		FunctionName: "computeFireAt",
		Workflow: events.WorkflowInfoJson{
			ID:   "00000000-0000-0000-0000-000000000001",
			Name: "order",
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got events.EntityFunctionCalculationRequestJson
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.FunctionID != "fn-1" {
		t.Errorf("FunctionID = %q, want %q", got.FunctionID, "fn-1")
	}
	if got.FunctionName != "computeFireAt" {
		t.Errorf("FunctionName = %q, want %q", got.FunctionName, "computeFireAt")
	}
	if got.Workflow.Name != "order" {
		t.Errorf("Workflow.Name = %q, want %q", got.Workflow.Name, "order")
	}
}
