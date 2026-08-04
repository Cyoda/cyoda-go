package entity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Reasons a payload cannot be stored. Kept as distinct sentences so the
// rejection tells the caller what to change.
const (
	reasonNul       = "contains a NUL character (U+0000), which cannot be stored"
	reasonSurrogate = "contains an unpaired UTF-16 surrogate escape, which is not storable text"
	reasonNotUTF8   = "is not valid UTF-8"
)

// rejectUnstorablePayload rejects an entity payload that no supported backend
// can persist, before it reaches a store.
//
// PostgreSQL's text and jsonb types cannot represent U+0000, an unpaired
// UTF-16 surrogate, or a byte sequence that is not valid UTF-8. All three are
// accepted by Go's JSON parser, so without this guard they reached the store
// and failed there — returning 500 with a support ticket for what is a client
// input error — while the memory and sqlite stores accepted them, making the
// set of storable values depend on which backend served the request.
//
// It operates on the RAW request bytes, not the decoded value, and that is
// load-bearing rather than stylistic: Go's decoder silently rewrites both
// unpaired surrogates and invalid UTF-8 to U+FFFD, so by the time a value is
// decoded a mangled surrogate is indistinguishable from a client legitimately
// sending U+FFFD. Rejecting on the decoded value would therefore be impossible
// for two of the three cases, and re-serialising the decoded value — the other
// obvious fix — would silently store a replacement character the client never
// sent, which is exactly the substituted-value outcome
// .claude/rules/correctness-over-availability.md forbids.
func rejectUnstorablePayload(raw []byte) error {
	// Whole-document check first. Invalid UTF-8 has no meaningful JSON path —
	// the bytes may not even parse as text — so it is reported against the
	// document rather than a field.
	if !utf8.Valid(raw) {
		return unstorableErr("(payload)", reasonNotUTF8)
	}
	if path, reason, found := findUnstorable(raw, ""); found {
		if path == "" {
			path = "(root)"
		}
		return unstorableErr(path, reason)
	}
	return nil
}

func unstorableErr(path, reason string) error {
	return common.Operational(
		http.StatusBadRequest,
		common.ErrCodeBadRequest,
		// %q so a control byte in a key or value is never echoed raw into the
		// response body.
		fmt.Sprintf("payload field %q %s", path, reason),
	)
}

// findUnstorable walks the raw JSON structure, returning the path and reason of
// the first unstorable string it finds. Object keys are visited in sorted order
// so the reported path is deterministic.
//
// Values are kept as json.RawMessage so each string is examined exactly as the
// client wrote it. Object KEYS are the one exception — decoding an object hands
// back keys as Go strings, already normalised — so a key is only checked for
// NUL, which survives decoding. A surrogate in a key is not detectable here.
func findUnstorable(raw json.RawMessage, path string) (string, string, bool) {
	trimmed := trimJSONSpace(raw)
	if len(trimmed) == 0 {
		return "", "", false
	}

	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal(trimmed, &obj) != nil {
			return "", "", false // malformed JSON is rejected by the decoder, not here
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := k
			if path != "" {
				child = path + "." + k
			}
			for _, r := range k {
				if r == 0 {
					return child, reasonNul, true
				}
			}
			if p, reason, found := findUnstorable(obj[k], child); found {
				return p, reason, true
			}
		}
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(trimmed, &arr) != nil {
			return "", "", false
		}
		for i, elem := range arr {
			if p, reason, found := findUnstorable(elem, fmt.Sprintf("%s[%d]", path, i)); found {
				return p, reason, true
			}
		}
	case '"':
		if reason := checkJSONStringToken(trimmed); reason != "" {
			return path, reason, true
		}
	}
	return "", "", false
}

// checkJSONStringToken inspects a raw JSON string token (quotes included) for
// content no backend can store. It walks escapes properly, so a literal
// backslash sequence such as `a\\u0000b` — six ordinary characters, not an
// escape — is correctly left alone.
func checkJSONStringToken(tok []byte) string {
	for i := 0; i < len(tok); {
		c := tok[i]
		if c == 0 {
			return reasonNul
		}
		if c != '\\' {
			i++
			continue
		}
		if i+1 >= len(tok) {
			break
		}
		if tok[i+1] != 'u' {
			// A two-character escape (\\ \" \/ \b \f \n \r \t). Consuming both
			// bytes is what keeps `\\uXXXX` from being read as an escape.
			i += 2
			continue
		}
		r, ok := parseHex4(tok, i+2)
		if !ok {
			i += 2
			continue
		}
		switch {
		case r == 0:
			return reasonNul
		case r >= 0xD800 && r <= 0xDBFF:
			// High surrogate — valid only when immediately followed by a low
			// surrogate escape.
			if lo, ok := parseHex4(tok, i+8); ok && i+7 < len(tok) &&
				tok[i+6] == '\\' && tok[i+7] == 'u' && lo >= 0xDC00 && lo <= 0xDFFF {
				i += 12
				continue
			}
			return reasonSurrogate
		case r >= 0xDC00 && r <= 0xDFFF:
			// Low surrogate with no preceding high surrogate: a paired one is
			// consumed above, so reaching here means it is unpaired.
			return reasonSurrogate
		}
		i += 6
	}
	return ""
}

// parseHex4 reads the four hex digits at off, reporting whether they are present
// and well-formed.
func parseHex4(b []byte, off int) (rune, bool) {
	if off+4 > len(b) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(b[off:off+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

// trimJSONSpace strips the whitespace JSON permits around a value.
func trimJSONSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isJSONSpace(b[start]) {
		start++
	}
	for end > start && isJSONSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// prefixItemErr restates an operational error against the collection item it
// came from, so a batch rejection says which element was at fault.
func prefixItemErr(err error, i int) error {
	appErr, ok := err.(*common.AppError)
	if !ok {
		return err
	}
	restated := common.Operational(appErr.Status, appErr.Code, fmt.Sprintf("item %d: %s", i, appErr.Message))
	restated.Props = appErr.Props
	return restated
}
