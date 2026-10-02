package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// TestSlowConfigurable_SleepsForSleepMS verifies the slow-configurable
// processor actually sleeps for sleep_ms. The server delivers the
// processor's context as a JSON string in parameters (dispatch.go passes
// req.Parameters through unchanged), so the config must be unwrapped from
// its string encoding before sleep_ms can be read.
func TestSlowConfigurable_SleepsForSleepMS(t *testing.T) {
	fn, ok := newCatalog(nil, nil).processor("slow-configurable")
	if !ok {
		t.Fatal("slow-configurable is not in the catalog")
	}
	cfg, _ := json.Marshal(`{"sleep_ms": 120}`)
	start := time.Now()
	if _, err := fn(context.Background(), &Entity{ID: "e"}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("slept %v, want at least 100ms", d)
	}
}

// TestRecordAuthType_RecordsBothPrincipals verifies that record-authtype
// writes the auth context the calc request carried into the entity data: the
// attributed principal (authtype, authid) and the executor (authexectype,
// authexecid).
func TestRecordAuthType_RecordsBothPrincipals(t *testing.T) {
	d := &dispatcher{cat: newCatalog(nil, nil)}
	msg, err := newCloudEvent(ceTypeProcessorRequest, map[string]any{
		"requestId": "r-1", "entityId": "e-1", "processorName": "record-authtype",
		"payload": map[string]any{"data": map[string]any{"name": "x"}},
	})
	if err != nil {
		t.Fatalf("newCloudEvent: %v", err)
	}
	msg.Attributes = map[string]*cepb.CloudEvent_CloudEventAttributeValue{}
	for k, v := range map[string]string{"authtype": "user", "authid": "alice", "authexectype": "service", "authexecid": "C9"} {
		msg.Attributes[k] = &cepb.CloudEvent_CloudEventAttributeValue{Attr: &cepb.CloudEvent_CloudEventAttributeValue_CeString{CeString: v}}
	}
	payload, err := extractTextData(msg)
	if err != nil {
		t.Fatalf("extractTextData: %v", err)
	}

	reply, err := d.answer(context.Background(), msg, payload, "")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	var body struct {
		Success bool `json:"success"`
		Payload struct {
			Data map[string]any `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(reply.GetTextData()), &body); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	if !body.Success {
		t.Fatalf("reply success = false: %s", reply.GetTextData())
	}
	for field, want := range map[string]string{
		"observedAuthType": "user", "observedAuthID": "alice",
		"observedAuthExecType": "service", "observedAuthExecID": "C9",
	} {
		if got, _ := body.Payload.Data[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}
