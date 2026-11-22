package common

import (
	"os"
	"regexp"
	"testing"
)

func TestPatchErrorCodes(t *testing.T) {
	if ErrCodePreconditionRequired != "PRECONDITION_REQUIRED" {
		t.Errorf("got %q", ErrCodePreconditionRequired)
	}
	if ErrCodeUnsupportedMediaType != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("got %q", ErrCodeUnsupportedMediaType)
	}
}

var errCodeConstPattern = regexp.MustCompile(`ErrCode[A-Z][A-Za-z0-9]*\s*=\s*"([A-Z0-9_]+)"`)

// TestKnownErrorCode_IsExactlyWhatThisFileDefines pins the registry to the
// constants beside it. A code defined and not registered would be refused
// wherever a code has to be recognised before it is used; a code registered and
// not defined would admit one nothing in the tree produces.
func TestKnownErrorCode_IsExactlyWhatThisFileDefines(t *testing.T) {
	src, err := os.ReadFile("error_codes.go")
	if err != nil {
		t.Fatalf("read error_codes.go: %v", err)
	}
	defined := map[string]bool{}
	for _, m := range errCodeConstPattern.FindAllStringSubmatch(string(src), -1) {
		defined[m[1]] = true
		if !KnownErrorCode(m[1]) {
			t.Errorf("%q is defined here but KnownErrorCode does not know it", m[1])
		}
	}
	if len(defined) == 0 {
		t.Fatal("no error code constant matched — the pattern has gone stale")
	}
	for code := range knownErrorCodes {
		if !defined[code] {
			t.Errorf("KnownErrorCode knows %q, which this file does not define", code)
		}
	}
}

func TestKnownErrorCode_RefusesWhatTheTreeDoesNotDefine(t *testing.T) {
	for _, code := range []string{"", "NOT_A_CODE", "model_not_found", "SERVER_ERROR ", "SERVER_ERROR\n"} {
		if KnownErrorCode(code) {
			t.Errorf("KnownErrorCode(%q) = true", code)
		}
	}
}
