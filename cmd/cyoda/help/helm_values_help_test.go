package help

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestHelmHelp_DocumentsEveryValue: the VALUES section of the helm topic
// documents every key of the chart's values.yaml. A key is documented when
// helm.md has a bold, back-quoted entry for it or for one of its parents (a
// parent entry such as `resources` covers its children). The two files live
// apart, so without this check a new value reaches users undocumented.
func TestHelmHelp_DocumentsEveryValue(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "helm", "cyoda", "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	doc, err := os.ReadFile(filepath.Join("content", "helm.md"))
	if err != nil {
		t.Fatalf("read helm.md: %v", err)
	}
	text := string(doc)
	start := strings.Index(text, "\n## VALUES\n")
	if start < 0 {
		t.Fatal("helm.md has no VALUES section")
	}
	section := text[start+1:]
	if end := strings.Index(section[len("## VALUES\n"):], "\n## "); end >= 0 {
		section = section[:len("## VALUES\n")+end]
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("\\*\\*`([A-Za-z0-9_.]+)`\\*\\*").FindAllStringSubmatch(section, -1) {
		documented[m[1]] = true
	}

	var leaves []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if child, ok := v.(map[string]any); ok && len(child) > 0 {
				walk(key, child)
				continue
			}
			leaves = append(leaves, key)
		}
	}
	walk("", values)

	for _, leaf := range leaves {
		covered := false
		for key := leaf; ; {
			if documented[key] {
				covered = true
				break
			}
			i := strings.LastIndex(key, ".")
			if i < 0 {
				break
			}
			key = key[:i]
		}
		if !covered {
			t.Errorf("values.yaml key %q has no entry in the VALUES section of helm.md", leaf)
		}
	}
}
