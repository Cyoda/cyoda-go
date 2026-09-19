package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

func serve(t *testing.T, h http.Handler, cfg app.HTTPConfig) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(h, cfg)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return lis.Addr().String()
}

func TestNewHTTPServer_CarriesTheFourTimeouts(t *testing.T) {
	cfg := app.HTTPConfig{ReadHeaderTimeout: 1 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 4 * time.Second}
	srv := newHTTPServer(http.NotFoundHandler(), cfg)
	if srv.ReadHeaderTimeout != cfg.ReadHeaderTimeout || srv.ReadTimeout != cfg.ReadTimeout ||
		srv.WriteTimeout != cfg.WriteTimeout || srv.IdleTimeout != cfg.IdleTimeout {
		t.Fatalf("server timeouts %v/%v/%v/%v do not match config", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}

// A client that never finishes its headers is cut off with no response.
func TestNewHTTPServer_SlowHeaderClientIsCutOff(t *testing.T) {
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
		app.HTTPConfig{ReadHeaderTimeout: 100 * time.Millisecond, ReadTimeout: time.Second, IdleTimeout: time.Second})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // no terminating blank line
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 64))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read n=%d err=%v; want 0 bytes and EOF (connection closed, no reply)", n, err)
	}
}

// A client that never finishes its body makes the handler's read fail with a
// timeout, and the request context is cancelled at that instant.
func TestNewHTTPServer_SlowBodyClientIsCutOff(t *testing.T) {
	sawErr := make(chan error, 1)
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		sawErr <- err
		w.WriteHeader(http.StatusBadRequest)
	}), app.HTTPConfig{ReadHeaderTimeout: time.Second, ReadTimeout: 150 * time.Millisecond, IdleTimeout: time.Second})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\npartial"))
	select {
	case err := <-sawErr:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("handler read err = %v, want a timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never saw the body read fail")
	}
}

// Once the body has been read, ReadTimeout does not cancel a handler that
// runs longer than it: the server imposes no time budget on work.
func TestNewHTTPServer_ReadTimeoutDoesNotCancelARunningHandler(t *testing.T) {
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(300 * time.Millisecond) // 3x ReadTimeout
		if r.Context().Err() != nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), app.HTTPConfig{ReadHeaderTimeout: time.Second, ReadTimeout: 100 * time.Millisecond, IdleTimeout: time.Second})
	resp, err := http.Post("http://"+addr+"/", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; a drained request must not be cancelled by ReadTimeout", resp.StatusCode)
	}
}

// An idle keep-alive connection is closed after IdleTimeout.
func TestNewHTTPServer_IdleConnectionIsClosed(t *testing.T) {
	addr := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
		app.HTTPConfig{ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, IdleTimeout: 100 * time.Millisecond})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	if _, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("idle connection still open: n=%d err=%v", n, err)
	}
}
