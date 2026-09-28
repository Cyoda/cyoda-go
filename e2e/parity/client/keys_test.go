package client_test

import (
	"encoding/base64"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

func TestTokenKID(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	tok := enc([]byte(`{"alg":"RS256","kid":"abc"}`)) + "." + enc([]byte(`{}`)) + ".sig"
	if got := client.TokenKID(tok); got != "abc" {
		t.Fatalf("TokenKID: %q, want abc", got)
	}
	for _, bad := range []string{"", "nodots", "!!!.x.y", enc([]byte(`not json`)) + ".x.y"} {
		if got := client.TokenKID(bad); got != "" {
			t.Errorf("TokenKID(%q) = %q, want empty", bad, got)
		}
	}
}
