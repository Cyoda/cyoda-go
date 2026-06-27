package common

import "testing"

func TestPatchErrorCodes(t *testing.T) {
	if ErrCodePreconditionRequired != "PRECONDITION_REQUIRED" {
		t.Errorf("got %q", ErrCodePreconditionRequired)
	}
	if ErrCodeUnsupportedMediaType != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("got %q", ErrCodeUnsupportedMediaType)
	}
}
