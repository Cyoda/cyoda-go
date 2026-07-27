package help

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

func testTree(t *testing.T) *Tree {
	fsys := fstest.MapFS{
		"content/cli.md": &fstest.MapFile{Data: []byte(`---
topic: cli
title: cyoda CLI
stability: stable
---

# cli

## DESCRIPTION

Operate the binary.
`)},
		"content/cli/serve.md": &fstest.MapFile{Data: []byte(`---
topic: cli.serve
title: cli serve
stability: stable
---

# serve

## DESCRIPTION

Serve API.
`)},
		"content/config.md": &fstest.MapFile{Data: []byte(`---
topic: config
title: config
stability: stable
---

# config

Body.
`)},
		"content/openapi.md": &fstest.MapFile{Data: []byte(`---
topic: openapi
title: openapi
stability: stable
---

# openapi

OpenAPI spec.
`)},
		"content/grpc.md": &fstest.MapFile{Data: []byte(`---
topic: grpc
title: grpc
stability: stable
---

# grpc

gRPC service.
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return tree
}

func TestRunHelp_NoArgs_ShowsTopics(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "cli") || !strings.Contains(s, "config") {
		t.Errorf("top-level summary missing topics: %q", s)
	}
	// Summary must now include USAGE and FLAGS headings.
	if !strings.Contains(s, "USAGE") {
		t.Errorf("top-level summary missing USAGE heading: %q", s)
	}
	if !strings.Contains(s, "FLAGS") {
		t.Errorf("top-level summary missing FLAGS heading: %q", s)
	}
}

func TestRunHelp_NoArgs_IncludesUsageAndFlags(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	for _, want := range []string{"USAGE", "FLAGS", "TOPICS", "--format", "--version"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}

func TestRunHelp_TopicLookup(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{"cli"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "Operate the binary.") {
		t.Errorf("cli body missing: %q", out.String())
	}
}

func TestRunHelp_Subtopic(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{"cli", "serve"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "Serve API.") {
		t.Errorf("cli serve body missing: %q", out.String())
	}
}

func TestRunHelp_UnknownTopic_Exit2(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{"widgetry"}, &out, "0.6.1", false, "")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "widgetry") {
		t.Errorf("error should name the topic: %q", out.String())
	}
}

func TestRunHelp_FormatJSON(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{"--format=json"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, `"schema": 1`) && !strings.Contains(s, `"schema":1`) {
		t.Errorf("json full-tree output missing schema field: %q", s)
	}
	if !strings.Contains(s, `"version": "0.6.1"`) && !strings.Contains(s, `"version":"0.6.1"`) {
		t.Errorf("json full-tree output missing version field: %q", s)
	}
}

func TestRunHelp_FormatJSONSingleTopic(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{"--format=json", "cli"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, `"topic": "cli"`) && !strings.Contains(s, `"topic":"cli"`) {
		t.Errorf("single-topic json malformed: %q", s)
	}
	// Single topic should NOT include the HelpPayload wrapper fields.
	if strings.Contains(s, `"topics":[`) || strings.Contains(s, `"topics": [`) {
		t.Errorf("single-topic output should not include wrapper: %q", s)
	}
}

func TestRunHelp_UnknownFormat_Exit2(t *testing.T) {
	var out bytes.Buffer
	code := RunHelp(testTree(t), []string{"--format=bogus"}, &out, "0.6.1", false, "")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "bogus") {
		t.Errorf("error must name the bad format: %q", out.String())
	}
}

func TestRunHelp_NoDuplicateSeeAlso(t *testing.T) {
	// Build a small tree with a topic whose body includes "## SEE ALSO"
	// and whose front-matter see_also is set.
	fsys := fstest.MapFS{
		"content/x.md": &fstest.MapFile{Data: []byte(`---
topic: x
title: x
stability: stable
see_also:
  - y
---

# x

body text

## SEE ALSO

- body-y
- body-z
`)},
		"content/y.md": &fstest.MapFile{Data: []byte(`---
topic: y
title: y
stability: stable
---

# y

body
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	// isTTY=true forces text mode, where the duplicate used to appear.
	code := RunHelp(tree, []string{"x"}, &out, "0.6.1", true, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	// Count occurrences of "SEE ALSO" — should be exactly one.
	seeAlsoCount := strings.Count(s, "SEE ALSO")
	if seeAlsoCount != 1 {
		t.Errorf("SEE ALSO appears %d times, want 1:\n%s", seeAlsoCount, s)
	}
	// Body's see-also content ("body-y", "body-z") must not appear.
	if strings.Contains(s, "body-y") || strings.Contains(s, "body-z") {
		t.Errorf("body-level see_also must be stripped, but appeared:\n%s", s)
	}
	// Front-matter's see_also must appear.
	if !strings.Contains(s, "y") {
		t.Errorf("front-matter see_also ('y') missing:\n%s", s)
	}
}

func TestRunHelp_SeeAlsoUsesCLISyntax(t *testing.T) {
	fsys := fstest.MapFS{
		"content/a.md": &fstest.MapFile{Data: []byte(`---
topic: a
title: a
stability: stable
see_also:
  - errors.VALIDATION_FAILED
---

# a
`)},
		"content/errors.md": &fstest.MapFile{Data: []byte(`---
topic: errors
title: errors
stability: stable
---

# errors
`)},
		"content/errors/VALIDATION_FAILED.md": &fstest.MapFile{Data: []byte(`---
topic: errors.VALIDATION_FAILED
title: vf
stability: stable
---

# vf
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	// isTTY=true forces text mode where CLI-syntax bullets are required.
	code := RunHelp(tree, []string{"a"}, &out, "0.6.1", true, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if strings.Contains(s, "errors.VALIDATION_FAILED") {
		t.Errorf("SEE ALSO must show space-separated form, not dotted:\n%s", s)
	}
	if !strings.Contains(s, "errors VALIDATION_FAILED") {
		t.Errorf("SEE ALSO must contain 'errors VALIDATION_FAILED':\n%s", s)
	}
}

func TestRunHelp_OpenAPIJSONAction(t *testing.T) {
	tree := testTree(t) // includes openapi topic

	var out bytes.Buffer
	code := RunHelp(tree, []string{"openapi", "json"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d, output = %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, `"openapi"`) {
		t.Errorf("output should contain openapi JSON field: %q", s[:min(200, len(s))])
	}
}

func TestRunHelp_UnknownActionOnKnownTopic(t *testing.T) {
	tree := testTree(t) // includes openapi topic

	var out bytes.Buffer
	code := RunHelp(tree, []string{"openapi", "xml"}, &out, "0.6.1", false, "")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	s := out.String()
	if !strings.Contains(s, "unknown action") || !strings.Contains(s, "xml") {
		t.Errorf("error should name the bad action: %q", s)
	}
	if !strings.Contains(s, "json") || !strings.Contains(s, "yaml") {
		t.Errorf("error should list available actions: %q", s)
	}
}

func TestWriteTopicText_ShowsActionsFooter(t *testing.T) {
	// Use an action-registered topic (openapi) in testTree.
	tree := testTree(t)
	var out bytes.Buffer
	code := RunHelp(tree, []string{"openapi"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "ACTIONS") {
		t.Errorf("output missing ACTIONS footer: %q", s)
	}
	if !strings.Contains(s, "cyoda help openapi json") ||
		!strings.Contains(s, "cyoda help openapi yaml") {
		t.Errorf("footer missing action commands: %q", s)
	}
}

func TestWriteTopicText_NoActionsFooterWhenNone(t *testing.T) {
	// cli has no registered actions
	tree := testTree(t)
	var out bytes.Buffer
	code := RunHelp(tree, []string{"cli"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// "ACTIONS" may appear in authored content; check for the command lines instead
	if strings.Contains(out.String(), "cyoda help cli json") {
		t.Errorf("cli should have no actions footer; output: %q", out.String())
	}
}

func TestWriteTreeSummary_ListsTopicsWithActions(t *testing.T) {
	tree := testTree(t)
	var out bytes.Buffer
	code := RunHelp(tree, nil, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "TOPIC ACTIONS") {
		t.Errorf("summary missing TOPIC ACTIONS block: %q", s)
	}
	if !strings.Contains(s, "cyoda help openapi json|tags|yaml") {
		t.Errorf("summary missing openapi actions: %q", s)
	}
	if !strings.Contains(s, "cyoda help grpc") {
		t.Errorf("summary missing grpc actions: %q", s)
	}
	// actions are sorted alphabetically: json before proto
	if !strings.Contains(s, "cyoda help grpc json|proto") {
		t.Errorf("summary missing grpc json|proto actions: %q", s)
	}
}

func TestDescriptor_IncludesActions(t *testing.T) {
	tree := testTree(t)
	t2 := tree.Find([]string{"openapi"})
	if t2 == nil {
		t.Skip("openapi topic not in testTree")
	}
	d := t2.Descriptor()
	wantSet := map[string]bool{"json": true, "yaml": true}
	got := map[string]bool{}
	for _, a := range d.Actions {
		got[a] = true
	}
	for k := range wantSet {
		if !got[k] {
			t.Errorf("descriptor missing action %q; got %v", k, d.Actions)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestWriteTopicText_ShowsSubtopicsFooter(t *testing.T) {
	// Build a tree with a parent + 2 children.
	fsys := fstest.MapFS{
		"content/x.md": &fstest.MapFile{Data: []byte(`---
topic: x
title: x
stability: stable
---

# x

body
`)},
		"content/x/child-a.md": &fstest.MapFile{Data: []byte(`---
topic: x.child-a
title: a
stability: stable
---

# a
`)},
		"content/x/child-b.md": &fstest.MapFile{Data: []byte(`---
topic: x.child-b
title: b
stability: stable
---

# b
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	code := RunHelp(tree, []string{"x"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "SUBTOPICS") {
		t.Errorf("output missing SUBTOPICS footer: %q", s)
	}
	if !strings.Contains(s, "cyoda help x child-a") {
		t.Errorf("footer missing child-a: %q", s)
	}
	if !strings.Contains(s, "cyoda help x child-b") {
		t.Errorf("footer missing child-b: %q", s)
	}
}

func TestWriteTopicText_NoSubtopicsFooterWhenLeaf(t *testing.T) {
	// A leaf topic (no children) should NOT emit SUBTOPICS.
	fsys := fstest.MapFS{
		"content/leaf.md": &fstest.MapFile{Data: []byte(`---
topic: leaf
title: leaf
stability: stable
---

# leaf

body
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	code := RunHelp(tree, []string{"leaf"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out.String(), "SUBTOPICS") {
		t.Errorf("leaf topic should not have SUBTOPICS footer; got: %q", out.String())
	}
}

func TestWriteTopicMarkdown_ShowsSubtopicsSection(t *testing.T) {
	fsys := fstest.MapFS{
		"content/x.md": &fstest.MapFile{Data: []byte(`---
topic: x
title: x
stability: stable
---

# x

body
`)},
		"content/x/child.md": &fstest.MapFile{Data: []byte(`---
topic: x.child
title: c
stability: stable
---

# c
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	code := RunHelp(tree, []string{"--format=markdown", "x"}, &out, "0.6.1", false, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "## SUBTOPICS") {
		t.Errorf("markdown output missing ## SUBTOPICS: %q", s)
	}
	if !strings.Contains(s, "`cyoda help x child`") {
		t.Errorf("markdown subtopic entry missing: %q", s)
	}
}

func TestRunHelp_ConfigAll_UnsupportedFormat(t *testing.T) {
	// config all supports only text (default) and json. markdown/yaml are
	// valid help formats generally but meaningless for this flat listing —
	// reject them rather than silently returning the text table.
	for _, f := range []string{"markdown", "yaml"} {
		var buf bytes.Buffer
		rc := RunHelp(DefaultTree, []string{"config", "all", "--format=" + f}, &buf, "v0.0.0", false, "")
		if rc != 2 {
			t.Errorf("config all --format=%s: rc=%d, want 2", f, rc)
		}
		if !strings.Contains(buf.String(), "text or json") {
			t.Errorf("config all --format=%s: missing guidance, got %q", f, buf.String())
		}
	}
}

func TestRunHelp_ConfigAll(t *testing.T) {
	var text bytes.Buffer
	if rc := RunHelp(DefaultTree, []string{"config", "all"}, &text, "v0.0.0", false, ""); rc != 0 {
		t.Fatalf("text rc=%d", rc)
	}
	if !strings.Contains(text.String(), "CYODA_HTTP_PORT") {
		t.Error("config all (text) missing vars")
	}
	if json.Valid(text.Bytes()) {
		t.Error("config all (no --format) should emit the text table, not JSON")
	}
	if !strings.Contains(text.String(), "[server]") {
		t.Error("config all (text) missing topic group header")
	}
	var js bytes.Buffer
	if rc := RunHelp(DefaultTree, []string{"config", "all", "--format=json"}, &js, "v0.0.0", false, ""); rc != 0 {
		t.Fatalf("json rc=%d", rc)
	}
	if !json.Valid(js.Bytes()) {
		t.Error("config all --format=json not valid JSON")
	}
	// CLI --format=json and the HTTP action must emit identical bytes — same
	// registry, same self-reported version — so the resource doesn't differ
	// by entry point.
	var action bytes.Buffer
	writeConfigAllJSON(&action)
	if js.String() != action.String() {
		t.Error("config all --format=json (CLI) differs from the HTTP action output; must be identical")
	}
}
