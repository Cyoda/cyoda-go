package fence

// check_call_sites_test.go — after a callout of its own a joined chain re-takes
// the transaction's lock, and the statement that follows the re-acquire must
// refuse the chain if its pass is no longer current. Otherwise the chain carries
// on writing to a transaction the owner has already given to another compute
// node.
//
// The guard is the companion of internal/txgate's suspend_call_sites_test.go:
// that one insists every Suspend defers its resume, this one insists every
// Suspend ALSO re-acquires explicitly and refuses straight afterwards. A sixth
// Suspend site therefore cannot be added without its check.
//
// Three things make it hard to slip past:
//
//   - The Suspend calls are counted from the AST, not from the file's text, so
//     a doc comment that happens to mention txgate.Suspend( neither inflates the
//     count nor breaks the guard.
//   - Counts are compared PER FUNCTION. A module-wide sum lets a function with
//     two explicit resumes pay for one with none.
//   - The statement after resume() must be an `if` that calls fence.Check in its
//     init or condition AND returns from its body. A statement that merely
//     mentions fence.Check — an ignored result, a check that only logs — is not
//     a refusal and is reported.
//
// The scan is AST-based, so gofmt-legal blank lines and comments between the
// resume and the check are invisible to it.

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

// stmtList returns the statement list a node owns, and whether it owns one.
// Every place Go admits a sequence of statements is covered: ordinary blocks,
// and the case/comm clauses of switch and select, whose bodies are bare []Stmt.
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

// countSuspends returns the number of txgate.Suspend calls in n, from the AST.
func countSuspends(n ast.Node) int {
	found := 0
	ast.Inspect(n, func(c ast.Node) bool {
		call, ok := c.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Suspend" {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "txgate" {
				found++
			}
		}
		return true
	})
	return found
}

// isResume reports whether s is the bare statement `resume()`. A DeferStmt is
// not an ExprStmt, so `defer resume()` is deliberately not matched: the deferred
// call re-acquires the lock as the frame unwinds, past every check.
func isResume(s ast.Stmt) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "resume"
}

// callsFenceCheck reports whether n contains a call to fence.Check.
func callsFenceCheck(n ast.Node) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(c ast.Node) bool {
		if call, ok := c.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Check" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fence" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// boundFenceCheckIdent returns the identifier the if statement's init binds a
// call to fence.Check to — `cerr` in `if cerr := fence.Check(ctx); …` — or ""
// when init has no such shape, including the condition form
// (`if fence.Check(ctx) != nil { … }`), which binds the result to no name at
// all, so there is nothing for the body to swallow by naming something else.
func boundFenceCheckIdent(init ast.Stmt) string {
	assign, ok := init.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return ""
	}
	ident, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return ""
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Check" {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fence" {
		return ""
	}
	return ident.Name
}

// identAppears reports whether an identifier named name occurs anywhere inside
// expr — as the expression itself, or as an argument buried in a wrapping call
// such as fmt.Errorf("…: %w", cerr).
func identAppears(name string, expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// refusalCheckDefect reports why s fails to refuse the chain, or "" when it
// does. s must be an `if` that calls fence.Check in its init or its condition
// and returns from its body.
//
// Where the init binds the check's result to a name (`cerr := fence.Check(ctx)`),
// the return must carry that identifier — directly, or wrapped in a call —
// so a swap for an unrelated value is caught: `_ = fence.Check(ctx)` throws
// the answer away, a body that logs and falls through never returns at all,
// and `if cerr := fence.Check(ctx); cerr != nil { return nil }` calls the
// check, returns from the body, and still discards the refusal.
//
// Where nothing is bound (`if fence.Check(ctx) != nil { … }`), there is no
// name to check the return against, so a returned value cannot be told apart
// from one that only coincidentally looks like a refusal. The condition form
// is therefore accepted only when its body returns no value at all — a bare
// `return`, the shape recordEvent's own fence.Check guard uses
// (engine.go's recordEvent). Any value-carrying return in the condition form
// is rejected: the author must bind the result in the if's init and return
// it, which is what the init-form rule above then verifies.
func refusalCheckDefect(s ast.Stmt) string {
	ifStmt, ok := s.(*ast.IfStmt)
	if !ok {
		return "must be `if cerr := fence.Check(ctx); cerr != nil { return ... }`"
	}
	if !callsFenceCheck(ifStmt.Init) && !callsFenceCheck(ifStmt.Cond) {
		return "must call fence.Check in its init or its condition"
	}
	ident := boundFenceCheckIdent(ifStmt.Init)
	for _, body := range ifStmt.Body.List {
		ret, isReturn := body.(*ast.ReturnStmt)
		if !isReturn {
			continue
		}
		if ident != "" {
			for _, result := range ret.Results {
				if identAppears(ident, result) {
					return ""
				}
			}
			return "must return " + ident + " (directly, or wrapped in a call such as fmt.Errorf(\"…: %w\", " + ident + ")), not discard it"
		}
		if len(ret.Results) == 0 {
			return ""
		}
		return "a condition-form check (fence.Check's result is bound to no name) may only return with no value; bind the result in the if's init instead — `if cerr := fence.Check(ctx); cerr != nil { return cerr }` — and return it"
	}
	return "its body must return"
}

// isRefusalCheck reports whether s refuses the chain: see refusalCheckDefect.
func isRefusalCheck(s ast.Stmt) bool {
	return refusalCheckDefect(s) == ""
}

// scanResumeSites reports how many txgate.Suspend calls src makes and every way
// it fails the rule: a resume() whose next statement does not refuse the chain,
// a function whose Suspend and explicit-resume counts disagree, and a Suspend
// outside any function declaration, which this guard cannot judge.
//
// Counts are attributed to the enclosing function declaration, function
// literals included. Two Suspends and two resumes inside one function is the
// correct shape; the per-resume rule above judges each of them on its own.
func scanResumeSites(filename string, src any) (suspends int, offenders []string, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return 0, nil, err
	}
	at := func(n ast.Node) string {
		return filename + ":" + strconv.Itoa(fset.Position(n.Pos()).Line)
	}

	// resumesIn counts the explicit resume() statements in n and reports the ones
	// not followed by a refusal.
	resumesIn := func(n ast.Node) (count int, bad []string) {
		ast.Inspect(n, func(c ast.Node) bool {
			list, owns := stmtList(c)
			if !owns {
				return true
			}
			for i, stmt := range list {
				if !isResume(stmt) {
					continue
				}
				count++
				if i+1 >= len(list) {
					bad = append(bad, at(stmt)+": the statement after resume() must be `if err := fence.Check(ctx); err != nil { return ... }`")
					continue
				}
				if defect := refusalCheckDefect(list[i+1]); defect != "" {
					bad = append(bad, at(list[i+1])+": the statement after resume() "+defect)
				}
			}
			return true
		})
		return count, bad
	}

	suspends = countSuspends(file)
	inFuncs := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		s := countSuspends(fn)
		inFuncs += s
		r, bad := resumesIn(fn.Body)
		offenders = append(offenders, bad...)
		if s != r {
			offenders = append(offenders, at(fn)+": func "+fn.Name.Name+" makes "+strconv.Itoa(s)+
				" txgate.Suspend call(s) but has "+strconv.Itoa(r)+
				" explicit resume() statement(s); every site re-acquires explicitly so a check can follow it")
		}
	}
	if inFuncs != suspends {
		offenders = append(offenders, filename+": "+strconv.Itoa(suspends-inFuncs)+
			" txgate.Suspend call(s) sit outside any function declaration, where this guard cannot judge them")
	}
	return suspends, offenders, nil
}

func TestEveryResumeIsFollowedByACheck(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var offenders []string
	sites := 0
	// The whole module, on the same terms as txgate's sibling guard: a Suspend
	// site outside internal/ — in app/ or cmd/ — must be checked too. Separate
	// modules cannot import cyoda-go/internal, and vendored or hidden trees are
	// not ours to police.
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch name := info.Name(); {
			case name == "txgate": // resume's own definition
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
		// rel names the file in the message; src is what is parsed, so the
		// test's own working directory is irrelevant.
		n, found, scanErr := scanResumeSites(rel, src)
		sites += n
		offenders = append(offenders, found...)
		return scanErr
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if sites == 0 {
		t.Fatal("no txgate.Suspend site found: the scan looked in the wrong place")
	}
	if len(offenders) > 0 {
		t.Fatalf("after a callout of its own a joined chain re-takes the transaction's lock; it must then refuse a pass that is no longer current:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestResumeSiteScanner_Discriminates proves the guard earns its keep in both
// directions. Every case is a shape someone could plausibly write, and each bad
// one is a way a superseded chain would reach a store.
func TestResumeSiteScanner_Discriminates(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantBad bool
	}{
		{
			name: "explicit resume followed by a refusal",
			src: `package p

func f() error {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if cerr := fence.Check(ctx); cerr != nil {
		return cerr
	}
	return nil
}
`,
		},
		{
			name: "the condition form returning a value is rejected: nothing is bound to tell a real refusal from a coincidence",
			src: `package p

func f() error {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if fence.Check(ctx) != nil {
		return errRefused
	}
	return nil
}
`,
			wantBad: true,
		},
		{
			name: "the condition form returning nil is rejected: the same shape as the swallow above, with nothing bound to catch it",
			src: `package p

func f() error {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if fence.Check(ctx) != nil {
		return nil
	}
	return nil
}
`,
			wantBad: true,
		},
		{
			name: "the condition form with a bare return passes: a void function has nothing to swallow, the shape recordEvent uses",
			src: `package p

func f() {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if fence.Check(ctx) != nil {
		return
	}
	use()
}
`,
		},
		{
			name: "a comment between the resume and the check",
			src: `package p

func f() error {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	// Before the savepoint is looked at.
	if cerr := fence.Check(ctx); cerr != nil {
		return cerr
	}
	return nil
}
`,
		},
		{
			name: "a doc comment mentioning txgate.Suspend( does not count as a site",
			src: `package p

// g releases the gate (txgate.Suspend(ctx)) across the dispatch.
func g() {
	dispatch()
}
`,
		},
		{
			name: "no explicit resume at all: the deferred one re-acquires past every check",
			src: `package p

func f() {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
}
`,
			wantBad: true,
		},
		{
			name: "the refusal is missing",
			src: `package p

func f() {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	use()
}
`,
			wantBad: true,
		},
		{
			name: "the check's result is thrown away",
			src: `package p

func f() {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	_ = fence.Check(ctx)
	use()
}
`,
			wantBad: true,
		},
		{
			name: "the check only logs and falls through",
			src: `package p

func f() {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if cerr := fence.Check(ctx); cerr != nil {
		slog.Warn("superseded", "err", cerr)
	}
	use()
}
`,
			wantBad: true,
		},
		{
			name: "swallows the refusal: returns nil instead of the checked error",
			src: `package p

func f() error {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if cerr := fence.Check(ctx); cerr != nil {
		return nil
	}
	return nil
}
`,
			wantBad: true,
		},
		{
			name: "two checked resumes in one func cannot pay for another func's none",
			src: `package p

func f() error {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
	resume()
	if cerr := fence.Check(ctx); cerr != nil {
		return cerr
	}
	dispatch()
	resume()
	if cerr := fence.Check(ctx); cerr != nil {
		return cerr
	}
	return nil
}

func g() {
	resume := txgate.Suspend(ctx)
	defer resume()
	dispatch()
}
`,
			wantBad: true,
		},
		{
			name: "a Suspend outside any function declaration cannot be judged",
			src: `package p

var resume = txgate.Suspend(ctx)
`,
			wantBad: true,
		},
		{
			name: "a file with no Suspend at all is untouched",
			src: `package p

func f() {
	dispatch()
	other()
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, offenders, err := scanResumeSites("synthetic.go", tc.src)
			if err != nil {
				t.Fatalf("parsing the synthetic source: %v\n%s", err, tc.src)
			}
			if tc.wantBad && len(offenders) == 0 {
				t.Errorf("this shape lets a superseded chain reach a store but the guard passed it:\n%s", tc.src)
			}
			if !tc.wantBad && len(offenders) > 0 {
				t.Errorf("the guard fired on a correct shape (%v):\n%s", offenders, tc.src)
			}
		})
	}
}
