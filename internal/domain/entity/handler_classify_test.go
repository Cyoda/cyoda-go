package entity

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/ingest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// TestClassifyValidateOrExtendErr_ChangeLevelViolation_GetsValidationFailed
// — a genuine change-level violation is not a polymorphic slot, and it is not
// a malformed request either: the payload parsed and then failed against the
// registered model's evolution policy. It gets 400 VALIDATION_FAILED, with the
// message still describing the level mismatch.
func TestClassifyValidateOrExtendErr_ChangeLevelViolation_GetsValidationFailed(t *testing.T) {
	underlying := fmt.Errorf("change level violation: new field %q at %s requires STRUCTURAL level, but level is %q",
		"b", ".b", "TYPE")

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("nil appErr")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeValidationFailed {
		t.Errorf("code = %q, want %q (not polymorphic, not a malformed request)", appErr.Code, common.ErrCodeValidationFailed)
	}
}

// TestClassifyValidateOrExtendErr_InternalSentinel_Is5xx — an error
// wrapping ingest.ErrInternalSchema (codec/diff/store failures in validateOrExtend)
// classifies as 5xx regardless of its message text. This is the contract
// that replaces the prior fragile string-matching classifier: if a future
// refactor renames a wrap like "failed to compute schema delta" to
// "schema delta: compute failed", classification still routes correctly
// because the sentinel is what's checked, not the string.
func TestClassifyValidateOrExtendErr_InternalSentinel_Is5xx(t *testing.T) {
	cases := []struct {
		name    string
		makeErr func() error
	}{
		{
			name: "renamed wrap string still classifies as 5xx",
			makeErr: func() error {
				// A message deliberately NOT matching the prior string-
				// matching patterns; would have been mis-routed to 4xx
				// under the old classifier.
				return fmt.Errorf("%w: schema codec: unmarshal: %w", ingest.ErrInternalSchema, fmt.Errorf("unexpected end of input"))
			},
		},
		{
			name: "plugin-layer extend failure wrapped with sentinel",
			makeErr: func() error {
				return fmt.Errorf("%w: failed to extend schema: %w", ingest.ErrInternalSchema, fmt.Errorf("pgx: connection refused"))
			},
		},
		{
			name: "diff failure wrapped with sentinel",
			makeErr: func() error {
				return fmt.Errorf("%w: failed to compute schema delta: %w", ingest.ErrInternalSchema, fmt.Errorf("unexpected node kind"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			appErr := classifyValidateOrExtendErr(tc.makeErr())
			if appErr == nil {
				t.Fatal("nil appErr")
			}
			if appErr.Status != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", appErr.Status)
			}
		})
	}
}

// TestClassifyValidateOrExtendErr_UntaggedError_Is4xx — an error that does
// NOT wrap ingest.ErrInternalSchema classifies as 4xx VALIDATION_FAILED.
// Represents e.g. a change-level violation, a validation failure, or an importer.Walk error from malformed user input — all payloads
// that parsed and then failed against the model: client-contract issues, not
// server faults, and not unparseable requests.
func TestClassifyValidateOrExtendErr_UntaggedError_Is4xx(t *testing.T) {
	underlying := fmt.Errorf("validation failed: field .foo must be a string")

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("nil appErr")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeValidationFailed {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeValidationFailed)
	}
}

// TestClassifyValidateOrExtendErr_IncompatibleType_GetsSpecificCode —
// validation errors carrying an ErrKindIncompatibleType entry must surface
// as 400 INCOMPATIBLE_TYPE with structured Props (`fieldPath`,
// `expectedType`, `actualType`) rather than the generic BAD_REQUEST.
// Wires the cyoda-go response to Cloud's
// FoundIncompatibleTypeWithEntityModelException dictionary class.
func TestClassifyValidateOrExtendErr_IncompatibleType_GetsSpecificCode(t *testing.T) {
	underlying := &ingest.IncompatibleTypeError{
		Path:          "price",
		ExpectedTypes: []schema.DataType{schema.Integer},
		ActualType:    schema.Double,
		Message:       "validation failed: price: value of type DOUBLE is not compatible with [INTEGER]",
	}

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("nil appErr")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeIncompatibleType {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeIncompatibleType)
	}
	if appErr.Props["fieldPath"] != "price" {
		t.Errorf("fieldPath: got %v, want %q", appErr.Props["fieldPath"], "price")
	}
	if appErr.Props["actualType"] != "DOUBLE" {
		t.Errorf("actualType: got %v, want %q", appErr.Props["actualType"], "DOUBLE")
	}
	expected, _ := appErr.Props["expectedType"].([]string)
	if len(expected) != 1 || expected[0] != "INTEGER" {
		t.Errorf("expectedType: got %v, want [INTEGER]", appErr.Props["expectedType"])
	}
}
