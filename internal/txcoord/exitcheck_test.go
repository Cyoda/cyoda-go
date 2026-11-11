package txcoord

// exitcheck_test.go — the gate and the fence are txcoord's own. Every other
// package reaches a transaction's coordination through txcoord, so that one
// implementation decides admission, marks and the cut-off before commit. The
// one exception is the pass token (internal/cluster/token), which encodes the
// pairs a pass carries (fence.Pair).
//
// The scan reads the import lists of every non-test .go file of the root
// module; test files may build a gate or a fence of their own. The same scan
// holds the transaction-coordination test double (txcoordtest) out of
// production code: only test files import it.
//
// A second scan holds savepoints to txcoord: a scope's savepoint is what its
// end releases or rolls back, so no other package takes one. The tracing
// wrapper of the transaction manager (internal/observability) passes the call
// through and is the one exception.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/cyoda-platform/cyoda-go"

// testDoublePath is the test double of the optional SPI capability: no
// production file imports it.
const testDoublePath = modulePath + "/internal/txcoord/txcoordtest"

// leafImporters is, for each leaf package, the packages that may import it.
var leafImporters = map[string][]string{
	modulePath + "/internal/txgate": {modulePath + "/internal/txcoord"},
	modulePath + "/internal/fence":  {modulePath + "/internal/txcoord", modulePath + "/internal/cluster/token"},
}

// skipDir reports whether the directory rel (slash-separated, relative to the
// module root) holds no package of the root module: plugins/ (each plugin is a
// module of its own) and vendor/, both at the root only, and at any depth the
// directories the go tool ignores — testdata, and names starting with a dot or
// an underscore.
func skipDir(rel string) bool {
	if rel == "." {
		return false
	}
	for _, top := range []string{"plugins", "vendor"} {
		if rel == top || strings.HasPrefix(rel, top+"/") {
			return true
		}
	}
	name := rel[strings.LastIndex(rel, "/")+1:]
	return name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// walkProduction calls fn for every non-test .go file of the root module, with
// its path relative to the module root and its package's import path.
func walkProduction(t *testing.T, fn func(path, rel, pkg string) error) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if skipDir(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		pkg := modulePath
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
			pkg += "/" + dir
		}
		return fn(path, rel, pkg)
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestOnlyTxcoordUsesGateAndFence(t *testing.T) {
	var offenders []string
	seen := map[string]int{} // leaf -> allowed imports found
	walkProduction(t, func(path, rel, pkg string) error {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			ipath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if ipath == testDoublePath {
				offenders = append(offenders, rel+" imports the test double "+ipath)
				continue
			}
			allowed, leaf := leafImporters[ipath]
			if !leaf {
				continue
			}
			if !slices.Contains(allowed, pkg) {
				offenders = append(offenders, rel+" imports "+ipath)
				continue
			}
			seen[ipath]++
		}
		return nil
	})
	for leaf := range leafImporters {
		if seen[leaf] == 0 {
			t.Errorf("no allowed import of %s found: the scan looked in the wrong place", leaf)
		}
	}
	if len(offenders) > 0 {
		slices.Sort(offenders)
		t.Fatalf("only txcoord uses the gate, only txcoord and the pass token use the fence, and only tests use the test double:\n%s", strings.Join(offenders, "\n"))
	}
}

// savepointCallers are the packages, besides txcoord and its test double,
// that may call a Savepoint method.
var savepointCallers = []string{modulePath + "/internal/observability"}

func TestOnlyTxcoordTakesSavepoints(t *testing.T) {
	var offenders []string
	allowed := 0 // calls found where they are allowed
	walkProduction(t, func(path, rel, pkg string) error {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Savepoint" {
				return true
			}
			txcoord := modulePath + "/internal/txcoord"
			if pkg == txcoord || strings.HasPrefix(pkg, txcoord+"/") || slices.Contains(savepointCallers, pkg) {
				allowed++
				return true
			}
			offenders = append(offenders, rel+" calls Savepoint")
			return true
		})
		return nil
	})
	if allowed == 0 {
		t.Error("no allowed Savepoint call found: the scan looked in the wrong place")
	}
	if len(offenders) > 0 {
		slices.Sort(offenders)
		t.Fatalf("only txcoord takes a savepoint (the tracing wrapper passes the call through):\n%s", strings.Join(offenders, "\n"))
	}
}

// TestSkipDir: only what holds no package of the root module is skipped. A
// package named plugins, vendor or testdata below the root is still scanned
// only if Go would build it — testdata and dot or underscore directories never
// are, at any depth.
func TestSkipDir(t *testing.T) {
	for rel, want := range map[string]bool{
		".":                        false,
		"plugins":                  true,
		"plugins/postgres":         true,
		"vendor":                   true,
		".git":                     true,
		"internal/domain/testdata": true,
		"internal/x/_scratch":      true,
		"internal/domain/plugins":  false,
		"internal/cluster/vendor":  false,
		"internal/txcoord":         false,
	} {
		if got := skipDir(rel); got != want {
			t.Errorf("skipDir(%q) = %t; want %t", rel, got, want)
		}
	}
}
