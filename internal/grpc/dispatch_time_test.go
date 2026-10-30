package grpc

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/testing/fakeclock"
)

func TestNewProcessorDispatcher_NilClockPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewProcessorDispatcher with a nil clock did not panic")
		}
	}()
	NewProcessorDispatcher(NewMemberRegistry(), nil, nil, time.Second, time.Second, time.Second, nil)
}

// A processor, criterion or schedule-function request carries the platform
// clock's time as the CloudEvents time attribute.
func TestDispatch_CalloutCarriesClockTime(t *testing.T) {
	at := time.Date(2015, 3, 1, 0, 0, 0, 0, time.UTC)
	reg := NewMemberRegistry()
	d := NewProcessorDispatcher(reg, NewRoundRobinSelector(reg), nil, time.Second, time.Second, time.Second, fakeclock.New(at))

	criterion := json.RawMessage(`{"type":"function","function":{"name":"c","config":{"calculationNodesTags":"python"}}}`)
	crit, failure := NewCriteriaCallout(testTenantID, testEntity(), criterion, "transition", "wf", "t", "p", "tx")
	if failure != nil {
		t.Fatalf("criterion callout: %v", failure)
	}
	calls := map[string]Callout{
		"processor":         NewProcessorCallout(testTenantID, testEntity(), spi.ProcessorDefinition{Name: "p"}, "wf", "t", "tx"),
		"criterion":         crit,
		"schedule function": NewFunctionCallout(testTenantID, testEntity(), spi.ScheduleFunction{Name: "f"}, "wf", "t", "tx"),
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			call.RequestID = "r1"
			ce, err := d.buildCalloutEvent(call)
			if err != nil {
				t.Fatalf("buildCalloutEvent: %v", err)
			}
			assertEventTime(t, ce, at)
		})
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
