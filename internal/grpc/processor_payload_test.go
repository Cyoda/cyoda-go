package grpc

import (
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The processor's answer payload is a DataPayload: type, data, meta. Its
// member names are exact — read leniently, a "Data" written by the processor
// would be applied as the entity's new data.
func TestApplyProcessorResponse_MemberNamesAreExact(t *testing.T) {
	entity := &spi.Entity{Data: []byte(`{"a":0}`)}
	for name, payload := range map[string]string{
		"case variant": `{"type":"TREE","Data":{"a":1}}`,
		"unknown":      `{"type":"TREE","data":{"a":1},"extra":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := applyProcessorResponse(entity, &ProcessingResponse{Payload: json.RawMessage(payload)})
			if !errors.Is(err, errProcessorPayload) {
				t.Fatalf("got entity %v, err %v; want errProcessorPayload", got, err)
			}
		})
	}
}

func TestApplyProcessorResponse_ReadsData(t *testing.T) {
	entity := &spi.Entity{Data: []byte(`{"a":0}`)}
	got, err := applyProcessorResponse(entity, &ProcessingResponse{
		Payload: json.RawMessage(`{"type":"TREE","data":{"a":1},"meta":{"source":"fixture"}}`)})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil || string(got.Data) != `{"a":1}` {
		t.Errorf("got %v", got)
	}
	for _, payload := range []string{`{"type":"TREE","data":null}`, `{"type":"TREE"}`} {
		got, err := applyProcessorResponse(entity, &ProcessingResponse{Payload: json.RawMessage(payload)})
		if err != nil || got != nil {
			t.Errorf("%s: got %v, %v; want no entity", payload, got, err)
		}
	}
}
