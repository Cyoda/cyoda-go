package ingest

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// TestRejectUnstorablePayload pins the boundary rule for values no supported
// backend can persist: U+0000, unpaired UTF-16 surrogates, and byte sequences
// that are not valid UTF-8. All three are accepted by Go's JSON parser but
// rejected by PostgreSQL text/jsonb, so without this guard they surfaced as a
// 500 with a support ticket on postgres and were silently accepted on memory
// and sqlite.
//
// The guard reads RAW bytes deliberately — see RejectUnstorable's doc
// comment. The "already-mangled" cases below are what make that necessary.
func TestRejectUnstorablePayload(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantPath string // "" means the payload must be accepted
		wantWord string // a distinguishing word from the expected reason
	}{
		// --- storable ---
		{name: "plain object", payload: `{"name":"ok","amount":1}`},
		{name: "empty object", payload: `{}`},
		{name: "empty array", payload: `[]`},
		{name: "nested", payload: `{"a":{"b":["x",{"c":"y"}]}}`},
		{name: "other control chars", payload: `{"name":"a\tb\nc"}`},
		{name: "escaped quote and backslash", payload: `{"name":"a\"b\\c"}`},
		{name: "valid surrogate pair (emoji)", payload: `{"name":"a😀b"}`},
		{name: "non-ascii escape", payload: `{"name":"café"}`},
		{name: "literal utf8 non-ascii", payload: `{"name":"café"}`},
		{name: "client-sent replacement char", payload: "{\"name\":\"a\ufffdb\"}"},
		{name: "numbers and null", payload: `{"a":1e400,"b":null,"c":true}`},
		{name: "leading/trailing space", payload: "  {\"name\":\"ok\"}  "},

		// A literal backslash followed by u0000 is six ordinary characters, not
		// an escape. The escape walker must not mistake it for one.
		{name: "literal backslash then u0000", payload: `{"name":"a\\u0000b"}`},
		{name: "literal backslash then ud800", payload: `{"name":"a\\ud800b"}`},

		// --- unstorable: NUL ---
		{name: "nul escape in value", payload: `{"name":"a\u0000b"}`, wantPath: "name", wantWord: "NUL"},
		{name: "nul nested", payload: `{"outer":{"inner":"\u0000"}}`, wantPath: "outer.inner", wantWord: "NUL"},
		{name: "nul in array element", payload: `{"tags":["ok","bad\u0000"]}`, wantPath: "tags[1]", wantWord: "NUL"},
		{name: "nul in nested array of objects", payload: `{"items":[{"sku":"ok"},{"sku":"\u0000x"}]}`, wantPath: "items[1].sku", wantWord: "NUL"},
		{name: "nul in object key", payload: "{\"bad\\u0000key\":\"v\"}", wantPath: "bad\x00key", wantWord: "NUL"},
		{name: "bare string payload", payload: `"a\u0000"`, wantPath: "(root)", wantWord: "NUL"},

		// --- unstorable: unpaired surrogate ---
		{name: "lone high surrogate", payload: `{"name":"a\ud800b"}`, wantPath: "name", wantWord: "surrogate"},
		{name: "lone low surrogate", payload: `{"name":"a\udc00b"}`, wantPath: "name", wantWord: "surrogate"},
		{name: "high surrogate at end", payload: `{"name":"ab\udbff"}`, wantPath: "name", wantWord: "surrogate"},
		{name: "high followed by non-surrogate escape", payload: `{"name":"\ud800A"}`, wantPath: "name", wantWord: "surrogate"},
		{name: "surrogate nested in array", payload: `{"t":["ok","\udfff"]}`, wantPath: "t[1]", wantWord: "surrogate"},

		// --- unstorable: invalid UTF-8 ---
		{name: "raw invalid utf8", payload: "{\"name\":\"a\xffb\"}", wantPath: "(payload)", wantWord: "UTF-8"},
		{name: "truncated utf8 sequence", payload: "{\"name\":\"a\xe2\x82\"}", wantPath: "(payload)", wantWord: "UTF-8"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectUnstorable([]byte(tc.payload))

			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("payload %q rejected but should be accepted: %v", tc.payload, err)
				}
				return
			}

			if err == nil {
				t.Fatalf("payload %q accepted but should be rejected (expected path %q)", tc.payload, tc.wantPath)
			}
			appErr, ok := err.(*common.AppError)
			if !ok {
				t.Fatalf("error is %T, want *common.AppError", err)
			}
			if appErr.Status != http.StatusBadRequest {
				t.Errorf("status=%d, want 400", appErr.Status)
			}
			if appErr.Code != common.ErrCodeBadRequest {
				t.Errorf("code=%q, want %q", appErr.Code, common.ErrCodeBadRequest)
			}
			// The path is rendered with %q so a control byte in a key is never
			// echoed raw into the response body.
			if want := strconv.Quote(tc.wantPath); !strings.Contains(appErr.Message, want) {
				t.Errorf("message %q does not name the offending path %s", appErr.Message, want)
			}
			if !strings.Contains(appErr.Message, tc.wantWord) {
				t.Errorf("message %q does not explain the reason (expected to mention %q)", appErr.Message, tc.wantWord)
			}
		})
	}
}

// TestRejectUnstorablePayload_MalformedJSONIsNotOurJob asserts the guard stays
// silent on syntactically broken input. Malformed JSON is the decoder's
// rejection to make, with its own message; the guard must not pre-empt it with
// a confusing "unstorable" error.
func TestRejectUnstorablePayload_MalformedJSONIsNotOurJob(t *testing.T) {
	// A RAW (unescaped) NUL byte inside a string literal belongs here rather
	// than with the unstorable cases: JSON forbids unescaped control
	// characters, so the decoder rejects the body before the guard sees it.
	// The caller still gets a 400, just from the decoder's message.
	for _, payload := range []string{`{"a":`, `not json`, ``, `{`, `[1,`, "{\"name\":\"a\x00b\"}"} {
		if err := RejectUnstorable([]byte(payload)); err != nil {
			t.Errorf("payload %q: guard returned %v; malformed JSON is the decoder's rejection", payload, err)
		}
	}
}
