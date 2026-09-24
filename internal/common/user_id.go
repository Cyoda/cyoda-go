package common

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxUserIDLen bounds a user identity at 255 characters (runes, not bytes).
const MaxUserIDLen = 255

// ErrInvalidUserID reports a user identity cyoda-go does not admit.
var ErrInvalidUserID = errors.New("invalid user id")

// ValidateUserID reports whether id is a user identity cyoda-go admits: not
// empty, at most MaxUserIDLen characters, and no ASCII control character
// (U+0000–U+001F, U+007F). Every door that takes a user id from outside —
// the first-party JWT claim, the OIDC sub, a token-exchange subject and
// CYODA_BOOTSTRAP_USER_ID — applies this one rule.
//
// A user id is not a key or a path segment, so unlike a tenant id it has no
// grammar beyond this: any printable character is admitted, and nothing is
// normalised.
//
// The returned error NEVER contains id. A rejected user id is attacker-chosen
// and reaches slog through the auth failure path's detail field. The error
// carries a reason and a character position instead.
func ValidateUserID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidUserID)
	}
	if n := utf8.RuneCountInString(id); n > MaxUserIDLen {
		return fmt.Errorf("%w: %d characters exceeds the %d-character limit",
			ErrInvalidUserID, n, MaxUserIDLen)
	}
	pos := 0
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character U+%04X at character %d",
				ErrInvalidUserID, r, pos)
		}
		pos++
	}
	return nil
}
