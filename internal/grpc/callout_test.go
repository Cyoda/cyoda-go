package grpc

import (
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func requestAsMap(t *testing.T, call Callout, requestID string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(call.buildRequest(requestID))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return m
}

func TestNewProcessorCallout(t *testing.T) {
	entity := testEntity()
	processor := spi.ProcessorDefinition{
		Name: "charge",
		Config: spi.ProcessorConfig{
			AttachEntity: true, CalculationNodesTags: "python,ml", ResponseTimeoutMs: 1500, Context: "role-a",
		},
	}
	call := NewProcessorCallout(testTenantID, entity, processor, "wf1", "t1", "tx-1")

	if call.Kind != ProcessorCallout || call.Kind.String() != "processor" {
		t.Errorf("Kind = %v", call.Kind)
	}
	if call.Name != "charge" || call.TenantID != testTenantID || call.Tags != "python,ml" ||
		call.ResponseTimeoutMs != 1500 || call.TxID != "tx-1" || call.EntityID != entity.Meta.ID {
		t.Errorf("descriptive fields wrong: %+v", call)
	}
	if call.RepeatSafe {
		t.Error("a processor is not repeat-safe unless its owner says so")
	}
	if call.eventType != EntityProcessorCalculationRequest {
		t.Errorf("eventType = %s", call.eventType)
	}

	req := requestAsMap(t, call, "rid-7")
	if req["id"] != "rid-7" || req["requestId"] != "rid-7" {
		t.Errorf("id/requestId = %v/%v, want the request id given", req["id"], req["requestId"])
	}
	if req["processorName"] != "charge" || req["parameters"] != "role-a" || req["transactionId"] != "tx-1" {
		t.Errorf("request fields wrong: %v", req)
	}
	if _, ok := req["payload"]; !ok {
		t.Error("attachEntity=true must attach the payload")
	}

	// the mapper keeps today's three cases
	same, err := call.mapResponse(&ProcessingResponse{Success: true})
	if err != nil || same.Entity != entity {
		t.Errorf("no payload: got (%v, %v), want the entity unchanged", same.Entity, err)
	}
	updated, err := call.mapResponse(&ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":{"foo":"new"}}`)})
	if err != nil || string(updated.Entity.Data) != `{"foo":"new"}` || updated.Entity.Meta.ID != entity.Meta.ID {
		t.Errorf("payload: got (%+v, %v)", updated.Entity, err)
	}
	if _, err := call.mapResponse(&ProcessingResponse{Success: true, Payload: json.RawMessage(`{"data":`)}); err == nil {
		t.Error("a payload that does not parse must be an error")
	}
}

func TestNewProcessorCallout_NoContextNoPayload(t *testing.T) {
	call := NewProcessorCallout(testTenantID, testEntity(), testProcessor("python", 0), "wf1", "t1", "tx-1")
	req := requestAsMap(t, call, "rid")
	if _, ok := req["parameters"]; ok {
		t.Error("an empty context must omit parameters")
	}
	if _, ok := req["payload"]; ok {
		t.Error("attachEntity=false must omit the payload")
	}
}

func TestNewCriteriaCallout(t *testing.T) {
	criterion := json.RawMessage(`{"type":"function","function":{"name":"amount-check",
		"config":{"calculationNodesTags":"python","responseTimeoutMs":700,"context":"ctx-1"}}}`)
	call, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf1", "t1", "proc-1", "tx-1")
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if call.Kind != CriteriaCallout || call.Name != "amount-check" || call.Tags != "python" || call.ResponseTimeoutMs != 700 {
		t.Errorf("descriptive fields wrong: %+v", call)
	}
	if !call.RepeatSafe {
		t.Error("a criterion is repeat-safe by rule")
	}
	req := requestAsMap(t, call, "rid-9")
	if req["id"] != "rid-9" || req["requestId"] != "rid-9" || req["criteriaName"] != "amount-check" ||
		req["target"] != "transition" || req["parameters"] != "ctx-1" {
		t.Errorf("request fields wrong: %v", req)
	}
	if _, ok := req["payload"]; !ok {
		t.Error("attachEntity defaults to true for a criterion")
	}
	if proc, _ := req["processor"].(map[string]any); proc["name"] != "proc-1" {
		t.Errorf("processor = %v", req["processor"])
	}

	yes := true
	got, err := call.mapResponse(&ProcessingResponse{Success: true, Matches: &yes, Reason: "big"})
	if err != nil || !got.Matches || got.Reason != "big" {
		t.Errorf("mapResponse = (%+v, %v)", got, err)
	}
	got, err = call.mapResponse(&ProcessingResponse{Success: true, Reason: "none given"})
	if err != nil || got.Matches || got.Reason != "none given" {
		t.Errorf("absent matches must read as false: (%+v, %v)", got, err)
	}
}

func TestNewCriteriaCallout_InvalidJSONIsTerminal(t *testing.T) {
	_, failure := NewCriteriaCallout(testTenantID, testEntity(), json.RawMessage(`{"function":`), "transition", "wf1", "t1", "", "tx-1")
	if failure == nil || failure.Kind != contract.Terminal {
		t.Fatalf("failure = %+v, want Terminal", failure)
	}
	// A criterion is a workflow configuration value, not this node's or a
	// compute member's fault; the client sees a fixed, sanitized message,
	// never the raw json parse error (see
	// TestNewCriteriaCallout_InvalidJSON_NoMarkerLeak for why).
	const wantMsg = "the workflow's criterion function could not be parsed"
	if failure.Message != wantMsg || failure.Error() != wantMsg {
		t.Errorf("Message/Error = %q/%q, want %q", failure.Message, failure.Error(), wantMsg)
	}
}

func TestNewFunctionCallout(t *testing.T) {
	fn := spi.ScheduleFunction{Name: "calcFire", ResultKind: "Schedule", CalculationNodesTags: "sched", AttachEntity: true, ResponseTimeoutMs: 900, Context: "c"}
	call := NewFunctionCallout(testTenantID, testEntity(), fn, "wf1", "t1", "tx-1")
	if call.Kind != FunctionCallout || call.Name != "calcFire" || call.Tags != "sched" || call.ResponseTimeoutMs != 900 || !call.RepeatSafe {
		t.Errorf("descriptive fields wrong: %+v", call)
	}
	req := requestAsMap(t, call, "rid-3")
	if req["id"] != "rid-3" || req["requestId"] != "rid-3" || req["functionName"] != "calcFire" || req["parameters"] != "c" {
		t.Errorf("request fields wrong: %v", req)
	}
	got, err := call.mapResponse(&ProcessingResponse{Success: true, ResultKind: "Schedule", Result: json.RawMessage(`{"fireAfterMs":1000}`)})
	if err != nil || got.Function.Kind != "Schedule" || string(got.Function.Value) != `{"fireAfterMs":1000}` {
		t.Errorf("mapResponse = (%+v, %v)", got, err)
	}
}

func TestCallout_SourceIsWhatItWasBuiltFrom(t *testing.T) {
	entity := testEntity()
	processor := testProcessor("python", 1500)
	criterion := json.RawMessage(`{"type":"function","function":{"name":"c","config":{"calculationNodesTags":"python"}}}`)
	fn := spi.ScheduleFunction{Name: "f", CalculationNodesTags: "python"}

	p := NewProcessorCallout(testTenantID, entity, processor, "wf1", "t1", "tx-1")
	if p.Source.Entity != entity || p.Source.WorkflowName != "wf1" || p.Source.TransitionName != "t1" ||
		p.Source.Processor == nil || p.Source.Processor.Name != processor.Name {
		t.Errorf("processor source = %+v", p.Source)
	}
	if p.Source.Criterion != nil || p.Source.Function != nil {
		t.Errorf("a processor callout carries only a processor: %+v", p.Source)
	}

	c, failure := NewCriteriaCallout(testTenantID, entity, criterion, "transition", "wf1", "t1", "proc-1", "tx-1")
	if failure != nil {
		t.Fatalf("NewCriteriaCallout: %v", failure)
	}
	if c.Source.Entity != entity || string(c.Source.Criterion) != string(criterion) ||
		c.Source.Target != "transition" || c.Source.ProcessorName != "proc-1" ||
		c.Source.WorkflowName != "wf1" || c.Source.TransitionName != "t1" {
		t.Errorf("criteria source = %+v", c.Source)
	}
	if c.Source.Processor != nil || c.Source.Function != nil {
		t.Errorf("a criteria callout carries only a criterion: %+v", c.Source)
	}

	f := NewFunctionCallout(testTenantID, entity, fn, "wf1", "t1", "tx-1")
	if f.Source.Entity != entity || f.Source.Function == nil || f.Source.Function.Name != "f" ||
		f.Source.WorkflowName != "wf1" || f.Source.TransitionName != "t1" {
		t.Errorf("function source = %+v", f.Source)
	}
	if f.Source.Processor != nil || f.Source.Criterion != nil || f.Source.Target != "" || f.Source.ProcessorName != "" {
		t.Errorf("a function callout carries only a function: %+v", f.Source)
	}
}
