package grpc

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	cyodapb "github.com/cyoda/cyoda-go/api/grpc/cyoda"
)

// A member that joins, then stops reading while the server keeps writing,
// fills the HTTP/2 write window (pinned to 64 KiB on the client) and is
// evicted by the write-progress rule within the keep-alive timeout; every
// dispatcher queued behind that stalled write is released, and the writer
// itself unwinds once the handler returns.
func TestFrozenMember_IsEvictedAndDispatchersAreReleased(t *testing.T) {
	// Timeout is 1s against a 100ms interval: it governs three clocks at once
	// (LastSeen freshness against the 100ms feeder below, the write-stall rule,
	// and the transport's ping-ack deadline), and a margin that tight makes all
	// three race each other on a loaded machine.
	ka := KeepAliveConfig{Interval: 100 * time.Millisecond, Timeout: 1 * time.Second}
	ts := startRecoveryTestServerWithKeepAlive(t, nil, ka)

	conn, err := googlegrpc.NewClient(ts.addr,
		googlegrpc.WithTransportCredentials(insecure.NewCredentials()),
		googlegrpc.WithInitialWindowSize(65535),
		googlegrpc.WithInitialConnWindowSize(65535))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := cyodapb.NewCloudEventsServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := client.StartStreaming(ctx)
	if err != nil {
		t.Fatal(err)
	}
	join, err := NewCloudEvent(CalculationMemberJoinEvent, map[string]any{
		"id":                  "j",
		"tags":                []string{"frozen"},
		"joinedLegalEntityId": string(ts.uc.Tenant.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(join); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil { // the greet
		t.Fatalf("greet: %v", err)
	}
	// From here the client never calls Recv again: frozen. The test keeps
	// feeding keep-alives on the client's behalf, standing in for the ping
	// goroutine a real compute node runs independently of its application —
	// which is what a frozen node looks like, and which takes the
	// inbound-silence rule out of play, so the only rule left that can evict
	// this member is write progress.
	feederDone := make(chan struct{})
	go func() {
		defer close(feederDone)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := stream.Send(makeKeepAliveEvent()); err != nil {
					return
				}
			}
		}
	}()

	t.Cleanup(func() { cancel(); <-feederDone })

	var member *Member
	for i := 0; i < 200 && member == nil; i++ {
		if ms := ts.registry.List(); len(ms) == 1 {
			member = ms[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	if member == nil {
		t.Fatal("member not registered")
	}

	// Big events, handed to the writer one at a time. How much a frozen client
	// absorbs before the writer wedges is not a fixed number — its receive
	// window, the socket buffers, and the one oversized write the server's
	// per-stream write quota lets through all contribute — so a filler keeps
	// pushing until the stall is observed rather than assuming one size fills
	// the pipe.
	big := make([]byte, 1024*1024)
	for i := range big {
		big[i] = 'x'
	}
	payload := map[string]any{"requestId": "big", "payload": string(big)}
	ce, err := NewCloudEvent(EntityProcessorCalculationRequest, payload)
	if err != nil {
		t.Fatal(err)
	}
	fillerDone := make(chan struct{})
	stopFiller := make(chan struct{})
	go func() {
		defer close(fillerDone)
		for {
			select {
			case <-stopFiller:
				return
			default:
			}
			fctx, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := member.Send(fctx, ce)
			fcancel()
			if err != nil { // evicted, or the writer has stopped taking events
				return
			}
		}
	}()
	// Stop the filler before waiting on it. On the happy path it has already
	// returned (the member is evicted, so Send fails); on a failure path —
	// "writer never wedged", where the client is still draining — it would
	// otherwise loop forever and the cleanup would hang instead of reporting
	// the failure.
	t.Cleanup(func() { close(stopFiller); <-fillerDone })

	// The writer must be stuck in a raw send, not merely idle: that stall is
	// what the keep-alive loop reads. One non-zero sample is not enough — a
	// keep-alive ping in flight shows up the same way — so the same start
	// timestamp has to still be in flight 100ms later.
	var wedgedAt time.Time
	for i := 0; i < 500 && wedgedAt.IsZero(); i++ {
		first := member.WriteInFlightSince()
		if first.IsZero() {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		time.Sleep(100 * time.Millisecond)
		if second := member.WriteInFlightSince(); second.Equal(first) {
			wedgedAt = first
		}
	}
	if wedgedAt.IsZero() {
		t.Fatal("writer never wedged: the frozen client kept draining")
	}

	// Five dispatchers now queue behind that wedged write. Not one of them may
	// stay wedged with it: eviction, or its own deadline, has to release each.
	var released atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer scancel()
			_ = member.Send(sctx, ce)
			released.Add(1)
		}()
	}

	select {
	case <-member.Evicted():
	case <-time.After(5 * time.Second):
		t.Fatal("frozen member was not evicted within the keep-alive timeout")
	}
	// The stall, not inbound silence, is what must have evicted it: a member
	// that never sends a keep-alive would eventually go on the LastSeen rule
	// too, and that would prove nothing about the writer.
	st, ok := status.FromError(member.EvictErr())
	if !ok || st.Code() != codes.DeadlineExceeded || st.Message() != "member not draining" {
		t.Fatalf("expected eviction by the write-progress rule, got %v", member.EvictErr())
	}
	// Joined through a channel, not a bare wg.Wait(): a dispatcher that stays
	// wedged does not come back at all, and waiting for it would hang the test
	// until the package timeout instead of failing with a readable count.
	allReleased := make(chan struct{})
	go func() { wg.Wait(); close(allReleased) }()
	select {
	case <-allReleased:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d of 5 senders released; the rest are still wedged", released.Load())
	}
	select {
	case <-member.WriterDone():
	case <-time.After(5 * time.Second):
		t.Fatal("writer still wedged after eviction: handler return did not unblock the raw send")
	}
}

// A connection whose bytes stop flowing in both directions (a pausable TCP
// proxy) is torn down by the transport keepalive within Time+Timeout, observed
// on the socket itself — the proxy sees the server close its end — and the
// member is unregistered, even though nothing at the application level ever
// errors.
func TestBlackholedConnection_IsTornDownByTransportKeepalive(t *testing.T) {
	ka := KeepAliveConfig{Interval: 1 * time.Second, Timeout: 1 * time.Second}
	ts := startRecoveryTestServerWithKeepAlive(t, nil, ka)
	proxy := newPausableProxy(t, ts.addr)

	conn, err := googlegrpc.NewClient(proxy.addr, googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := cyodapb.NewCloudEventsServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.StartStreaming(ctx)
	if err != nil {
		t.Fatal(err)
	}
	join, err := NewCloudEvent(CalculationMemberJoinEvent, map[string]any{
		"id":                  "j",
		"tags":                []string{"bh"},
		"joinedLegalEntityId": string(ts.uc.Tenant.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(join); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("greet: %v", err)
	}
	proxy.pause() // both directions stop; TCP stays open.

	// The transport itself, observed directly: the proxy's upstream socket
	// only reports a close when the server closes its end, and while the
	// server is serving normally nothing else can make it do that — the proxy
	// holds both TCP connections open, the client is healthy, and no
	// application-level error ever occurs. Time+Timeout is 2s; 5s is slack.
	// It is deliberately not longer: grpc-go's graceful-stop path has its own
	// 5s fallback that closes a stream-less connection, and only a bound below
	// that (and taken before GracefulStop is called at all, as here) tells the
	// keepalive apart from the drain.
	select {
	case <-proxy.upstreamClosed():
	case <-time.After(5 * time.Second):
		t.Fatal("server never closed the black-holed connection: transport keepalive did not fire")
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(ts.registry.List()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("black-holed member still registered")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the server can then shut down: GracefulStop waits for every
	// connection to close, and the dead one is already gone. The bound stays
	// under grpc-go's 5s graceful-stop fallback — above it, a stop that only
	// finished because of that fallback would look like a pass.
	stopped := make(chan struct{})
	go func() { ts.srv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("GracefulStop hung on the black-holed connection")
	}
}

// pausableProxy forwards bytes between a client and target until pause() is
// called, after which it silently drops everything while keeping both TCP
// connections open. It also reports when the target closed its end of the
// upstream connection, which is how a test sees the server's transport act on
// its own rather than inferring it from application-level state.
type pausableProxy struct {
	addr     string
	paused   atomic.Bool
	upstream chan struct{} // closed when a read from the target's side ends
	once     sync.Once
}

// upstreamClosed is closed once the target has closed the upstream connection.
func (p *pausableProxy) upstreamClosed() <-chan struct{} { return p.upstream }

func (p *pausableProxy) noteUpstreamClosed() { p.once.Do(func() { close(p.upstream) }) }

func newPausableProxy(t *testing.T, target string) *pausableProxy {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &pausableProxy{addr: lis.Addr().String(), upstream: make(chan struct{})}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			// srcIsTarget marks the direction whose source is the upstream
			// connection: a read ending there is the server closing on us.
			pump := func(dst, src net.Conn, srcIsTarget bool) {
				defer func() { _ = dst.Close(); _ = src.Close() }()
				buf := make([]byte, 32*1024)
				for {
					n, err := src.Read(buf)
					if n > 0 && !p.paused.Load() {
						if _, werr := dst.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						if srcIsTarget {
							p.noteUpstreamClosed()
						}
						return
					}
				}
			}
			go pump(up, c, false)
			go pump(c, up, true)
		}
	}()
	return p
}

func (p *pausableProxy) pause() { p.paused.Store(true) }
