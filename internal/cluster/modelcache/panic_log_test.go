package modelcache

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// errorRecords is a slog handler that keeps every ERROR record's message and
// attributes.
type errorRecords struct {
	mu      sync.Mutex
	records []map[string]string
}

func (h *errorRecords) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }

func (h *errorRecords) Handle(_ context.Context, r slog.Record) error {
	rec := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		rec[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, rec)
	return nil
}

func (h *errorRecords) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorRecords) WithGroup(string) slog.Handler      { return h }

func (h *errorRecords) snapshot() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]string(nil), h.records...)
}

// captureErrors routes slog's default logger to a capturing handler for the
// test's duration.
func captureErrors(t *testing.T) *errorRecords {
	t.Helper()
	h := &errorRecords{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func checkPanicRecord(t *testing.T, recs []map[string]string, value string) {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("ERROR records = %d, want exactly 1: %v", len(recs), recs)
	}
	r := recs[0]
	if r["pkg"] != "modelcache" {
		t.Errorf("pkg = %q, want modelcache", r["pkg"])
	}
	if !strings.Contains(r["panic"], value) {
		t.Errorf("panic attr = %q, want it to contain %q", r["panic"], value)
	}
	if !strings.Contains(r["stack"], "panic_log_test.go") {
		t.Errorf("stack attr does not show the panicking frame: %q", r["stack"])
	}
}

// TestSharedCallPanic_LoggedWhenEveryCallerLeft: a shared call that panics
// after its last caller has gone has no door to answer it, so the recovery
// site logs it at ERROR, once.
func TestSharedCallPanic_LoggedWhenEveryCallerLeft(t *testing.T) {
	logs := captureErrors(t)
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	release := make(chan struct{})
	inner.extFn = func(context.Context, int) error {
		<-release
		panic("extension exploded after its callers left")
	}
	s := &requestScopedStore{cache: c, inner: inner}

	ctx, cancel := context.WithCancel(tenantCtx())
	done := make(chan error, 1)
	go func() { done <- s.ExtendSchema(ctx, testRef, testDelta) }()
	waitSignal(t, inner.extEntered, "the extension to reach storage")
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller error = %v, want context.Canceled", err)
	}
	close(release)

	deadline := time.After(5 * time.Second)
	for len(logs.snapshot()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the panic of a shared call nobody waits on was not logged")
		case <-time.After(5 * time.Millisecond):
		}
	}
	checkPanicRecord(t, logs.snapshot(), "extension exploded after its callers left")
}

// TestSharedCallPanic_LoggedOnceWithLiveCaller: with a caller still waiting,
// the panic is logged once at the recovery site and re-raised in the caller.
func TestSharedCallPanic_LoggedOnceWithLiveCaller(t *testing.T) {
	logs := captureErrors(t)
	c := newTestCache(nil)
	inner := newFakeInner(lockedDesc())
	inner.getFn = func(context.Context, int) error { panic("read exploded with a caller waiting") }
	s := &requestScopedStore{cache: c, inner: inner}

	if got := recoverFrom(func() { _, _ = s.Get(tenantCtx(), testRef) }); got == nil {
		t.Fatal("the caller did not see the panic")
	}
	checkPanicRecord(t, logs.snapshot(), "read exploded with a caller waiting")
}
