//go:build !windows

package main

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestShutdown_SecondSignal_ForcesHardExit pins the second-signal escape
// hatch: the first SIGINT/SIGTERM cancels rootCtx and
// triggers graceful drain, but if the operator presses Ctrl+C again
// because the drain is hanging, the process must hard-exit with code 2.
//
// This is the safety valve for stuck in-flight RPCs or slow-closing
// storage pools. signal.NotifyContext on its own only cancels on the
// first signal — every subsequent signal is a no-op, leaving the
// operator with no recourse short of SIGKILL.
//
// The test starts the cyoda binary, waits for it to bind, sends two
// SIGTERMs, and asserts (a) exit code 2 and (b) the
// "hard exit forced by second signal" warn log line in stderr.
func TestShutdown_SecondSignal_ForcesHardExit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess shutdown test in -short mode")
	}

	child := startCyoda(t)
	cmd := child.cmd
	httpAddr := child.loopbackAddr(t, "HTTP")

	// Pin an in-flight HTTP request on the application server so the
	// graceful drain has something to wait on. Without this, an idle
	// child exits in ~1ms — far faster than we can deliver a second
	// signal — and the hard-exit branch is never exercised.
	//
	// Earlier revisions sent only "GET /livez HTTP/1.1\r\nHost: x\r\n"
	// (one trailing CRLF, headers never terminated) and relied on the
	// server having parsed those bytes before SIGTERM arrived. On slow
	// CI workers the bytes were still in the kernel's TCP buffer when
	// the first signal fired, so http.Server.Shutdown saw the
	// connection as idle, closed it immediately, completed graceful
	// drain in ~ms, and the child exited 0 before the second signal
	// could land. The fix below removes that timing dependency.
	//
	// We send a complete chunked POST: a valid request line, terminated
	// headers (\r\n\r\n), one chunk-size announcement (256 bytes), and
	// only a few bytes of the announced chunk body. The server parses
	// the request, dispatches to its handler, writes a response (we
	// read at least one byte back as the synchronisation gate), and
	// then — crucially — stays blocked draining the unfinished request
	// body before it can transition the connection to keep-alive idle.
	// While that drain blocks, http.Server.Shutdown classifies the
	// connection as StateActive and waits for the full drain budget,
	// keeping the graceful-shutdown path open long enough for the
	// second SIGTERM to exercise the hard-exit branch.
	hold, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatalf("hold-open dial: %v", err)
	}
	defer hold.Close()
	// 256-byte chunk announced (0x100), only 5 bytes of body sent — no
	// chunk terminator, no zero chunk, no trailing CRLF. The server's
	// body discard after the handler returns will block reading the
	// missing bytes, which is what makes the connection stay active
	// during Shutdown.
	const inflightReq = "POST /livez HTTP/1.1\r\n" +
		"Host: x\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"\r\n" +
		"100\r\n" +
		"hello"
	if _, err := hold.Write([]byte(inflightReq)); err != nil {
		t.Fatalf("hold-open write: %v", err)
	}

	// Synchronisation gate: read at least one response byte from the
	// hold-open connection. Once we see bytes, the server has fully
	// parsed the request and dispatched the handler, so the connection
	// is unambiguously past the kernel's TCP buffer and has reached
	// StateActive. This eliminates the "bytes still in flight when
	// SIGTERM arrives" race that produced the original CI flake.
	hold.SetReadDeadline(time.Now().Add(1 * time.Second))
	probe := make([]byte, 1)
	if _, err := hold.Read(probe); err != nil {
		// A net.OpError with a timeout is acceptable — the server is
		// blocked reading our partial chunk before sending the
		// response, which still counts as StateActive. We only fail
		// on hard errors like ECONNRESET that mean the conn was
		// dropped.
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("hold-open response read: %v", err)
		}
	}
	hold.SetReadDeadline(time.Time{})

	// First SIGTERM: cancels rootCtx and starts the graceful drain.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send first SIGTERM: %v", err)
	}
	// Pause so the first-signal path is observably engaged before the
	// second arrives — this exercises the second-signal handler rather
	// than a coalesced double-fire. 500ms gives the goroutine that
	// arms the hard-exit channel time to register reliably across all
	// schedulers, including slow CI workers.
	time.Sleep(500 * time.Millisecond)

	// Second SIGTERM: must force os.Exit(2).
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send second SIGTERM: %v", err)
	}

	if code := child.wait(t, 20*time.Second); code != 2 {
		t.Errorf("exit code %d; want 2 (forced by second signal)", code)
	}

	if got := child.out.String(); !strings.Contains(got, "hard exit forced by second signal") {
		t.Errorf("expected 'hard exit forced by second signal' warn log; got:\n%s", got)
	}
}
