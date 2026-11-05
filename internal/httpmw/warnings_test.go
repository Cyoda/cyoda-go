package httpmw

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// A warning's text can be a compute member's own: it must never put a control
// character on the wire, and it must reach the client as the text it is.
func TestWarningHeaderValue_EncodesSafely(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain ASCII is unchanged", "data quality issue: missing field X", "data quality issue: missing field X"},
		{"CR and LF become spaces", "a\r\nCyoda-Injected: yes", "a  Cyoda-Injected: yes"},
		{"every C0 control, DEL and C1 control becomes a space", "a\x00b\tc\x1fd\x7fe\u0085f\u009fg", "a b c d e f g"},
		{"non-ASCII is percent-encoded UTF-8", "model — using default", "model %E2%80%94 using default"},
		{"a percent sign is encoded so the value decodes unambiguously", "100% done", "100%25 done"},
		{"invalid UTF-8 becomes U+FFFD", "bad\xffbyte", "bad%EF%BF%BDbyte"},
		{"a space at either end is encoded so it survives header trimming", " edge ", "%20edge%20"},
		{"a control character at an end becomes an encoded space", "x\n", "x%20"},
		{"a two-byte control character at the end becomes an encoded space", "x\u0085", "x%20"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := warningHeaderValue(tc.in)
			if got != tc.want {
				t.Fatalf("warningHeaderValue(%q) = %q; want %q", tc.in, got, tc.want)
			}
			for i := 0; i < len(got); i++ {
				if c := got[i]; c < 0x20 || c > 0x7e {
					t.Fatalf("value %q carries byte 0x%02x: a header value must be visible ASCII and space only", got, c)
				}
			}
		})
	}
}

// The encoding is the URL path-segment one, so a client decodes a value with any
// standard percent-decoder.
func TestWarningHeaderValue_RoundTripsThroughPercentDecoding(t *testing.T) {
	for _, in := range []string{"no workflows imported for model — using default workflow", "100% ünïcödé 日本", " a ", "+plus+"} {
		got, err := url.PathUnescape(warningHeaderValue(in))
		if err != nil {
			t.Fatalf("decode %q: %v", warningHeaderValue(in), err)
		}
		if got != in {
			t.Errorf("round trip of %q gave %q", in, got)
		}
	}
}

// One value is bounded: a cut never splits an escape or a rune, and is marked
// with an encoded "…" the way boundMemberText marks one.
func TestWarningHeaderValue_BoundsOneValue(t *testing.T) {
	for _, in := range []string{strings.Repeat("a", 2000), strings.Repeat("é", 2000), strings.Repeat("a", maxWarningValueBytes-1) + "日本"} {
		got := warningHeaderValue(in)
		if len(got) > maxWarningValueBytes {
			t.Fatalf("value is %d bytes; want at most %d", len(got), maxWarningValueBytes)
		}
		if !strings.HasSuffix(got, "%E2%80%A6") {
			t.Fatalf("a cut value must end with an encoded ellipsis: ...%q", got[len(got)-20:])
		}
		dec, err := url.PathUnescape(got)
		if err != nil {
			t.Fatalf("a cut value must still decode: %v", err)
		}
		if !strings.HasPrefix(in, strings.TrimSuffix(dec, "…")) {
			t.Fatalf("a cut value must be a prefix of the warning")
		}
	}
	exact := strings.Repeat("a", maxWarningValueBytes)
	if got := warningHeaderValue(exact); got != exact {
		t.Fatalf("a value of exactly the bound must not be cut")
	}
}

// The whole set is bounded in count and in size; when cut, one more value says
// the rest were omitted. The order is kept, and nothing after the cut is sent.
func TestWarningHeaderValues_BoundsTheSet(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		in := make([]string, maxHeaderWarnings+5)
		for i := range in {
			in[i] = "w"
		}
		got := warningHeaderValues(in)
		if len(got) != maxHeaderWarnings+1 {
			t.Fatalf("got %d values; want %d warnings and one omission notice", len(got), maxHeaderWarnings)
		}
		if got[maxHeaderWarnings] != omittedWarnings {
			t.Fatalf("last value = %q; want %q", got[maxHeaderWarnings], omittedWarnings)
		}
	})
	t.Run("exactly the count is not cut", func(t *testing.T) {
		in := make([]string, maxHeaderWarnings)
		for i := range in {
			in[i] = "w"
		}
		if got := warningHeaderValues(in); len(got) != maxHeaderWarnings {
			t.Fatalf("got %d values; want %d and no omission notice", len(got), maxHeaderWarnings)
		}
	})
	t.Run("size", func(t *testing.T) {
		big := strings.Repeat("b", maxWarningValueBytes)
		in := []string{big, big, big, big, "small"}
		got := warningHeaderValues(in)
		total := 0
		for _, v := range got[:len(got)-1] {
			total += len(v)
		}
		if total > maxWarningHeaderBytes {
			t.Fatalf("warnings take %d bytes; want at most %d", total, maxWarningHeaderBytes)
		}
		if got[len(got)-1] != omittedWarnings {
			t.Fatalf("last value = %q; want the omission notice", got[len(got)-1])
		}
		for _, v := range got {
			if v == "small" {
				t.Fatal("a warning after the cut was sent: the order must be kept")
			}
		}
	})
	t.Run("none", func(t *testing.T) {
		if got := warningHeaderValues(nil); len(got) != 0 {
			t.Fatalf("got %q; want no values", got)
		}
	})
}

// warn is a handler that adds each of ws, then answers with respond.
func warn(respond func(w http.ResponseWriter, r *http.Request), ws ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, s := range ws {
			common.AddWarning(r.Context(), s)
		}
		respond(w, r)
	})
}

func TestWarnings_SendsEachWarningAsOneFieldLine(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"WriteHeader": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
		"Write":       func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) },
		"Flush":       func(w http.ResponseWriter, _ *http.Request) { w.(http.Flusher).Flush() },
		"ResponseController.Flush": func(w http.ResponseWriter, _ *http.Request) {
			_ = http.NewResponseController(w).Flush()
		},
		"WriteJSON": func(w http.ResponseWriter, _ *http.Request) { common.WriteJSON(w, http.StatusOK, []string{}) },
		"WriteError": func(w http.ResponseWriter, r *http.Request) {
			common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "no"))
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Warnings(warn(respond, "first", "second — two")).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			got := rec.Result().Header.Values(common.WarningHeader)
			want := []string{"first", "second %E2%80%94 two"}
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("%s = %q; want %q", common.WarningHeader, got, want)
			}
		})
	}
}

func TestWarnings_NoWarningNoHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	Warnings(warn(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if _, ok := rec.Result().Header[common.WarningHeader]; ok {
		t.Fatalf("a response with no warning carries %s: %q", common.WarningHeader, rec.Result().Header.Values(common.WarningHeader))
	}
}

// A warning added once the response has started cannot be sent: the header has
// gone. It is not sent later either (no trailer, no second header).
func TestWarnings_AWarningAfterTheResponseStartedIsNotSent(t *testing.T) {
	rec := httptest.NewRecorder()
	Warnings(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		common.AddWarning(r.Context(), "before")
		w.WriteHeader(http.StatusOK)
		common.AddWarning(r.Context(), "after")
		_, _ = w.Write([]byte("{}"))
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Result().Header.Values(common.WarningHeader); len(got) != 1 || got[0] != "before" {
		t.Fatalf("%s = %q; want only the warning added before the response started", common.WarningHeader, got)
	}
}

// The bounds apply on the wire, not only in the helper.
func TestWarnings_BoundsTheResponse(t *testing.T) {
	ws := make([]string, maxHeaderWarnings+3)
	for i := range ws {
		ws[i] = "w"
	}
	rec := httptest.NewRecorder()
	Warnings(warn(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, ws...)).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	got := rec.Result().Header.Values(common.WarningHeader)
	if len(got) != maxHeaderWarnings+1 || got[maxHeaderWarnings] != omittedWarnings {
		t.Fatalf("got %d values ending %q; want %d and the omission notice", len(got), got[len(got)-1], maxHeaderWarnings+1)
	}
}

// A handler that returns without writing gets net/http's implicit 200, which
// is written below this wrapper; the warnings it raised are still sent.
func TestWarnings_AHandlerThatWritesNothingStillSendsItsWarnings(t *testing.T) {
	srv := httptest.NewServer(Warnings(warn(func(http.ResponseWriter, *http.Request) {}, "quiet")))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Values(common.WarningHeader); len(got) != 1 || got[0] != "quiet" {
		t.Fatalf("%s = %q; want the warning of a handler that wrote nothing", common.WarningHeader, got)
	}
}
