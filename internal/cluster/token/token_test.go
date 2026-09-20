package token_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

func TestNewSigner_SecretTooShort(t *testing.T) {
	_, err := token.NewSigner([]byte("short"))
	if err == nil {
		t.Fatal("expected error for short secret")
	}
	if err != token.ErrSecretTooShort {
		t.Errorf("expected ErrSecretTooShort, got %v", err)
	}
}

func TestNewSigner_SecretExactly32Bytes(t *testing.T) {
	secret := []byte("exactly-32-bytes-long-secret!!!!")
	if len(secret) != 32 {
		t.Fatalf("test setup: secret length = %d, want 32", len(secret))
	}
	s, err := token.NewSigner(secret)
	if err != nil {
		t.Fatalf("expected no error for 32-byte secret, got %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil signer")
	}
}

func TestRoundTrip(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	tok, err := signer.Issue(token.Claims{NodeID: "node-1", TxRef: "tx-uuid-abc", ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Callout: "req-tx-uuid-abc", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.NodeID != "node-1" {
		t.Errorf("NodeID = %q, want %q", claims.NodeID, "node-1")
	}
	if claims.TxRef != "tx-uuid-abc" {
		t.Errorf("TxRef = %q, want %q", claims.TxRef, "tx-uuid-abc")
	}
}

func TestExpiredToken(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	tok, err := signer.Issue(token.Claims{NodeID: "node-1", TxRef: "tx-uuid-abc", ExpiresAt: time.Now().Add(-1 * time.Second).Unix(), Callout: "req-tx-uuid-abc", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = signer.Verify(tok)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if err != token.ErrTokenExpired {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

func TestTamperedToken(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	tok, err := signer.Issue(token.Claims{NodeID: "node-1", TxRef: "tx-uuid-abc", ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Callout: "req-tx-uuid-abc", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	tampered := []byte(tok)
	tampered[len(tampered)/2] ^= 0xFF
	_, err = signer.Verify(string(tampered))
	if err == nil {
		t.Fatal("expected error for tampered token")
	}
}

func TestWrongSecret(t *testing.T) {
	signer1, _ := token.NewSigner([]byte("secret-one-at-least-32-bytes!!!!"))
	signer2, _ := token.NewSigner([]byte("secret-two-at-least-32-bytes!!!!"))

	tok, err := signer1.Issue(token.Claims{NodeID: "node-1", TxRef: "tx-uuid-abc", ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Callout: "req-tx-uuid-abc", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = signer2.Verify(tok)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func validClaims(exp time.Time) token.Claims {
	return token.Claims{NodeID: "node-1", TxRef: "tx-uuid-abc", ExpiresAt: exp.Unix(), Callout: "req-1", Major: 1}
}

func TestRoundTrip_CarriesTheCalloutAndItsNumber(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	in := token.Claims{
		NodeID: "owner", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(),
		Callout: "req-inner", Major: 3, Minor: 2,
		Outer: []token.Pair{{Callout: "req-outer", Major: 1}},
	}
	tok, err := signer.Issue(in)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := signer.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.NodeID != in.NodeID || got.TxRef != in.TxRef || got.ExpiresAt != in.ExpiresAt ||
		got.Callout != in.Callout || got.Major != in.Major || got.Minor != in.Minor ||
		len(got.Outer) != 1 || got.Outer[0] != in.Outer[0] {
		t.Fatalf("claims = %+v; want %+v", got, in)
	}
}

// The number is covered by the HMAC like every other claim.
func TestVerify_TamperedNumberIsRefused(t *testing.T) {
	secret := []byte("test-secret-key-at-least-32-bytes!")
	signer, _ := token.NewSigner(secret)
	tok, _ := signer.Issue(validClaims(time.Now().Add(time.Minute)))
	dot := strings.LastIndexByte(tok, '.')
	payload, _ := base64.RawURLEncoding.DecodeString(tok[:dot])
	forged := strings.Replace(string(payload), `"j":1`, `"j":2`, 1)
	if forged == string(payload) {
		t.Fatal("test setup: the major was not found in the payload")
	}
	if _, err := signer.Verify(base64.RawURLEncoding.EncodeToString([]byte(forged)) + tok[dot:]); !errors.Is(err, token.ErrTokenTampered) {
		t.Fatalf("err = %v; want ErrTokenTampered", err)
	}
}

func TestVerify_PassWithoutCalloutAndNumberIsInvalid(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	exp := time.Now().Add(time.Minute).Unix()
	tests := []struct {
		name   string
		claims token.Claims
	}{
		{"a pass of an earlier version: no callout, no number", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp}},
		{"callout without a number", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Callout: "c"}},
		{"number without a callout", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Major: 1}},
		{"enclosing pair without a callout", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Callout: "c", Major: 1, Outer: []token.Pair{{Major: 1}}}},
		{"enclosing pair without a number", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: exp, Callout: "c", Major: 1, Outer: []token.Pair{{Callout: "o"}}}},
		{"no callout AND expired: invalid wins", token.Claims{NodeID: "n", TxRef: "t", ExpiresAt: time.Now().Add(-time.Minute).Unix()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := signer.Issue(tc.claims)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if _, err := signer.Verify(tok); !errors.Is(err, token.ErrTokenInvalid) {
				t.Fatalf("err = %v; want ErrTokenInvalid", err)
			}
		})
	}
}

// The pass expires exactly when its claims say; what that moment is — the
// answer limit plus the pass allowance — is the minter's to compute.
func TestIssue_ExpiryComesFromTheClaims(t *testing.T) {
	signer, _ := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	live, _ := signer.Issue(validClaims(time.Now().Add(30 * time.Second)))
	if _, err := signer.Verify(live); err != nil {
		t.Fatalf("a pass inside its life: %v", err)
	}
	dead, _ := signer.Issue(validClaims(time.Now().Add(-2 * time.Second)))
	if _, err := signer.Verify(dead); !errors.Is(err, token.ErrTokenExpired) {
		t.Fatalf("err = %v; want ErrTokenExpired", err)
	}
}
