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
