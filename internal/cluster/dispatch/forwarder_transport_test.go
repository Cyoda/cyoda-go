package dispatch_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
)

func okAnswer(*http.Request, []byte) any {
	one := 1
	return dispatch.DispatchCalloutResponse{Outcome: dispatch.OutcomeOK, TriesUsed: &one}
}

func TestHTTPForwarder_EveryHandOverOpensItsOwnConnection(t *testing.T) {
	auth := newTestPeerAuth(t)
	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(sealingHandler(t, auth, okAnswer))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	for i := 0; i < 3; i++ {
		if _, err := f.ForwardCallout(context.Background(), testNodeID, srv.URL, makeProcessorReq()); err != nil {
			t.Fatalf("hand-over %d: %v", i, err)
		}
	}
	if got := opened.Load(); got != 3 {
		t.Fatalf("3 hand-overs opened %d connections, want 3", got)
	}
}

func TestHTTPForwarder_TheWaitIsNotBoundedByTheConnectTimeout(t *testing.T) {
	auth := newTestPeerAuth(t)
	srv := sealingPeer(t, auth, func(r *http.Request, p []byte) any {
		time.Sleep(400 * time.Millisecond) // a cnode thinking, far longer than the connect timeout
		return okAnswer(r, p)
	})
	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 50*time.Millisecond).AllowLoopbackForTesting()
	if _, err := f.ForwardCallout(context.Background(), testNodeID, srv.URL, makeProcessorReq()); err != nil {
		t.Fatalf("the connect timeout cut off the wait for the answer: %v", err)
	}
}

func TestHTTPForwarder_TheWaitIsTheContextsDeadline(t *testing.T) {
	auth := newTestPeerAuth(t)
	release := make(chan struct{})
	var once sync.Once
	srv := sealingPeer(t, auth, func(r *http.Request, p []byte) any {
		<-release
		return okAnswer(r, p)
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), time.Second).AllowLoopbackForTesting()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.ForwardCallout(ctx, testNodeID, srv.URL, makeProcessorReq())

	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) || fe.Stage != dispatch.StageAfterConnect {
		t.Fatalf("err = %v, want an after-connect forward error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("returned after %s, want the context's 150ms", elapsed)
	}
}

// With an https:// node address the connection opens and the handshake never
// completes. That is not a dial error — the peer may be there — and it is
// bounded by the connect timeout, not by the whole wait.
func TestHTTPForwarder_StalledTLSHandshake_IsAfterConnect_AndBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				mu.Lock()
				defer mu.Unlock()
				held = append(held, c) // accept, then say nothing
			}()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	f := dispatch.NewHTTPForwarder(newTestPeerAuth(t), 200*time.Millisecond).AllowLoopbackForTesting()
	start := time.Now()
	_, err = f.ForwardCallout(context.Background(), testNodeID, "https://"+ln.Addr().String(), makeProcessorReq())

	var fe *dispatch.ForwardError
	if !errors.As(err, &fe) || fe.Stage != dispatch.StageAfterConnect {
		t.Fatalf("err = %v, want an after-connect forward error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("a stalled handshake held the hand-over for %s", elapsed)
	}
}
