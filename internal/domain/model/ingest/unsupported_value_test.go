package ingest

import (
	"context"
	"errors"
	"testing"
)

// TestValidateOrExtend_UnsupportedValueRoutesToInternalSchema is the L1
// security-review regression. schema.Admit (reached via schema.Extend) returns
// a *schema.UnsupportedValueError for a value its traversal cannot classify —
// a raw float64 leaking through without json.UseNumber decoding, or a Go type
// none of our decoders ever produce. That error's own text is a caller-side
// contract violation, not a client-facing one ("received float64 value;
// callers must use json.UseNumber() decoding" — an internal decoding
// instruction, not something a tenant would understand or can act on). Before
// this fix, ValidateOrExtend's catch-all ("schema admission failed: %w")
// wrapped it unmarked, so classifyValidateOrExtendErr's catch-all routed it to
// a 400 VALIDATION_FAILED carrying that internal detail verbatim into the
// response body. It must instead be marked with ErrInternalSchema so the
// handler routes it to a 5xx with a logged ticket, matching every other
// caller-side (not tenant-side) failure this function already marks.
//
// Every production ingress decodes with json.UseNumber, so this path is
// unreachable today (see schema.UnsupportedValueError's own doc comment) —
// this test reaches it directly by handing ValidateOrExtend a raw float64,
// exercising the same seam a caller-contract regression (a decoder losing its
// UseNumber option) would actually hit.
func TestValidateOrExtend_UnsupportedValueRoutesToInternalSchema(t *testing.T) {
	_, err := ValidateOrExtend(context.Background(), &recordingModelStore{}, descWithChangeLevel(t),
		map[string]any{"x": float64(1.5)})
	if err == nil {
		t.Fatal("want an error for a float64 value reaching schema admission")
	}
	if !errors.Is(err, ErrInternalSchema) {
		t.Errorf("error = %v, want errors.Is(err, ErrInternalSchema)", err)
	}
}
