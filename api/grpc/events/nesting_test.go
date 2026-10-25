package events_test

import (
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go-spi/strictjson"
	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// On gRPC an event's data is read as one JSON document through strictjson,
// as the handlers decode it, so the 10 000-deep nesting limit counts from the
// root of the event's data: an EntityCreateRequest's entity data sits inside
// two objects, the event's root and its payload.
func TestEventDecode_NestingLimitCountsFromTheEventDataRoot(t *testing.T) {
	event := func(depth int) []byte {
		arrays := strings.Repeat("[", depth) + strings.Repeat("]", depth)
		return []byte(`{"id":"e","dataFormat":"JSON","payload":{"model":{"name":"m","version":1},"data":` + arrays + `}}`)
	}
	var req events.EntityCreateRequestJson
	if err := strictjson.Decode(event(9998), &req); err != nil {
		t.Errorf("data nested 9 998 deep (10 000 from the root): %v; want decoded", err)
	}
	err := strictjson.Decode(event(9999), &req)
	if err == nil || !strings.Contains(err.Error(), "exceeded max depth") {
		t.Errorf("data nested 9 999 deep (10 001 from the root): %v; want the nesting-limit refusal", err)
	}
}
