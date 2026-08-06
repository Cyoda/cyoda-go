package txgate

// suspend_call_sites_test.go — every txgate.Suspend site must defer its resume.
//
// Suspend hands back the only thing that can put the gate back. If the blocking
// callout it spans panics and resume never runs, the caller's own deferred gate
// release unlocks a mutex nobody holds: `sync: unlock of unlocked mutex` is a
// runtime fatal, uncatchable by recover, so the door's recovery interceptors and
// the health latch are all bypassed and the node dies. A site may ALSO call
// resume explicitly (to re-acquire before its next buffer write) — resume is
// sync.Once-guarded, so having both costs nothing. Only the defer is mandatory.
//
// The scan is AST-based rather than line-based, which is what makes it both
// quiet and hard to slip past: gofmt-legal blank lines and comments between the
// Suspend and its defer are invisible to it, while a resume bound to anything
// other than a bare local (`h.resume = Suspend(ctx)`), bound positionally in a
// multi-assignment, or discarded outright — strictly the worst case, since the
// gate then never comes back at all — are all caught.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// scanSuspendSites reports every Suspend call in src that is not immediately
// followed by a defer of the resume it returned. src may be nil to read
// filename from disk (see go/parser.ParseFile).
func scanSuspendSites(filename string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	pos := func(n ast.Node) string {
		p := fset.Position(n.Pos())
		return filename + ":" + strconv.Itoa(p.Line)
	}
	render := func(n ast.Node) string {
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, n); err != nil {
			return ""
		}
		return buf.String()
	}

	// isSuspendCall matches both the in-package Suspend(...) and the
	// txgate.Suspend(...) every caller outside this package writes.
	isSuspendCall := func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			return fun.Name == "Suspend"
		case *ast.SelectorExpr:
			return fun.Sel.Name == "Suspend"
		}
		return false
	}
	// stmtList returns the statement list a node owns, and whether it owns one.
	// Every place Go admits a sequence of statements is covered: ordinary
	// blocks, and the case/comm clauses of switch and select, whose bodies are
	// bare []Stmt rather than a BlockStmt.
	stmtList := func(n ast.Node) ([]ast.Stmt, bool) {
		switch s := n.(type) {
		case *ast.BlockStmt:
			return s.List, true
		case *ast.CaseClause:
			return s.Body, true
		case *ast.CommClause:
			return s.Body, true
		}
		return nil, false
	}

	// suspendCallsIn finds Suspend calls belonging to n itself. It deliberately
	// does NOT descend into anything that owns its own statement list, nor into
	// a function literal: each of those is visited in its own right and judged
	// on its own statements. Without the stop, a Suspend correctly deferred
	// inside `if ... { }` would also be attributed to the enclosing `if`, which
	// is not an assignment and so would be reported as discarded.
	suspendCallsIn := func(n ast.Node) []*ast.CallExpr {
		var found []*ast.CallExpr
		ast.Inspect(n, func(c ast.Node) bool {
			if c == nil {
				return false
			}
			if c != n {
				if _, isLit := c.(*ast.FuncLit); isLit {
					return false
				}
				if _, owns := stmtList(c); owns {
					return false
				}
			}
			if isSuspendCall(c) {
				found = append(found, c.(*ast.CallExpr))
			}
			return true
		})
		return found
	}
	// allSuspendCalls is the same search without the func-literal stop, used to
	// build the "every call must be accounted for" set.
	allSuspendCalls := func(n ast.Node) []*ast.CallExpr {
		var found []*ast.CallExpr
		ast.Inspect(n, func(c ast.Node) bool {
			if c != nil && isSuspendCall(c) {
				found = append(found, c.(*ast.CallExpr))
			}
			return true
		})
		return found
	}

	// Every Suspend call in the file. Anything still in here after the block
	// walk below sits in a shape the walk does not recognise (an if-init, a
	// composite literal, a bare argument) and is reported rather than assumed
	// safe.
	unaccounted := map[*ast.CallExpr]bool{}
	for _, call := range allSuspendCalls(file) {
		unaccounted[call] = true
	}

	var offenders []string
	report := func(n ast.Node, why string) {
		offenders = append(offenders, pos(n)+": "+why)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		list, owns := stmtList(n)
		if !owns {
			return true
		}
		for i, stmt := range list {
			calls := suspendCallsIn(stmt)
			if len(calls) == 0 {
				continue
			}
			for _, c := range calls {
				delete(unaccounted, c)
			}
			if len(calls) > 1 {
				report(stmt, "more than one Suspend in a single statement; give each its own statement and its own deferred resume")
				continue
			}
			call := calls[0]

			// What was the resume bound to? An assignment whose RHS at index k
			// IS the Suspend call binds it to LHS[k]; anything else — most
			// importantly a bare ExprStmt — discards it, and a discarded resume
			// means the gate is released and never re-acquired at all.
			assign, ok := stmt.(*ast.AssignStmt)
			if !ok {
				report(stmt, "the resume is discarded; the gate is released and never re-acquired")
				continue
			}
			target := ""
			for k, rhs := range assign.Rhs {
				if rhs == ast.Expr(call) && k < len(assign.Lhs) {
					target = render(assign.Lhs[k])
					break
				}
			}
			if target == "" {
				report(stmt, "the resume is discarded; the gate is released and never re-acquired")
				continue
			}
			if target == "_" {
				report(stmt, "the resume is assigned to _; the gate is released and never re-acquired")
				continue
			}

			// The very next statement must defer exactly that, with no
			// arguments. Blank lines and comments are not statements, so a
			// gofmt-legal gap between the two is invisible here.
			if i+1 >= len(list) {
				report(stmt, "no statement follows, so `defer "+target+"()` is missing")
				continue
			}
			deferStmt, ok := list[i+1].(*ast.DeferStmt)
			if !ok {
				report(stmt, "the next statement is not `defer "+target+"()`")
				continue
			}
			if len(deferStmt.Call.Args) != 0 || render(deferStmt.Call.Fun) != target {
				report(stmt, "the next statement defers `"+render(deferStmt.Call)+"`, not `"+target+"()`")
			}
		}
		return true
	})

	for call := range unaccounted {
		report(call, "Suspend called in a shape this guard cannot verify; bind the resume to a local and defer it on the next line")
	}
	return offenders, nil
}

func TestSuspend_EveryCallSiteDefersItsResume(t *testing.T) {
	root := moduleRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Separate modules cannot import cyoda-go/internal, and vendored or
			// hidden trees are not ours to police.
			if name := info.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "plugins" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// rel names the file in the message; src is what is parsed, so the
		// test's own working directory is irrelevant.
		found, scanErr := scanSuspendSites(rel, src)
		if scanErr != nil {
			return scanErr
		}
		offenders = append(offenders, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("a Suspend whose resume is not deferred leaves the gate released when the callout panics, "+
			"and the caller's deferred release then unlocks an unlocked mutex (runtime fatal):\n%s",
			strings.Join(offenders, "\n"))
	}
}

// TestSuspendSiteScanner_Discriminates proves the guard earns its keep in both
// directions. A guard that fires on gofmt-legal formatting gets deleted by the
// first person it annoys; a guard that misses the worst shape is worse than
// none. Every case below is a shape someone could plausibly write.
func TestSuspendSiteScanner_Discriminates(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantBad bool
	}{
		{
			name: "defer immediately after",
			body: `resume := txgate.Suspend(ctx)
			defer resume()
			dispatch()`,
		},
		{
			name: "defer plus the explicit re-acquire",
			body: `resume := txgate.Suspend(ctx)
			defer resume()
			dispatch()
			resume()`,
		},
		{
			name: "blank line and a comment between the two",
			body: `resume := txgate.Suspend(ctx)

			// Deferred so a panicking dispatch still puts the gate back.
			defer resume()
			dispatch()`,
		},
		{
			name: "bound to a field, deferred through the same selector",
			body: `h.resume = txgate.Suspend(ctx)
			defer h.resume()
			dispatch()`,
		},
		{
			name: "multi-assignment, resume deferred",
			body: `resume, n := txgate.Suspend(ctx), 1
			defer resume()
			_ = n`,
		},
		{
			name: "no defer at all",
			body: `resume := txgate.Suspend(ctx)
			dispatch()
			resume()`,
			wantBad: true,
		},
		{
			name: "return discarded entirely",
			body: `txgate.Suspend(ctx)
			dispatch()`,
			wantBad: true,
		},
		{
			name: "return assigned to the blank identifier",
			body: `_ = txgate.Suspend(ctx)
			dispatch()`,
			wantBad: true,
		},
		{
			name: "bound to a field, never deferred",
			body: `h.resume = txgate.Suspend(ctx)
			dispatch()`,
			wantBad: true,
		},
		{
			name: "multi-assignment, resume never deferred",
			body: `resume, n := txgate.Suspend(ctx), 1
			dispatch()
			_ = n
			_ = resume`,
			wantBad: true,
		},
		{
			name: "defers a different identifier",
			body: `resume := txgate.Suspend(ctx)
			defer other()
			dispatch()`,
			wantBad: true,
		},
		{
			name: "Suspend hidden in an if-init",
			body: `if resume := txgate.Suspend(ctx); resume != nil {
				dispatch()
			}`,
			wantBad: true,
		},
		{
			name: "a function with no Suspend at all is untouched",
			body: `dispatch()
			other()`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\n\nfunc f() {\n\t\t\t" + tc.body + "\n}\n"
			offenders, err := scanSuspendSites("synthetic.go", src)
			if err != nil {
				t.Fatalf("parsing the synthetic source: %v\n%s", err, src)
			}
			if tc.wantBad && len(offenders) == 0 {
				t.Errorf("this shape leaves the gate released but the guard passed it:\n%s", src)
			}
			if !tc.wantBad && len(offenders) > 0 {
				t.Errorf("the guard fired on a correct shape (%v):\n%s", offenders, src)
			}
		})
	}
}

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := wd
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate the module root; test skipped")
			return ""
		}
		root = parent
	}
}
