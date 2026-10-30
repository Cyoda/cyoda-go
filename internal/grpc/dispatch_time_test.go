package grpc

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// A processor, criterion or schedule-function request carries the callout's
// platform time — the time the owner took it up — as the CloudEvents time
// attribute, not anything a clock of the dispatching pnode reads.
func TestDispatch_CalloutCarriesItsPlatformTime(t *testing.T) {
	at := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), nil, time.Second, time.Second, time.Second)

	for name, call := range calloutsOfEveryKind(t) {
		t.Run(name, func(t *testing.T) {
			call.RequestID = "r1"
			call.At = at
			ce, err := d.buildCalloutEvent(call)
			if err != nil {
				t.Fatalf("buildCalloutEvent: %v", err)
			}
			assertEventTime(t, ce, at)
		})
	}
}

// A callout without a platform time is not sent: the dispatching pnode has no
// time of its own to put in its place.
func TestDispatch_CalloutWithoutPlatformTimeIsRefused(t *testing.T) {
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), nil, time.Second, time.Second, time.Second)

	for name, call := range calloutsOfEveryKind(t) {
		t.Run(name, func(t *testing.T) {
			call.RequestID = "r1"
			if ce, err := d.buildCalloutEvent(call); err == nil {
				t.Fatalf("buildCalloutEvent built %s with no platform time", ce.Type)
			}
		})
	}
}

// calloutsOfEveryKind is one processor, one criterion and one schedule-function
// callout, by name.
func calloutsOfEveryKind(t *testing.T) map[string]Callout {
	t.Helper()
	criterion := json.RawMessage(`{"type":"function","function":{"name":"c","config":{"calculationNodesTags":"python"}}}`)
	crit, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf", "t", "p", "tx")
	if failure != nil {
		t.Fatalf("criterion callout: %v", failure)
	}
	return map[string]Callout{
		"processor":         NewProcessorCallout(testTenantID, testEntity(), spi.ProcessorDefinition{Name: "p"}, "wf", "t", "tx"),
		"criterion":         crit,
		"schedule function": NewFunctionCallout(testTenantID, testEntity(), spi.ScheduleFunction{Name: "f"}, "wf", "t", "tx"),
	}
}

func assertEventTime(t *testing.T, ce *cepb.CloudEvent, want time.Time) {
	t.Helper()
	a, ok := ce.Attributes["time"]
	if !ok {
		t.Fatalf("no time attribute on %s", ce.Type)
	}
	if got := a.GetCeTimestamp().AsTime(); !got.Equal(want) {
		t.Fatalf("time = %v, want %v", got, want)
	}
}
