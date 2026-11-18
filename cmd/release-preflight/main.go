// release-preflight validates that the repository is in a releasable
// state before a v* tag is built. It performs two checks:
//
//  1. No github.com/cyoda/* module resolves to a pseudo-version. Upstream
//     pseudo-versions (normal Go ecosystem artifacts) are tolerated.
//
//  2. No `replace` directive in go.mod diverts to an external module
//     path. Local-path replaces of the form `=> ./...`, `=> ../...`, or
//     `=> /...` are required for in-repo multi-module layouts and are
//     allowed.
//
// Usage:
//
//	release-preflight --go-mod go.mod --org github.com/cyoda/
//
// Exits 0 on clean pre-flight, 1 on violations (with a human-readable
// report on stderr).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
)

func main() {
	goModPath := flag.String("go-mod", "go.mod", "path to go.mod")
	orgPrefix := flag.String("org", "github.com/cyoda/", "module path prefix to enforce pseudo-version check against; empty disables the filter")
	flag.Parse()

	goModBytes, err := os.ReadFile(*goModPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "release-preflight: read go.mod: %v\n", err)
		os.Exit(2)
	}

	// `GOWORK=off go list -m all` resolves every dep to a concrete version
	// without workspace overrides. We match the release workflow's env
	// exactly so the pre-flight reflects the environment the release
	// actually builds in.
	cmd := exec.Command("go", "list", "-m", "all")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stderr = os.Stderr
	goListBytes, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "release-preflight: go list -m all failed: %v\n", err)
		os.Exit(2)
	}

	var all []Violation
	all = append(all, checkPseudoVersions(goListBytes, *orgPrefix)...)
	all = append(all, checkReplaces(goModBytes)...)

	if len(all) > 0 {
		fmt.Fprintln(os.Stderr, formatViolations(all))
		os.Exit(1)
	}
	fmt.Println("release-preflight: clean")
}
