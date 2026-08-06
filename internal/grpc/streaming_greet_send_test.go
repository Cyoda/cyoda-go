package grpc

// streaming_greet_send_test.go — every send to a member's stream must be
// serialised by that member's sendMu.
//
// StartStreaming publishes the member to the registry (Register) BEFORE it
// greets. From that instant a dispatch can be routed to it and call
// Member.Send. If the greet writes to the stream directly instead of through
// Member.Send, the two sends run concurrently on one gRPC stream — which
// grpc-go documents as unsupported, and whose failure mode is a corrupted
// HTTP/2 frame, not a clean error.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// overlapDetectingStream reports whether two sends were ever inside Send at the
// same time. The first send (the greet) is held open long enough for a
// concurrent Member.Send to land in the window if nothing is serialising them.
type overlapDetectingStream struct {
	*mockBidiStream
	inFlight atomic.Int32
	overlaps atomic.Int32
	firstMu  sync.Mutex
	first    bool
	release  chan struct{}
	entered  chan struct{}
}

func newOverlapDetectingStream(ctx context.Context) *overlapDetectingStream {
	return &overlapDetectingStream{
		mockBidiStream: newMockBidiStream(ctx),
		release:        make(chan struct{}),
		entered:        make(chan struct{}),
	}
}

func (s *overlapDetectingStream) Send(ce *cepb.CloudEvent) error {
	if n := s.inFlight.Add(1); n > 1 {
		s.overlaps.Add(1)
	}
	defer s.inFlight.Add(-1)

	s.firstMu.Lock()
	isFirst := !s.first
	s.first = true
	s.firstMu.Unlock()

	if isFirst {
		// The greet. Park inside Send so a concurrent dispatch has a real
		// window to collide in.
		close(s.entered)
		<-s.release
	}
	return s.mockBidiStream.Send(ce)
}

func TestStreaming_GreetIsSerialisedWithConcurrentDispatch(t *testing.T) {
	svc := newServiceForTest()
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()

	stream := newOverlapDetectingStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"go"}))

	done := make(chan error, 1)
	go func() { done <- svc.StartStreaming(stream) }()

	// Wait until the greet is parked inside Send.
	select {
	case <-stream.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the greet never reached the stream")
	}

	// The member is registered by now — that is the whole hazard. Dispatch to
	// it while the greet is still in flight.
	var member *Member
	for i := 0; i < 200 && member == nil; i++ {
		if members := svc.registry.List(); len(members) == 1 {
			member = members[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if member == nil {
		t.Fatal("member was never registered")
	}

	kaCE, err := NewCloudEvent(CalculationMemberKeepAliveEvent, map[string]any{"success": true})
	if err != nil {
		t.Fatalf("build keep-alive: %v", err)
	}

	sent := make(chan struct{})
	go func() {
		_ = member.Send(kaCE)
		close(sent)
	}()

	// Give the dispatch every chance to enter Send. If nothing serialises the
	// two, it does, and the counter records it.
	select {
	case <-sent:
		// It got all the way through while the greet was still parked — that
		// IS the overlap; the assertion below reports it.
	case <-time.After(300 * time.Millisecond):
		// Blocked, which is what serialisation looks like.
	}

	close(stream.release)
	<-sent
	stream.closeRecv()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartStreaming did not return")
	}
	cancel()

	if n := stream.overlaps.Load(); n != 0 {
		t.Errorf("%d concurrent send(s) on one gRPC stream: the greet bypasses Member.Send, "+
			"so a dispatch routed between Register and the greet writes interleaved HTTP/2 frames", n)
	}
}
