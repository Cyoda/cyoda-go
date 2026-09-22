package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestControlSurface_RecordAndRelease(t *testing.T) {
	const pass = "pass-value-never-shown"

	// Stands in for cyoda's HTTP door: notes the pass it was shown, refuses.
	var sawPass atomic.Bool
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tx-Token") == pass && r.URL.Path == "/api/entity/JSON/late-secondary/1" {
			sawPass.Store(true)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"properties":{"errorCode":"TRANSACTION_NOT_FOUND"}}`)
	}))
	defer door.Close()

	rec := newRecorder()
	rec.setMemberID("member-1")
	cat := newCatalog(newCallbackClient(door.URL, "bearer"), nil)
	d := newDispatcher("", "", cat, nil, []string{"x"}, behaviourLateCallback, rec)

	hs, err := newHealthServer(rec, d.release)
	if err != nil {
		t.Fatalf("newHealthServer: %v", err)
	}
	hs.start()
	defer hs.stop()
	base := "http://" + hs.addr()

	ce, payload := processorRequest(t, "r-1", "noop", pass, `{"secondaryModel":"late-secondary","secondaryVersion":1,"marker":"m"}`)
	if reply, _, err := d.handleCallout(context.Background(), ce, payload); err != nil || reply != nil {
		t.Fatalf("late-callback must stay silent: reply=%v err=%v", reply, err)
	}

	fetch := func(method, path string) (recordDoc, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
		var doc recordDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return doc, string(raw)
	}

	before, _ := fetch(http.MethodGet, "/record")
	if before.MemberID != "member-1" || len(before.Received) != 1 || before.Received[0].Callback != nil {
		t.Fatalf("record before release = %+v", before)
	}
	if sawPass.Load() {
		t.Fatal("the callback was made before /release")
	}

	after, raw := fetch(http.MethodPost, "/release")
	if !sawPass.Load() {
		t.Fatal("/release did not present the held pass on the HTTP door")
	}
	cb := after.Received[0].Callback
	if cb == nil {
		t.Fatal("/release recorded no callback outcome")
	}
	if cb.HTTPStatus != http.StatusNotFound || cb.HTTPErrorCode != "TRANSACTION_NOT_FOUND" {
		t.Errorf("outcome = %+v; want 404 TRANSACTION_NOT_FOUND", *cb)
	}
	if cb.GRPCAttempted {
		t.Error("gRPC callback reported as attempted with no gRPC callback client")
	}
	if strings.Contains(raw, pass) {
		t.Error("the control surface exposes the pass")
	}

	// A second release has nothing left to do and changes nothing.
	again, _ := fetch(http.MethodPost, "/release")
	if again.Received[0].Callback == nil || again.Received[0].Callback.HTTPStatus != http.StatusNotFound {
		t.Errorf("second release altered the outcome: %+v", again.Received[0].Callback)
	}

	if resp, err := http.Get(base + "/release"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET /release = %d; want 405", resp.StatusCode)
		}
	}
}
