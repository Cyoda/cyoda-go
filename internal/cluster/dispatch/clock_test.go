package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

func clockSetting(version uint64) platformclock.Setting {
	now := time.Now().UnixMicro() // realtime: test fixture wall anchor
	return platformclock.Setting{StartUnixMicro: now, AnchorUnixMicro: now, RatePPM: 2_000_000, Version: version, Writer: "node-a"}
}

type fakeHolder struct {
	calls int
	h     ClockHolder
}

func (f *fakeHolder) Hold(p []byte) (platformclock.Setting, error) {
	f.calls++
	return f.h.Hold(p)
}

func clockServer(t *testing.T, auth PeerAuth, holder ClockHolder) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewClockHandler(auth, holder).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func clockSender(t *testing.T) *HTTPForwarder {
	t.Helper()
	return NewHTTPForwarder(newAEAD(t), time.Second).AllowLoopbackForTesting()
}

func TestClockPush_PeerHoldsAndEchoes(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	srv := clockServer(t, newAEAD(t), platformclock.NewHolder(c))
	s := clockSetting(5)
	got, err := clockSender(t).PushClock(context.Background(), testSelfNodeID, srv.URL, s)
	if err != nil || got != s {
		t.Fatalf("PushClock = %+v, %v; want %+v", got, err, s)
	}
	if c.Setting() != s {
		t.Fatalf("peer holds %+v, want %+v", c.Setting(), s)
	}
}

func TestClockPush_ClockOffIsSealedRefusal(t *testing.T) {
	auth := newAEAD(t)
	holder := platformclock.NewHolder(nil)
	srv := clockServer(t, auth, holder)
	_, err := clockSender(t).PushClock(context.Background(), testSelfNodeID, srv.URL, clockSetting(5))
	if err == nil || !strings.Contains(err.Error(), "clock control is off") {
		t.Fatalf("err = %v, want the clock-off refusal", err)
	}

	// At the handler: the refusal is a 200 whose body opens under the binding.
	mux := http.NewServeMux()
	NewClockHandler(auth, holder).Register(mux)
	raw, _ := json.Marshal(clockSetting(5))
	req, binding := signedRequestWithBinding(t, newAEAD(t), http.MethodPost, ClockPath, raw)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	plain, err := auth.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("answer does not open: %v", err)
	}
	var ans ClockPushAnswer
	if err := json.Unmarshal(plain, &ans); err != nil || ans.Refused == "" || ans.Held != nil {
		t.Fatalf("answer = %+v, %v; want a refusal", ans, err)
	}
}

func TestClockPush_InvalidSettingIsSealedRefusal(t *testing.T) {
	c := platformclock.NewTest("node-b", nil)
	held := clockSetting(3)
	c.Apply(held)
	srv := clockServer(t, newAEAD(t), platformclock.NewHolder(c))
	s := clockSetting(5)
	s.Writer = ""
	_, err := clockSender(t).PushClock(context.Background(), testSelfNodeID, srv.URL, s)
	if err == nil || !strings.Contains(err.Error(), "empty writer") {
		t.Fatalf("err = %v, want empty writer", err)
	}
	if c.Setting() != held {
		t.Fatalf("peer setting = %+v, want unchanged %+v", c.Setting(), held)
	}
}

// A refused push is one WARN line, the route's, and it carries the reason.
func TestClockPush_InvalidSettingLogsOneWarn(t *testing.T) {
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := platformclock.NewTest("node-b", nil)
	c.Apply(clockSetting(3))
	srv := clockServer(t, newAEAD(t), platformclock.NewHolder(c))
	s := clockSetting(5)
	s.Writer = ""
	if _, err := clockSender(t).PushClock(context.Background(), testSelfNodeID, srv.URL, s); err == nil {
		t.Fatal("an invalid setting was held")
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d WARN lines for one refused push, want 1:\n%s", len(lines), logged.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("decode %q: %v", lines[0], err)
	}
	if rec["msg"] != "clock push refused" || !strings.Contains(fmt.Sprint(rec["reason"]), "empty writer") {
		t.Fatalf("WARN line = %v, want \"clock push refused\" with the reason", rec)
	}
}

func postSigned(t *testing.T, h http.Handler, signFor, recipient, path string) *httptest.ResponseRecorder {
	t.Helper()
	sender := newAEAD(t)
	raw, _ := json.Marshal(clockSetting(5))
	req := httptest.NewRequest(http.MethodPost, signFor, nil)
	wire, _, err := sender.Sign(req, recipient, raw)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req.URL.Path = path
	req.Body = io.NopCloser(bytes.NewReader(wire))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func clockMux(auth PeerAuth, holder ClockHolder) http.Handler {
	mux := http.NewServeMux()
	NewClockHandler(auth, holder).Register(mux)
	return mux
}

func TestClockRoute_EnvelopeForCalloutPathRefused(t *testing.T) {
	fh := &fakeHolder{h: platformclock.NewHolder(platformclock.NewTest("n", nil))}
	rec := postSigned(t, clockMux(newAEAD(t), fh), "/internal/dispatch/callout", testSelfNodeID, ClockPath)
	if rec.Code != http.StatusForbidden || fh.calls != 0 {
		t.Fatalf("status = %d, holder calls = %d; want bare 403 and none", rec.Code, fh.calls)
	}
}

func TestClockRoute_EnvelopeForAnotherNodeRefused(t *testing.T) {
	fh := &fakeHolder{h: platformclock.NewHolder(platformclock.NewTest("n", nil))}
	rec := postSigned(t, clockMux(newAEAD(t), fh), ClockPath, "node-other", ClockPath)
	if rec.Code != http.StatusForbidden || fh.calls != 0 {
		t.Fatalf("status = %d, holder calls = %d; want bare 403 and none", rec.Code, fh.calls)
	}
}

type replayFullAuth struct{ *AEADPeerAuth }

func (a replayFullAuth) Verify(r *http.Request) ([]byte, PeerIdentity, ResponseBinding, error) {
	b, id, bind, err := a.AEADPeerAuth.Verify(r)
	if err != nil {
		return b, id, bind, err
	}
	return b, id, bind, ErrReplayCacheFull
}

func TestClockRoute_ReplayCacheFullIsSealedRefusal(t *testing.T) {
	inner := newAEAD(t)
	fh := &fakeHolder{h: platformclock.NewHolder(platformclock.NewTest("n", nil))}
	raw, _ := json.Marshal(clockSetting(5))
	req, binding := signedRequestWithBinding(t, newAEAD(t), http.MethodPost, ClockPath, raw)
	rec := httptest.NewRecorder()
	clockMux(replayFullAuth{inner}, fh).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || fh.calls != 0 {
		t.Fatalf("status = %d, holder calls = %d; want 200 and none", rec.Code, fh.calls)
	}
	plain, err := inner.OpenResponse(rec.Header(), binding, rec.Body.Bytes())
	if err != nil {
		t.Fatalf("answer does not open: %v", err)
	}
	var ans ClockPushAnswer
	if err := json.Unmarshal(plain, &ans); err != nil || ans.Refused == "" {
		t.Fatalf("answer = %+v, %v; want a refusal", ans, err)
	}
}

func TestClockPush_HonoursContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now() // realtime: measures that the call returns promptly
	_, err := NewHTTPForwarder(newAEAD(t), 10*time.Second).AllowLoopbackForTesting().PushClock(ctx, testSelfNodeID, srv.URL, clockSetting(5))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if time.Since(start) > 3*time.Second { // realtime: as above
		t.Fatal("PushClock outlived its context")
	}
}
