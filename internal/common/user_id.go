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

// ValidateUserID reports whether id is a user identity cyoda-go admits: valid
// UTF-8, not empty, at most MaxUserIDLen characters, and none of these
// characters:
//
//   - a control character, U+0000–U+001F and U+007F–U+009F;
//   - a noncharacter, U+FDD0–U+FDEF and the last two code points of every
//     plane (U+FFFE, U+FFFF, U+1FFFE, … U+10FFFF);
//   - U+FFFD, the replacement character.
//
// The first two are the characters the CloudEvents spec forbids in a String
// attribute, and a user id is sent to compute nodes as the authid attribute.
// U+FFFD is excluded because a JSON decoder turns every invalid UTF-8 byte and
// every lone surrogate escape into it: admitting it would let distinct signed
// claims name one user.
//
// Every door that takes a user id from outside — the first-party JWT claim, the
// OIDC sub, a token-exchange subject and CYODA_BOOTSTRAP_USER_ID — applies this
// one rule. A user id is not a key or a path segment, so unlike a tenant id it
// has no grammar beyond this: any other character is admitted, and nothing is
// normalised.
//
// The returned error NEVER contains id. A rejected user id is attacker-chosen
// and reaches slog through the auth failure path's detail field. The error
// carries the reason, and for a rejected character its code point and
// position, instead.
func ValidateUserID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidUserID)
	}
	if !utf8.ValidString(id) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidUserID)
	}
	if n := utf8.RuneCountInString(id); n > MaxUserIDLen {
		return fmt.Errorf("%w: %d characters exceeds the %d-character limit",
			ErrInvalidUserID, n, MaxUserIDLen)
	}
	pos := 0
	for _, r := range id {
		if isDisallowedUserIDRune(r) {
			return fmt.Errorf("%w: disallowed character U+%04X at character %d",
				ErrInvalidUserID, r, pos)
		}
		pos++
	}
	return nil
}

func isDisallowedUserIDRune(r rune) bool {
	switch {
	case r <= 0x1f, r >= 0x7f && r <= 0x9f: // control characters
		return true
	case r >= 0xfdd0 && r <= 0xfdef, r&0xfffe == 0xfffe: // noncharacters
		return true
	case r == utf8.RuneError: // U+FFFD
		return true
	}
	return false
}
