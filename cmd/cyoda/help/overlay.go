package help

import (
	"fmt"
	"io/fs"
	"sync"
)

// Overlay registry. Plugins register additional help content from their
// init(); the runtime tree (BuildTree) merges the embedded OSS base with every
// registered overlay. The eager DefaultTree remains the OSS-only base, because
// it is computed at package-init time — before any plugin init() has run — and
// is still used by OSS-only consumers and tests.
var (
	overlayMu sync.Mutex
	overlays  []fs.FS

	buildOnce sync.Once
	builtTree *Tree
)

// RegisterOverlay registers an additional help-content source to be merged over
// the embedded OSS base when the runtime tree is built. root must contain a
// top-level "content/" directory of markdown topic files, exactly like the OSS
// embed. Intended to be called from a plugin's init(): overlays merge in
// registration order, after the base, with the documented "later wins"
// collision semantics (see Load). A plugin adding a topic at a fresh path
// simply contributes a new node; overriding an OSS topic requires colliding on
// its path deliberately.
func RegisterOverlay(root fs.FS) {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlays = append(overlays, root)
}

// buildMergedTree merges the embedded OSS content with every registered overlay
// into a single tree. It is the un-memoised builder behind BuildTree.
func buildMergedTree() (*Tree, error) {
	overlayMu.Lock()
	roots := make([]fs.FS, 0, len(overlays)+1)
	roots = append(roots, embeddedContent)
	roots = append(roots, overlays...)
	overlayMu.Unlock()
	return Load(roots...)
}

// BuildTree returns the runtime help tree: the embedded OSS content merged with
// every overlay registered via RegisterOverlay. It builds once, on first call —
// by which point all plugin init() registrations have run — and memoises the
// result. It panics if the merged content is malformed, matching DefaultTree's
// fail-fast contract.
func BuildTree() *Tree {
	buildOnce.Do(func() {
		t, err := buildMergedTree()
		if err != nil {
			panic(fmt.Sprintf("help: failed to build merged tree: %v", err))
		}
		builtTree = t
	})
	return builtTree
}
