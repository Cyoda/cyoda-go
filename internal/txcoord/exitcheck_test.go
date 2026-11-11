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
			// plugins/* are modules of their own; dot directories and
			// testdata hold no package of this module.
			if name := info.Name(); path != root && (strings.HasPrefix(name, ".") || name == "plugins" || name == "vendor" || name == "testdata") {
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
