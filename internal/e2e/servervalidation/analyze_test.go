package servervalidation

import (
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"sort"
)

// site is one construct in one function of one file. fn is the enclosing
// function or method's name ("" outside any function); a func literal counts
// as part of the function it is written in.
type site struct {
	file, fn, construct string
}

// analyze reports every construct in files that can serve HTTP without the
// conformance validator, as "file:line: fn: construct", and every allow entry
// that matched nothing. The constructs:
//
//   - httptest.NewServer / NewTLSServer: the handler must be validated(…).
//   - httptest.NewUnstartedServer: the handler must be validated(…), or nil
//     with a .Config.Handler assignment in the same function.
//   - x.Config.Handler = h: h must be validated(…).
//   - http.Server{…}: its Handler must be validated(…); none serves
//     http.DefaultServeMux.
//   - net.Listen and httptest.NewRecorder: a listener can serve anything and
//     a recorder serves a handler in-process, so each must be allowlisted.
func analyze(fset *token.FileSet, files map[string]*ast.File, allow map[site]string) (violations, unused []string) {
	used := map[site]bool{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		for _, decl := range files[name].Decls {
			fn := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			record := func(pos token.Pos, construct string) {
				s := site{name, fn, construct}
				if _, ok := allow[s]; ok {
					used[s] = true
					return
				}
				violations = append(violations, fmt.Sprintf("%s:%d: %s: %s", name, fset.Position(pos).Line, fn, construct))
			}
			assignsHandler := assignsConfigHandler(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					switch pkgFunc(n.Fun) {
					case "httptest.NewServer", "httptest.NewTLSServer":
						if len(n.Args) != 1 || !isValidated(n.Args[0]) {
							record(n.Pos(), pkgFunc(n.Fun))
						}
					case "httptest.NewUnstartedServer":
						switch {
						case len(n.Args) == 1 && isValidated(n.Args[0]):
						case len(n.Args) == 1 && isNil(n.Args[0]) && assignsHandler:
							// The assignment is checked where it stands.
						default:
							record(n.Pos(), "httptest.NewUnstartedServer")
						}
					case "net.Listen", "httptest.NewRecorder":
						record(n.Pos(), pkgFunc(n.Fun))
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						if isConfigHandler(lhs) && (i >= len(n.Rhs) || !isValidated(n.Rhs[i])) {
							record(n.Pos(), ".Config.Handler =")
						}
					}
				case *ast.CompositeLit:
					if pkgFunc(n.Type) == "http.Server" && !hasValidatedHandler(n) {
						record(n.Pos(), "http.Server{}")
					}
				}
				return true
			})
		}
	}

	for s, reason := range allow {
		if !used[s] {
			unused = append(unused, fmt.Sprintf("%s: %s: %s (%s)", s.file, s.fn, s.construct, reason))
		}
	}
	slices.Sort(unused)
	return violations, unused
}

// pkgFunc returns "pkg.Name" for a selector on a package identifier, or "".
func pkgFunc(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

// isValidated reports whether e is a call of validated.
func isValidated(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "validated"
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// isConfigHandler reports whether e is x.Config.Handler.
func isConfigHandler(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Handler" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Config"
}

// assignsConfigHandler reports whether decl assigns any x.Config.Handler.
func assignsConfigHandler(decl ast.Decl) bool {
	found := false
	ast.Inspect(decl, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range as.Lhs {
				if isConfigHandler(lhs) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// hasValidatedHandler reports whether an http.Server literal sets Handler to
// validated(…).
func hasValidatedHandler(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Handler" {
			return isValidated(kv.Value)
		}
	}
	return false
}
