package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfig_JWTSigningKeyFromFile verifies that CYODA_JWT_SIGNING_KEY_FILE
// is honoured end-to-end through DefaultConfig, covering the _FILE precedence
// for the most complex credential (multi-line PEM).
func TestDefaultConfig_JWTSigningKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "jwt-signing-key.pem")
	pem := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBAD...\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(pemPath, []byte(pem), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYODA_JWT_SIGNING_KEY", "")
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", pemPath)

	cfg := DefaultConfig()
	if !strings.Contains(cfg.IAM.JWTSigningKey, "BEGIN PRIVATE KEY") {
		t.Errorf("expected PEM content loaded via _FILE; got %q", cfg.IAM.JWTSigningKey)
	}
}

// TestDefaultConfig_JWTSigningKeyFileTakesPrecedence verifies that
// CYODA_JWT_SIGNING_KEY_FILE wins over CYODA_JWT_SIGNING_KEY when both are set.
func TestDefaultConfig_JWTSigningKeyFileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "jwt-key.pem")
	filePEM := "-----BEGIN PRIVATE KEY-----\nfrom-file\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(pemPath, []byte(filePEM), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYODA_JWT_SIGNING_KEY", "-----BEGIN PRIVATE KEY-----\nfrom-env\n-----END PRIVATE KEY-----\n")
	t.Setenv("CYODA_JWT_SIGNING_KEY_FILE", pemPath)

	cfg := DefaultConfig()
	if !strings.Contains(cfg.IAM.JWTSigningKey, "from-file") {
		t.Errorf("_FILE should win over plain env; got %q", cfg.IAM.JWTSigningKey)
	}
}

// TestDefaultConfig_HMACSecretFromFile verifies that CYODA_HMAC_SECRET_FILE
// is honoured end-to-end through DefaultConfig.
func TestDefaultConfig_HMACSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "hmac-secret")
	// 32 bytes of hex = 64 hex chars
	hexSecret := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	if err := os.WriteFile(secretPath, []byte(hexSecret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYODA_HMAC_SECRET", "")
	t.Setenv("CYODA_HMAC_SECRET_FILE", secretPath)

	cfg := DefaultConfig()
	if len(cfg.Cluster.HMACSecret) == 0 {
		t.Errorf("expected HMAC secret loaded via _FILE; got empty")
	}
}
