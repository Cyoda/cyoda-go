package help

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help/renderer"
)

func TestParseFrontMatter_ValidMinimal(t *testing.T) {
	src := []byte(`---
topic: cli
title: "cyoda CLI — subcommand reference"
stability: stable
---

# cli

NAME section follows here.
`)
	fm, body, err := parseFrontMatter(src)
	if err != nil {
		t.Fatalf("parseFrontMatter: %v", err)
	}
	if fm.Topic != "cli" {
		t.Errorf("topic = %q, want %q", fm.Topic, "cli")
	}
	if fm.Stability != "stable" {
		t.Errorf("stability = %q, want %q", fm.Stability, "stable")
	}
	if !strings.HasPrefix(string(body), "# cli") {
		t.Errorf("body must start with '# cli'; got %q", body[:min(20, len(body))])
	}
}

func TestParseFrontMatter_RejectsMissingTopic(t *testing.T) {
	src := []byte(`---
title: "missing topic"
stability: stable
---

body
`)
	_, _, err := parseFrontMatter(src)
	if err == nil {
		t.Fatal("parseFrontMatter must reject missing topic field")
	}
	if !strings.Contains(err.Error(), "topic") {
		t.Errorf("error must mention 'topic': %v", err)
	}
}

func TestParseFrontMatter_RejectsMissingTitle(t *testing.T) {
	src := []byte(`---
topic: cli
stability: stable
---

body
`)
	_, _, err := parseFrontMatter(src)
	if err == nil {
		t.Fatal("parseFrontMatter must reject missing title field")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error must mention 'title': %v", err)
	}
}

func TestParseFrontMatter_RejectsInvalidStability(t *testing.T) {
	src := []byte(`---
topic: cli
title: x
stability: bogus
---

body
`)
	_, _, err := parseFrontMatter(src)
	if err == nil {
		t.Fatal("parseFrontMatter must reject unknown stability")
	}
	if !strings.Contains(err.Error(), "stability") {
		t.Errorf("error must mention 'stability': %v", err)
	}
}

func TestParseFrontMatter_ParsesSeeAlso(t *testing.T) {
	src := []byte(`---
topic: cli
title: x
stability: stable
see_also:
  - config
  - run
---
`)
	fm, _, err := parseFrontMatter(src)
	if err != nil {
		t.Fatalf("parseFrontMatter: %v", err)
	}
	want := []string{"config", "run"}
	if !reflect.DeepEqual(fm.SeeAlso, want) {
		t.Errorf("see_also = %v, want %v", fm.SeeAlso, want)
	}
}

func TestLoad_SingleFS(t *testing.T) {
	fsys := fstest.MapFS{
		"content/cli.md": &fstest.MapFile{Data: []byte(`---
topic: cli
title: "cli reference"
stability: stable
---

# cli

Body.
`)},
		"content/cli/serve.md": &fstest.MapFile{Data: []byte(`---
topic: cli.serve
title: "cli serve"
stability: stable
---

# serve

Serve body.
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cli := tree.Find([]string{"cli"})
	if cli == nil {
		t.Fatal("cli topic missing")
	}
	if cli.Title != "cli reference" {
		t.Errorf("cli.Title = %q", cli.Title)
	}
	if len(cli.Children) != 1 {
		t.Fatalf("cli.Children = %d, want 1", len(cli.Children))
	}
	serve := tree.Find([]string{"cli", "serve"})
	if serve == nil {
		t.Fatal("cli.serve missing")
	}
	if serve.Title != "cli serve" {
		t.Errorf("serve.Title = %q", serve.Title)
	}
}

func TestLoad_PathMismatch_IsError(t *testing.T) {
	fsys := fstest.MapFS{
		"content/cli.md": &fstest.MapFile{Data: []byte(`---
topic: wrong
title: x
stability: stable
---
body
`)},
	}
	_, err := Load(fsys)
	if err == nil {
		t.Fatal("Load must error when front-matter topic doesn't match filesystem path")
	}
}

func TestLoad_MissingFrontMatter_IsError(t *testing.T) {
	fsys := fstest.MapFS{
		"content/cli.md": &fstest.MapFile{Data: []byte("no front-matter here")},
	}
	_, err := Load(fsys)
	if err == nil {
		t.Fatal("Load must error on missing front-matter")
	}
}

func TestTreeFind_NilRoot(t *testing.T) {
	tree := &Tree{}
	if got := tree.Find([]string{"anything"}); got != nil {
		t.Errorf("Find on nil Root must return nil; got %v", got)
	}
}

func TestTreeFind_EmptyPathReturnsRoot(t *testing.T) {
	tree := &Tree{Root: &Topic{}}
	if got := tree.Find([]string{}); got != tree.Root {
		t.Errorf("Find with empty path must return Root")
	}
}

func TestLoad_OverlayMerge_UnionSeeAlso(t *testing.T) {
	oss := fstest.MapFS{
		"content/topic-a.md": &fstest.MapFile{Data: []byte(`---
topic: topic-a
title: oss-a
stability: stable
see_also: [x, y]
---

oss body
`)},
		"content/topic-c.md": &fstest.MapFile{Data: []byte(`---
topic: topic-c
title: oss-c
stability: stable
---

oss c
`)},
	}
	ent := fstest.MapFS{
		"content/topic-a.md": &fstest.MapFile{Data: []byte(`---
topic: topic-a
title: ent-a
stability: stable
see_also: [z]
---

ent body
`)},
		"content/topic-b.md": &fstest.MapFile{Data: []byte(`---
topic: topic-b
title: ent-b
stability: stable
---

ent b
`)},
	}
	tree, err := Load(oss, ent)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"topic-a", "topic-b", "topic-c"} {
		if tree.Find([]string{name}) == nil {
			t.Errorf("topic %q missing from merged tree", name)
		}
	}
	a := tree.Find([]string{"topic-a"})
	if a.Title != "ent-a" {
		t.Errorf("topic-a.Title = %q, want %q (Enterprise wins)", a.Title, "ent-a")
	}
	if string(a.Body) != "ent body\n" {
		t.Errorf("topic-a.Body = %q, want ent body", a.Body)
	}
	wantSeeAlso := []string{"x", "y", "z"}
	if !reflect.DeepEqual(a.SeeAlso, wantSeeAlso) {
		t.Errorf("topic-a.SeeAlso = %v, want %v (union)", a.SeeAlso, wantSeeAlso)
	}
}

func TestLoad_OverlayMerge_ReplaceSeeAlso(t *testing.T) {
	oss := fstest.MapFS{
		"content/topic-a.md": &fstest.MapFile{Data: []byte(`---
topic: topic-a
title: oss-a
stability: stable
see_also: [x, y]
---
body
`)},
	}
	ent := fstest.MapFS{
		"content/topic-a.md": &fstest.MapFile{Data: []byte(`---
topic: topic-a
title: ent-a
stability: stable
see_also_replace: true
see_also: [z]
---
body
`)},
	}
	tree, err := Load(oss, ent)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := tree.Find([]string{"topic-a"})
	want := []string{"z"}
	if !reflect.DeepEqual(a.SeeAlso, want) {
		t.Errorf("topic-a.SeeAlso = %v, want %v (replace)", a.SeeAlso, want)
	}
}

func TestTopicDescriptor_FromTopic(t *testing.T) {
	topic := &Topic{
		Path:      []string{"cli", "serve"},
		Title:     "cli serve",
		Stability: "stable",
		SeeAlso:   []string{"config"},
		Body:      []byte("# serve\n\n## DESCRIPTION\n\nServe the HTTP API.\n"),
	}
	d := topic.Descriptor()
	if d.Topic != "cli.serve" {
		t.Errorf("Topic = %q", d.Topic)
	}
	if d.Synopsis != "Serve the HTTP API." {
		t.Errorf("Synopsis = %q", d.Synopsis)
	}
}

func TestTopicDescriptor_SeeAlsoAlwaysNonNil(t *testing.T) {
	// Topics without see_also must still serialize as "see_also":[]
	// so API consumers can rely on the field being a JSON array.
	topic := &Topic{
		Path:      []string{"x"},
		Title:     "x",
		Stability: "stable",
		Body:      []byte("body"),
	}
	d := topic.Descriptor()
	if d.SeeAlso == nil {
		t.Error("SeeAlso must be non-nil empty slice, got nil")
	}
	if len(d.SeeAlso) != 0 {
		t.Errorf("SeeAlso should be empty, got %v", d.SeeAlso)
	}
}

func TestTree_WalkDescriptors_NilRoot(t *testing.T) {
	tree := &Tree{}
	got := tree.WalkDescriptors()
	if got != nil {
		t.Errorf("WalkDescriptors on nil Root = %v, want nil", got)
	}
}

func TestTree_WalkDescriptors_GrandchildPreOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"content/a.md": &fstest.MapFile{Data: []byte(`---
topic: a
title: a
stability: stable
---
`)},
		"content/a/b.md": &fstest.MapFile{Data: []byte(`---
topic: a.b
title: ab
stability: stable
---
`)},
		"content/a/b/c.md": &fstest.MapFile{Data: []byte(`---
topic: a.b.c
title: abc
stability: stable
---
`)},
		"content/a/d.md": &fstest.MapFile{Data: []byte(`---
topic: a.d
title: ad
stability: stable
---
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := tree.WalkDescriptors()
	want := []string{"a", "a.b", "a.b.c", "a.d"}
	if len(got) != len(want) {
		t.Fatalf("got %d descriptors, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Topic != want[i] {
			t.Errorf("got[%d].Topic = %q, want %q", i, got[i].Topic, want[i])
		}
	}
}

func TestTree_WalkDescriptors_DepthFirst(t *testing.T) {
	fsys := fstest.MapFS{
		"content/a.md": &fstest.MapFile{Data: []byte(`---
topic: a
title: a
stability: stable
---

## DESCRIPTION

aa
`)},
		"content/a/b.md": &fstest.MapFile{Data: []byte(`---
topic: a.b
title: ab
stability: stable
---

## DESCRIPTION

ab
`)},
	}
	tree, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := tree.WalkDescriptors()
	if len(got) != 2 {
		t.Fatalf("got %d descriptors, want 2: %+v", len(got), got)
	}
	if got[0].Topic != "a" || got[1].Topic != "a.b" {
		t.Errorf("depth-first order wrong: %+v", got)
	}
	if len(got[0].Children) != 1 || got[0].Children[0] != "a.b" {
		t.Errorf("parent Children = %+v, want [a.b]", got[0].Children)
	}
}

// topLevelTopicsV061 is the authoritative list of top-level topics
// for v0.6.1. Task 12 authors the stubs; this list pins them.
var topLevelTopicsV061 = []string{
	"cli", "config", "errors", "crud", "search", "analytics",
	"models", "workflows", "run", "helm", "telemetry",
	"openapi", "grpc", "quickstart", "admin", "cluster",
}

// TestAllTopLevelTopicsPresent guards against accidental deletion of a
// top-level topic.
func TestAllTopLevelTopicsPresent(t *testing.T) {
	tree := DefaultTree
	for _, name := range topLevelTopicsV061 {
		if tree.Find([]string{name}) == nil {
			t.Errorf("top-level topic %q missing from embedded content", name)
		}
	}
}

var envVarPattern = regexp.MustCompile(`CYODA_[A-Z][A-Z0-9_]*`)

// Test-only env var prefixes — referenced in tests but not meant to be
// documented in config/*.md.
var testOnlyEnvPrefixes = []string{"CYODA_TEST_", "CYODA_MARKER", "CYODA_DEBUG_"}

// testOnlyEnvSuffix marks env vars that are deliberately-undocumented test-only
// escape hatches (e.g. CYODA_DISPATCH_ALLOW_LOOPBACK_FOR_TESTING, which reopens
// the dispatch-forwarder SSRF guard for loopback multi-node fixtures). These
// must NOT be advertised in user-facing config docs — documenting them invites
// production misuse — so any CYODA_*_FOR_TESTING var is exempt from coverage.
const testOnlyEnvSuffix = "_FOR_TESTING"

func isTestOnlyEnv(v string) bool {
	if strings.HasSuffix(v, testOnlyEnvSuffix) {
		return true
	}
	for _, p := range testOnlyEnvPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// repoRoot walks up from the current working directory until it finds
// go.mod, returning that directory. Skips the test if no go.mod is found.
func repoRoot(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := wd
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate repo root; test skipped")
			return ""
		}
		root = parent
	}
}

// TestConfig_EnvVarCoverage asserts every CYODA_* env var referenced in
// source also appears in cmd/cyoda/help/content/config/**/*.md (or
// config.md). Scope: cmd, app, plugins, internal (excluding _test.go).
func TestConfig_EnvVarCoverage(t *testing.T) {
	root := repoRoot(t)

	referenced := scanEnvVarsInGoSource(t, root, []string{"cmd", "app", "plugins", "internal"})
	documented := scanEnvVarsInConfigDocs(t, filepath.Join(root, "cmd/cyoda/help/content"))

	var missing []string
	for v := range referenced {
		if isTestOnlyEnv(v) {
			continue
		}
		if !documented[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("CYODA_* vars referenced in source but not documented under config/**/*.md:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// scanEnvVarsInGoSource walks dirs under root, finds every CYODA_*
// identifier in *.go files (excluding *_test.go).
func scanEnvVarsInGoSource(t *testing.T, root string, dirs []string) map[string]bool {
	out := map[string]bool{}
	for _, d := range dirs {
		base := filepath.Join(root, d)
		err := filepath.WalkDir(base, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return fs.SkipDir
				}
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			for _, m := range envVarPattern.FindAll(data, -1) {
				out[string(m)] = true
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return out
}

var errCodePattern = regexp.MustCompile(`ErrCode[A-Z][A-Za-z0-9]+\s*=\s*"([A-Z0-9_]+)"`)

// TestErrCode_Parity asserts every ErrCode* in internal/common/error_codes.go
// has a matching errors/<CODE>.md topic file, and vice versa.
func TestErrCode_Parity(t *testing.T) {
	wd, _ := os.Getwd()
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate repo root")
			return
		}
		root = parent
	}
	src, err := os.ReadFile(filepath.Join(root, "internal/common/error_codes.go"))
	if err != nil {
		t.Fatalf("read error_codes.go: %v", err)
	}
	defined := map[string]bool{}
	for _, m := range errCodePattern.FindAllStringSubmatch(string(src), -1) {
		defined[m[1]] = true
	}
	errorsDir := filepath.Join(root, "cmd/cyoda/help/content/errors")
	entries, err := os.ReadDir(errorsDir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("errors content directory missing: %s", errorsDir)
		}
		t.Fatalf("read errors/: %v", err)
	}
	documented := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			documented[strings.TrimSuffix(e.Name(), ".md")] = true
		}
	}
	for code := range defined {
		if !documented[code] {
			t.Errorf("ErrCode %q defined in error_codes.go but no errors/%s.md", code, code)
		}
	}
	for code := range documented {
		if !defined[code] {
			t.Errorf("errors/%s.md exists but no matching ErrCode in error_codes.go", code)
		}
	}
}

// Phrases that MUST appear somewhere under cli/*.md or config/*.md
// after the printHelp() migration. Pins content that the env-var
// grep (test #11) alone doesn't cover.
var printHelpMustAppearPhrases = []string{
	"_FILE",          // secret-from-file pattern
	"--force",        // cyoda init flag
	"--timeout",      // cyoda health/migrate flag
	"CYODA_PROFILES", // profile loader (config.md covers this)
	"mock",           // mock IAM default warning
	"docker",         // run-docker.sh reference or docker run example
}

func TestPrintHelp_ContentMigrationParity(t *testing.T) {
	wd, _ := os.Getwd()
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate repo root")
			return
		}
		root = parent
	}
	var combined strings.Builder
	for _, dir := range []string{"cmd/cyoda/help/content/cli", "cmd/cyoda/help/content/config"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			b, _ := os.ReadFile(p)
			combined.Write(b)
			combined.WriteString("\n")
			return nil
		})
	}
	for _, rel := range []string{"cmd/cyoda/help/content/cli.md", "cmd/cyoda/help/content/config.md"} {
		b, _ := os.ReadFile(filepath.Join(root, rel))
		combined.Write(b)
		combined.WriteString("\n")
	}
	text := combined.String()
	for _, phrase := range printHelpMustAppearPhrases {
		if !strings.Contains(text, phrase) {
			t.Errorf("phrase %q missing from cli/*.md + config/*.md — printHelp content not fully migrated", phrase)
		}
	}
}

// TestContentMarkdownSubsetLinter rejects any help file using markdown
// constructs outside the pinned subset. Enforces the tokenizer's scope
// — tables, nested lists, HTML blocks, blockquotes.
func TestContentMarkdownSubsetLinter(t *testing.T) {
	err := fs.WalkDir(embeddedContent, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		raw, rerr := fs.ReadFile(embeddedContent, p)
		if rerr != nil {
			return rerr
		}
		_, body, ferr := parseFrontMatter(raw)
		if ferr != nil {
			// Front-matter parse errors are caught by other tests; skip.
			return nil
		}
		issues := renderer.FindUnsupported(body)
		for _, iss := range issues {
			t.Errorf("%s: %s", p, iss)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestSeeAlsoResolution asserts every see_also entry across the
// embedded topic tree resolves to a real topic via tree.Find. Prevents
// dead cross-references from landing silently.
func TestSeeAlsoResolution(t *testing.T) {
	for _, desc := range DefaultTree.WalkDescriptors() {
		for _, ref := range desc.SeeAlso {
			segs := strings.Split(ref, ".")
			if DefaultTree.Find(segs) == nil {
				t.Errorf("topic %q has see_also %q but no such topic exists",
					desc.Topic, ref)
			}
		}
	}
}

// scanEnvVarsInConfigDocs walks the help content directory and extracts
// every CYODA_* mention from config.md and config/**/*.md.
func scanEnvVarsInConfigDocs(t *testing.T, contentRoot string) map[string]bool {
	out := map[string]bool{}
	scan := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
			return
		}
		for _, m := range envVarPattern.FindAll(data, -1) {
			out[string(m)] = true
		}
	}
	// Top-level config.md (if present).
	configMd := filepath.Join(contentRoot, "config.md")
	if _, err := os.Stat(configMd); err == nil {
		scan(configMd)
	}
	// Everything under config/.
	configDir := filepath.Join(contentRoot, "config")
	_ = filepath.WalkDir(configDir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		scan(p)
		return nil
	})
	return out
}

// TestDefaultTree_AuthTopics ensures the new auth topic tree loads from
// embedded content and that every topic's body contains the rigid
// 7-section anchors the LLM-targeted template promises (per spec D5).
func TestDefaultTree_AuthTopics(t *testing.T) {
	tree := DefaultTree
	if tree == nil || tree.Root == nil {
		t.Fatal("DefaultTree.Root nil")
	}

	cases := []struct {
		path           []string
		title          string
		requireAnchors []string
	}{
		{[]string{"auth"}, "auth — authenticate client applications against cyoda",
			[]string{"## NAME", "## GOAL", "## WHICH PATH DO I NEED?", "## TOKEN PRESENTATION", "## SEE ALSO"}},
		{[]string{"auth", "clients"}, "auth.clients — M2M client lifecycle",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## ERRORS", "## SEE ALSO"}},
		{[]string{"auth", "tokens"}, "auth.tokens — /oauth/token grants and JWT claim contract",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## ERRORS", "## SEE ALSO"}},
		{[]string{"auth", "oidc"}, "auth.oidc — federated OIDC providers",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## DIAGNOSTICS", "## ERRORS", "## SEE ALSO"}},
		{[]string{"auth", "trusted-keys"}, "auth.trusted-keys — register public keys for offline JWT signing",
			[]string{"## NAME", "## GOAL", "## PREREQUISITES", "## REQUEST FLOW", "## TOKEN", "## ERRORS", "## SEE ALSO"}},
	}

	for _, tc := range cases {
		t.Run(strings.Join(tc.path, "."), func(t *testing.T) {
			topic := tree.Find(tc.path)
			if topic == nil {
				t.Fatalf("Find(%v): topic missing", tc.path)
			}
			if topic.Title != tc.title {
				t.Errorf("title: got %q, want %q", topic.Title, tc.title)
			}
			if topic.Stability != "evolving" {
				t.Errorf("stability: got %q, want %q", topic.Stability, "evolving")
			}
			body := string(topic.Body)
			for _, anchor := range tc.requireAnchors {
				if !strings.Contains(body, anchor) {
					t.Errorf("body missing anchor %q", anchor)
				}
			}
		})
	}
}

// TestDefaultTree_AuthCurlExamplesMatchOpenAPI verifies that every
// `curl -X METHOD https://.../api/<path>` example in the auth.* page
// bodies references a real (method, path) tuple declared in
// api/openapi.yaml. Catches the class of drift where help-content
// invents endpoints or uses wrong HTTP verbs — exactly the bug that
// shipped to auth/clients.md in the first round (POST instead of PUT
// for reset-secret, etc.) and the bug-class the rigid 7-section
// template (D5) is supposed to prevent. The lint pairs with the
// template: D5 enforces structural uniformity; this enforces wire
// fidelity.
func TestDefaultTree_AuthCurlExamplesMatchOpenAPI(t *testing.T) {
	// Build the (method, path) set from api/openapi.yaml. Parse is
	// line-anchored — the spec's shape (`  /path:` at column 2,
	// `    METHOD:` at column 4) is stable.
	specBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	type opKey struct{ method, path string }
	declared := map[opKey]bool{}
	pathHead := regexp.MustCompile(`^  (/\S+):`)
	verb := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	var curPath string
	for _, line := range strings.Split(string(specBytes), "\n") {
		if m := pathHead.FindStringSubmatch(line); m != nil {
			curPath = m[1]
			continue
		}
		if m := verb.FindStringSubmatch(line); m != nil && curPath != "" {
			declared[opKey{strings.ToUpper(m[1]), curPath}] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("OpenAPI parser returned no operations — parse logic broken")
	}

	// curl-example pattern: `curl -X <METHOD> ... /api<path>` or
	// `curl -X <METHOD> "<url>?query"`. Path is extracted between
	// `/api` and the first whitespace, `\`, `"`, `?`, or end-of-line.
	curlPat := regexp.MustCompile(`curl\s+-X\s+(GET|POST|PUT|PATCH|DELETE)\s+"?[^"\s]*?/api(/[^\s"?\\]+)`)

	// Path parameters in curl examples are concrete (e.g. ${CLIENT_ID}
	// or a real-looking UUID); translate them back to the spec's
	// `{name}` placeholders by matching against declared paths whose
	// shape is identical when parameter segments are stripped.
	pathParam := regexp.MustCompile(`\$\{[^}]+\}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

	resolves := func(method, concrete string) bool {
		// Fast path: literal match.
		if declared[opKey{method, concrete}] {
			return true
		}
		// Substitute concrete params back to the spec's `{name}` shape
		// by comparing segment-by-segment against every declared path
		// with the same method.
		concreteSegs := strings.Split(strings.TrimPrefix(concrete, "/"), "/")
		for k := range declared {
			if k.method != method {
				continue
			}
			declSegs := strings.Split(strings.TrimPrefix(k.path, "/"), "/")
			if len(declSegs) != len(concreteSegs) {
				continue
			}
			match := true
			for i, declSeg := range declSegs {
				cSeg := concreteSegs[i]
				if strings.HasPrefix(declSeg, "{") && strings.HasSuffix(declSeg, "}") {
					// Spec parameter — accept any non-empty concrete segment.
					if cSeg == "" {
						match = false
						break
					}
					continue
				}
				if declSeg != cSeg && !pathParam.MatchString(cSeg) {
					match = false
					break
				}
				if declSeg != cSeg {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		return false
	}

	for _, pageName := range []string{"auth.md", "auth/clients.md", "auth/tokens.md", "auth/oidc.md", "auth/trusted-keys.md"} {
		t.Run(pageName, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("content", pageName))
			if err != nil {
				t.Fatalf("read %s: %v", pageName, err)
			}
			matches := curlPat.FindAllStringSubmatch(string(body), -1)
			if len(matches) == 0 {
				return // pages without HTTP examples are valid (e.g. auth.md landing)
			}
			for _, m := range matches {
				method, concrete := m[1], m[2]
				if !resolves(method, concrete) {
					t.Errorf("curl example %s %s does not resolve to a declared operation in api/openapi.yaml", method, concrete)
				}
			}
		})
	}
}

// TestDefaultTree_AuthLandingListsAllChildren verifies the auth landing
// page renders descriptors for every subtopic — the renderer auto-populates
// the parent topic's Children slice from the embedded filesystem.
func TestDefaultTree_AuthLandingListsAllChildren(t *testing.T) {
	tree := DefaultTree
	auth := tree.Find([]string{"auth"})
	if auth == nil {
		t.Fatal("Find([auth]): topic missing")
	}
	want := map[string]bool{"clients": true, "tokens": true, "oidc": true, "trusted-keys": true}
	got := map[string]bool{}
	for _, c := range auth.Children {
		if len(c.Path) == 0 {
			t.Fatalf("child with empty path: %#v", c)
		}
		got[c.Path[len(c.Path)-1]] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("auth missing child %q (have %v)", k, got)
		}
	}
}

// TestDefaultTree_ConfigClusterSubtopic verifies the cluster/dispatch env
// vars live under their own config.cluster subtopic (mirroring
// auth/cors/database/grpc/schema) and that config.md's see_also lists both
// config.cluster and config.cors.
func TestDefaultTree_ConfigClusterSubtopic(t *testing.T) {
	node := DefaultTree.Find([]string{"config", "cluster"})
	if node == nil {
		t.Fatal("config.cluster topic not found")
	}
	// The cluster/dispatch vars must now live under config.cluster, not config.
	body := string(node.Body)
	for _, want := range []string{"CYODA_CLUSTER_ENABLED", "CYODA_SEED_NODES", "CYODA_DISPATCH_WAIT_TIMEOUT"} {
		if !strings.Contains(body, want) {
			t.Errorf("config.cluster body missing %s", want)
		}
	}
	// config.md must list cluster (frontmatter see_also drives Descriptor.SeeAlso).
	cfg := DefaultTree.Find([]string{"config"})
	if cfg == nil {
		t.Fatal("config topic not found")
	}
	joined := strings.Join(cfg.Descriptor().SeeAlso, ",")
	for _, want := range []string{"config.cluster", "config.cors"} {
		if !strings.Contains(joined, want) {
			t.Errorf("config see_also missing %s (got %q)", want, joined)
		}
	}
}
