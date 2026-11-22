package model

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

// TestClassifyImportErr_UnsupportedValueRoutesToInternal is the L1
// security-review regression on the model-import door. ImportModel's
// importer.Walk failure classification is the sibling of
// ingest.ValidateOrExtend's identical routing on the entity-write door: a
// *schema.UnsupportedValueError is a caller-side contract violation (a Go
// %T-formatted type name, or an internal decoding instruction), never a
// tenant-facing one, and its own doc comment says every production ingress
// decodes with json.UseNumber so this is unreachable today — but the
// classifier must still route it to a 5xx-with-ticket, not echo it into a
// 400 body, should a decoder ever regress.
//
// The public ImportModel(ctx, ImportModelInput) door parses via ParseJSON
// (json.UseNumber) or ParseXML (which also infers json.Number), so there is
// no way to hand it a raw float64 through its own signature — this test
// exercises the classification helper directly, the same way
// entity.classifyValidateOrExtendErr is unit-tested independent of the HTTP
// handler it backs.
func TestClassifyImportErr_UnsupportedValueRoutesToInternal(t *testing.T) {
	unsupported := &schema.UnsupportedValueError{Path: ".x", Kind: "received float64 value; callers must use json.UseNumber() decoding"}

	appErr := classifyImportErr(unsupported, "person", 1)

	if appErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", appErr.Status, http.StatusInternalServerError)
	}
	if appErr.Level != common.LevelInternal {
		t.Errorf("level = %v, want LevelInternal", appErr.Level)
	}
	// The internal detail must never leak into the client-safe Message.
	if want := "float64"; strings.Contains(appErr.Message, want) {
		t.Errorf("client-facing Message = %q must not contain internal detail %q", appErr.Message, want)
	}
	if !errors.Is(appErr.Unwrap(), unsupported) {
		t.Errorf("AppError must wrap the original *schema.UnsupportedValueError for log correlation")
	}
}

// TestClassifyImportErr_InvalidFieldNameStays400 is the negative control: the
// existing content-level rejections (bad field name, non-document body) keep
// their 400 VALIDATION_FAILED classification with domain detail in the body.
func TestClassifyImportErr_InvalidFieldNameStays400(t *testing.T) {
	fieldErr := &schema.InvalidFieldNameError{Path: ".bad name", Parent: "", Name: "bad name"}

	appErr := classifyImportErr(fieldErr, "person", 1)

	if appErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
	if appErr.Code != common.ErrCodeValidationFailed {
		t.Errorf("code = %q, want %q", appErr.Code, common.ErrCodeValidationFailed)
	}
}
