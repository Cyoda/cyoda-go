package txcoord

// pause_call_sites_test.go — every txcoord.Pause site defers its resume, and
// also calls it explicitly and refuses the chain on its error.
//
// The deferred resume puts the gate back when the callout the pause spans
// panics: without it the chain's scope would end holding no slot, and its
// gate's queue would never move. The explicit resume retakes the gate before
// the chain touches the transaction again, and its error is the chain's
// check: a pass that is no longer current, or a marked scope, must stop the
// chain there, before it writes to a transaction the owner has moved on from.
//
// The scan is AST-based: comments and blank lines between the statements are
// invisible to it, and a doc comment that mentions txcoord.Pause( is not a
// site.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// isPauseCall reports whether n is a call to txcoord.Pause.
func isPauseCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Pause" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "txcoord"
}

func countPauses(n ast.Node) int {
	found := 0
	ast.Inspect(n, func(c ast.Node) bool {
		if isPauseCall(c) {
			found++
		}
		return true
	})
	return found
}

// stmtList returns the statement list a node owns, and whether it owns one.
func stmtList(n ast.Node) ([]ast.Stmt, bool) {
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

// pausedName returns NAME when s is `NAME := txcoord.Pause(...)`.
func pausedName(s ast.Stmt) string {
	assign, ok := s.(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !isPauseCall(assign.Rhs[0]) {
		return ""
	}
	id, ok := assign.Lhs[0].(*ast.Ident)
	if !ok || id.Name == "_" {
		return ""
	}
	return id.Name
}

// callsName reports whether e is the call `name()`.
func callsName(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}

// isDeferOf reports whether s is `defer name()`.
func isDeferOf(s ast.Stmt, name string) bool {
	d, ok := s.(*ast.DeferStmt)
	return ok && callsName(d.Call, name)
}

func identAppears(name string, n ast.Node) bool {
	found := false
	ast.Inspect(n, func(c ast.Node) bool {
		if id, ok := c.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// isCheckedResume reports whether s is `if E := name(); E != nil { ... return
// ...E... }`: the explicit resume, whose error the chain returns.
func isCheckedResume(s ast.Stmt, name string) bool {
	ifStmt, ok := s.(*ast.IfStmt)
	if !ok {
		return false
	}
	assign, ok := ifStmt.Init.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !callsName(assign.Rhs[0], name) {
		return false
	}
	errID, ok := assign.Lhs[0].(*ast.Ident)
	if !ok || errID.Name == "_" {
		return false
	}
	for _, body := range ifStmt.Body.List {
		ret, ok := body.(*ast.ReturnStmt)
		if !ok {
			continue
		}
		for _, r := range ret.Results {
			if identAppears(errID.Name, r) {
				return true
			}
		}
	}
	return false
}

// scanPauseSites reports how many txcoord.Pause calls src makes and every way
// it breaks the rule.
func scanPauseSites(filename string, src any) (pauses int, offenders []string, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return 0, nil, err
	}
	at := func(n ast.Node) string {
		return filename + ":" + strconv.Itoa(fset.Position(n.Pos()).Line)
	}
	pauses = countPauses(file)
	inFuncs := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		n := countPauses(fn)
		inFuncs += n
		if n == 0 {
			continue
		}
		var names []string
		ast.Inspect(fn.Body, func(c ast.Node) bool {
			list, owns := stmtList(c)
			if !owns {
				return true
			}
			for i, stmt := range list {
				name := pausedName(stmt)
				if name == "" {
					continue
				}
				names = append(names, name)
				if i+1 >= len(list) || !isDeferOf(list[i+1], name) {
					offenders = append(offenders, at(stmt)+": the statement after "+name+" := txcoord.Pause(...) must be `defer "+name+"()`")
				}
			}
			return true
		})
		if len(names) != n {
			offenders = append(offenders, at(fn)+": func "+fn.Name.Name+" makes "+strconv.Itoa(n)+
				" txcoord.Pause call(s) but binds "+strconv.Itoa(len(names))+" of them as `resume := txcoord.Pause(ctx)`")
		}
		for _, name := range names {
			checked := 0
			ast.Inspect(fn.Body, func(c ast.Node) bool {
				list, owns := stmtList(c)
				if !owns {
					return true
				}
				for _, stmt := range list {
					if isCheckedResume(stmt, name) {
						checked++
					}
				}
				return true
			})
			if checked == 0 {
				offenders = append(offenders, at(fn)+": func "+fn.Name.Name+" must call "+name+
					"() explicitly as `if err := "+name+"(); err != nil { return ...err... }` before it touches the transaction again")
			}
		}
	}
	if inFuncs != pauses {
		offenders = append(offenders, filename+": "+strconv.Itoa(pauses-inFuncs)+
			" txcoord.Pause call(s) sit outside any function declaration, where this guard cannot judge them")
	}
	return pauses, offenders, nil
}

func TestEveryPauseIsDeferredAndChecked(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var offenders []string
	sites := 0
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch name := info.Name(); {
			case name == "txcoord": // Pause's own definition
				return filepath.SkipDir
			case name != "." && (strings.HasPrefix(name, ".") || name == "plugins" || name == "vendor"):
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
		n, found, scanErr := scanPauseSites(rel, src)
		sites += n
		offenders = append(offenders, found...)
		return scanErr
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if sites == 0 {
		t.Fatal("no txcoord.Pause site found: the scan looked in the wrong place")
	}
	if len(offenders) > 0 {
		t.Fatalf("every Pause defers its resume and checks it explicitly:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestPauseSiteScanner_Discriminates proves the guard passes the correct shape
// and catches each way a chain could reach a store unchecked.
func TestPauseSiteScanner_Discriminates(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantBad bool
	}{
		{
			name: "deferred and checked",
			src: `package p

func f() error {
	resume := txcoord.Pause(ctx)
	defer resume()
	dispatch()
	// Before the transaction is touched again.
	if err := resume(); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	return nil
}
`,
		},
		{
			name: "a doc comment mentioning txcoord.Pause( is not a site",
			src: `package p

// g pauses the scope (txcoord.Pause(ctx)) across the dispatch.
func g() { dispatch() }
`,
		},
		{
			name: "no defer",
			src: `package p

func f() error {
	resume := txcoord.Pause(ctx)
	dispatch()
	if err := resume(); err != nil {
		return err
	}
	return nil
}
`,
			wantBad: true,
		},
		{
			name: "no explicit resume: the deferred one retakes the gate past every check",
			src: `package p

func f() {
	resume := txcoord.Pause(ctx)
	defer resume()
	dispatch()
}
`,
			wantBad: true,
		},
		{
			name: "the resume's error is thrown away",
			src: `package p

func f() {
	resume := txcoord.Pause(ctx)
	defer resume()
	dispatch()
	_ = resume()
	use()
}
`,
			wantBad: true,
		},
		{
			name: "the resume's error is swallowed",
			src: `package p

func f() error {
	resume := txcoord.Pause(ctx)
	defer resume()
	dispatch()
	if err := resume(); err != nil {
		return nil
	}
	return nil
}
`,
			wantBad: true,
		},
		{
			name: "a Pause whose resume is not bound to a name",
			src: `package p

func f() {
	defer txcoord.Pause(ctx)()
	dispatch()
}
`,
			wantBad: true,
		},
		{
			name: "a Pause outside any function declaration cannot be judged",
			src: `package p

var resume = txcoord.Pause(ctx)
`,
			wantBad: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, offenders, err := scanPauseSites("synthetic.go", tc.src)
			if err != nil {
				t.Fatalf("parsing the synthetic source: %v\n%s", err, tc.src)
			}
			if tc.wantBad && len(offenders) == 0 {
				t.Errorf("this shape lets a chain reach a store unchecked but the guard passed it:\n%s", tc.src)
			}
			if !tc.wantBad && len(offenders) > 0 {
				t.Errorf("the guard fired on a correct shape (%v):\n%s", offenders, tc.src)
			}
		})
	}
}
