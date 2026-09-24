package common

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateUserID_Accepts(t *testing.T) {
	accepted := []string{
		"admin",                                // CYODA_BOOTSTRAP_USER_ID default
		"user-42",                              // e2e fixtures
		"9f8c7b6a5d4e3f2a1b0c9d8e7f6a5b4c",     // generated M2M client id
		"1a2b3c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d", // UUID
		"alice@example.com",
		"oidc-style/sub",
		"用户",                     // non-ASCII is admitted
		"a",                      // shortest legal
		strings.Repeat("u", 255), // longest legal, in characters
		strings.Repeat("é", 255), // 255 characters, 510 bytes
		"user\u00a0x",            // first character after the C1 range
		"user\ufdcf\ufdf0",       // either side of the FDD0–FDEF noncharacters
		"user\ufffc",             // just below U+FFFD
		"user\U0001f600",         // outside the BMP
	}
	for _, id := range accepted {
		if err := ValidateUserID(id); err != nil {
			t.Errorf("ValidateUserID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidateUserID_Rejects(t *testing.T) {
	rejected := map[string]string{
		"empty":    "",
		"too-long": strings.Repeat("u", 256),
		"newline":  "user\ninjected",
		"nul":      "user\x00",
		"cr":       "user\r",
		"tab":      "user\tid",
		"del":      "user\x7f",
		"escape":   "user\x1b[31m",
		// C1 controls and noncharacters: the CloudEvents spec forbids them in
		// a String attribute, and a user id is sent as the authid attribute.
		"c1-first":        "user\u0080",
		"c1-nel":          "user\u0085",
		"c1-last":         "user\u009f",
		"nonchar-fdd0":    "user\ufdd0",
		"nonchar-fdef":    "user\ufdef",
		"nonchar-fffe":    "user\ufffe",
		"nonchar-ffff":    "user\uffff",
		"nonchar-plane1":  "user\U0001fffe",
		"nonchar-plane16": "user\U0010ffff",
		// Invalid UTF-8 and U+FFFD: a JSON decoder turns every invalid byte
		// and lone surrogate into U+FFFD, so admitting it would let distinct
		// inputs name one user.
		"invalid-utf8":     "user\xff",
		"replacement-char": "user\ufffd",
	}
	for name, id := range rejected {
		t.Run(name, func(t *testing.T) {
			err := ValidateUserID(id)
			if err == nil {
				t.Fatalf("ValidateUserID(%q) = nil, want an error", id)
			}
			if !errors.Is(err, ErrInvalidUserID) {
				t.Errorf("error %v does not wrap ErrInvalidUserID", err)
			}
			if id != "" && strings.Contains(err.Error(), id) {
				t.Errorf("error echoes the rejected id: %q", err.Error())
			}
		})
	}
}

// The error carries a position so an operator can find the bad character
// without the log carrying the value itself.
func TestValidateUserID_ReportsCharacterPosition(t *testing.T) {
	err := ValidateUserID("用户\nx")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "U+000A at character 2") {
		t.Errorf("err = %q, want it to name U+000A at character 2", err.Error())
	}
}

// A first-party user id cannot begin with the prefix the OIDC path gives its
// principals, in any case, or it could name the same user as an OIDC principal.
func TestValidateFirstPartyUserID_ReservesOIDCPrefix(t *testing.T) {
	for _, id := range []string{
		"oidc:11111111-2222-3333-4444-555555555555:alice",
		"oidc:",
		"OIDC:x",
		"Oidc:x",
	} {
		err := ValidateFirstPartyUserID(id)
		if !errors.Is(err, ErrInvalidUserID) {
			t.Errorf("ValidateFirstPartyUserID(%q) = %v, want ErrInvalidUserID", id, err)
		}
	}
	for _, id := range []string{"oidc", "oidc-user", "my-oidc:x", "admin"} {
		if err := ValidateFirstPartyUserID(id); err != nil {
			t.Errorf("ValidateFirstPartyUserID(%q) = %v, want nil", id, err)
		}
	}
	// Everything ValidateUserID rejects, it rejects too.
	if err := ValidateFirstPartyUserID("a\nb"); !errors.Is(err, ErrInvalidUserID) {
		t.Errorf("ValidateFirstPartyUserID(control char) = %v, want ErrInvalidUserID", err)
	}
}
