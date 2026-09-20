package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// healthServer serves /healthz on a chosen port. The fixture uses this
// to verify the compute test client is alive and gRPC-connected before
// running scenarios.
type healthServer struct {
	listener net.Listener
	srv      *http.Server
	ready    *atomic.Bool
}

// newHealthServer constructs the client's local HTTP server, bound to an
// ephemeral port: /healthz for the fixture's readiness probe, /record and
// /release as the test's control surface.
func newHealthServer(rec *recorder, release func(context.Context)) (*healthServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hs := &healthServer{
		listener: ln,
		ready:    new(atomic.Bool),
	}
	writeRecord := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rec.snapshot())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !hs.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"DOWN"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"UP"}`))
	})
	// /record: every calculation request this client received, in order.
	mux.HandleFunc("/record", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeRecord(w)
	})
	// /release: make the held late callbacks, then answer with the record,
	// which now carries their outcomes. Not bound to the request's context: a
	// caller that gives up must not cut a callback off half-way.
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		release(ctx)
		writeRecord(w)
	})
	hs.srv = &http.Server{Handler: mux}
	return hs, nil
}

// start serves on the bound listener in a goroutine. The server stops
// when stop() is called.
func (h *healthServer) start() {
	go func() { _ = h.srv.Serve(h.listener) }()
}

func (h *healthServer) stop() {
	_ = h.srv.Close()
}

func (h *healthServer) addr() string {
	return h.listener.Addr().String()
}

func (h *healthServer) markReady() {
	h.ready.Store(true)
}
