package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/internal/domain/model/schema"
)

// TestValidateFieldName_TruncatesLongNameAndParent is the L3 security-review
// regression. InvalidFieldNameError.Error() interpolated the offending field
// name and its parent path with %q and no length bound, so a pathological
// field name (or a deeply-nested parent path) produced a client-facing
// message roughly 4x the size of the attacker-supplied name/path — the name
// and parent each appear once, and %q itself escapes/doubles some bytes. The
// fix truncates both to a bounded prefix before formatting; the wording
// otherwise (and the InvalidFieldNameError's own Path/Parent/Name fields,
// which callers use programmatically) is unchanged.
func TestValidateFieldName_TruncatesLongNameAndParent(t *testing.T) {
	bigName := strings.Repeat("a ", 50*1024) // ~100 KB; the embedded spaces make it unaddressable

	err := schema.ValidateFieldName("$.parent", bigName)
	if err == nil {
		t.Fatal("want an error for an unaddressable field name")
	}

	var fe *schema.InvalidFieldNameError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %T, want *schema.InvalidFieldNameError", err)
	}

	const maxSaneLen = 1024
	if len(fe.Error()) >= maxSaneLen {
		t.Errorf("message length = %d, want it bounded (<%d bytes) despite a 100 KB field name", len(fe.Error()), maxSaneLen)
	}
	if !errors.Is(err, schema.ErrInvalidFieldName) {
		t.Errorf("error = %v, want errors.Is(err, schema.ErrInvalidFieldName)", err)
	}
}

// TestValidateFieldName_TruncatesLongParent covers the parent half of the
// same interpolation: a pathologically long (but otherwise well-formed)
// parent path must not blow up the message either.
func TestValidateFieldName_TruncatesLongParent(t *testing.T) {
	bigParent := "$" + strings.Repeat(".nested", 20000) // ~140 KB parent path

	err := schema.ValidateFieldName(bigParent, "bad name")
	if err == nil {
		t.Fatal("want an error for an unaddressable field name")
	}

	const maxSaneLen = 1024
	if len(err.Error()) >= maxSaneLen {
		t.Errorf("message length = %d, want it bounded (<%d bytes) despite a ~140 KB parent path", len(err.Error()), maxSaneLen)
	}
}

// TestValidateFieldName_ShortNameMessageUnchanged pins the exact wording for
// the ordinary case, so the truncation fix does not alter behaviour when no
// truncation is needed.
func TestValidateFieldName_ShortNameMessageUnchanged(t *testing.T) {
	err := schema.ValidateFieldName("", "bad name")
	if err == nil {
		t.Fatal("want an error")
	}
	want := `invalid field name: "bad name" in object at "$" — a field name must be addressable as a jsonPath segment: ASCII letters, digits, "_" and "-" only, and not empty; rename the field`
	if err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}
