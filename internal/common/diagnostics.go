package common

import (
	"context"
	"sync"
)

// RequestDiagnostics accumulates warnings during request processing.
// Thread-safe for concurrent access from multiple goroutines (e.g., async processors).
type RequestDiagnostics struct {
	mu       sync.Mutex
	warnings []string
}

type diagnosticsKey struct{}

// WithDiagnostics creates a new context with an empty RequestDiagnostics.
// Call at the start of every request (HTTP handler or gRPC handler entry).
func WithDiagnostics(ctx context.Context) context.Context {
	return context.WithValue(ctx, diagnosticsKey{}, &RequestDiagnostics{})
}

// GetDiagnostics returns the RequestDiagnostics from the context, or nil if not present.
func GetDiagnostics(ctx context.Context) *RequestDiagnostics {
	d, _ := ctx.Value(diagnosticsKey{}).(*RequestDiagnostics)
	return d
}

// AddWarning appends a warning message to the diagnostics.
// Safe to call even if diagnostics is not initialized on the context (silent no-op).
func AddWarning(ctx context.Context, msg string) {
	if d := GetDiagnostics(ctx); d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.warnings = append(d.warnings, msg)
	}
}

// GetWarnings returns accumulated warnings, or an empty slice (never nil).
func (d *RequestDiagnostics) GetWarnings() []string {
	if d == nil {
		return []string{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.warnings) == 0 {
		return []string{}
	}
	cp := make([]string, len(d.warnings))
	copy(cp, d.warnings)
	return cp
}
