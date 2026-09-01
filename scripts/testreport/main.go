// Command testreport turns a `go test -json` stream into a per-package summary
// plus the verbatim output of anything that failed, and fails the run when a
// package that was required to execute tests did not execute any.
//
// It exists because `go test` cannot express the difference between "this
// package passed" and "this package never started". A TestMain that calls
// os.Exit(0) — which is how the parity suites and internal/e2e opt out of
// -short — emits a package-level "pass" and no test events whatsoever. The
// result is `ok  pkg  2.80s`, identical in shape to a real pass. A cheap tier
// whose green cannot be falsified is a tier nobody can rely on, so every
// verification round escalates to the full suite.
//
// Usage:
//
//	go test -json ./... | go run ./scripts/testreport -must-run e2e/parity,internal/e2e
//
// -must-run takes comma-separated substrings matched against the package path.
// A matching package that ran no tests, or whose every test skipped, is a
// failure naming the package.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Status is a package's outcome, drawing the distinction `go test` does not:
// passing is not the same as never having run.
type Status string

const (
	StatusPass       Status = "PASS"
	StatusFail       Status = "FAIL"
	StatusAllSkipped Status = "SKIPPED"
	StatusNoTests    Status = "NO-TESTS"
)

type event struct {
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
	// Compile errors arrive on "build-output" events keyed by ImportPath
	// rather than Package, and the path carries a " [pkg.test]" suffix. Keying
	// only on Package silently discards the line that says what is wrong.
	ImportPath string
}

type pkgResult struct {
	name    string
	pass    int
	fail    int
	skip    int
	elapsed float64
	failed  bool
	// failedTests is which tests actually failed. Without it there is no way
	// to tell a failing test's buffered output from a chatty passing one's,
	// and every test that logged anything gets relabelled as a failure.
	failedTests map[string]bool
	failOutput  []string // verbatim, in order, for failing tests and build errors
}

// Result is the analysed run.
type Result struct {
	pkgs     map[string]*pkgResult
	order    []string
	missed   []string // required packages that executed nothing
	buffers  map[string]*perTest
	ExitCode int
}

// Status reports how a package finished.
func (r *Result) Status(pkg string) Status {
	p, ok := r.pkgs[pkg]
	if !ok {
		return StatusNoTests
	}
	switch {
	case p.failed || p.fail > 0:
		return StatusFail
	case p.pass > 0:
		return StatusPass
	case p.skip > 0:
		return StatusAllSkipped
	default:
		return StatusNoTests
	}
}

// analyse consumes a `go test -json` stream. mustRun holds substrings; any
// package whose path contains one must have executed at least one passing
// test, or the run fails.
func analyse(src io.Reader, mustRun []string) (*Result, error) {
	return analyseTo(src, mustRun, io.Discard)
}

// analyseTo is analyse with a progress sink. Failures are announced to it the
// moment they are seen so a long tier can be abandoned early instead of
// running to completion before revealing anything. Passes stay silent — the
// whole point is to avoid recreating the -v firehose.
func analyseTo(src io.Reader, mustRun []string, progress io.Writer) (*Result, error) {
	res := &Result{pkgs: map[string]*pkgResult{}, buffers: map[string]*perTest{}}

	get := func(name string) *pkgResult {
		p, ok := res.pkgs[name]
		if !ok {
			p = &pkgResult{name: name, failedTests: map[string]bool{}}
			res.pkgs[name] = p
			res.order = append(res.order, name)
		}
		return p
	}

	sc := bufio.NewScanner(src)
	// Test output lines can be long; the default 64KiB token cap truncates a
	// panic's goroutine dump, which is exactly the detail worth keeping.
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue // `go test` interleaves non-JSON on some toolchain errors
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		pkgName := e.Package
		if pkgName == "" && e.ImportPath != "" {
			pkgName = importPathToPackage(e.ImportPath)
		}
		if pkgName == "" {
			continue
		}
		p := get(pkgName)

		// Build diagnostics precede any test event and carry the real cause.
		if e.Action == "build-output" || e.Action == "build-fail" {
			if e.Action == "build-fail" {
				p.failed = true
			}
			for _, l := range strings.Split(strings.TrimRight(e.Output, "\n"), "\n") {
				// The "# pkg [pkg.test]" banner repeats the package name.
				if l != "" && !strings.HasPrefix(l, "# ") {
					p.failOutput = append(p.failOutput, l)
				}
			}
			continue
		}

		if e.Test == "" {
			switch e.Action {
			case "pass", "fail", "skip":
				p.elapsed = e.Elapsed
				if e.Action == "fail" {
					p.failed = true
				}
			case "output":
				// Package-scoped output with no test: build errors live here.
				if s := strings.TrimRight(e.Output, "\n"); s != "" && !isNoise(s) {
					p.failOutput = append(p.failOutput, s)
				}
			}
			continue
		}

		switch e.Action {
		case "pass":
			p.pass++
		case "fail":
			p.fail++
			p.failedTests[e.Test] = true
			fmt.Fprintf(progress, "FAIL %s.%s\n", shortPkg(pkgName), e.Test)
		case "skip":
			p.skip++
		case "output":
			// Buffered against the test; kept only if that test fails.
			res.bufferOutput(p.name, e.Test, e.Output)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading test output: %w", err)
	}

	res.finalise(mustRun)
	return res, nil
}

// perTest buffers output per test so a passing test's chatter is discarded and
// a failing test's is kept in full.
type perTest struct {
	order  []string
	byTest map[string][]string
}

func (r *Result) bufferOutput(pkg, test, out string) {
	b, ok := r.buffers[pkg]
	if !ok {
		b = &perTest{byTest: map[string][]string{}}
		r.buffers[pkg] = b
	}
	if _, seen := b.byTest[test]; !seen {
		b.order = append(b.order, test)
	}
	b.byTest[test] = append(b.byTest[test], strings.TrimRight(out, "\n"))
}

// isNoise drops go test's own framing lines, which carry no diagnostic value.
func isNoise(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" ||
		strings.HasPrefix(t, "=== RUN") || strings.HasPrefix(t, "=== PAUSE") ||
		strings.HasPrefix(t, "=== CONT") || strings.HasPrefix(t, "=== NAME") ||
		strings.HasPrefix(t, "--- PASS") || strings.HasPrefix(t, "--- SKIP") ||
		strings.HasPrefix(t, "PASS") || strings.HasPrefix(t, "ok ") ||
		// The report writes its own "--- FAIL: <test>" header and names the
		// package itself, so the toolchain's copies of both are duplication.
		strings.HasPrefix(t, "--- FAIL") || t == "FAIL" ||
		strings.HasPrefix(t, "FAIL\t")
}

func (r *Result) finalise(mustRun []string) {
	// Attach the buffered output of failing tests, in the order they ran.
	for name, p := range r.pkgs {
		b, ok := r.buffers[name]
		if !ok {
			continue
		}
		for _, test := range b.order {
			if !p.failedTests[test] {
				continue // a passing test's output is noise, not a failure
			}
			lines := b.byTest[test]
			p.failOutput = append(p.failOutput, "--- FAIL: "+test)
			for _, l := range lines {
				if !isNoise(l) {
					p.failOutput = append(p.failOutput, l)
				}
			}
		}
	}
	r.buffers = nil

	for _, name := range r.order {
		if !matchesAny(name, mustRun) {
			continue
		}
		if r.Status(name) != StatusPass && r.Status(name) != StatusFail {
			r.missed = append(r.missed, name)
		}
	}
	// A required package absent from the stream entirely never compiled or was
	// never selected — as much a hole as one that ran nothing.
	for _, want := range mustRun {
		if want == "" {
			continue
		}
		if !anyPackageMatches(r.order, want) {
			r.missed = append(r.missed, want+"  (no package matched)")
		}
	}
	sort.Strings(r.missed)

	for _, p := range r.pkgs {
		if p.failed || p.fail > 0 {
			r.ExitCode = 1
		}
	}
	if len(r.missed) > 0 {
		r.ExitCode = 1
	}
}

// isMissed reports whether a package was required to run tests and did not.
func (r *Result) isMissed(pkg string) bool {
	for _, m := range r.missed {
		if m == pkg {
			return true
		}
	}
	return false
}

// importPathToPackage strips the " [pkg.test]" qualifier go test appends to a
// build diagnostic's ImportPath, yielding the package the report keys on.
func importPathToPackage(ip string) string {
	if i := strings.Index(ip, " ["); i >= 0 {
		return ip[:i]
	}
	return ip
}

// shortPkg trims the module prefix so a streamed line stays readable.
func shortPkg(pkg string) string {
	if i := strings.Index(pkg, "/cyoda-go/"); i >= 0 {
		return pkg[i+len("/cyoda-go/"):]
	}
	return pkg
}

// matchesRequirement reports whether pkg IS the package a requirement names.
// Substring matching is wrong here: "internal/e2e" would be satisfied by
// "internal/e2e/goroutinesafety", a different package that really does exist,
// and the requirement would silently evaporate.
func matchesRequirement(pkg, req string) bool {
	if req == "" {
		return false
	}
	return pkg == req || shortPkg(pkg) == req || strings.HasSuffix(pkg, "/"+req)
}

func matchesAny(pkg string, subs []string) bool {
	for _, s := range subs {
		if matchesRequirement(pkg, s) {
			return true
		}
	}
	return false
}

func anyPackageMatches(pkgs []string, req string) bool {
	for _, p := range pkgs {
		if matchesRequirement(p, req) {
			return true
		}
	}
	return false
}

// Render produces the human-facing report: failures verbatim and first, then
// anything that did not run, then a one-line-per-package summary of what did.
func (r *Result) Render() string {
	var b strings.Builder

	for _, name := range r.order {
		p := r.pkgs[name]
		if len(p.failOutput) == 0 || r.Status(name) != StatusFail {
			continue
		}
		fmt.Fprintf(&b, "\n=== FAILURES in %s ===\n", name)
		for _, l := range p.failOutput {
			b.WriteString(l + "\n")
		}
	}

	if len(r.missed) > 0 {
		b.WriteString("\n=== REQUIRED SUITES THAT DID NOT RUN ===\n")
		for _, m := range r.missed {
			fmt.Fprintf(&b, "  %s\n", m)
		}
		b.WriteString("\nThese were required to execute tests and did not. A package whose\n" +
			"TestMain exits early reports `ok` with no tests — that is not a pass.\n")
	}

	var pass, fail, skipped, notests int
	var ranTests, failedTests, skippedTests int
	for _, p := range r.pkgs {
		ranTests += p.pass + p.fail
		failedTests += p.fail
		skippedTests += p.skip
	}
	var sum strings.Builder
	for _, name := range r.order {
		p := r.pkgs[name]
		st := r.Status(name)
		switch st {
		case StatusPass:
			pass++
			continue // the happy majority is a count, not a list
		case StatusFail:
			fail++
		case StatusAllSkipped:
			skipped++
		case StatusNoTests:
			notests++
			// A package with no tests is usually correct — generated code,
			// scenario libraries, skeletons. Only call it out when something
			// required it to run. It stays in the count either way.
			if !r.isMissed(name) {
				continue
			}
		}
		fmt.Fprintf(&sum, "  %-8s %6.2fs  %s\n", st, p.elapsed, name)
	}
	if sum.Len() > 0 {
		b.WriteString("\n=== PACKAGES NOT PASSING NORMALLY ===\n")
		b.WriteString(sum.String())
	}
	// Both axes, because they answer different questions. The defect that
	// prompted this tool looked like "7885 tests passing" — healthy — while
	// five PACKAGES had run nothing at all.
	fmt.Fprintf(&b, "\n%d tests (%d failed, %d skipped) across %d packages\n",
		ranTests, failedTests, skippedTests, len(r.order))
	fmt.Fprintf(&b, "%d packages passed, %d failed, %d all-skipped, %d ran no tests\n",
		pass, fail, skipped, notests)
	return b.String()
}

func main() {
	mustRun := flag.String("must-run", "",
		"comma-separated package-path substrings that MUST execute at least one test")
	flag.Parse()

	var required []string
	for _, s := range strings.Split(*mustRun, ",") {
		if s = strings.TrimSpace(s); s != "" {
			required = append(required, s)
		}
	}

	res, err := analyseTo(os.Stdin, required, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(2)
	}
	fmt.Print(res.Render())
	os.Exit(res.ExitCode)
}
