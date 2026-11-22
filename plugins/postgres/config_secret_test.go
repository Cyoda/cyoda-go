package postgres

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseConfig_URLFromFile verifies that CYODA_POSTGRES_URL_FILE is
// honoured when CYODA_POSTGRES_URL is empty.
func TestParseConfig_URLFromFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "pg-url")
	url := "postgres://user:pass@host:5432/db"
	if err := os.WriteFile(secretPath, []byte(url+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	getenv := func(key string) string {
		switch key {
		case "CYODA_POSTGRES_URL":
			return ""
		case "CYODA_POSTGRES_URL_FILE":
			return secretPath
		default:
			return ""
		}
	}

	cfg, err := parseConfig(getenv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.URL != url {
		t.Errorf("want URL %q, got %q", url, cfg.URL)
	}
}

// TestParseConfig_URLFileTakesPrecedence verifies that CYODA_POSTGRES_URL_FILE
// wins over CYODA_POSTGRES_URL when both are set.
func TestParseConfig_URLFileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "pg-url")
	fileURL := "postgres://from-file@host/db"
	if err := os.WriteFile(secretPath, []byte(fileURL), 0600); err != nil {
		t.Fatal(err)
	}

	getenv := func(key string) string {
		switch key {
		case "CYODA_POSTGRES_URL":
			return "postgres://from-env@host/db"
		case "CYODA_POSTGRES_URL_FILE":
			return secretPath
		default:
			return ""
		}
	}

	cfg, err := parseConfig(getenv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.URL != fileURL {
		t.Errorf("_FILE must win; want %q, got %q", fileURL, cfg.URL)
	}
}

// TestParseConfig_URLFileUnreadable verifies that an unreadable _FILE path
// returns an error rather than silently treating the URL as empty.
func TestParseConfig_URLFileUnreadable(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "CYODA_POSTGRES_URL":
			return ""
		case "CYODA_POSTGRES_URL_FILE":
			return "/nonexistent/path/to/pg-url"
		default:
			return ""
		}
	}

	_, err := parseConfig(getenv)
	if err == nil {
		t.Fatal("expected error for unreadable _FILE path, got nil")
	}
}

// TestResolveSecretWith_EmptyFileNoFallback verifies that a _FILE whose
// contents trim to empty yields an empty value rather than falling back to
// the plain variable.
func TestResolveSecretWith_EmptyFileNoFallback(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "pg-url")
	if err := os.WriteFile(secretPath, []byte("   \n"), 0600); err != nil {
		t.Fatal(err)
	}

	getenv := func(key string) string {
		switch key {
		case "CYODA_POSTGRES_URL":
			return "postgres://from-env@host/db"
		case "CYODA_POSTGRES_URL_FILE":
			return secretPath
		default:
			return ""
		}
	}

	got, err := resolveSecretWith(getenv, "CYODA_POSTGRES_URL")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("file with only whitespace should return empty, not fall back to CYODA_POSTGRES_URL; got %q", got)
	}
}

func TestParseConfig_SchemaSavepointInterval(t *testing.T) {
	env := map[string]string{
		"CYODA_POSTGRES_URL":              "postgres://localhost/x",
		"CYODA_SCHEMA_SAVEPOINT_INTERVAL": "128",
	}
	cfg, err := parseConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.SchemaSavepointInterval != 128 {
		t.Errorf("SchemaSavepointInterval = %d, want 128", cfg.SchemaSavepointInterval)
	}
}

func TestParseConfig_SchemaSavepointInterval_DefaultOnUnset(t *testing.T) {
	env := map[string]string{
		"CYODA_POSTGRES_URL": "postgres://localhost/x",
	}
	cfg, _ := parseConfig(func(k string) string { return env[k] })
	if cfg.SchemaSavepointInterval != 64 {
		t.Errorf("SchemaSavepointInterval default = %d, want 64", cfg.SchemaSavepointInterval)
	}
}

func TestParseConfig_SchemaSavepointInterval_DefaultOnInvalid(t *testing.T) {
	env := map[string]string{
		"CYODA_POSTGRES_URL":              "postgres://localhost/x",
		"CYODA_SCHEMA_SAVEPOINT_INTERVAL": "not-an-int",
	}
	cfg, _ := parseConfig(func(k string) string { return env[k] })
	if cfg.SchemaSavepointInterval != 64 {
		t.Errorf("SchemaSavepointInterval on invalid input = %d, want 64 (fallback)", cfg.SchemaSavepointInterval)
	}
}

func TestParseConfig_SchemaSavepointInterval_DefaultOnZero(t *testing.T) {
	env := map[string]string{
		"CYODA_POSTGRES_URL":              "postgres://localhost/x",
		"CYODA_SCHEMA_SAVEPOINT_INTERVAL": "0",
	}
	cfg, _ := parseConfig(func(k string) string { return env[k] })
	if cfg.SchemaSavepointInterval != 64 {
		t.Errorf("SchemaSavepointInterval on 0 = %d, want 64 (min 1 with fallback to default)", cfg.SchemaSavepointInterval)
	}
}
