package grpc

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
)

// wedgingStream parks every send after the greet until released, and keeps
// the client "alive" by feeding inbound keep-alives on demand.
type wedgingStream struct {
	*mockBidiStream
	greeted chan struct{}
	release chan struct{}
	sends   int
}

func newWedgingStream(ctx context.Context) *wedgingStream {
	return &wedgingStream{mockBidiStream: newMockBidiStream(ctx), greeted: make(chan struct{}), release: make(chan struct{})}
}

func (s *wedgingStream) Send(ce *cepb.CloudEvent) error {
	s.sends++
	if s.sends == 1 {
		err := s.mockBidiStream.Send(ce)
		close(s.greeted)
		return err
	}
	<-s.release
	return s.mockBidiStream.Send(ce)
}

type bidiStream = googlegrpc.BidiStreamingServer[cepb.CloudEvent, cepb.CloudEvent]

func startStream(t *testing.T, svc *CloudEventsServiceImpl, stream bidiStream) (chan error, *Member) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- svc.StartStreaming(stream) }()
	var member *Member
	for range 400 {
		if ms := svc.registry.List(); len(ms) == 1 {
			member = ms[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if member == nil {
		t.Fatal("member never registered")
	}
	return done, member
}

// A member whose own keep-alive goroutine keeps pinging but whose application
// has stopped reading is evicted by write progress, within the keep-alive
// timeout, and the stream ends with "member not draining".
func TestStreaming_PingingButNotReading_IsEvictedByWriteProgress(t *testing.T) {
	svc := newServiceWithKeepAlive(20*time.Millisecond, 120*time.Millisecond)
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()
	stream := newWedgingStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"go"}))
	done, member := startStream(t, svc, stream)
	<-stream.greeted

	// Wedge the writer with a dispatch, then keep the inbound side chatty. The
	// release is deferred so a failure below still frees the parked writer.
	defer close(stream.release)
	wedge := mustCE(t)
	go func() { _ = member.Send(context.Background(), wedge) }()
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				stream.tryEnqueue(makeKeepAliveEvent())
			}
		}
	}()
	defer close(stop)

	select {
	case err := <-done:
		st, _ := status.FromError(err)
		if st.Code() != codes.DeadlineExceeded || !strings.Contains(st.Message(), "not draining") {
			t.Fatalf("stream ended with %v, want DeadlineExceeded 'member not draining'", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pinging-but-not-reading member was never evicted")
	}
}

// A clean client disconnect ends the stream promptly with the Recv error and
// unregisters the member (regression guard for the single receive goroutine).
func TestStreaming_ClientClose_EndsPromptly(t *testing.T) {
	svc := newServiceWithKeepAlive(time.Hour, 2*time.Hour)
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()
	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", nil))
	done, member := startStream(t, svc, stream)
	_ = stream.waitForSent(t, 2*time.Second)

	stream.closeRecv()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not end on client close")
	}
	if svc.registry.Get(member.ID) != nil {
		t.Fatal("member still registered after close")
	}
	select {
	case <-member.WriterDone():
	case <-time.After(time.Second):
		t.Fatal("writer still running after stream end")
	}
}

// A panic inside stream.Recv is contained: the client gets a ticket status and
// the member is evicted and unregistered rather than the process dying.
func TestStreaming_RecvPanic_IsContained(t *testing.T) {
	svc := newServiceWithKeepAlive(time.Hour, 2*time.Hour)
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()
	stream := &panickingRecvStream{mockBidiStream: newMockBidiStream(ctx)}
	stream.enqueue(makeJoinEvent(t, "tenant-1", nil))
	done, member := startStream(t, svc, stream)
	_ = stream.waitForSent(t, 2*time.Second)
	stream.armPanic()

	select {
	case err := <-done:
		st, _ := status.FromError(err)
		if st.Code() != codes.Internal || !strings.Contains(st.Message(), "[ticket: ") {
			t.Fatalf("err = %v, want Internal with ticket", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after Recv panic")
	}
	if svc.registry.Get(member.ID) != nil {
		t.Fatal("member still registered after a contained Recv panic")
	}
}

// A keep-alive interval of zero panics time.NewTicker inside keepAliveLoop.
// That panic is contained the same way: ticket status, member evicted, node
// still up — a misconfigured interval must not take the process down.
func TestStreaming_KeepAliveLoopPanic_IsContained(t *testing.T) {
	svc := newServiceWithKeepAlive(0, time.Hour)
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()
	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", nil))
	// Not startStream: the panic fires the instant the member is published, so
	// there is no window in which polling the registry reliably sees it.
	done := make(chan error, 1)
	go func() { done <- svc.StartStreaming(stream) }()

	select {
	case err := <-done:
		st, _ := status.FromError(err)
		if st.Code() != codes.Internal || !strings.Contains(st.Message(), "[ticket: ") {
			t.Fatalf("err = %v, want Internal with ticket", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after the keep-alive loop panicked")
	}
	if ms := svc.registry.List(); len(ms) != 0 {
		t.Fatalf("%d members still registered after a contained keep-alive panic", len(ms))
	}
}

// panickingRecvStream returns queued events normally until armed, then panics
// on the next Recv.
type panickingRecvStream struct {
	*mockBidiStream
	armed atomic.Bool
}

func (s *panickingRecvStream) armPanic() { s.armed.Store(true); s.enqueue(makeKeepAliveEvent()) }
func (s *panickingRecvStream) Recv() (*cepb.CloudEvent, error) {
	ce, err := s.mockBidiStream.Recv()
	if s.armed.Load() {
		panic("recv exploded")
	}
	return ce, err
}

// Processor, criteria and function responses each count as liveness. The
// requestId matches nothing, so only the liveness half is under test.
func TestStreaming_ResponseRefreshesLastSeen(t *testing.T) {
	svc := newServiceWithKeepAlive(time.Hour, 2*time.Hour)
	ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
	defer cancel()
	stream := newMockBidiStream(ctx)
	stream.enqueue(makeJoinEvent(t, "tenant-1", nil))
	done, member := startStream(t, svc, stream)
	_ = stream.waitForSent(t, 2*time.Second)

	for _, evtType := range []string{
		EntityProcessorCalculationResponse,
		EntityCriteriaCalculationResponse,
		EntityFunctionCalculationResponse,
	} {
		before := member.LastSeen()
		time.Sleep(2 * time.Millisecond)
		ce, err := NewCloudEvent(evtType, map[string]any{"requestId": "unknown", "success": true})
		if err != nil {
			t.Fatalf("failed to create %s: %v", evtType, err)
		}
		stream.enqueue(ce)
		deadline := time.Now().Add(2 * time.Second)
		for !member.LastSeen().After(before) {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not refresh LastSeen", evtType)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	stream.closeRecv()
	<-done
}
