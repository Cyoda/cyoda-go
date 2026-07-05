package entity

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// TestClassifyValidateOrExtendErr_PolymorphicSlot asserts that a
// schema.ErrPolymorphicSlot-wrapped error is classified as an operational
// 4xx with the dedicated POLYMORPHIC_SLOT code — NOT a generic BAD_REQUEST
// and NOT a 5xx internal error. SDKs detect this code to display the
// "normalize the field" guidance instead of the misleading
// "change level violation" text previously exposed.
func TestClassifyValidateOrExtendErr_PolymorphicSlot(t *testing.T) {
	// Wrap the sentinel the same way schema.Extend does at its call site.
	underlying := fmt.Errorf("%w at %q: existing %s, incoming %s — normalize the field",
		schema.ErrPolymorphicSlot, ".roles_and_permissions.custom_permissions", "ARRAY", "LEAF")

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("classifyValidateOrExtendErr returned nil")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
	if appErr.Code != common.ErrCodePolymorphicSlot {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodePolymorphicSlot)
	}
	if strings.Contains(appErr.Message, "change level violation") {
		t.Errorf("message must NOT say 'change level violation' for polymorphic slot (misleading); got: %q", appErr.Message)
	}
	if !strings.Contains(appErr.Message, "polymorphic") {
		t.Errorf("message must name polymorphism so clients can search docs; got: %q", appErr.Message)
	}
}

// TestClassifyValidateOrExtendErr_ChangeLevelViolation_StillGetsBadRequest
// — genuine change-level violations keep the existing classification path:
// 4xx BAD_REQUEST, with the message still describing the level mismatch.
func TestClassifyValidateOrExtendErr_ChangeLevelViolation_StillGetsBadRequest(t *testing.T) {
	underlying := fmt.Errorf("change level violation: new field %q at %s requires STRUCTURAL level, but level is %q",
		"b", ".b", "TYPE")

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("nil appErr")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeBadRequest {
		t.Errorf("code = %q, want %q (not polymorphic)", appErr.Code, common.ErrCodeBadRequest)
	}
}

// TestClassifyValidateOrExtendErr_InternalSentinel_Is5xx — an error
// wrapping errInternalSchema (codec/diff/store failures in validateOrExtend)
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
				return fmt.Errorf("%w: schema codec: unmarshal: %w", errInternalSchema, fmt.Errorf("unexpected end of input"))
			},
		},
		{
			name: "plugin-layer extend failure wrapped with sentinel",
			makeErr: func() error {
				return fmt.Errorf("%w: failed to extend schema: %w", errInternalSchema, fmt.Errorf("pgx: connection refused"))
			},
		},
		{
			name: "diff failure wrapped with sentinel",
			makeErr: func() error {
				return fmt.Errorf("%w: failed to compute schema delta: %w", errInternalSchema, fmt.Errorf("unexpected node kind"))
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

// TestClassifyValidateOrExtendErr_UntaggedError_Is4xx — an error NOT
// wrapping either ErrPolymorphicSlot or errInternalSchema classifies as
// 4xx BAD_REQUEST. Represents e.g. a change-level violation, a validation
// failure, or an importer.Walk error from malformed user input — all
// client-contract issues, not server faults.
func TestClassifyValidateOrExtendErr_UntaggedError_Is4xx(t *testing.T) {
	underlying := fmt.Errorf("validation failed: field .foo must be a string")

	appErr := classifyValidateOrExtendErr(underlying)
	if appErr == nil {
		t.Fatal("nil appErr")
	}
	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", appErr.Status)
	}
	if appErr.Code != common.ErrCodeBadRequest {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeBadRequest)
	}
}

// TestClassifyValidateOrExtendErr_IncompatibleType_GetsSpecificCode —
// validation errors carrying an ErrKindIncompatibleType entry must surface
// as 400 INCOMPATIBLE_TYPE with structured Props (`fieldPath`,
// `expectedType`, `actualType`) rather than the generic BAD_REQUEST.
// Wires the cyoda-go response to Cloud's
// FoundIncompatibleTypeWithEntityModelException dictionary class.
func TestClassifyValidateOrExtendErr_IncompatibleType_GetsSpecificCode(t *testing.T) {
	underlying := &incompatibleTypeError{
		path:          "price",
		expectedTypes: []schema.DataType{schema.Integer},
		actualType:    schema.Double,
		message:       "validation failed: price: value of type DOUBLE is not compatible with [INTEGER]",
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
