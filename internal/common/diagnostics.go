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

// WarningHeader is the HTTP response header that carries a request's warnings,
// one header field line per warning (internal/httpmw.Warnings); a gRPC
// response carries them in its `warnings` array.
const WarningHeader = "Cyoda-Warning"

// WithDiagnostics creates a new context with an empty RequestDiagnostics.
// Called once per request: by internal/httpmw.Warnings for an HTTP data route,
// at each gRPC entry point, and by the peer callout hand-over handler.
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
