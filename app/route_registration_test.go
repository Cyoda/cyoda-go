package app_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// muxRegistrars are the calls app.go may hand one of its muxes to, other than
// Handle and HandleFunc. None of them registers an authenticated route: health,
// discovery and help are public; cluster dispatch is AEAD-authenticated;
// StripPrefix mounts the main mux under the context path.
var muxRegistrars = map[string]bool{
	"internalapi.RegisterHealthRoutes":    true,
	"internalapi.RegisterDiscoveryRoutes": true,
	"internalapi.RegisterHelpRoutes":      true,
	"dispatchHandler.Register":            true,
	"http.StripPrefix":                    true,
}

// publicMuxPatterns are the unauthenticated routes app.go registers itself:
// JWKS discovery and the token endpoint.
var publicMuxPatterns = map[string]bool{"/.well-known/": true, "/tenants/{tenant}/oauth/token": true}

// tokenFreeTenantRoutes are the routes of the tenant route group. The group
// holds token-free routes only.
var tokenFreeTenantRoutes = map[string]bool{"/tenants/{tenant}/oauth/token": true}

// catchAllMuxPattern mounts the generated router, whose routes
// TestRouteClassification_EveryRouteClassified classifies one by one.
const catchAllMuxPattern = "/"

type muxRegistration struct {
	pattern string
	handler ast.Expr
	pos     token.Position
}

// TestRouteRegistration_EveryMuxRouteClassified parses app.go and requires
// every route it registers on its own muxes to be classified: public,
// operator (/admin), data, or the catch-all of the generated router. A data
// route's handler must call RequireM2M. A pattern that is not a string
// literal, a mux handed to an unknown function, or an unclassified pattern
// fails, so a new route cannot be added without being classified here.
func TestRouteRegistration_EveryMuxRouteClassified(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "app.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	funcLits := localFuncLits(file)
	regs := collectMuxRegistrations(t, fset, file)

	data := map[string]bool{}
	for _, rt := range handRegisteredDataRoutes {
		data[rt[0]+" "+rt[1]] = true
	}
	operator := map[string]bool{}
	for _, rt := range handRegisteredOperatorRoutes {
		operator[rt[0]+" "+rt[1]] = true
	}
	for p := range publicMuxPatterns {
		if data[p] || operator[p] || p == catchAllMuxPattern {
			t.Fatalf("pattern %q is in more than one class", p)
		}
	}
	for p := range data {
		if operator[p] {
			t.Fatalf("pattern %q is in more than one class", p)
		}
	}

	seen := map[string]bool{}
	for _, r := range regs {
		seen[r.pattern] = true
		// The tenant route group holds token-free routes only. A route that
		// carries a bearer token needs the group's tenant-equality check
		// (internal/tenantroute) built first; list it here only then.
		if strings.HasPrefix(r.pattern, "/tenants/") && !tokenFreeTenantRoutes[r.pattern] {
			t.Errorf("%s: %q is in the tenant group but not a known token-free route", r.pos, r.pattern)
		}
		switch {
		case publicMuxPatterns[r.pattern], operator[r.pattern], r.pattern == catchAllMuxPattern:
		case data[r.pattern]:
			if !callsRequireM2M(r.handler, funcLits, map[string]bool{}) {
				t.Errorf("%s: data route %q: its handler does not call RequireM2M", r.pos, r.pattern)
			}
		default:
			t.Errorf("%s: route %q is not classified: add it to the public, operator (/admin) or data set in app/route_classification_test.go or app/route_registration_test.go (a data route must be wrapped with internalapi.RequireM2M)", r.pos, r.pattern)
		}
	}
	for _, set := range []map[string]bool{publicMuxPatterns, operator, data, {catchAllMuxPattern: true}} {
		for p := range set {
			if !seen[p] {
				t.Errorf("classified pattern %q is not registered in app.go: remove the stale entry", p)
			}
		}
	}
}

// collectMuxRegistrations returns every Handle/HandleFunc call on mux in
// app.go. It fails the test on a pattern that is not a string literal, on a
// Handle/HandleFunc call on any other receiver except the context-path mount,
// and on a mux handed to a function not in muxRegistrars.
func collectMuxRegistrations(t *testing.T, fset *token.FileSet, file *ast.File) []muxRegistration {
	t.Helper()
	var regs []muxRegistration
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		pos := fset.Position(call.Pos())
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") {
			recv := exprString(fset, sel.X)
			switch {
			case recv == "mux" && len(call.Args) == 2:
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: mux.%s pattern %s is not a string literal; the route cannot be classified", pos, sel.Sel.Name, exprString(fset, call.Args[0]))
					return true
				}
				p, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", pos, err)
				}
				regs = append(regs, muxRegistration{pattern: p, handler: call.Args[1], pos: pos})
			case recv == "outerMux" && len(call.Args) == 2 &&
				exprString(fset, call.Args[0]) == `contextPath + "/"` &&
				exprString(fset, call.Args[1]) == "http.StripPrefix(contextPath, mux)":
				// The context-path mount of the main mux, classified above.
			default:
				t.Errorf("%s: %s.%s(%s) is not a classifiable registration", pos, recv, sel.Sel.Name, exprString(fset, call))
			}
			return true
		}
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok && (id.Name == "mux" || id.Name == "outerMux") {
				if name := exprString(fset, call.Fun); !muxRegistrars[name] {
					t.Errorf("%s: %s receives %s; registering routes through it is not classified", pos, name, id.Name)
				}
			}
		}
		return true
	})
	return regs
}

// localFuncLits maps each name app.go binds to a function literal
// (`name := func(...) ... { ... }`) to that literal.
func localFuncLits(file *ast.File) map[string]*ast.FuncLit {
	out := map[string]*ast.FuncLit{}
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if fl, ok := as.Rhs[i].(*ast.FuncLit); ok {
				out[id.Name] = fl
			}
		}
		return true
	})
	return out
}

// callsRequireM2M reports whether expr calls internalapi.RequireM2M, directly
// or through a local function literal it calls.
func callsRequireM2M(expr ast.Node, funcLits map[string]*ast.FuncLit, visiting map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && x.Name == "internalapi" && fn.Sel.Name == "RequireM2M" {
				found = true
			}
		case *ast.Ident:
			if fl, ok := funcLits[fn.Name]; ok && !visiting[fn.Name] {
				visiting[fn.Name] = true
				if callsRequireM2M(fl.Body, funcLits, visiting) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func exprString(fset *token.FileSet, n ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, n); err != nil {
		return "?"
	}
	return b.String()
}
