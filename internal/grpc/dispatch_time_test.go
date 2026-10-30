package grpc

import (
	"context"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// A processor, criterion or schedule-function request carries the platform
// clock's time as the CloudEvents time attribute.
func TestDispatch_CalloutCarriesClockTime(t *testing.T) {
	at := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), nil, time.Second, time.Second, time.Second, fixedClock{at})
	call := Callout{
		Kind: ProcessorCallout, Name: "p", AnswerLimit: time.Second, RequestID: "r1",
		eventType:    EntityProcessorCalculationRequest,
		buildRequest: func(id string) any { return map[string]any{"requestId": id} },
	}
	ce, err := d.buildCalloutEvent(call)
	if err != nil {
		t.Fatalf("buildCalloutEvent: %v", err)
	}
	assertEventTime(t, ce, at)
}

func assertEventTime(t *testing.T, ce *cepb.CloudEvent, want time.Time) {
	t.Helper()
	a, ok := ce.Attributes["time"]
	if !ok {
		t.Fatalf("no time attribute on %s", ce.Type)
	}
	got := a.GetCeTimestamp().AsTime()
	if !got.Equal(want) {
		t.Fatalf("time = %v, want %v", got, want)
	}
}

var _ = context.Background
