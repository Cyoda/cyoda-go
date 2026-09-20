package fence

// check_call_sites_test.go — after a callout of its own a joined chain re-takes
// the transaction's lock, and the statement that follows the re-acquire must be
// the fence's check. Otherwise the chain carries on writing to a transaction the
// owner has already given to another compute node.
//
// The guard is the companion of internal/txgate's suspend_call_sites_test.go:
// that one insists every Suspend defers its resume, this one insists every
// Suspend ALSO re-acquires explicitly and checks straight afterwards. A sixth
// Suspend site therefore cannot be added without its check.
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

// uncheckedResumes reports every statement `resume()` in src whose next
// statement does not call fence.Check, and how many explicit `resume()`
// statements the file has at all — a site that only defers its resume never
// reaches a check, so the count is checked against the number of Suspend sites.
func uncheckedResumes(filename string, src any) (offenders []string, explicit int, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, 0, err
	}
	callsCheck := func(n ast.Node) bool {
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
	// isResume matches the bare `resume()` statement, not `defer resume()`:
	// a DeferStmt is not an ExprStmt.
	isResume := func(s ast.Stmt) bool {
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
	ast.Inspect(file, func(n ast.Node) bool {
		var list []ast.Stmt
		switch s := n.(type) {
		case *ast.BlockStmt:
			list = s.List
		case *ast.CaseClause:
			list = s.Body
		case *ast.CommClause:
			list = s.Body
		default:
			return true
		}
		for i, stmt := range list {
			if !isResume(stmt) {
				continue
			}
			explicit++
			if i+1 >= len(list) || !callsCheck(list[i+1]) {
				p := fset.Position(stmt.Pos())
				offenders = append(offenders, filename+":"+strconv.Itoa(p.Line))
			}
		}
		return true
	})
	return offenders, explicit, nil
}

func TestEveryResumeIsFollowedByACheck(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var offenders []string
	sites, explicit := 0, 0
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
		sites += strings.Count(string(src), "txgate.Suspend(")
		// rel names the file in the message; src is what is parsed, so the
		// test's own working directory is irrelevant.
		found, n, scanErr := uncheckedResumes(rel, src)
		offenders = append(offenders, found...)
		explicit += n
		return scanErr
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if sites == 0 {
		t.Fatal("no txgate.Suspend site found: the scan looked in the wrong place")
	}
	if len(offenders) > 0 {
		t.Fatalf("after a callout of its own a joined chain re-takes the transaction's lock; the statement after resume() must be the fence's check:\n%s", strings.Join(offenders, "\n"))
	}
	// A site that only defers its resume re-acquires the lock as the frame
	// unwinds, past every check: the explicit call is what a check can follow.
	if explicit != sites {
		t.Fatalf("%d txgate.Suspend sites but %d explicit resume() statements; every site re-acquires explicitly so a check can follow it", sites, explicit)
	}
}

func TestUncheckedResumes_Discriminates(t *testing.T) {
	bad := "package p\n\nfunc f() {\n\tresume := s()\n\tdefer resume()\n\td()\n\tresume()\n\tuse()\n}\n"
	good := "package p\n\nfunc f() error {\n\tresume := s()\n\tdefer resume()\n\td()\n\tresume()\n\tif err := fence.Check(ctx); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n"
	deferredOnly := "package p\n\nfunc f() {\n\tresume := s()\n\tdefer resume()\n\td()\n}\n"
	if got, n, _ := uncheckedResumes("bad.go", bad); len(got) != 1 || n != 1 {
		t.Fatalf("bad: offenders %v, explicit %d; want 1 and 1", got, n)
	}
	if got, n, _ := uncheckedResumes("good.go", good); len(got) != 0 || n != 1 {
		t.Fatalf("good: offenders %v, explicit %d; want none and 1", got, n)
	}
	if got, n, _ := uncheckedResumes("deferred_only.go", deferredOnly); len(got) != 0 || n != 0 {
		t.Fatalf("deferred only: offenders %v, explicit %d; want none and 0 — the count is what catches it", got, n)
	}
}
