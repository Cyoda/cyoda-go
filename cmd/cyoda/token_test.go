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

// setTokenEnv sets the JWT variables runToken reads and makes its call to
// app.LoadEnvFiles hermetic: HOME, XDG_CONFIG_HOME and the working directory
// point at empty temporary directories, so no user config and no ./.env is
// read (and nothing from them is left set after the test).
func setTokenEnv(t *testing.T, pemText string) {
	t.Helper()
	isolateEnvFiles(t)
	t.Setenv("CYODA_JWT_SIGNING_KEY", pemText)
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", "")
	t.Setenv("CYODA_JWT_ISSUER", "cyoda-test")
	t.Setenv("CYODA_JWT_AUDIENCE", "")
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "3600")
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
	// runToken itself writes nothing to its stderr writer on success. Log
	// lines from app.LoadEnvFiles (which env files were loaded) go to the
	// process's stderr through slog's default handler, not to errOut, so this
	// check does not see them; cli/token.md documents that they may appear.
	if errOut.Len() != 0 {
		t.Fatalf("runToken wrote to its stderr writer on success: %q", errOut.String())
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

// Without --ttl the lifetime is 15 minutes, or CYODA_JWT_EXPIRY_SECONDS when
// that is shorter: the default never exceeds a valid configured cap.
func TestRunToken_DefaultTTL(t *testing.T) {
	for _, tc := range []struct {
		expiry  string
		wantSec float64
	}{
		{expiry: "3600", wantSec: 900},
		{expiry: "900", wantSec: 900},
		{expiry: "300", wantSec: 300},
	} {
		t.Run(tc.expiry, func(t *testing.T) {
			_, pemText := tokenTestKey(t)
			setTokenEnv(t, pemText)
			t.Setenv("CYODA_JWT_EXPIRY_SECONDS", tc.expiry)
			var out, errOut bytes.Buffer
			if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut.String())
			}
			p, err := auth.Parse(strings.TrimSpace(out.String()))
			if err != nil {
				t.Fatal(err)
			}
			exp, _ := p.Claims["exp"].(float64)
			iat, _ := p.Claims["iat"].(float64)
			if got := exp - iat; got != tc.wantSec {
				t.Fatalf("exp - iat = %v, want %v", got, tc.wantSec)
			}
		})
	}
}

// An explicit --ttl above CYODA_JWT_EXPIRY_SECONDS is refused, also when the
// cap is shorter than the default lifetime.
func TestRunToken_ExplicitTTLAboveShortExpiryExit2(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "300")
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme", "--ttl", "301s"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2 (stderr %q)", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must be empty on error: %q", out.String())
	}
}

// Leaving CYODA_JWT_EXPIRY_SECONDS unset uses the real default (300 s, not
// the old 3600 s): the default 15-minute TTL clamps to it.
func TestRunToken_DefaultTTLClampsToDefaultExpiry(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "")
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut.String())
	}
	p, err := auth.Parse(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	exp, _ := p.Claims["exp"].(float64)
	iat, _ := p.Claims["iat"].(float64)
	if got := exp - iat; got != 300 {
		t.Fatalf("exp - iat = %v, want 300 (the default CYODA_JWT_EXPIRY_SECONDS)", got)
	}
}

// An explicit --ttl of 2h is refused against the real default expiry (300 s).
func TestRunToken_ExplicitTTL2hRefusedAgainstDefaultExpiry(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "")
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme", "--ttl", "2h"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2 (stderr %q)", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must be empty on error: %q", out.String())
	}
}

// The --ttl flag error states both bounds, so a sub-second value is told why.
func TestRunToken_TTLErrorStatesTheBounds(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme", "--ttl", "1ns"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2 (stderr %q)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "at least 1s and at most 3600s") {
		t.Fatalf("stderr %q does not state the bounds", errOut.String())
	}
}

func TestRunToken_FlagErrorsExit2(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	for name, args := range map[string][]string{
		"no tenant":    {},
		"bad tenant":   {"--tenant", "a:b"},
		"system user":  {"--tenant", "acme", "--user", "system"},
		"empty role":   {"--tenant", "acme", "--roles", "ROLE_ADMIN,,ROLE_M2M"},
		"zero ttl":     {"--tenant", "acme", "--ttl", "0s"},
		"1ns ttl":      {"--tenant", "acme", "--ttl", "1ns"},
		"999ms ttl":    {"--tenant", "acme", "--ttl", "999ms"},
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

// TestSetTokenEnv_IgnoresDeveloperEnvFiles pins that runToken's call to
// app.LoadEnvFiles cannot pick up the developer's user config or a ./.env:
// setTokenEnv points HOME, XDG_CONFIG_HOME and the working directory at empty
// temporary directories.
func TestSetTokenEnv_IgnoresDeveloperEnvFiles(t *testing.T) {
	const userVar, cwdVar = "CYODA_TOKEN_TEST_FROM_USER_CONFIG", "CYODA_TOKEN_TEST_FROM_DOTENV"
	for _, v := range []string{userVar, cwdVar} {
		t.Setenv(v, "") // restores the variable's absence at cleanup
		os.Unsetenv(v)
	}
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "cyoda"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "cyoda", "cyoda.env"), []byte(userVar+"=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".env"), []byte(cwdVar+"=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, v := range []string{userVar, cwdVar} {
		if _, ok := os.LookupEnv(v); ok {
			t.Errorf("%s was loaded from a developer env file", v)
		}
	}
}

func TestRunToken_ExpiryOutOfRangeExit1(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	for _, v := range []string{"abc", "0", "3601", "9300000000"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CYODA_JWT_EXPIRY_SECONDS", v)
			var out, errOut bytes.Buffer
			if code := runToken([]string{"--tenant", "acme", "--ttl", "1m"}, &out, &errOut); code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, errOut.String())
			}
			if out.Len() != 0 || !strings.Contains(errOut.String(), "CYODA_JWT_EXPIRY_SECONDS") {
				t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
			}
		})
	}
}

// An empty CYODA_JWT_ISSUER is a configuration error (exit 1), as it is for
// the server, not a flag error.
func TestRunToken_EmptyIssuerExit1(t *testing.T) {
	_, pemText := tokenTestKey(t)
	setTokenEnv(t, pemText)
	t.Setenv("CYODA_JWT_ISSUER", "")
	var out, errOut bytes.Buffer
	if code := runToken([]string{"--tenant", "acme"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, errOut.String())
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "CYODA_JWT_ISSUER") {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
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

// -h and --help print the usage to stderr and succeed: asking for help is not
// a flag error. stdout stays empty, so a script capturing it gets no token.
func TestRunToken_HelpExit0(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runToken([]string{arg}, &out, &errOut); code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("stdout must be empty for help: %q", out.String())
			}
			if !strings.Contains(errOut.String(), "-tenant") {
				t.Fatalf("usage not printed to stderr: %q", errOut.String())
			}
		})
	}
}
