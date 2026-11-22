package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A panic on the admin surface is contained the way one on the API surface
// is — 500 with a ticket, /livez still answering — but it does not latch the
// node. Probes and scrapes do no engine or store work on the application's
// behalf, so a panic there says nothing about the correctness of the node's
// state. The handler is built with no health latch, and the proof that nothing
// latched is that a later /readyz on the same handler, with the readiness
// check healthy again, answers 200.
func TestAdminHandler_PanicIsContained(t *testing.T) {
	explode := true
	h := newAdminHandler(func() error {
		if explode {
			panic("probe exploded")
		}
		return nil
	}, "")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	// /livez keeps answering (the node is drained by nothing here; it simply
	// never went unhealthy).
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/livez = %d after a recovered panic, want 200", w.Code)
	}

	explode = false
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz = %d once readiness is healthy again, want 200 — the admin panic latched something it must not", w.Code)
	}
}
