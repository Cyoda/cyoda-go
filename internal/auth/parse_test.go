package auth

import "testing"

func TestParseTokenHeader_HappyPath(t *testing.T) {
	tok := buildTestJWT(t, "K1", "RS256", map[string]any{
		"iss": "https://idp.example",
		"aud": "api1",
		"exp": int64(1700000000),
		"iat": int64(1699999000),
		"sub": "user1",
	})

	h, err := parseTokenHeader(tok)
	if err != nil {
		t.Fatalf("parseTokenHeader: %v", err)
	}
	if h.kid != "K1" {
		t.Errorf("kid = %q, want K1", h.kid)
	}
	if h.alg != "RS256" {
		t.Errorf("alg = %q, want RS256", h.alg)
	}
	if h.iss != "https://idp.example" {
		t.Errorf("iss = %q, want https://idp.example", h.iss)
	}
	if h.aud != "api1" {
		t.Errorf("aud = %q, want api1", h.aud)
	}
	if h.sub != "user1" {
		t.Errorf("sub = %q, want user1", h.sub)
	}
	if h.exp != 1700000000 {
		t.Errorf("exp = %d", h.exp)
	}
	if h.iat != 1699999000 {
		t.Errorf("iat = %d", h.iat)
	}
}

func TestParseTokenHeader_MalformedRejects(t *testing.T) {
	cases := []struct {
		name, tok string
	}{
		{"empty", ""},
		{"single-segment", "abc"},
		{"two-segments", "abc.def"},
		{"invalid-base64", "%%%.%%%.%%%"},
		{"non-json-header", encodeSeg([]byte("not-json")) + ".eyJ9.sig"},
		{"non-json-claims", encodeSeg([]byte(`{"alg":"RS256"}`)) + "." + encodeSeg([]byte("not-json")) + ".sig"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseTokenHeader(c.tok)
			if err == nil {
				t.Errorf("expected error for %q", c.name)
			}
		})
	}
}
