package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cyoda-platform/cyoda-go/app"
	sqliteplugin "github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// goos mirrors runtime.GOOS so tests can inspect it without importing
// runtime themselves. Declared here so init_test.go sees it.
var goos = runtime.GOOS

// runInit implements 'cyoda init'. See the desktop provisioning spec for
// design rationale. Exit codes: 0 success (incl. idempotent no-op), 1
// I/O error, 2 bad flags or arguments. -h/--help prints the usage and exits 0.
func runInit(args []string) int {
	fs := flag.NewFlagSet("cyoda init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing user config or bypass the system-config check")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help: the flag package printed the usage to stderr
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "cyoda init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	// 1. If any system config file already exists, do nothing (unless --force).
	for _, p := range app.SystemConfigPaths() {
		if _, err := os.Stat(p); err == nil {
			if !*force {
				fmt.Printf("system-wide cyoda config already present at %s; no user config needed (use --force to write a user config anyway)\n", p)
				return 0
			}
			// --force was set; system config present is expected but we proceed to write the user config.
		}
	}

	// 2. Resolve user config path.
	userPath := app.UserConfigPath()
	if userPath == "" {
		fmt.Fprintln(os.Stderr, "cyoda init: cannot compute user config path (no home directory detected)")
		return 1
	}

	// 3. If user config exists and --force not set, do nothing.
	if _, err := os.Stat(userPath); err == nil && !*force {
		fmt.Printf("config already exists at %s (use --force to overwrite)\n", userPath)
		return 0
	}

	// 4. Write user config.
	if err := os.MkdirAll(filepath.Dir(userPath), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "cyoda init: cannot create config directory: %v\n", err)
		return 1
	}
	content := fmt.Sprintf(`# cyoda user config — written by 'cyoda init'
# Shell-exported vars override values here.

CYODA_STORAGE_BACKEND=sqlite
# CYODA_SQLITE_PATH=%s   # uncomment to override
`, sqliteplugin.DefaultDBPath())
	if err := os.WriteFile(userPath, []byte(content), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "cyoda init: cannot write config: %v\n", err)
		return 1
	}
	fmt.Printf("wrote config to %s\n", userPath)
	return 0
}
