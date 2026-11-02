package app

import (
	"context"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

func lifecycleTestApp(t *testing.T) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ContextPath = ""
	return New(cfg)
}

func TestNew_ReturnsNormal(t *testing.T) {
	a := lifecycleTestApp(t)
	t.Cleanup(func() { a.Shutdown(); _ = a.Close() })
	if got := a.Lifecycle().State(); got != lifecycle.Normal {
		t.Fatalf("state after New = %s, want NORMAL", got)
	}
}

func TestDrainScheduler_SetsDraining(t *testing.T) {
	a := lifecycleTestApp(t)
	t.Cleanup(func() { a.Shutdown(); _ = a.Close() })
	a.DrainScheduler(context.Background())
	if got := a.Lifecycle().State(); got != lifecycle.Draining {
		t.Fatalf("state = %s, want DRAINING", got)
	}
}

func TestShutdown_Alone_SetsDraining(t *testing.T) {
	a := lifecycleTestApp(t)
	t.Cleanup(func() { _ = a.Close() })
	a.Shutdown()
	if got := a.Lifecycle().State(); got != lifecycle.Draining {
		t.Fatalf("state = %s, want DRAINING", got)
	}
}

func TestClose_Alone_SetsDraining(t *testing.T) {
	a := lifecycleTestApp(t)
	_ = a.Close()
	if got := a.Lifecycle().State(); got != lifecycle.Draining {
		t.Fatalf("state = %s, want DRAINING", got)
	}
}

func TestLatchTrip_SetsUnhealthy(t *testing.T) {
	a := lifecycleTestApp(t)
	t.Cleanup(func() { a.Shutdown(); _ = a.Close() })
	recoverPanicOnce(a)
	if got := a.Lifecycle().State(); got != lifecycle.Unhealthy {
		t.Fatalf("state = %s, want UNHEALTHY", got)
	}
}
