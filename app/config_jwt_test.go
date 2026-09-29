package app

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func base64Std(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestLoadJWTSettings_Defaults(t *testing.T) {
	for _, v := range []string{"CYODA_JWT_SIGNING_KEY", "CYODA_JWT_SIGNING_KEY_FILE", "CYODA_JWT_ISSUER", "CYODA_JWT_AUDIENCE", "CYODA_JWT_EXPIRY_SECONDS"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	s, err := LoadJWTSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Issuer != "cyoda" || s.Audience != "" || s.ExpirySeconds != 3600 || s.SigningKeyPEM != "" {
		t.Fatalf("defaults = %+v", s)
	}
}

func TestLoadJWTSettings_UnreadableKeyFileIsAnErrorNotAPanic(t *testing.T) {
	t.Setenv("CYODA_JWT_SIGNING_KEY", "")
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	_, err := LoadJWTSettings()
	if err == nil || !strings.Contains(err.Error(), "CYODA_JWT_SIGNING_KEY_FILE") {
		t.Fatalf("err = %v, want one naming CYODA_JWT_SIGNING_KEY_FILE", err)
	}
}

func TestLoadJWTSettings_Base64PEMIsDecoded(t *testing.T) {
	pemText := "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", "")
	t.Setenv("CYODA_JWT_SIGNING_KEY", base64Std(pemText))
	s, err := LoadJWTSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.SigningKeyPEM != pemText {
		t.Fatalf("PEM = %q", s.SigningKeyPEM)
	}
}

func TestLoadJWTSettings_BadExpiryIsAnError(t *testing.T) {
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "soon")
	if _, err := LoadJWTSettings(); err == nil {
		t.Fatal("want error")
	}
}
