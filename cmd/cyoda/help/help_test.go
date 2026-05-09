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
	"openapi", "grpc", "quickstart", "admin",
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

func isTestOnlyEnv(v string) bool {
	for _, p := range testOnlyEnvPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// TestConfig_EnvVarCoverage asserts every CYODA_* env var referenced in
// source also appears in cmd/cyoda/help/content/config/**/*.md (or
// config.md). Scope: cmd, app, plugins, internal (excluding _test.go).
func TestConfig_EnvVarCoverage(t *testing.T) {
	// Walk up from getwd until we find go.mod.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := wd
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Skip("cannot locate repo root; test skipped")
			return
		}
		root = parent
	}

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
