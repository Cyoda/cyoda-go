package grpc

import (
	"fmt"
	"testing"
	"time"

	cepb "github.com/cyoda/cyoda-go/api/grpc/cloudevents"
	events "github.com/cyoda/cyoda-go/api/grpc/events"
)

// A member is published to the registry before its writer puts the greet on
// the wire. The greet is the member's "you are registered" signal: a client
// that holds it, and any test that looks the member up the instant it sees
// it, must find the member. When the writer was started before the map
// insert, a fast writer could send the greet first and the lookup answered
// nil ("member not found" on CI). The probe is the send function itself:
// it runs on the writer goroutine at the moment the greet goes out, so what
// the registry holds right then is exactly what a client could observe.
func TestRegister_MemberIsPublishedWhenGreetIsSent(t *testing.T) {
	reg := NewMemberRegistry()
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("m-%d", i)
		greet, err := NewCloudEvent(CalculationMemberGreetEvent, events.CalculationMemberGreetEventJson{
			ID: id, MemberID: id, JoinedLegalEntityID: "tenant-1", Success: true,
		})
		if err != nil {
			t.Fatalf("greet: %v", err)
		}
		atGreet := make(chan *Member, 1)
		m := reg.Register(id, "tenant-1", nil, func(*cepb.CloudEvent) error {
			select {
			case atGreet <- reg.Get(id):
			default: // only the greet is probed; later writes are not expected here
			}
			return nil
		}, greet)
		select {
		case got := <-atGreet:
			if got != m {
				t.Fatalf("iteration %d: registry held %v when the greet was sent, want the member being registered", i, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: greet was never sent", i)
		}
		reg.Unregister(m)
	}
}
