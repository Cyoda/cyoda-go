package common

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestAddWarning_GetWarnings(t *testing.T) {
	ctx := WithDiagnostics(context.Background())
	AddWarning(ctx, "w1")
	AddWarning(ctx, "w2")

	got := GetDiagnostics(ctx).GetWarnings()
	if len(got) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(got))
	}
	if got[0] != "w1" || got[1] != "w2" {
		t.Fatalf("unexpected warnings: %v", got)
	}
}

func TestGetDiagnostics_NilContext(t *testing.T) {
	ctx := context.Background()
	d := GetDiagnostics(ctx)
	if d != nil {
		t.Fatal("expected nil diagnostics from plain context")
	}
}

func TestAddWarning_NilContext(t *testing.T) {
	// Must not panic when diagnostics is absent from context.
	ctx := context.Background()
	AddWarning(ctx, "should not panic")
}

func TestGetWarnings_NilReceiver(t *testing.T) {
	var d *RequestDiagnostics
	got := d.GetWarnings()
	if got == nil {
		t.Fatal("expected non-nil empty slice from nil receiver")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestConcurrentAddWarning(t *testing.T) {
	ctx := WithDiagnostics(context.Background())
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			AddWarning(ctx, fmt.Sprintf("w-%d", idx))
		}(i)
	}
	wg.Wait()

	got := GetDiagnostics(ctx).GetWarnings()
	if len(got) != n {
		t.Fatalf("expected %d warnings, got %d", n, len(got))
	}
}

func TestGetWarnings_ReturnsCopy(t *testing.T) {
	ctx := WithDiagnostics(context.Background())
	AddWarning(ctx, "original")

	got := GetDiagnostics(ctx).GetWarnings()
	got[0] = "mutated"

	again := GetDiagnostics(ctx).GetWarnings()
	if again[0] != "original" {
		t.Fatalf("internal state was mutated through returned slice: got %q", again[0])
	}
}
