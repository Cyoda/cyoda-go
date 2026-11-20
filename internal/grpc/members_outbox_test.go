package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cepb "github.com/cyoda/cyoda-go/api/grpc/cloudevents"
)

// The greet is the first event on the wire, ahead of anything a dispatcher
// enqueues the instant the member becomes visible in the registry.
func TestMember_GreetIsFirstOnTheWire(t *testing.T) {
	reg := NewMemberRegistry()
	sent := make(chan *cepb.CloudEvent, 8)
	greet, _ := NewCloudEvent(CalculationMemberGreetEvent, map[string]any{"memberId": "m1", "success": true})
	m := reg.Register("m1", "tenant-1", []string{"go"}, func(ce *cepb.CloudEvent) error { sent <- ce; return nil }, greet)
	defer reg.Unregister(m)

	if err := m.Send(context.Background(), mustCE(t)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	first := <-sent
	if first.Type != CalculationMemberGreetEvent {
		t.Fatalf("first event on the wire = %s, want greet", first.Type)
	}
}

// Send returns nil only once the writer holds the event: with the writer
// wedged, a caller is released by its own deadline, not by the stream.
func TestMember_SendHonoursDeadlineWhileWriterWedged(t *testing.T) {
	reg := NewMemberRegistry()
	release := make(chan struct{})
	m := reg.Register("m1", "tenant-1", nil, func(*cepb.CloudEvent) error { <-release; return nil }, nil)
	defer func() { close(release); reg.Unregister(m) }()

	// First send is taken by the writer and wedges inside the raw send.
	if err := m.Send(context.Background(), mustCE(t)); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := m.Send(ctx, mustCE(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Send err = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Send blocked %v past its 50ms deadline", time.Since(start))
	}
	if m.WriteInFlightSince().IsZero() {
		t.Fatal("WriteInFlightSince should be non-zero while the writer is wedged")
	}
}

// A sender parked on the outbox is released by its own cancellation, and
// nothing it queued is ever written. (The writer's own ctx check on a
// received item is belt-and-braces for the unbuffered handoff and is not
// separately observable; it is reviewed, not unit-tested.)
func TestMember_SendReturnsWhenCallerGivesUp(t *testing.T) {
	reg := NewMemberRegistry()
	release := make(chan struct{})
	sent := make(chan *cepb.CloudEvent, 8)
	m := reg.Register("m1", "tenant-1", nil, func(ce *cepb.CloudEvent) error {
		sent <- ce
		<-release
		return nil
	}, nil)
	defer reg.Unregister(m)

	_ = m.Send(context.Background(), mustCE(t)) // wedges the writer
	<-sent

	// Park a sender on the unbuffered outbox behind the wedged writer, then
	// cancel its ctx: the sender is released by its own cancellation while
	// still parked, so the item never reaches the writer at all.
	ctx, cancel := context.WithCancel(context.Background())
	enqueued := make(chan error, 1)
	go func() { enqueued <- m.Send(ctx, mustCE(t)) }()
	time.Sleep(20 * time.Millisecond) // sender is parked on the unbuffered outbox
	cancel()
	if err := <-enqueued; !errors.Is(err, context.Canceled) {
		t.Fatalf("Send err = %v, want Canceled", err)
	}
	close(release) // writer resumes; nothing else is queued
	select {
	case ce := <-sent:
		t.Fatalf("writer sent %s for a caller that had given up", ce.Type)
	case <-time.After(100 * time.Millisecond):
	}
}

// Evict releases a sender blocked on the outbox and a waiter blocked on a
// tracked request, and both see the member as gone.
func TestMember_EvictReleasesSendersAndWaiters(t *testing.T) {
	reg := NewMemberRegistry()
	release := make(chan struct{})
	m := reg.Register("m1", "tenant-1", nil, func(*cepb.CloudEvent) error { <-release; return nil }, nil)
	defer close(release)

	_ = m.Send(context.Background(), mustCE(t)) // wedge the writer
	ch, err := m.TrackRequest("req-1")
	if err != nil {
		t.Fatalf("TrackRequest: %v", err)
	}
	sendErr := make(chan error, 1)
	go func() { sendErr <- m.Send(context.Background(), mustCE(t)) }()
	time.Sleep(20 * time.Millisecond)

	want := status.Error(codes.DeadlineExceeded, "member not draining")
	m.Evict(want)
	m.Evict(errors.New("second call must not win"))

	if err := <-sendErr; !errors.Is(err, ErrMemberEvicted) {
		t.Fatalf("blocked Send err = %v, want ErrMemberEvicted", err)
	}
	select {
	case resp := <-ch:
		if !resp.Disconnected {
			t.Fatal("pending waiter should see Disconnected")
		}
	case <-time.After(time.Second):
		t.Fatal("pending waiter was not released")
	}
	<-m.Evicted()
	if m.EvictErr() != want {
		t.Fatalf("EvictErr = %v, want the first error", m.EvictErr())
	}
	if _, err := m.TrackRequest("req-2"); !errors.Is(err, ErrMemberEvicted) {
		t.Fatalf("TrackRequest after Evict err = %v, want ErrMemberEvicted", err)
	}
	if m.TrySend(mustCE(t)) {
		t.Fatal("TrySend after Evict must be false")
	}
}

// The writer exits after eviction once the raw send returns, and a raw send
// failure evicts on its own.
func TestMember_WriterExitsAndSendFailureEvicts(t *testing.T) {
	reg := NewMemberRegistry()
	m := reg.Register("m1", "tenant-1", nil, func(*cepb.CloudEvent) error { return errors.New("wire broke") }, nil)
	_ = m.Send(context.Background(), mustCE(t))
	select {
	case <-m.Evicted():
	case <-time.After(time.Second):
		t.Fatal("send failure did not evict")
	}
	if st, _ := status.FromError(m.EvictErr()); st.Code() != codes.Unavailable {
		t.Fatalf("EvictErr code = %v, want Unavailable", st.Code())
	}
	select {
	case <-m.WriterDone():
	case <-time.After(time.Second):
		t.Fatal("writer did not exit after eviction")
	}
	reg.Unregister(m)
}

// A panic inside the raw send is contained: ticket status, member evicted,
// process alive.
func TestMember_WriterPanicIsContained(t *testing.T) {
	reg := NewMemberRegistry()
	m := reg.Register("m1", "tenant-1", nil, func(*cepb.CloudEvent) error { panic("transport bug") }, nil)
	defer reg.Unregister(m)
	_ = m.Send(context.Background(), mustCE(t))
	select {
	case <-m.Evicted():
	case <-time.After(time.Second):
		t.Fatal("panic in writer did not evict")
	}
	st, _ := status.FromError(m.EvictErr())
	if st.Code() != codes.Internal || !strings.Contains(st.Message(), "[ticket: ") {
		t.Fatalf("EvictErr = %v, want Internal with ticket", m.EvictErr())
	}
}

// Re-registering an ID displaces the member holding it: the old one is
// evicted, so its writer exits and its waiters are released instead of being
// stranded behind a registry entry nobody can reach any more.
func TestMember_ReRegisteredIDEvictsTheDisplacedMember(t *testing.T) {
	reg := NewMemberRegistry()
	first := reg.Register("m1", "tenant-1", nil, noopSend, nil)

	second := reg.Register("m1", "tenant-1", nil, noopSend, nil)
	defer reg.Unregister(second)

	select {
	case <-first.Evicted():
	case <-time.After(time.Second):
		t.Fatal("the displaced member was not evicted")
	}
	select {
	case <-first.WriterDone():
	case <-time.After(time.Second):
		t.Fatal("the displaced member's writer did not exit")
	}
}
