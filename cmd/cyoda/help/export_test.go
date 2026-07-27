package help

import (
	"io/fs"
	"sync"
)

// snapshotOverlaysForTest returns a copy of the current overlay registry so a
// test can restore it afterwards.
func snapshotOverlaysForTest() []fs.FS {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	return append([]fs.FS(nil), overlays...)
}

// resetOverlaysForTest clears the overlay registry so a test starts from the
// OSS-only base.
func resetOverlaysForTest() {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlays = nil
}

// restoreOverlaysForTest restores a registry snapshot taken by
// snapshotOverlaysForTest.
func restoreOverlaysForTest(saved []fs.FS) {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlays = saved
}

// resetBuildTreeForTest clears BuildTree's memoisation so the next call
// rebuilds from the current overlay registry. Without this, a BuildTree call in
// one test would memoise a tree for the whole test binary and leak into later
// tests (e.g. one that registers an overlay and then asserts on BuildTree).
func resetBuildTreeForTest() {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	buildOnce = sync.Once{}
	builtTree = nil
}
