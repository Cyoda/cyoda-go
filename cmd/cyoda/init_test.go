package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each init test isolates XDG_CONFIG_HOME (Linux/macOS user path driver)
// and AppData (Windows user path driver) to a t.TempDir, and points
// ProgramData at a non-existent path so tests never see a real system
// config. /etc/cyoda/cyoda.env on Linux cannot be isolated from within
// a test; the tests below are designed so a missing /etc/cyoda dir on
// the test host still produces the expected behavior.
func setupIsolatedConfig(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("AppData", tmp)
	t.Setenv("ProgramData", filepath.Join(tmp, "no-such-programdata"))
	return tmp
}

// isolateEnvFiles makes a call to app.LoadEnvFiles hermetic: the user config
// (XDG_CONFIG_HOME, AppData), the Windows system config, ./.env and profile
// files are looked up in empty temporary directories, so the developer's own
// user config and env files are never read, and nothing from them is left
// set in the test process. HOME is left alone: the user config path never
// falls back to it while XDG_CONFIG_HOME is set, and testcontainers reads its
// own settings and the Docker socket from it. The Linux system config
// (/etc/cyoda/cyoda.env) cannot be redirected, so a test that depends on a
// variable sets it itself. It returns the XDG_CONFIG_HOME directory, under
// which a test may write a user config.
func isolateEnvFiles(t *testing.T) string {
	t.Helper()
	xdg := setupIsolatedConfig(t)
	t.Setenv("CYODA_PROFILES", "")
	t.Chdir(t.TempDir())
	return xdg
}

func TestCyodaInit_WritesUserConfigFresh(t *testing.T) {
	tmp := setupIsolatedConfig(t)

	code := runInit([]string{})
	if code != 0 {
		t.Fatalf("runInit exit code = %d, want 0", code)
	}
	path := filepath.Join(tmp, "cyoda", "cyoda.env")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected config at %s, got %v", path, err)
	}
	content := string(data)
	if !strings.Contains(content, "CYODA_STORAGE_BACKEND=sqlite") {
		t.Errorf("missing CYODA_STORAGE_BACKEND=sqlite in:\n%s", content)
	}
	if !strings.Contains(content, "# CYODA_SQLITE_PATH=") {
		t.Errorf("missing commented CYODA_SQLITE_PATH line in:\n%s", content)
	}
	// Must be a resolved absolute path, never a shell-variable placeholder.
	if strings.Contains(content, "$XDG_DATA_HOME") {
		t.Errorf("found unresolved $XDG_DATA_HOME placeholder in:\n%s", content)
	}
	// File mode 0600 (only the permission bits matter).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file mode = %v, want 0600", perm)
	}
}

func TestCyodaInit_ExitsZeroWhenUserConfigExists(t *testing.T) {
	tmp := setupIsolatedConfig(t)
	path := filepath.Join(tmp, "cyoda", "cyoda.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a marker the test can verify is preserved.
	if err := os.WriteFile(path, []byte("CYODA_MARKER=preserve-me\n"), 0600); err != nil {
		t.Fatal(err)
	}

	code := runInit([]string{})
	if code != 0 {
		t.Fatalf("runInit exit code = %d, want 0 (idempotent on existing user config)", code)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "CYODA_MARKER=preserve-me") {
		t.Errorf("existing file was clobbered; content: %s", data)
	}
}

func TestCyodaInit_ForceOverwritesUserConfig(t *testing.T) {
	tmp := setupIsolatedConfig(t)
	path := filepath.Join(tmp, "cyoda", "cyoda.env")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, []byte("CYODA_MARKER=preserve-me\n"), 0600)

	code := runInit([]string{"--force"})
	if code != 0 {
		t.Fatalf("runInit exit code = %d, want 0", code)
	}

	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "CYODA_MARKER=preserve-me") {
		t.Errorf("--force should have overwritten file; content: %s", s)
	}
	if !strings.Contains(s, "CYODA_STORAGE_BACKEND=sqlite") {
		t.Errorf("--force should have written a fresh config; content: %s", s)
	}
}

func TestCyodaInit_RunTwice_SecondIsNoOp(t *testing.T) {
	tmp := setupIsolatedConfig(t)
	path := filepath.Join(tmp, "cyoda", "cyoda.env")

	if code := runInit([]string{}); code != 0 {
		t.Fatalf("first runInit exit code = %d, want 0", code)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code := runInit([]string{}); code != 0 {
		t.Fatalf("second runInit exit code = %d, want 0", code)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("second runInit modified file; before:\n%s\nafter:\n%s", before, after)
	}
}

// When a system config exists on disk AND --force is unset, init should
// exit 0 without writing a user config. This tests the positive case by
// pointing ProgramData at a directory where we write a fake system config.
// It only runs meaningfully on Windows (where ProgramData drives
// systemConfigPaths). On Linux, /etc/cyoda/cyoda.env can't be isolated
// from a test. On macOS, no system path exists by design.
func TestCyodaInit_SkipsWhenSystemConfigPresent_Windows(t *testing.T) {
	if runtimeGOOS() != "windows" {
		t.Skip("system-config detection on Linux/macOS is not isolable from a test")
	}
	tmp := t.TempDir()
	sysRoot := filepath.Join(tmp, "sysconf")
	userRoot := filepath.Join(tmp, "userconf")
	sysFile := filepath.Join(sysRoot, "cyoda", "cyoda.env")
	_ = os.MkdirAll(filepath.Dir(sysFile), 0700)
	_ = os.WriteFile(sysFile, []byte("CYODA_STORAGE_BACKEND=postgres\n"), 0600)

	t.Setenv("AppData", userRoot)
	t.Setenv("ProgramData", sysRoot)

	code := runInit([]string{})
	if code != 0 {
		t.Fatalf("runInit exit code = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(userRoot, "cyoda", "cyoda.env")); err == nil {
		t.Error("user config should NOT be written when a system config is present")
	}
}

// Helper to isolate runtime.GOOS for conditional-skip logic — don't
// depend on the stdlib runtime import directly in the test body.
func runtimeGOOS() string {
	return goos
}
