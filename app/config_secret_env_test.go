package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSecretEnv_PlainEnvOnly(t *testing.T) {
	t.Setenv("TEST_SECRET", "plain-value")
	t.Setenv("TEST_SECRET_FILE", "")

	got, err := ResolveSecretEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "plain-value" {
		t.Errorf("want %q, got %q", "plain-value", got)
	}
}

func TestResolveSecretEnv_FileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("file-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", path)

	got, err := ResolveSecretEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "file-value" {
		t.Errorf("want trimmed %q, got %q", "file-value", got)
	}
}

func TestResolveSecretEnv_FileWinsOverPlain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("from-file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "from-env")
	t.Setenv("TEST_SECRET_FILE", path)

	got, err := ResolveSecretEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "from-file" {
		t.Errorf("_FILE must win; want %q, got %q", "from-file", got)
	}
}

func TestResolveSecretEnv_FileUnreadable(t *testing.T) {
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", "/nonexistent/path/to/secret")

	_, err := ResolveSecretEnv("TEST_SECRET")
	if err == nil {
		t.Fatal("expected error reading nonexistent file, got nil")
	}
}

func TestResolveSecretEnv_TrimsTrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("value\n\n   \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", path)

	got, err := ResolveSecretEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "value" {
		t.Errorf("trailing whitespace not trimmed; want %q, got %q", "value", got)
	}
}

func TestResolveSecretEnv_EmptyFileYieldsEmptyNoFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("   \n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "plain")
	t.Setenv("TEST_SECRET_FILE", path)

	got, err := ResolveSecretEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("file with only whitespace should return empty, not fall back to TEST_SECRET; got %q", got)
	}
}

func TestResolveSecretEnv_NeitherSet(t *testing.T) {
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", "")

	got, err := ResolveSecretEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("neither set should return empty; got %q", got)
	}
}
