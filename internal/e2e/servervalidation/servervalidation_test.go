package servervalidation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// e2eSourceDir is the package whose sources this guard analyses.
const e2eSourceDir = ".."

// allowlist names the constructs in internal/e2e that may serve HTTP without
// the conformance validator, by file, enclosing function and construct, each
// with the reason it serves no cyoda API. An entry that matches nothing fails
// the guard, so the list cannot go stale.
var allowlist = map[site]string{
	{"callout_handover_lost_test.go", "newStandInPeer", "httptest.NewServer"}:                           "a stand-in cluster peer: serves only the internal peer-dispatch route, which is not in the API spec",
	{"callout_handover_lost_test.go", "freeLoopbackPort", "net.Listen"}:                                 "reserves a port for the gossip layer and closes it at once; serves nothing",
	{"callback_harness_test.go", "newCalloutHarnessWithKey", "net.Listen"}:                              "the harness's gRPC listener; the HTTP validator does not apply to gRPC",
	{"pool_metrics_test.go", "TestMetrics_PostgresPoolSeriesAreExported", "httptest.NewServer"}:         "the Prometheus metrics handler, which is not in the API spec",
	{"torn_connection_e2e_test.go", "newPGProxy", "net.Listen"}:                                         "a TCP proxy in front of PostgreSQL; it carries no HTTP",
	{"tx_lifecycle_e2e_test.go", "TestE2E_PanicInOwnedWrite_ReturnsConnection", "httptest.NewRecorder"}: "drives the admin handler's /readyz and /livez, operational paths outside the API spec",
}

// TestEveryServerIsValidated asserts that every server, handler assignment,
// listener and recorder in internal/e2e goes through validated, or is on the
// allowlist with its reason.
func TestEveryServerIsValidated(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, e2eSourceDir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", e2eSourceDir, err)
	}
	files := map[string]*ast.File{}
	for _, pkg := range pkgs {
		for path, f := range pkg.Files {
			files[filepath.Base(path)] = f
		}
	}
	if len(files) == 0 {
		t.Fatalf("no files parsed from %s", e2eSourceDir)
	}
	violations, unused := analyze(fset, files, allowlist)
	if len(violations) > 0 {
		t.Errorf("%d server(s) in internal/e2e serve without the conformance validator. "+
			"Wrap the handler in validated(…), or add the site to the allowlist with the reason it serves no cyoda API:\n\t%s",
			len(violations), strings.Join(violations, "\n\t"))
	}
	if len(unused) > 0 {
		t.Errorf("allowlist entries that match nothing; remove them:\n\t%s", strings.Join(unused, "\n\t"))
	}
}

// TestAnalyze checks the guard itself on small sources.
func TestAnalyze(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		allow map[site]string
		want  []string // the construct of each violation, in order
	}{
		{"NewServer validated", `func f() { httptest.NewServer(validated(h, inErrorCodeMatrix)) }`, nil, nil},
		{"NewServer unwrapped", `func f() { httptest.NewServer(h) }`, nil, []string{"httptest.NewServer"}},
		{"NewTLSServer unwrapped", `func f() { httptest.NewTLSServer(h) }`, nil, []string{"httptest.NewTLSServer"}},
		{"NewUnstartedServer validated", `func f() { httptest.NewUnstartedServer(validated(h, x)) }`, nil, nil},
		{"NewUnstartedServer(nil) then validated handler", `func f() { s := httptest.NewUnstartedServer(nil); s.Config.Handler = validated(h, x) }`, nil, nil},
		{"NewUnstartedServer(nil) never given a handler", `func f() { httptest.NewUnstartedServer(nil) }`, nil, []string{"httptest.NewUnstartedServer"}},
		{"NewUnstartedServer(nil) given an unwrapped handler", `func f() { s := httptest.NewUnstartedServer(nil); s.Config.Handler = h }`, nil, []string{".Config.Handler ="}},
		{"NewUnstartedServer unwrapped", `func f() { httptest.NewUnstartedServer(h) }`, nil, []string{"httptest.NewUnstartedServer"}},
		{"http.Server validated", `func f() { _ = &http.Server{Handler: validated(h, x)} }`, nil, nil},
		{"http.Server unwrapped", `func f() { _ = http.Server{Handler: h} }`, nil, []string{"http.Server{}"}},
		{"http.Server without a handler", `func f() { _ = http.Server{Addr: ":0"} }`, nil, []string{"http.Server{}"}},
		{"net.Listen", `func f() { net.Listen("tcp", "x") }`, nil, []string{"net.Listen"}},
		{"httptest.NewRecorder", `func f() { httptest.NewRecorder() }`, nil, []string{"httptest.NewRecorder"}},
		{"allowlisted", `func f() { httptest.NewServer(h) }`, map[site]string{{"x_test.go", "f", "httptest.NewServer"}: "reason"}, nil},
		{"allowlisted in another function only", `func g() { httptest.NewServer(h) }`, map[site]string{{"x_test.go", "f", "httptest.NewServer"}: "reason"}, []string{"httptest.NewServer"}},
		{"inside a method and a func literal", `func (s *T) m() { go func() { httptest.NewServer(h) }() }`, nil, []string{"httptest.NewServer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x_test.go", "package p\n"+tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			violations, _ := analyze(fset, map[string]*ast.File{"x_test.go": f}, tc.allow)
			var got []string
			for _, v := range violations {
				got = append(got, v[strings.LastIndex(v, ": ")+2:])
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("violations = %q, want constructs %q", violations, tc.want)
			}
		})
	}
}

// An allowlist entry that matches no site is reported as unused.
func TestAnalyze_UnusedAllowlistEntry(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x_test.go", "package p\nfunc f() {}", 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, unused := analyze(fset, map[string]*ast.File{"x_test.go": f}, map[site]string{{"x_test.go", "f", "net.Listen"}: "reason"})
	if len(unused) != 1 {
		t.Errorf("unused = %q, want the one entry", unused)
	}
}
