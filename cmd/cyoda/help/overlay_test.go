package help

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

// overlayFixture is a minimal, valid single-topic content overlay a plugin
// would embed and register.
func overlayFixture() fstest.MapFS {
	return fstest.MapFS{
		"content/storagenote.md": &fstest.MapFile{Data: []byte(`---
topic: storagenote
title: Storage Note
stability: experimental
---

Cassandra plain reads are a latest-committed dirty read.
`)},
	}
}

// withOverlays saves the global overlay registry, resets it (and BuildTree's
// memoisation), runs fn, and restores the registry — so overlay-registration
// tests neither see nor leak state through the global registry or the memoised
// runtime tree.
func withOverlays(t *testing.T, fn func()) {
	t.Helper()
	saved := snapshotOverlaysForTest()
	resetOverlaysForTest()
	resetBuildTreeForTest()
	defer func() {
		restoreOverlaysForTest(saved)
		resetBuildTreeForTest()
	}()
	fn()
}

// TestRegisterOverlay_TopicAppearsInMergedTree is the core of the bridge: a
// topic registered by a plugin overlay is merged over the OSS base, and the
// OSS topics remain present.
func TestRegisterOverlay_TopicAppearsInMergedTree(t *testing.T) {
	withOverlays(t, func() {
		RegisterOverlay(overlayFixture())

		tree, err := buildMergedTree()
		if err != nil {
			t.Fatalf("buildMergedTree: %v", err)
		}
		if tree.Find([]string{"storagenote"}) == nil {
			t.Error("overlay topic 'storagenote' missing from merged tree")
		}
		if tree.Find([]string{"cli"}) == nil {
			t.Error("OSS topic 'cli' missing from merged tree — base was dropped")
		}
	})
}

// TestRegisterOverlay_RendersViaRunHelp exercises the full CLI render path on
// an overlay topic, proving the bridge surfaces plugin content through the
// same command users invoke.
func TestRegisterOverlay_RendersViaRunHelp(t *testing.T) {
	withOverlays(t, func() {
		RegisterOverlay(overlayFixture())

		tree, err := buildMergedTree()
		if err != nil {
			t.Fatalf("buildMergedTree: %v", err)
		}
		var buf bytes.Buffer
		if rc := RunHelp(tree, []string{"storagenote"}, &buf, "v0.0.0", false, ""); rc != 0 {
			t.Fatalf("RunHelp rc = %d, want 0; output:\n%s", rc, buf.String())
		}
		if !strings.Contains(buf.String(), "dirty read") {
			t.Errorf("rendered overlay topic missing body text; got:\n%s", buf.String())
		}
	})
}

// TestRegisterOverlay_AppearsInJSONOutput exercises the machine-readable
// surfaces named in the acceptance criteria: the overlay topic must appear both
// in the topic-JSON for the topic itself and in the full tree-summary JSON.
func TestRegisterOverlay_AppearsInJSONOutput(t *testing.T) {
	withOverlays(t, func() {
		RegisterOverlay(overlayFixture())

		tree, err := buildMergedTree()
		if err != nil {
			t.Fatalf("buildMergedTree: %v", err)
		}

		var topicJSON bytes.Buffer
		if rc := RunHelp(tree, []string{"storagenote", "--format=json"}, &topicJSON, "v0.0.0", false, ""); rc != 0 {
			t.Fatalf("topic JSON rc = %d, want 0; output:\n%s", rc, topicJSON.String())
		}
		if !strings.Contains(topicJSON.String(), `"storagenote"`) {
			t.Errorf("topic JSON missing overlay topic; got:\n%s", topicJSON.String())
		}

		var treeJSON bytes.Buffer
		if rc := RunHelp(tree, []string{"--format=json"}, &treeJSON, "v0.0.0", false, ""); rc != 0 {
			t.Fatalf("tree-summary JSON rc = %d, want 0; output:\n%s", rc, treeJSON.String())
		}
		if !strings.Contains(treeJSON.String(), "storagenote") {
			t.Errorf("tree-summary JSON missing overlay topic; got:\n%s", treeJSON.String())
		}
	})
}

// TestBuildTree_NoOverlays_HasOSSTopics confirms the merged runtime tree equals
// the OSS base when no plugin registers an overlay (the default binary).
func TestBuildTree_NoOverlays_HasOSSTopics(t *testing.T) {
	withOverlays(t, func() {
		tree, err := buildMergedTree()
		if err != nil {
			t.Fatalf("buildMergedTree: %v", err)
		}
		if tree.Find([]string{"cli"}) == nil {
			t.Error("OSS topic 'cli' missing from no-overlay merged tree")
		}
		if tree.Find([]string{"storagenote"}) != nil {
			t.Error("unexpected overlay topic present with no overlay registered")
		}
	})
}

// TestRegisterOverlay_MalformedContentErrors confirms overlay content is held
// to the same front-matter contract as the OSS base: a malformed overlay fails
// tree construction rather than being silently skipped (BuildTree turns this
// error into a fail-fast panic).
func TestRegisterOverlay_MalformedContentErrors(t *testing.T) {
	withOverlays(t, func() {
		RegisterOverlay(fstest.MapFS{
			"content/bad.md": &fstest.MapFile{Data: []byte("no front-matter here\n")},
		})
		if _, err := buildMergedTree(); err == nil {
			t.Error("buildMergedTree accepted an overlay with missing front-matter; want error")
		}
	})
}

// TestBuildTree_Memoized verifies the runtime accessor builds the tree once and
// returns the same instance on subsequent calls.
func TestBuildTree_Memoized(t *testing.T) {
	resetBuildTreeForTest()
	defer resetBuildTreeForTest()
	if a, b := BuildTree(), BuildTree(); a != b {
		t.Errorf("BuildTree returned distinct instances %p vs %p; expected memoized", a, b)
	}
}
