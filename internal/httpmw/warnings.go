package httpmw

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Bounds on the warnings one HTTP response carries. They are header field
// lines, and a proxy refuses a response whose header block is larger than its
// buffer (nginx's proxy_buffer_size is 4 KiB on most platforms), so the whole
// set stays well inside that: at most 2 KiB of values, plus the marker of a cut
// set and the field names.
// The gRPC `warnings` array is not bounded by these.
const (
	// maxHeaderWarnings is the most warnings one response carries.
	maxHeaderWarnings = 16
	// maxWarningValueBytes bounds one encoded value; a longer one is cut and
	// ends with an encoded "…".
	maxWarningValueBytes = 512
	// maxWarningHeaderBytes bounds the encoded values of all warnings together.
	maxWarningHeaderBytes = 2048
)

// omittedWarnings is the one value added when warnings were left out, the way
// maxMemberWarnings's bound adds one in internal/grpc/dispatch.go.
const omittedWarnings = "further warnings were omitted"

// encodedEllipsis is "…" percent-encoded: the mark of a cut value.
const encodedEllipsis = "%E2%80%A6"

// Warnings installs the request's diagnostics bag (common.WithDiagnostics) and
// sends what has been added to it (common.AddWarning) as Cyoda-Warning
// response header field lines, one per warning, in the order they were added.
// Headers go before the status line, so the warnings are added when the
// response starts — at the first WriteHeader, Write or Flush, or when the
// handler returns without writing. A warning added after that cannot be sent.
func Warnings(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := common.WithDiagnostics(r.Context())
		ww := &warningWriter{ResponseWriter: w, diag: common.GetDiagnostics(ctx)}
		next.ServeHTTP(ww, r.WithContext(ctx))
		// A handler that wrote nothing gets net/http's implicit 200, written
		// below this wrapper from the header as it stands now.
		ww.start()
	})
}

// warningWriter adds the bag's warnings to the header when the response
// starts.
type warningWriter struct {
	http.ResponseWriter
	diag    *common.RequestDiagnostics
	started bool
}

func (w *warningWriter) start() {
	if w.started {
		return
	}
	w.started = true
	h := w.ResponseWriter.Header()
	for _, v := range warningHeaderValues(w.diag.GetWarnings()) {
		h.Add(common.WarningHeader, v)
	}
}

func (w *warningWriter) WriteHeader(status int) {
	w.start()
	w.ResponseWriter.WriteHeader(status)
}

func (w *warningWriter) Write(p []byte) (int, error) {
	w.start()
	return w.ResponseWriter.Write(p)
}

// Flush starts the response first: http.ResponseController looks for Flush
// before it unwraps, so a flush never sends the header without the warnings.
func (w *warningWriter) Flush() {
	w.start()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the writer beneath.
func (w *warningWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// warningHeaderValues encodes warnings as header values and bounds the set:
// at most maxHeaderWarnings values and maxWarningHeaderBytes bytes. When a
// warning does not fit, it and every later one are left out and one value,
// omittedWarnings, says so.
func warningHeaderValues(warnings []string) []string {
	out := make([]string, 0, min(len(warnings), maxHeaderWarnings+1))
	total := 0
	for _, w := range warnings {
		v := warningHeaderValue(w)
		if len(out) == maxHeaderWarnings || total+len(v) > maxWarningHeaderBytes {
			return append(out, omittedWarnings)
		}
		out = append(out, v)
		total += len(v)
	}
	return out
}

// warningHeaderValue encodes one warning as a header value of visible ASCII and
// space only. A warning's text can be a compute member's own, so nothing of it
// reaches the wire unencoded:
//   - each control character (C0, DEL, C1: CR and LF among them) becomes a
//     space, so a value can never end the header line or start another;
//   - invalid UTF-8 becomes U+FFFD;
//   - every byte of a non-ASCII character, and '%' itself, is percent-encoded
//     (UTF-8, upper-case hex), as is a space at either end, which a header
//     parser would trim. A standard percent-decoder therefore gives back the
//     text ('+' is a literal plus, not a space).
//
// A value longer than maxWarningValueBytes is cut between characters and ends
// with an encoded "…".
func warningHeaderValue(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	pieces := make([]string, 0, len(s))
	total := 0
	for i, r := range s {
		atEdge := i == 0 || i+utf8.RuneLen(r) == len(s) // the original rune's width
		if unicode.IsControl(r) {
			r = ' '
		}
		p := encodeWarningRune(r, atEdge)
		pieces = append(pieces, p)
		total += len(p)
	}
	if total <= maxWarningValueBytes {
		return strings.Join(pieces, "")
	}
	var b strings.Builder
	for _, p := range pieces {
		if b.Len()+len(p) > maxWarningValueBytes-len(encodedEllipsis) {
			break
		}
		b.WriteString(p)
	}
	b.WriteString(encodedEllipsis)
	return b.String()
}

// encodeWarningRune encodes one character of a warning. edge says it is the
// first or the last character, where a space must be encoded to survive.
func encodeWarningRune(r rune, edge bool) string {
	switch {
	case r == '%':
		return "%25"
	case r == ' ' && edge:
		return "%20"
	case r >= 0x20 && r <= 0x7e:
		return string(r)
	}
	const hex = "0123456789ABCDEF"
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], r)
	out := make([]byte, 0, 3*n)
	for _, c := range buf[:n] {
		out = append(out, '%', hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}
