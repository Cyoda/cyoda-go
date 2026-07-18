package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// TestSystemUserContext_HasTenant proves SystemUserContext synthesises a
// real identity — not just a struct that happens to have a Tenant field —
// by driving it through the actual TransactionManager.Begin gate that
// rejects a missing tenant (plugins/memory/txmanager.go Begin). Without
// this identity, the scheduler's background fire (which has no
// caller-derived UserContext at all) could never open a transaction.
func TestSystemUserContext_HasTenant(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)

	ctx := SystemUserContext(spi.TenantID("tenant-x"))

	uc := spi.GetUserContext(ctx)
	if uc == nil {
		t.Fatal("SystemUserContext must attach a UserContext")
	}
	if uc.UserID != "scheduler" {
		t.Errorf("UserID = %q, want %q", uc.UserID, "scheduler")
	}
	if uc.Tenant.ID != "tenant-x" {
		t.Errorf("Tenant.ID = %q, want %q", uc.Tenant.ID, "tenant-x")
	}

	txID, _, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(SystemUserContext(...)) failed: %v", err)
	}
	if txID == "" {
		t.Error("expected a non-empty txID")
	}

	// Prove the identity is load-bearing, not incidental: Begin rejects a
	// context with no tenant at all — exactly what the scan loop's own
	// context.Background() would produce without this helper.
	if _, _, err := txMgr.Begin(context.Background()); err == nil {
		t.Error("Begin(context.Background()) should fail with no user context/tenant")
	}
}

// fakeEngine is a scheduler.Engine test double that records what it was
// called with, so tests can assert LocalExecutor built the right context
// and passed the task through untouched.
type fakeEngine struct {
	calls   int
	gotCtx  context.Context
	gotTask spi.ScheduledTask
	outcome string
	err     error
}

func (f *fakeEngine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (string, error) {
	f.calls++
	f.gotCtx = ctx
	f.gotTask = task
	return f.outcome, f.err
}

func TestLocalExecutor_FiresViaEngineWithSystemContext(t *testing.T) {
	fake := &fakeEngine{outcome: "fired"}
	exec := NewLocalExecutor(fake)

	task := spi.ScheduledTask{ID: "t1", TenantID: spi.TenantID("tenant-x"), EntityID: "e1"}
	exec.Execute(context.Background(), task, "self")

	if fake.calls != 1 {
		t.Fatalf("expected 1 engine call, got %d", fake.calls)
	}
	if fake.gotTask.ID != "t1" {
		t.Errorf("task not passed through unchanged: got %+v", fake.gotTask)
	}
	uc := spi.GetUserContext(fake.gotCtx)
	if uc == nil {
		t.Fatal("engine did not receive a UserContext at all")
	}
	if uc.Tenant.ID != "tenant-x" {
		t.Errorf("engine's context tenant = %q, want %q", uc.Tenant.ID, "tenant-x")
	}
}

func TestLocalExecutor_ErrorDoesNotPanic(t *testing.T) {
	fake := &fakeEngine{err: errors.New("boom")}
	exec := NewLocalExecutor(fake)

	// Must not panic even though the engine reports an error.
	exec.Execute(context.Background(), spi.ScheduledTask{ID: "t2", TenantID: "tenant-x"}, "self")

	if fake.calls != 1 {
		t.Fatalf("expected the engine to still be invoked once, got %d", fake.calls)
	}
}

// capturedRecord is a decoded slog record — level, message, and attrs — used
// by captureSlog below so tests can assert on log level without depending on
// a specific handler's text/JSON encoding.
type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// captureHandler is a minimal slog.Handler that records every emitted
// record (at any level) into a shared slice, guarded by a mutex since the
// scheduler's fire path runs on its own goroutine in production.
type captureHandler struct {
	mu      *sync.Mutex
	records *[]capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, capturedRecord{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// captureSlog swaps slog.Default with captureHandler for the duration of the
// test and returns the backing slice of decoded records, restoring the
// previous default handler on cleanup.
func captureSlog(t *testing.T) *[]capturedRecord {
	t.Helper()
	records := &[]capturedRecord{}
	prev := slog.Default()
	slog.SetDefault(slog.New(&captureHandler{mu: &sync.Mutex{}, records: records}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return records
}

// TestLocalExecutor_FireFailure_LogsErrorWithStructuredFields drives a fire
// whose engine call fails — the scenario the design calls out: a downstream
// arm-Function's compute node is unavailable, the fire transaction rolls
// back, and today's only surface is a slog.Warn. A broken downstream
// function silently blocking an unrelated scheduled transition (and
// retrying every scan) must be observable at ERROR, with the task's
// identity in the structured fields so an operator can find it.
func TestLocalExecutor_FireFailure_LogsErrorWithStructuredFields(t *testing.T) {
	engineErr := errors.New("compute node unavailable")
	fake := &fakeEngine{err: engineErr}
	exec := NewLocalExecutor(fake)

	task := spi.ScheduledTask{
		ID:          "t-fail-1",
		TenantID:    "tenant-x",
		EntityID:    "entity-1",
		Transition:  "ARM_NEXT",
		SourceState: "PENDING",
	}
	records := captureSlog(t)

	exec.Execute(context.Background(), task, "self")

	if fake.calls != 1 {
		t.Fatalf("expected the engine to still be invoked once, got %d", fake.calls)
	}

	var found *capturedRecord
	for i := range *records {
		if (*records)[i].msg == "scheduled task local fire failed" {
			found = &(*records)[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a 'scheduled task local fire failed' log record, got %+v", *records)
	}
	if found.level != slog.LevelError {
		t.Errorf("level = %v, want %v (a broken downstream arm-Function must be ERROR-visible, not WARN)", found.level, slog.LevelError)
	}
	if got := found.attrs["pkg"]; got != "scheduler" {
		t.Errorf("pkg = %v, want %q", got, "scheduler")
	}
	if got := found.attrs["entityId"]; got != task.EntityID {
		t.Errorf("entityId = %v, want %q", got, task.EntityID)
	}
	if got := found.attrs["transition"]; got != task.Transition {
		t.Errorf("transition = %v, want %q", got, task.Transition)
	}
	if got := found.attrs["sourceState"]; got != task.SourceState {
		t.Errorf("sourceState = %v, want %q", got, task.SourceState)
	}
	if found.attrs["err"] == nil {
		t.Errorf("expected an 'err' attr carrying the engine error, got none")
	}
}
