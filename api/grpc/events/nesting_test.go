package events_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// On gRPC an event's data is read as one JSON document, so the 10 000-deep
// nesting limit counts from the root of the event's data: the entity data of
// an EntityCreateRequest sits two objects down (payload, data).
func TestEventDecode_NestingLimitCountsFromTheEventDataRoot(t *testing.T) {
	event := func(depth int) []byte {
		arrays := strings.Repeat("[", depth) + strings.Repeat("]", depth)
		return []byte(`{"id":"e","dataFormat":"JSON","payload":{"model":{"name":"m","version":1},"data":` + arrays + `}}`)
	}
	var req events.EntityCreateRequestJson
	if err := json.Unmarshal(event(9998), &req); err != nil {
		t.Errorf("data nested 9 998 deep (10 000 from the root): %v; want decoded", err)
	}
	if err := json.Unmarshal(event(9999), &req); err == nil {
		t.Error("data nested 9 999 deep (10 001 from the root) decoded; want refused")
	}
}
