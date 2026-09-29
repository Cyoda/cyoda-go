package app

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func TestLoadJWTSettings_ExpiryBounds(t *testing.T) {
	for _, v := range []string{"soon", "0", "-5", strconv.Itoa(MaxJWTExpirySeconds + 1), "9300000000"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CYODA_JWT_EXPIRY_SECONDS", v)
			_, err := LoadJWTSettings()
			if err == nil || !strings.Contains(err.Error(), "CYODA_JWT_EXPIRY_SECONDS") {
				t.Fatalf("CYODA_JWT_EXPIRY_SECONDS=%s: err = %v, want one naming the variable", v, err)
			}
		})
	}
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", strconv.Itoa(MaxJWTExpirySeconds))
	s, err := LoadJWTSettings()
	if err != nil || s.ExpirySeconds != MaxJWTExpirySeconds {
		t.Fatalf("at the cap: settings = %+v, err = %v", s, err)
	}
}

// TestDefaultConfig_RefusesBadJWTExpiry pins that the server reads the JWT
// variables through LoadJWTSettings: a value `cyoda token` refuses is refused
// at server start too, rather than replaced by the default or accepted.
func TestDefaultConfig_RefusesBadJWTExpiry(t *testing.T) {
	for _, v := range []string{"abc", "0", "-5", strconv.Itoa(MaxJWTExpirySeconds + 1)} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CYODA_JWT_EXPIRY_SECONDS", v)
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("DefaultConfig accepted CYODA_JWT_EXPIRY_SECONDS=%s", v)
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, "CYODA_JWT_EXPIRY_SECONDS") {
					t.Fatalf("panic %q does not name CYODA_JWT_EXPIRY_SECONDS", msg)
				}
			}()
			_ = DefaultConfig()
		})
	}
}

// An explicitly empty CYODA_JWT_ISSUER is refused, not replaced by the
// default: tokens would carry an empty iss. Unset still means the default.
func TestLoadJWTSettings_EmptyIssuerIsAnError(t *testing.T) {
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", "")
	t.Setenv("CYODA_JWT_ISSUER", "")
	_, err := LoadJWTSettings()
	if err == nil || !strings.Contains(err.Error(), "CYODA_JWT_ISSUER") {
		t.Fatalf("err = %v, want one naming CYODA_JWT_ISSUER", err)
	}
}

// TestDefaultConfig_RefusesEmptyJWTIssuer pins that the server refuses the
// empty issuer `cyoda token` refuses.
func TestDefaultConfig_RefusesEmptyJWTIssuer(t *testing.T) {
	t.Setenv("CYODA_JWT_ISSUER", "")
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("DefaultConfig accepted an empty CYODA_JWT_ISSUER")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "CYODA_JWT_ISSUER") {
			t.Fatalf("panic %q does not name CYODA_JWT_ISSUER", msg)
		}
	}()
	_ = DefaultConfig()
}

// TestDefaultConfig_JWTSettingsMatchLoadJWTSettings pins that the server's
// config carries the values LoadJWTSettings resolves.
func TestDefaultConfig_JWTSettingsMatchLoadJWTSettings(t *testing.T) {
	t.Setenv("CYODA_JWT_ISSUER", "iss-x")
	t.Setenv("CYODA_JWT_AUDIENCE", "aud-x")
	t.Setenv("CYODA_JWT_EXPIRY_SECONDS", "120")
	cfg := DefaultConfig()
	if cfg.IAM.JWTIssuer != "iss-x" || cfg.IAM.JWTAudience != "aud-x" || cfg.IAM.JWTExpiry != 120 {
		t.Fatalf("IAM = issuer %q audience %q expiry %d", cfg.IAM.JWTIssuer, cfg.IAM.JWTAudience, cfg.IAM.JWTExpiry)
	}
}
