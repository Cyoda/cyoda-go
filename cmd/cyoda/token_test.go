package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

func tokenTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func setTokenEnv(t *testing.T, pemText string) {
	t.Helper()
	t.Setenv("CYODA_JWT_SIGNING_KEY", pemText)
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", "")
	t.Setenv("CYODA_JWT_ISSUER", "cyoda-test")
	t.Setenv("CYODA_JWT_AUDIENCE", "")
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "3600")
	t.Setenv("CYODA_PROFILES", "")
}

func TestRunToken_StdoutIsOnlyTheToken(t *testing.T) {
	key, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	s := out.String()
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("stdout must be exactly one line: %q", s)
	}
	p, err := auth.Parse(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Verify(p.SigningInput, p.Signature, &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	if p.Claims["caas_org_id"] != "acme" || p.Claims["caas_user_id"] != "operator" || p.Claims["iss"] != "cyoda-test" {
		t.Fatalf("claims = %v", p.Claims)
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr not empty: %q", errOut.String())
	}
}

func TestRunToken_AudienceFromConfig(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	t.Setenv("CYODA_JWT_AUDIENCE", "cyoda-api")
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	p, _ := auth.Parse(strings.TrimSpace(out.String()))
	if p.Claims["aud"] != "cyoda-api" {
		t.Fatalf("aud = %v", p.Claims["aud"])
	}
}

func TestRunToken_FlagErrorsExit2(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	for name, args := range map[string][]string{
		"no tenant":    {},
		"bad tenant":   {"--tenant", "a:b"},
		"oidc user":    {"--tenant", "acme", "--user", "oidc:x"},
		"empty role":   {"--tenant", "acme", "--roles", "ROLE_ADMIN,,ROLE_M2M"},
		"zero ttl":     {"--tenant", "acme", "--ttl", "0s"},
		"ttl too long": {"--tenant", "acme", "--ttl", "61m"},
		"unknown flag": {"--tenant", "acme", "--nope"},
		"positional":   {"--tenant", "acme", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runToken(args, &out, &errOut); code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", code, errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("stdout must be empty on error: %q", out.String())
			}
		})
	}
}

func TestRunToken_MissingKeyExit1(t *testing.T) {
	setTokenEnv(t, "")
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestRunToken_UnreadableKeyFile(t *testing.T) {
	setTokenEnv(t, "")
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	var out, errOut bytes.Buffer
	code := runToken([]string{"--tenant", "acme"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Count(strings.TrimSpace(errOut.String()), "\n") != 0 {
		t.Fatalf("stderr must be one line, no stack trace: %q", errOut.String())
	}
}

func TestRunToken_BadKeyExit1AndNoKeyMaterialInStderr(t *testing.T) {
	bogus := "-----BEGIN PRIVATE KEY-----\nTk9UQUtFWQ==\n-----END PRIVATE KEY-----\n"
	setTokenEnv(t, bogus)
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(errOut.String(), "Tk9UQUtFWQ") {
		t.Fatal("stderr echoes key material")
	}
}

func TestRunToken_KeyFromFile(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, "")
	path := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(path, []byte(pemText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", path)
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
}
