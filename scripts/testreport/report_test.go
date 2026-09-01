package main

import (
	"strings"
	"testing"
)

// eventStream builds a go test -json stream from compact literals so the tests
// read as the scenario they pin rather than as JSON.
func eventStream(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

const (
	pkgParity = "github.com/cyoda-platform/cyoda-go/e2e/parity/memory"
	pkgSearch = "github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

func runEvent(pkg, test string) string {
	return `{"Action":"run","Package":"` + pkg + `","Test":"` + test + `"}`
}
func testEvent(action, pkg, test string) string {
	return `{"Action":"` + action + `","Package":"` + pkg + `","Test":"` + test + `"}`
}
func outEvent(pkg, test, out string) string {
	return `{"Action":"output","Package":"` + pkg + `","Test":"` + test + `","Output":"` + out + `"}`
}
func pkgEvent(action, pkg string, elapsed float64) string {
	return `{"Action":"` + action + `","Package":"` + pkg + `","Elapsed":` + trimFloat(elapsed) + `}`
}

func trimFloat(f float64) string {
	switch f {
	case 2.8:
		return "2.8"
	case 1.5:
		return "1.5"
	default:
		return "0"
	}
}

// A package that exits TestMain with os.Exit(0) emits a package-level "pass"
// and no test events at all. That is the exact shape this tool exists to
// catch: it is indistinguishable from a real pass in `go test` output.
func TestSilentSkip_RequiredPackageThatRanNoTests_IsAFailure(t *testing.T) {
	stream := eventStream(pkgEvent("pass", pkgParity, 2.8))

	res, err := analyse(strings.NewReader(stream), []string{"e2e/parity"})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a required package that ran zero tests must fail; got exit 0\n%s", res.Render())
	}
	if got := res.Status(pkgParity); got != StatusNoTests {
		t.Errorf("status = %q, want %q", got, StatusNoTests)
	}
	if !strings.Contains(res.Render(), pkgParity) {
		t.Errorf("report must name the offending package; got:\n%s", res.Render())
	}
}

// The same shape in a package nobody requires is reported but not fatal —
// plenty of packages legitimately hold no tests. The requirement here is met
// by a different package that did run, so nothing is missing.
func TestSilentSkip_UnrequiredPackageThatRanNoTests_IsReportedNotFatal(t *testing.T) {
	stream := eventStream(
		pkgEvent("pass", pkgParity, 2.8),
		runEvent(pkgSearch, "TestThing"),
		testEvent("pass", pkgSearch, "TestThing"),
		pkgEvent("pass", pkgSearch, 1.5),
	)

	res, err := analyse(strings.NewReader(stream), []string{"internal/domain/search"})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("no required package was missed; want exit 0, got %d\n%s", res.ExitCode, res.Render())
	}
	if got := res.Status(pkgParity); got != StatusNoTests {
		t.Errorf("status = %q, want %q", got, StatusNoTests)
	}
}

// A required package that ran tests and passed them is the happy path.
func TestRequiredPackageThatPassed_IsClean(t *testing.T) {
	stream := eventStream(
		runEvent(pkgParity, "TestThing"),
		testEvent("pass", pkgParity, "TestThing"),
		pkgEvent("pass", pkgParity, 2.8),
	)

	res, err := analyse(strings.NewReader(stream), []string{"e2e/parity"})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("want exit 0, got %d\n%s", res.ExitCode, res.Render())
	}
	if got := res.Status(pkgParity); got != StatusPass {
		t.Errorf("status = %q, want %q", got, StatusPass)
	}
}

// A package whose every test skipped is NOT a pass: it is reported distinctly,
// and it does not satisfy a must-run requirement.
func TestPackageWhereEveryTestSkipped_DoesNotSatisfyMustRun(t *testing.T) {
	stream := eventStream(
		runEvent(pkgParity, "TestThing"),
		testEvent("skip", pkgParity, "TestThing"),
		pkgEvent("pass", pkgParity, 2.8),
	)

	res, err := analyse(strings.NewReader(stream), []string{"e2e/parity"})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an all-skipped required package must fail; got exit 0\n%s", res.Render())
	}
	if got := res.Status(pkgParity); got != StatusAllSkipped {
		t.Errorf("status = %q, want %q", got, StatusAllSkipped)
	}
}

// Failure output is reproduced verbatim and in full. This is the truncation
// trap the tool exists to remove: piping -v through `tail` loses the failure.
func TestFailureOutput_IsReproducedVerbatim(t *testing.T) {
	stream := eventStream(
		runEvent(pkgSearch, "TestBroken"),
		outEvent(pkgSearch, "TestBroken", "    service_test.go:42: want 7, got 3\\n"),
		testEvent("fail", pkgSearch, "TestBroken"),
		pkgEvent("fail", pkgSearch, 1.5),
	)

	res, err := analyse(strings.NewReader(stream), []string{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a failing test must produce a non-zero exit")
	}
	out := res.Render()
	if !strings.Contains(out, "service_test.go:42: want 7, got 3") {
		t.Errorf("failure detail must appear verbatim; got:\n%s", out)
	}
	if !strings.Contains(out, "TestBroken") {
		t.Errorf("failing test name must appear; got:\n%s", out)
	}
}

// A passing run must not reproduce per-test output: that is the 45k-event
// noise that made agents pipe the output through `tail` in the first place.
func TestPassingRun_DoesNotEchoPerTestOutput(t *testing.T) {
	stream := eventStream(
		runEvent(pkgSearch, "TestFine"),
		outEvent(pkgSearch, "TestFine", "chatty progress line\\n"),
		testEvent("pass", pkgSearch, "TestFine"),
		pkgEvent("pass", pkgSearch, 1.5),
	)

	res, err := analyse(strings.NewReader(stream), []string{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if strings.Contains(res.Render(), "chatty progress line") {
		t.Errorf("passing output must not be echoed; got:\n%s", res.Render())
	}
}

// A build failure emits package-level output with no test events. It must be
// surfaced, not silently classed as "no tests".
func TestBuildFailure_IsFatalAndShown(t *testing.T) {
	stream := eventStream(
		`{"Action":"output","Package":"`+pkgSearch+`","Output":"service.go:9:2: undefined: wat\n"}`,
		pkgEvent("fail", pkgSearch, 0),
	)

	res, err := analyse(strings.NewReader(stream), []string{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a build failure must produce a non-zero exit")
	}
	if !strings.Contains(res.Render(), "undefined: wat") {
		t.Errorf("build error must be shown; got:\n%s", res.Render())
	}
}

// A required package that never appears in the stream at all did not compile,
// or was not selected by the package pattern. Either way the suite it names
// did not run, and saying so is the whole point of the tool.
func TestRequiredPackageAbsentFromStream_IsAFailure(t *testing.T) {
	stream := eventStream(
		runEvent(pkgSearch, "TestThing"),
		testEvent("pass", pkgSearch, "TestThing"),
		pkgEvent("pass", pkgSearch, 1.5),
	)

	res, err := analyse(strings.NewReader(stream), []string{"e2e/parity"})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("a required package absent from the run must fail; got exit 0\n%s", res.Render())
	}
	if !strings.Contains(res.Render(), "e2e/parity") {
		t.Errorf("report must name the missing requirement; got:\n%s", res.Render())
	}
}

// Most packages that hold no tests hold none legitimately — generated protobuf,
// scenario libraries the runners consume, skeleton packages. Listing them all
// as anomalies buries the ones that matter, which is the same noise problem
// that made -v output unreadable.
func TestTestFreePackages_AreNotListedUnlessRequired(t *testing.T) {
	const pkgProto = "github.com/cyoda-platform/cyoda-go/proto"
	stream := eventStream(
		pkgEvent("pass", pkgProto, 0),
		runEvent(pkgSearch, "TestThing"),
		testEvent("pass", pkgSearch, "TestThing"),
		pkgEvent("pass", pkgSearch, 1.5),
	)

	res, err := analyse(strings.NewReader(stream), []string{"internal/domain/search"})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("want exit 0, got %d\n%s", res.ExitCode, res.Render())
	}
	if strings.Contains(res.Render(), pkgProto) {
		t.Errorf("an unrequired test-free package must not be listed; got:\n%s", res.Render())
	}
	// It is still counted, so the totals stay honest.
	if !strings.Contains(res.Render(), "ran no tests") {
		t.Errorf("the count must still report it; got:\n%s", res.Render())
	}
}

// A required package that ran nothing must still be listed in the summary, not
// only in the missing-suites block.
func TestRequiredTestFreePackage_IsListed(t *testing.T) {
	stream := eventStream(pkgEvent("pass", pkgParity, 2.8))

	res, err := analyse(strings.NewReader(stream), []string{pkgParity})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if !strings.Contains(res.Render(), pkgParity) {
		t.Errorf("a required package that ran nothing must be listed; got:\n%s", res.Render())
	}
}

// A 15-minute tier that stays silent until the end wastes the whole run when
// something fails at minute three. Failures must surface as they happen; the
// verbatim detail still lands in the final report.
func TestFailures_AreStreamedAsTheyHappen(t *testing.T) {
	stream := eventStream(
		runEvent(pkgSearch, "TestBroken"),
		outEvent(pkgSearch, "TestBroken", "    x_test.go:9: boom\\n"),
		testEvent("fail", pkgSearch, "TestBroken"),
		pkgEvent("fail", pkgSearch, 1.5),
	)

	var progress strings.Builder
	res, err := analyseTo(strings.NewReader(stream), []string{}, &progress)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if !strings.Contains(progress.String(), "TestBroken") {
		t.Errorf("a failing test must be announced while the run proceeds; got %q", progress.String())
	}
	// The final report still carries the detail.
	if !strings.Contains(res.Render(), "x_test.go:9: boom") {
		t.Errorf("final report must still carry the detail; got:\n%s", res.Render())
	}
}

// Passing tests must not stream, or the progress channel becomes the same
// 45k-line firehose the -v output was.
func TestPasses_AreNotStreamed(t *testing.T) {
	stream := eventStream(
		runEvent(pkgSearch, "TestFine"),
		testEvent("pass", pkgSearch, "TestFine"),
		pkgEvent("pass", pkgSearch, 1.5),
	)

	var progress strings.Builder
	if _, err := analyseTo(strings.NewReader(stream), []string{}, &progress); err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if progress.Len() != 0 {
		t.Errorf("passing tests must stay quiet; got %q", progress.String())
	}
}

// go test reports compile errors on "build-output" events keyed by ImportPath
// — NOT by Package, and the ImportPath carries a " [pkg.test]" suffix. A
// parser that keys only on Package drops the one line that says what is
// actually wrong, leaving "[build failed]" with no reason. This is the exact
// shape the toolchain emits.
func TestBuildError_KeyedByImportPath_IsAttributedAndShown(t *testing.T) {
	const ip = pkgSearch + " [" + pkgSearch + ".test]"
	stream := eventStream(
		`{"ImportPath":"`+ip+`","Action":"build-output","Output":"# `+ip+`\n"}`,
		`{"ImportPath":"`+ip+`","Action":"build-output","Output":"internal/domain/search/x_test.go:6:2: undefined: undefinedSymbol\n"}`,
		`{"ImportPath":"`+ip+`","Action":"build-fail"}`,
		`{"Action":"output","Package":"`+pkgSearch+`","Output":"FAIL\tpkg [build failed]\n"}`,
		pkgEvent("fail", pkgSearch, 0),
	)

	res, err := analyse(strings.NewReader(stream), []string{})
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a build failure must produce a non-zero exit")
	}
	out := res.Render()
	if !strings.Contains(out, "undefined: undefinedSymbol") {
		t.Errorf("the compile error must be shown; got:\n%s", out)
	}
	if !strings.Contains(out, "x_test.go:6:2") {
		t.Errorf("the file:line must be shown; got:\n%s", out)
	}
}
