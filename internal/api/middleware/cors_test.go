package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// dummyHandler returns 200 with body "ok" so we can confirm pass-through.
var dummyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

func newCORSPolicy(t *testing.T, enabled, wildcard bool, allowed ...string) *CORSPolicy {
	t.Helper()
	return NewCORSPolicy(enabled, wildcard, allowed)
}

func TestCORS_DisabledIsIdentityWrapper(t *testing.T) {
	p := newCORSPolicy(t, false, false)
	h := CORS(p)(dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty (disabled mode)", got)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want empty (disabled mode)", got)
	}
}

func TestCORS_WildcardActualRequest(t *testing.T) {
	p := newCORSPolicy(t, true, true)
	h := CORS(p)(dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want \"*\" (wildcard never reflects)", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestCORS_WildcardWithNullOrigin(t *testing.T) {
	p := newCORSPolicy(t, true, true)
	h := CORS(p)(dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "null")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Wildcard mode emits literal * for every origin including null.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want \"*\" (null + wildcard)", got)
	}
}

func TestCORS_LoopbackMatch(t *testing.T) {
	cases := []string{
		"http://localhost",
		"http://localhost:3000",
		"https://localhost:8443",
		"http://127.0.0.1",
		"http://127.0.0.1:5173",
		"https://127.0.0.1:8080",
		"http://[::1]",
		"http://[::1]:8080",
		"https://[::1]:8443",
	}
	p := newCORSPolicy(t, true, false) // loopback mode
	h := CORS(p)(dummyHandler)
	for _, origin := range cases {
		t.Run(origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
			}
		})
	}
}

func TestCORS_LoopbackRejects(t *testing.T) {
	cases := []string{
		"https://evil.example",
		"http://localhost.evil.example",    // suffix attack
		"https://xn--lcalhost-ksb.example", // IDN homograph
		"http://localhost@evil.example",    // hostname injection via userinfo syntax — host is evil.example, not localhost
		"http://127.000.000.001:3000",      // non-canonical IPv4
		"http://[0:0:0:0:0:0:0:1]",         // non-canonical IPv6
		"null",                             // file://
		"ftp://localhost",                  // wrong scheme
		"http://localhost/admin",           // path component
		"http://Localhost:3000",            // case (must be lowercase)
		"HTTP://localhost:3000",            // uppercase scheme
	}
	p := newCORSPolicy(t, true, false)
	h := CORS(p)(dummyHandler)
	for _, origin := range cases {
		t.Run(origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want empty (rejected)", got)
			}
			// Underlying request must still succeed — only the header is omitted.
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (request still processed)", rec.Code)
			}
		})
	}
}

func TestCORS_AllowlistMatch(t *testing.T) {
	p := newCORSPolicy(t, true, false, "https://admin.example.com", "http://app.local:3000")
	h := CORS(p)(dummyHandler)
	t.Run("matched", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://admin.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
			t.Errorf("ACAO = %q, want match", got)
		}
	})
	t.Run("unmatched", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://other.example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("ACAO = %q, want empty (not in allowlist)", got)
		}
	})
	t.Run("loopback not auto-allowed in allowlist mode", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "http://localhost:3000") // not in the list
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("ACAO = %q, want empty (allowlist mode does not auto-allow loopback)", got)
		}
	})
}

func TestCORS_PreflightShortCircuit(t *testing.T) {
	p := newCORSPolicy(t, true, false, "https://admin.example.com")
	// downstream that would 200 if reached — preflight must NOT reach it
	downstreamReached := false
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamReached = true
		w.WriteHeader(http.StatusOK)
	})
	h := CORS(p)(downstream)

	req := httptest.NewRequest(http.MethodOptions, "/anything", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if downstreamReached {
		t.Error("downstream should not have been reached on preflight")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Errorf("ACAO = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, PUT, PATCH, DELETE, OPTIONS" {
		t.Errorf("Allow-Methods = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type, traceparent, tracestate" {
		t.Errorf("Allow-Headers = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Errorf("Max-Age = %q", got)
	}
}

func TestCORS_PreflightWithoutACRMFallsThrough(t *testing.T) {
	// OPTIONS with Origin but no Access-Control-Request-Method is NOT a preflight.
	p := newCORSPolicy(t, true, true)
	downstreamCode := http.StatusTeapot
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(downstreamCode)
	})
	h := CORS(p)(downstream)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://x.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (downstream); CORS must not short-circuit non-preflight OPTIONS", rec.Code, downstreamCode)
	}
}

func TestCORS_PreflightWithoutOriginFallsThrough(t *testing.T) {
	// OPTIONS with ACRM but no Origin: also not a preflight.
	p := newCORSPolicy(t, true, true)
	downstreamCode := http.StatusTeapot
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(downstreamCode)
	})
	h := CORS(p)(downstream)

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, downstreamCode)
	}
}

func TestCORS_PreflightUnmatchedOriginInAllowlist(t *testing.T) {
	// Preflight is still 204, but Access-Control-Allow-Origin is omitted.
	p := newCORSPolicy(t, true, false, "https://allowed.example.com")
	h := CORS(p)(dummyHandler)
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://forbidden.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty (origin not allowed)", got)
	}

	// Static preflight headers must be emitted regardless of origin match.
	// Only Access-Control-Allow-Origin is conditionally omitted. Pinning
	// this prevents a regression where a future refactor moves the static
	// header writes inside the "if allowed != \"\"" gate.
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods missing on rejected-origin preflight")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Access-Control-Allow-Headers missing on rejected-origin preflight")
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Error("Access-Control-Max-Age missing on rejected-origin preflight")
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want \"Origin\" (must be present even for rejected origins)", got)
	}
}

func TestCORS_VaryOriginAlwaysWhenEnabled(t *testing.T) {
	cases := []struct {
		name string
		p    *CORSPolicy
	}{
		{"wildcard", NewCORSPolicy(true, true, nil)},
		{"loopback (default)", NewCORSPolicy(true, false, nil)},
		{"allowlist", NewCORSPolicy(true, false, []string{"https://x.example"})},
	}
	for _, c := range cases {
		t.Run(c.name+"/with-origin", func(t *testing.T) {
			h := CORS(c.p)(dummyHandler)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Origin", "https://x.example")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			vary := rec.Header().Values("Vary")
			hasOrigin := false
			for _, v := range vary {
				if v == "Origin" {
					hasOrigin = true
					break
				}
			}
			if !hasOrigin {
				t.Errorf("Vary missing Origin: %v", vary)
			}
		})
		t.Run(c.name+"/no-origin", func(t *testing.T) {
			// Vary: Origin must be present even when Origin is absent
			// (cache-poisoning protection on mode-flip).
			h := CORS(c.p)(dummyHandler)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			vary := rec.Header().Values("Vary")
			hasOrigin := false
			for _, v := range vary {
				if v == "Origin" {
					hasOrigin = true
					break
				}
			}
			if !hasOrigin {
				t.Errorf("Vary missing Origin: %v", vary)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("ACAO = %q, want empty on no-Origin requests", got)
			}
		})
	}
}

func TestCORS_VaryAppendedNotOverwritten(t *testing.T) {
	// A downstream handler that also sets Vary: Accept must coexist with
	// Vary: Origin — both values present, neither overwritten.
	p := NewCORSPolicy(true, true, nil)
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept")
		w.WriteHeader(http.StatusOK)
	})
	h := CORS(p)(downstream)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://x.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	vary := rec.Header().Values("Vary")
	hasOrigin, hasAccept := false, false
	for _, v := range vary {
		if v == "Origin" {
			hasOrigin = true
		}
		if v == "Accept" {
			hasAccept = true
		}
	}
	if !hasOrigin {
		t.Errorf("Vary missing Origin: %v", vary)
	}
	if !hasAccept {
		t.Errorf("Vary missing Accept: %v", vary)
	}
}

func TestCORS_InternalPrefixSkipped(t *testing.T) {
	p := NewCORSPolicy(true, true, nil) // wildcard — would otherwise emit *
	h := CORS(p)(dummyHandler)
	req := httptest.NewRequest(http.MethodGet, "/internal/dispatch/processor", nil)
	req.Header.Set("Origin", "https://anything.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want empty on /internal/dispatch/", got)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want empty on /internal/dispatch/", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("downstream not reached on /internal/dispatch/")
	}
}

func TestCORS_InternalPrefixBoundaries(t *testing.T) {
	// Pin the design decision that /internal/dispatch/ (with trailing slash)
	// is the excluded prefix. Paths /internal/dispatch (no slash) and
	// /internal/dispatchfoo are regular paths that get full CORS treatment.
	// A future "cleanup" that drops the trailing slash from internalPrefix
	// would silently change this behaviour.
	p := NewCORSPolicy(true, true, nil)
	h := CORS(p)(dummyHandler)

	cases := []struct {
		name       string
		path       string
		varyOrigin bool // true: Vary: Origin should be present
	}{
		{"with trailing slash (excluded)", "/internal/dispatch/", false},
		{"under prefix (excluded)", "/internal/dispatch/processor", false},
		{"no trailing slash (NOT excluded)", "/internal/dispatch", true},
		{"prefix-like but distinct (NOT excluded)", "/internal/dispatchfoo", true},
		{"middle of path (NOT excluded)", "/foo/internal/dispatch/bar", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", "https://x.example")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			gotVary := rec.Header().Get("Vary") == "Origin"
			if gotVary != tc.varyOrigin {
				t.Errorf("Vary: Origin presence = %v, want %v", gotVary, tc.varyOrigin)
			}
		})
	}
}

func TestCORS_InternalPrefixSkippedOnPreflight(t *testing.T) {
	p := NewCORSPolicy(true, true, nil)
	downstreamReached := false
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamReached = true
		w.WriteHeader(http.StatusTeapot)
	})
	h := CORS(p)(downstream)

	// A preflight-shaped request to /internal/dispatch/ must NOT short-circuit;
	// it must reach downstream so peer-side AEAD-auth can reject it.
	req := httptest.NewRequest(http.MethodOptions, "/internal/dispatch/processor", nil)
	req.Header.Set("Origin", "https://anything.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !downstreamReached {
		t.Error("downstream should have been reached for /_internal/ preflight")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d (downstream sentinel)", rec.Code, http.StatusTeapot)
	}
}
