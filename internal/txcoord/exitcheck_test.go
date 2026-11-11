package txcoord

// exitcheck_test.go — the gate and the fence are txcoord's own. Every other
// package reaches a transaction's coordination through txcoord, so that one
// implementation decides admission, marks and the cut-off before commit. The
// one exception is the pass token (internal/cluster/token), which encodes the
// pairs a pass carries (fence.Pair).
//
// The scan reads the import lists of every non-test .go file of the root
// module; test files may build a gate or a fence of their own.

import (
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

func TestOnlyTxcoordUsesGateAndFence(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var offenders []string
	seen := map[string]int{} // leaf -> allowed imports found
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
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			ipath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
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
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for leaf := range leafImporters {
		if seen[leaf] == 0 {
			t.Errorf("no allowed import of %s found: the scan looked in the wrong place", leaf)
		}
	}
	if len(offenders) > 0 {
		slices.Sort(offenders)
		t.Fatalf("only txcoord uses the gate, and only txcoord and the pass token use the fence:\n%s", strings.Join(offenders, "\n"))
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
