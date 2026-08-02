package goroutinesafety

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// e2eSourceDir is the package whose sources this guard analyses.
const e2eSourceDir = ".."

// safeTMethods are the *testing.T methods that the testing package documents
// as callable from any goroutine. Everything else — Fatal, FailNow, Skip and
// friends — terminates only the calling goroutine via runtime.Goexit, so the
// test keeps running past a failure it believes it aborted on.
var safeTMethods = map[string]bool{
	"Deadline": true,
	"Error":    true,
	"Errorf":   true,
	"Fail":     true,
	"Failed":   true,
	"Helper":   true,
	"Log":      true,
	"Logf":     true,
	"Name":     true,
	"Skipped":  true,
}

// TestNoTestingTUseInGoroutines asserts that no `go` statement in the E2E
// suite touches a *testing.T except through safeTMethods.
//
// The rule is deliberately stricter than "no direct t.Fatal in a go func":
// passing t to a helper hides the Fatal one call away, which is exactly how
// doAuth/getToken/readBody smuggled t.Fatalf into concurrent tests. The
// goroutine-safe shape is to return values, collect them, and assert on the
// test goroutine after wg.Wait().
//
// Scope, stated precisely so the guard is not mistaken for more than it is.
// It reasons syntactically about `go` statements in this one package, and
// tracks *testing.T through function parameters and direct aliases (tt := t).
// It does NOT catch a t that reaches another goroutine by:
//   - being stored in a struct field or sent over a channel,
//   - being captured by a func literal that some registrar invokes later on
//     its own goroutine — notably callbackHarness.RegisterProc/RegisterCriterion/
//     RegisterFunction bodies, which run on the compute-member goroutine,
//   - a method value (f := t.Fatalf) called elsewhere.
//
// Those need type/callgraph analysis. The syntactic check covers the class
// that actually regressed here; the blind spots are listed so a reviewer knows
// to look for them by hand rather than trusting a green run to mean more than
// it does.
func TestNoTestingTUseInGoroutines(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, e2eSourceDir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", e2eSourceDir, err)
	}
	if len(pkgs) == 0 {
		t.Fatalf("no packages parsed from %s", e2eSourceDir)
	}

	var files []*ast.File
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files = append(files, file)
		}
	}

	if violations := analyze(fset, files); len(violations) > 0 {
		t.Errorf("*testing.T used from a goroutine in %d place(s) — "+
			"Fatal/FailNow/Skip only stop the calling goroutine, so the test "+
			"races on past the failure. Return the outcome from the goroutine "+
			"and assert it on the test goroutine instead:\n\t%s",
			len(violations), strings.Join(violations, "\n\t"))
	}
}

// analyze reports every *testing.T use that happens on a non-test goroutine,
// sorted for stable output.
func analyze(fset *token.FileSet, files []*ast.File) []string {
	c := &checker{fset: fset}
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			c.walk(fd.Body, tNamesOf(fd.Type, nil), false)
		}
	}
	sort.Strings(c.violations)
	return c.violations
}

type checker struct {
	fset       *token.FileSet
	violations []string
}

func (c *checker) reportf(pos token.Pos, format string, args ...any) {
	p := c.fset.Position(pos)
	loc := fmt.Sprintf("%s:%d", filepath.Join("internal", "e2e", filepath.Base(p.Filename)), p.Line)
	c.violations = append(c.violations, loc+": "+fmt.Sprintf(format, args...))
}

// walk traverses n, tracking the *testing.T identifiers in scope and whether
// the code being visited runs on a goroutine other than the test's.
func (c *checker) walk(n ast.Node, tNames map[string]bool, inGoroutine bool) {
	ast.Inspect(n, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.FuncLit:
			// A literal inherits the enclosing scope's t and goroutine-ness,
			// plus any *testing.T of its own (e.g. a t.Run subtest body).
			c.walk(v.Body, tNamesOf(v.Type, tNames), inGoroutine)
			return false

		case *ast.AssignStmt:
			// Track aliases (`tt := t`) so a goroutine using the alias is still
			// caught. Over-approximates across sibling scopes, which is the
			// safe direction for a lint.
			for i, rhs := range v.Rhs {
				id, ok := rhs.(*ast.Ident)
				if !ok || !tNames[id.Name] || i >= len(v.Lhs) {
					continue
				}
				if lhs, ok := v.Lhs[i].(*ast.Ident); ok && lhs.Name != "_" {
					tNames[lhs.Name] = true
				}
			}
			return true

		case *ast.GoStmt:
			// Arguments are evaluated on the current goroutine, so walk them
			// with the enclosing goroutine-ness...
			for _, arg := range v.Call.Args {
				c.walk(arg, tNames, inGoroutine)
			}
			// ...but the call itself runs on the new goroutine. This covers
			// both `go t.Fatal(...)` (unsafe method on the receiver) and
			// `go f(t)` (handing t to a concurrently-running body).
			c.checkCall(v.Call, tNames)
			c.walk(v.Call.Fun, tNames, true)
			return false

		case *ast.CallExpr:
			if inGoroutine {
				c.checkCall(v, tNames)
			}
			return true
		}
		return true
	})
}

// checkCall flags any use of an in-scope *testing.T inside a goroutine: an
// unsafe method on it, or handing it to another function that may Fatal.
func (c *checker) checkCall(call *ast.CallExpr, tNames map[string]bool) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && tNames[id.Name] && !safeTMethods[sel.Sel.Name] {
			c.reportf(call.Pos(), "calls %s.%s from a goroutine", id.Name, sel.Sel.Name)
			return
		}
	}
	for _, arg := range call.Args {
		if id, ok := arg.(*ast.Ident); ok && tNames[id.Name] {
			c.reportf(call.Pos(), "passes %s to %s(…) from a goroutine", id.Name, exprString(call.Fun))
		}
	}
}

// tNamesOf returns inherited plus the *testing.T/testing.TB parameter names
// declared by fn.
func tNamesOf(fn *ast.FuncType, inherited map[string]bool) map[string]bool {
	out := make(map[string]bool, len(inherited)+1)
	for k := range inherited {
		out[k] = true
	}
	if fn.Params == nil {
		return out
	}
	for _, field := range fn.Params.List {
		if !isTestingT(field.Type) {
			continue
		}
		for _, name := range field.Names {
			if name.Name != "_" {
				out[name.Name] = true
			}
		}
	}
	return out
}

// isTestingT reports whether expr denotes *testing.T, *testing.B or testing.TB.
func isTestingT(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "testing" {
		return false
	}
	switch sel.Sel.Name {
	case "T", "B", "TB":
		return true
	}
	return false
}

// exprString renders a callee expression for the violation message.
func exprString(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.FuncLit:
		return "func literal"
	}
	return "call"
}
