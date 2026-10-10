package app

import (
	"fmt"
	"os"
	"strings"
)

// Mirrors plugins/postgres.resolveSecretWith (separate go.mod; keep behavior in sync).
// Exported so cmd/cyoda can share the same _FILE resolution (including
// trailing-whitespace trim) without duplicating the logic.
//
// ResolveSecretEnv returns the contents of the file at the path given by
// <name>_FILE when that env var is set, and otherwise the value of the
// named env var. <name>_FILE wins whenever it is set, whatever <name>
// holds. The _FILE path is the canonical Docker/Kubernetes pattern for
// passing credentials without exposing them in `env` output.
//
// Trailing whitespace (spaces, tabs, \n, \r) is stripped from file
// contents — safe for both DSN strings and multi-line PEM keys. A file
// whose contents trim to empty yields an empty value, with no fallback
// to <name>; the caller's normal downstream validation reports the
// real problem.
//
// Errors: returned only when <name>_FILE points at a path that cannot
// be read. Silent fallthrough to empty would let a typo'd path look
// like a missing credential, which is hard to debug.
func ResolveSecretEnv(name string) (string, error) {
	fileVar := name + "_FILE"
	if path := os.Getenv(fileVar); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s=%q: %w", fileVar, path, err)
		}
		return strings.TrimRight(string(data), " \t\n\r"), nil
	}
	return os.Getenv(name), nil
}

// mustResolveSecretEnv calls ResolveSecretEnv and panics on error.
//
// Use at startup-time config loading (DefaultConfig) where the binary cannot
// meaningfully continue if a specified _FILE path is unreadable — the operator
// set the path, so a typo or permission error is a fatal misconfiguration.
// Panicking here is consistent with other startup-fatal errors (missing required
// flags, bad port numbers) and produces a clear message in the process log.
//
// Never call mustResolveSecretEnv on a code path that runs after startup.
func mustResolveSecretEnv(name string) string {
	v, err := ResolveSecretEnv(name)
	if err != nil {
		panic(fmt.Sprintf("config: %v", err))
	}
	return v
}
