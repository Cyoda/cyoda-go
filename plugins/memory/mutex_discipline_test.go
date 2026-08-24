package memory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTxManagerMutexDiscipline enforces .claude/rules/go-mutex-discipline.md
// on txmanager.go: every Lock()/RLock() is immediately followed by a deferred
// Unlock()/RUnlock(), with early-release sections wrapped in an IIFE so the
// defer still applies. A bare Unlock() leaves the manager mutex held forever
// if anything between the two panics, which in this file would wedge every
// transaction in the process.
//
// The check is structural rather than behavioural because the failure mode is
// panic-unwinding, which has no ordinary code path to exercise.
func TestTxManagerMutexDiscipline(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "txmanager.go", nil, 0)
	if err != nil {
		t.Fatalf("parse txmanager.go: %v", err)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, stmt := range block.List {
			recv, method, ok := lockCall(stmt)
			if !ok {
				continue
			}
			pos := fset.Position(stmt.Pos())
			if i+1 >= len(block.List) {
				t.Errorf("%s: %s.%s() is the last statement in its block — no deferred unlock", pos, recv, method)
				continue
			}
			def, ok := block.List[i+1].(*ast.DeferStmt)
			if !ok {
				t.Errorf("%s: %s.%s() is not followed by a defer — use `defer %s.Unlock()`, or wrap the critical section in an IIFE for early release",
					pos, recv, method, recv)
				continue
			}
			if !deferReleases(def, recv, method) {
				t.Errorf("%s: %s.%s() is followed by a defer that does not release it", pos, recv, method)
			}
		}
		return true
	})
}

// lockCall reports whether stmt is a `<recv>.Lock()` / `<recv>.RLock()` call,
// returning the receiver expression rendered as source and the method name.
func lockCall(stmt ast.Stmt) (recv, method string, ok bool) {
	exprStmt, isExpr := stmt.(*ast.ExprStmt)
	if !isExpr {
		return "", "", false
	}
	call, isCall := exprStmt.X.(*ast.CallExpr)
	if !isCall || len(call.Args) != 0 {
		return "", "", false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	if sel.Sel.Name != "Lock" && sel.Sel.Name != "RLock" {
		return "", "", false
	}
	return exprString(sel.X), sel.Sel.Name, true
}

// deferReleases reports whether def is `defer <recv>.Unlock()` /
// `defer <recv>.RUnlock()`, or a `defer func(){...}()` whose body releases it.
func deferReleases(def *ast.DeferStmt, recv, method string) bool {
	want := "Unlock"
	if method == "RLock" {
		want = "RUnlock"
	}
	if sel, ok := def.Call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == want && exprString(sel.X) == recv
	}
	lit, ok := def.Call.Fun.(*ast.FuncLit)
	if !ok {
		return false
	}
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == want && exprString(sel.X) == recv {
			found = true
		}
		return !found
	})
	return found
}

// exprString renders a selector chain like `m.mu` or `tx.OpMu` as source.
// Anything more complex is rendered as the empty string, which simply means
// two occurrences never compare equal — the check stays conservative.
func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		base := exprString(v.X)
		if base == "" {
			return ""
		}
		return base + "." + v.Sel.Name
	}
	return ""
}
