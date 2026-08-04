package ingest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Reasons a payload cannot be stored. Kept as distinct sentences so the
// rejection tells the caller what to change.
const (
	reasonNul          = "contains a NUL character (U+0000), which cannot be stored"
	reasonSurrogate    = "contains an unpaired UTF-16 surrogate escape, which is not storable text"
	reasonNotUTF8      = "is not valid UTF-8"
	reasonDuplicateKey = "appears more than once in the same object, so different parts of the system would read different values for it"
	reasonNumWeight    = "is a number with too many digits before the decimal point to be stored"
	reasonNumScale     = "is a number with too many digits after the decimal point to be stored"
)

// Limits of PostgreSQL's numeric type, which jsonb parses every JSON number
// into. Past either, the write fails inside the store rather than at the
// boundary.
const (
	maxNumericWeightDigits = 131072
	maxNumericScaleDigits  = 16383
	// PostgreSQL bounds the exponent itself, whatever the coefficient: a zero
	// coefficient has no weight, but 0e2000000000 still overflows on input.
	maxNumericExponent = 1073741823
	// maxExponentMagnitude saturates exponent accumulation. Any exponent this
	// large already breaches both limits, so clamping cannot change a verdict —
	// it only stops a 20-digit exponent from overflowing the accumulator.
	maxExponentMagnitude = 1 << 40
)

// RejectUnstorable rejects an entity payload that no supported backend
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
func RejectUnstorable(raw []byte) error {
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

// maxReportedPathLen bounds the field path echoed back. The path comes from the
// caller's own document, so an oversized key would otherwise inflate both the
// response body and the log line WriteError emits for every operational error.
const maxReportedPathLen = 200

func unstorableErr(path, reason string) error {
	if len(path) > maxReportedPathLen {
		path = path[:maxReportedPathLen] + "…(truncated)"
	}
	return common.Operational(
		http.StatusBadRequest,
		common.ErrCodeBadRequest,
		// %q so a control byte in a key or value is never echoed raw into the
		// response body.
		fmt.Sprintf("payload field %q %s", path, reason),
	)
}

// findUnstorable walks the raw JSON structure, returning the path and reason of
// the first unstorable content it finds. Object members are visited in document
// order.
//
// Every string — object keys included — is examined as the client wrote it,
// which is why the scan works on byte offsets rather than decoded values. The
// decoded form of a key is used only for duplicate comparison and for the
// reported path, never for judging its content.
func findUnstorable(raw []byte, path string) (string, string, bool) {
	st := make([]pathSeg, 0, 64)
	if path != "" {
		st = append(st, pathSeg{key: path})
	}
	p, r, found, _ := scanValue(raw, 0, &st, 0)
	return p, r, found
}

// pathSeg is one step of a JSON path. The scan carries a stack of these rather
// than an accumulated string: building the string at every node would itself be
// quadratic in depth, which is the cost this scanner exists to avoid. The string
// is rendered only when something is actually rejected.
//
// The stack is threaded by POINTER and pushed/popped, not derived per node. An
// earlier version passed `append(segs, seg)` to each child, which looks
// harmless but re-runs growslice for every sibling whenever len == cap at that
// depth — copying the whole path stack each time. That reintroduced the
// O(size x depth) blowup this scanner was written to remove, just relocated
// from the subtree into the path: a 7.8 MB well-formed payload allocated 1.45 TB
// and took nearly four minutes, and was then accepted.
type pathSeg struct {
	key   string
	idx   int
	isIdx bool
}

func renderPath(segs []pathSeg) string {
	var b strings.Builder
	for _, s := range segs {
		if s.isIdx {
			fmt.Fprintf(&b, "[%d]", s.idx)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		if s.key == "" {
			b.WriteString(`""`) // an empty name, not the document root
			continue
		}
		b.WriteString(s.key)
	}
	return b.String()
}

// maxScanDepth caps nesting. encoding/json refuses to decode deeper than this
// anyway, so a document beyond it is rejected by the decoder regardless; the cap
// exists so the scan itself can never be driven into unbounded recursion.
const maxScanDepth = 10000

// scanValue examines the JSON value starting at i and returns the offset just
// past it, along with the first unstorable content found inside.
//
// It scans in place. An earlier version re-parsed each subtree once per nesting
// level — json.Unmarshal into []json.RawMessage copies every element, and a
// json.Decoder per object level copies every member — which made the walk
// O(size × depth) in both time and allocation. Measured on the version this
// replaces: a 21 KB payload nested 9999 deep took 1.1 s and allocated 1.76 GiB,
// an ~85,000x amplification on one authenticated request. Nothing bounds the
// product of size and depth: the 10 MB body limit bounds only size.
//
// This version allocates only a per-object key set, and only for objects.
func scanValue(b []byte, i int, st *[]pathSeg, depth int) (string, string, bool, int) {
	i = skipJSONSpace(b, i)
	if i >= len(b) {
		return "", "", false, i
	}
	if depth > maxScanDepth {
		// Deeper than any decoder will accept; let the decoder reject it.
		return "", "", false, len(b)
	}

	switch b[i] {
	case '{':
		i++
		seen := make(map[string]struct{})
		*st = append(*st, pathSeg{})
		defer func() { *st = (*st)[:len(*st)-1] }()
		for {
			i = skipJSONSpace(b, i)
			if i >= len(b) {
				return "", "", false, i
			}
			if b[i] == '}' {
				return "", "", false, i + 1
			}
			if b[i] == ',' {
				i++
				continue
			}
			if b[i] != '"' {
				return "", "", false, len(b) // malformed; the decoder rejects it
			}
			keyStart := i
			keyEnd, ok := scanString(b, i)
			if !ok {
				return "", "", false, len(b)
			}
			keyRaw := b[keyStart:keyEnd]
			key, kok := unquoteJSONKey(keyRaw)
			if !kok {
				return "", "", false, len(b)
			}
			(*st)[len(*st)-1] = pathSeg{key: key}
			// The key's RAW bytes, so an unpaired surrogate or a NUL escape in a
			// key is caught. Decoding the key first would normalise both away.
			if reason := checkJSONStringToken(keyRaw); reason != "" {
				return renderPath(*st), reason, true, i
			}
			if _, dup := seen[key]; dup {
				return renderPath(*st), reasonDuplicateKey, true, i
			}
			seen[key] = struct{}{}

			i = skipJSONSpace(b, keyEnd)
			if i >= len(b) || b[i] != ':' {
				return "", "", false, len(b)
			}
			i++
			var p, r string
			var found bool
			before := i
			p, r, found, i = scanValue(b, i, st, depth+1)
			if found {
				return p, r, true, i
			}
			if i <= before {
				return "", "", false, len(b) // no progress: malformed
			}
		}

	case '[':
		i++
		idx := 0
		*st = append(*st, pathSeg{isIdx: true})
		defer func() { *st = (*st)[:len(*st)-1] }()
		for {
			i = skipJSONSpace(b, i)
			if i >= len(b) {
				return "", "", false, i
			}
			if b[i] == ']' {
				return "", "", false, i + 1
			}
			if b[i] == ',' {
				i++
				continue
			}
			var p, r string
			var found bool
			(*st)[len(*st)-1] = pathSeg{idx: idx, isIdx: true}
			before := i
			p, r, found, i = scanValue(b, i, st, depth+1)
			if found {
				return p, r, true, i
			}
			if i <= before {
				return "", "", false, len(b) // no progress: malformed
			}
			idx++
		}

	case '"':
		end, ok := scanString(b, i)
		if !ok {
			return "", "", false, len(b)
		}
		if reason := checkJSONStringToken(b[i:end]); reason != "" {
			return renderPath(*st), reason, true, end
		}
		return "", "", false, end

	default:
		end := scanScalar(b, i)
		if end <= i {
			// b[i] cannot start a value — a mismatched '}' or ']', or a stray
			// byte. The document is malformed and the decoder will reject it;
			// stop rather than re-examine the same offset forever.
			return "", "", false, len(b)
		}
		if c := b[i]; c == '-' || (c >= '0' && c <= '9') {
			if reason := checkJSONNumberToken(b[i:end]); reason != "" {
				return renderPath(*st), reason, true, end
			}
		}
		return "", "", false, end
	}
}

// scanString returns the offset just past the string token starting at i
// (which must be the opening quote), honouring escapes.
func scanString(b []byte, i int) (int, bool) {
	if i >= len(b) || b[i] != '"' {
		return 0, false
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++ // skip the escaped byte; \uXXXX's digits are ordinary bytes here
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// scanScalar returns the offset just past a number, true, false or null.
func scanScalar(b []byte, i int) int {
	for j := i; j < len(b); j++ {
		switch b[j] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return j
		}
	}
	return len(b)
}

// unquoteJSONKey decodes a raw key token to the string the rest of the system
// will see, for duplicate detection and path reporting.
func unquoteJSONKey(raw []byte) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// skipJSONSpace advances past the whitespace JSON permits.
func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && isJSONSpace(b[i]) {
		i++
	}
	return i
}

// checkJSONNumberToken reports whether a raw JSON number literal is outside the
// range PostgreSQL's numeric type can hold.
//
// The check is on effective weight and scale rather than on the digits as
// written, because the exponent moves both: 1.5e-16383 has a single fraction
// digit and an in-range exponent yet still overflows the scale, and 12e131071
// overflows the weight that 1e131071 does not. Leading zeros are not
// significant, so 0.0001e131075 is really 1e131071 and fits.
//
// It is purely lexical and allocates nothing — materialising 1e1000000 would
// mean building a million-digit value to decide it is too big.
func checkJSONNumberToken(tok []byte) string {
	i := 0
	if i < len(tok) && (tok[i] == '-' || tok[i] == '+') {
		i++
	}
	intStart := i
	for i < len(tok) && isDigit(tok[i]) {
		i++
	}
	intEnd := i

	fracStart, fracEnd := i, i
	if i < len(tok) && tok[i] == '.' {
		i++
		fracStart = i
		for i < len(tok) && isDigit(tok[i]) {
			i++
		}
		fracEnd = i
	}

	var exp int64
	if i < len(tok) && (tok[i] == 'e' || tok[i] == 'E') {
		i++
		neg := false
		if i < len(tok) && (tok[i] == '+' || tok[i] == '-') {
			neg = tok[i] == '-'
			i++
		}
		var v int64
		for i < len(tok) && isDigit(tok[i]) {
			if v <= maxExponentMagnitude {
				v = v*10 + int64(tok[i]-'0')
			}
			i++
		}
		if v > maxExponentMagnitude {
			v = maxExponentMagnitude
		}
		if neg {
			v = -v
		}
		exp = v
	}

	// Only a syntactically complete JSON number gets a range verdict. A token
	// like `1e1000000x` is a syntax error, and reporting it as "too many digits"
	// would send the caller looking in the wrong place. The decoder rejects it
	// a moment later with an accurate message.
	if i != len(tok) || intEnd == intStart {
		return ""
	}

	intDigits := int64(intEnd - intStart)
	fracDigits := int64(fracEnd - fracStart)

	// The exponent alone, before any coefficient reasoning: PostgreSQL rejects
	// a literal past this even when the coefficient is zero.
	if exp > maxNumericExponent || exp < -maxNumericExponent {
		return reasonNumWeight
	}

	// Scale: digits that end up to the right of the point. Counted lexically,
	// so trailing zeros bite — 1.00000000e-16383 really does overflow.
	if fracDigits-exp > maxNumericScaleDigits {
		return reasonNumScale
	}

	// Weight: digits to the left of the point, ignoring leading zeros. A zero
	// coefficient has no weight, however large the exponent — within the
	// exponent bound checked above.
	leadingZeros, allZero := int64(0), true
	for _, r := range [2][2]int{{intStart, intEnd}, {fracStart, fracEnd}} {
		for j := r[0]; j < r[1]; j++ {
			if tok[j] != '0' {
				allZero = false
				break
			}
			leadingZeros++
		}
		if !allZero {
			break
		}
	}
	if !allZero && intDigits-leadingZeros+exp > maxNumericWeightDigits {
		return reasonNumWeight
	}
	return ""
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

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

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// PrefixItemErr restates an operational error against the collection item it
// came from, so a batch rejection says which element was at fault.
func PrefixItemErr(err error, i int) error {
	appErr, ok := err.(*common.AppError)
	if !ok {
		return err
	}
	restated := common.Operational(appErr.Status, appErr.Code, fmt.Sprintf("item %d: %s", i, appErr.Message))
	restated.Props = appErr.Props
	return restated
}
