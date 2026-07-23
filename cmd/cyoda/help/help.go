// Package help embeds and renders the cyoda help topic tree.
package help

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cyoda-platform/cyoda-go/cmd/cyoda/help/renderer"
)

//go:embed content
var embeddedContent embed.FS

// DefaultTree is the tree loaded from embedded OSS content. Populated
// at package init; panics if content is malformed (a compile-time
// guarantee would be preferable, but go:embed can't enforce topic
// structure).
//
// This is the OSS-only base. Runtime consumers (the CLI and the HTTP help
// routes) build from BuildTree (see overlay.go), which merges this base with
// any plugin-registered overlays. DefaultTree is retained for tests and
// OSS-only consumers; its eager init also keeps process-start fail-fast for
// malformed OSS content, independent of when BuildTree is first called.
var DefaultTree = func() *Tree {
	t, err := Load(embeddedContent)
	if err != nil {
		panic(fmt.Sprintf("help: failed to load embedded content: %v", err))
	}
	return t
}()

// FrontMatter is the YAML header on every help topic source file.
type FrontMatter struct {
	Topic          string   `yaml:"topic"`
	Title          string   `yaml:"title"`
	Stability      string   `yaml:"stability"`
	SeeAlso        []string `yaml:"see_also,omitempty"`
	VersionAdded   string   `yaml:"version_added,omitempty"`
	SeeAlsoReplace bool     `yaml:"see_also_replace,omitempty"`
}

var frontMatterDelim = []byte("---\n")

// parseFrontMatter extracts the YAML front-matter from a markdown source
// and returns the parsed header, the body (front-matter stripped), and
// any error. Malformed front-matter or missing required fields are
// errors — this fails at tree-load time, not at query time.
func parseFrontMatter(src []byte) (*FrontMatter, []byte, error) {
	if !bytes.HasPrefix(src, frontMatterDelim) {
		return nil, nil, fmt.Errorf("front-matter missing: file must start with '---\\n'")
	}
	rest := src[len(frontMatterDelim):]
	end := bytes.Index(rest, frontMatterDelim)
	if end < 0 {
		return nil, nil, fmt.Errorf("front-matter unterminated: no closing '---' found")
	}
	header := rest[:end]
	body := bytes.TrimLeft(rest[end+len(frontMatterDelim):], "\n")

	fm := &FrontMatter{}
	if err := yaml.Unmarshal(header, fm); err != nil {
		return nil, nil, fmt.Errorf("front-matter YAML: %w", err)
	}
	if fm.Topic == "" {
		return nil, nil, fmt.Errorf("front-matter: required field 'topic' is empty")
	}
	if fm.Title == "" {
		return nil, nil, fmt.Errorf("front-matter: required field 'title' is empty")
	}
	switch fm.Stability {
	case "stable", "evolving", "experimental":
		// ok
	default:
		return nil, nil, fmt.Errorf("front-matter: stability must be stable|evolving|experimental, got %q", fm.Stability)
	}
	return fm, body, nil
}

// Topic is a node in the help tree.
type Topic struct {
	Path      []string // ["cli", "serve"]
	Title     string
	Stability string // stable | evolving | experimental
	SeeAlso   []string
	Body      []byte // markdown body, front-matter stripped
	Children  []*Topic
}

// DottedPath returns the canonical dotted identifier, e.g. "cli.serve".
func (t *Topic) DottedPath() string { return strings.Join(t.Path, ".") }

// Tree holds the synthetic root and provides lookup.
type Tree struct{ Root *Topic }

// Find returns the topic at path, or nil if not present. An empty
// path returns the synthetic Root node (useful for rendering the
// top-level summary of children).
func (t *Tree) Find(path []string) *Topic {
	if t.Root == nil {
		return nil
	}
	cur := t.Root
	for _, seg := range path {
		var next *Topic
		for _, c := range cur.Children {
			if len(c.Path) > 0 && c.Path[len(c.Path)-1] == seg {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// Load reads one or more fs.FS roots and merges them into a single
// Tree. The first argument is the base (typically the OSS embed); each
// subsequent argument is an overlay. On topic-path collision across
// overlays, later-argument values replace earlier ones for body/title/
// stability; see_also unions unless the later entry sets
// see_also_replace: true.
//
// All roots are expected to contain a top-level directory called
// "content/" — the markdown tree lives there.
func Load(roots ...fs.FS) (*Tree, error) {
	tree := &Tree{Root: &Topic{}}
	for i, root := range roots {
		if err := loadInto(tree, root); err != nil {
			return nil, fmt.Errorf("root %d: %w", i, err)
		}
	}
	sortTree(tree.Root)
	return tree, nil
}

// loadInto walks content/ of a single root and inserts each topic
// into the tree. Overlay semantics are applied on collision.
func loadInto(tree *Tree, root fs.FS) error {
	return fs.WalkDir(root, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		raw, err := fs.ReadFile(root, p)
		if err != nil {
			return fmt.Errorf("%s: read: %w", p, err)
		}
		fm, body, err := parseFrontMatter(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		// Derive canonical path from filesystem.
		rel := strings.TrimPrefix(p, "content/")
		rel = strings.TrimSuffix(rel, ".md")
		segs := strings.Split(rel, "/")
		dotted := strings.Join(segs, ".")
		if fm.Topic != dotted {
			return fmt.Errorf("%s: front-matter topic %q does not match filesystem path %q", p, fm.Topic, dotted)
		}
		insertOrMerge(tree.Root, segs, &Topic{
			Path:      segs,
			Title:     fm.Title,
			Stability: fm.Stability,
			SeeAlso:   fm.SeeAlso,
			Body:      body,
		}, fm.SeeAlsoReplace)
		return nil
	})
}

// insertOrMerge places topic under the root at path. If a topic
// already exists at that path, fields are replaced (Enterprise wins)
// except SeeAlso which is unioned unless replace==true.
func insertOrMerge(root *Topic, path []string, topic *Topic, replace bool) {
	cur := root
	for i, seg := range path {
		var found *Topic
		for _, c := range cur.Children {
			if len(c.Path) > 0 && c.Path[len(c.Path)-1] == seg {
				found = c
				break
			}
		}
		if found == nil {
			// Build intermediate placeholder if needed (not the final target).
			newNode := &Topic{Path: append([]string(nil), path[:i+1]...)}
			cur.Children = append(cur.Children, newNode)
			found = newNode
		}
		if i == len(path)-1 {
			// Replace body/title/stability; merge see_also.
			found.Title = topic.Title
			found.Stability = topic.Stability
			found.Body = topic.Body
			if replace {
				found.SeeAlso = topic.SeeAlso
			} else {
				found.SeeAlso = unionSeeAlso(found.SeeAlso, topic.SeeAlso)
			}
		}
		cur = found
	}
}

func unionSeeAlso(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range a {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func sortTree(t *Topic) {
	sort.Slice(t.Children, func(i, j int) bool {
		return t.Children[i].Path[len(t.Children[i].Path)-1] <
			t.Children[j].Path[len(t.Children[j].Path)-1]
	})
	for _, c := range t.Children {
		sortTree(c)
	}
}

// Descriptor builds a renderer.TopicDescriptor for this topic. SeeAlso
// and Actions are always non-nil slices so the JSON representation is
// consistently an array even when absent.
func (t *Topic) Descriptor() renderer.TopicDescriptor {
	seeAlso := []string{}
	if len(t.SeeAlso) > 0 {
		seeAlso = append(seeAlso, t.SeeAlso...)
	}
	actions := actionsFor(t.DottedPath())
	if actions == nil {
		actions = []string{}
	}
	desc := renderer.TopicDescriptor{
		Topic:     t.DottedPath(),
		Path:      append([]string{}, t.Path...),
		Title:     t.Title,
		Synopsis:  renderer.ExtractSynopsis(t.Body),
		Body:      string(t.Body),
		Sections:  renderer.ExtractSections(t.Body),
		SeeAlso:   seeAlso,
		Stability: t.Stability,
		Actions:   actions,
	}
	for _, c := range t.Children {
		desc.Children = append(desc.Children, c.DottedPath())
	}
	return desc
}

// WalkDescriptors returns every topic's descriptor, depth-first,
// parents before children. The synthetic root is not included.
func (t *Tree) WalkDescriptors() []renderer.TopicDescriptor {
	if t.Root == nil {
		return nil
	}
	var out []renderer.TopicDescriptor
	var visit func(*Topic)
	visit = func(n *Topic) {
		if len(n.Path) > 0 {
			out = append(out, n.Descriptor())
		}
		for _, c := range n.Children {
			visit(c)
		}
	}
	visit(t.Root)
	return out
}
