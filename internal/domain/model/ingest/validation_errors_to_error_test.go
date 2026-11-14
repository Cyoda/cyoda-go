package ingest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// TestValidationErrorsToError_CapsRenderedEntries is the M2(a) security-review
// regression: ValidationErrorsToError used to join EVERY ValidationError into
// one string with no cap. Validate emits one entry per offending path with no
// limit, so a body carrying hundreds of thousands of undeclared one-character
// fields would render a response body proportional to the attacker's request
// size — tens of megabytes for a body well within the existing 10 MiB request
// cap. The fix renders only the first maxRenderedValidationErrors entries
// verbatim and appends a "... and N more" summary for the rest, while the
// incompatible-type selection (FirstIncompatibleType) keeps reading the
// slice directly and is therefore unaffected by how the string is capped.
func TestValidationErrorsToError_CapsRenderedEntries(t *testing.T) {
	const n = 100
	errs := make([]schema.ValidationError, n)
	for i := range errs {
		errs[i] = schema.ValidationError{
			Path:    fmt.Sprintf("field%d", i),
			Message: "unexpected field not present in model",
			Kind:    schema.ErrKindGeneric,
		}
	}

	err := ValidationErrorsToError(errs)
	if err == nil {
		t.Fatal("want a non-nil error for 100 validation failures")
	}
	msg := err.Error()

	renderedCount := 0
	for i := 0; i < n; i++ {
		if strings.Contains(msg, fmt.Sprintf("field%d:", i)) {
			renderedCount++
		}
	}
	if renderedCount != maxRenderedValidationErrors {
		t.Errorf("rendered %d entries verbatim, want exactly %d", renderedCount, maxRenderedValidationErrors)
	}

	wantSuffix := fmt.Sprintf("and %d more", n-maxRenderedValidationErrors)
	if !strings.Contains(msg, wantSuffix) {
		t.Errorf("message = %q, want it to contain %q", msg, wantSuffix)
	}

	// A tiny, arbitrary-but-generous bound: the old unbounded join of 100
	// short entries would already run past this; a capped message of 32
	// short entries plus the summary comfortably fits.
	const maxSaneLen = 4096
	if len(msg) > maxSaneLen {
		t.Errorf("message length = %d, want it bounded (<%d) regardless of how many errors were passed in", len(msg), maxSaneLen)
	}
}

// TestValidationErrorsToError_FewerThanCapRendersAllNoSuffix is the negative
// control: when the offending-path count is at or below the cap, every entry
// is rendered and no "and N more" summary is appended.
func TestValidationErrorsToError_FewerThanCapRendersAllNoSuffix(t *testing.T) {
	errs := []schema.ValidationError{
		{Path: "a", Message: "unexpected field not present in model", Kind: schema.ErrKindGeneric},
		{Path: "b", Message: "unexpected field not present in model", Kind: schema.ErrKindGeneric},
	}
	msg := ValidationErrorsToError(errs).Error()
	if !strings.Contains(msg, "a:") || !strings.Contains(msg, "b:") {
		t.Errorf("message = %q, want both entries rendered", msg)
	}
	if strings.Contains(msg, "more") {
		t.Errorf("message = %q, must not carry a summary suffix under the cap", msg)
	}
}
