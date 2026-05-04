package help

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TestSlugifyTag pins the tag-name → slug normalization for the 12
// canonical tags shipped with v0.6.2. The slug is the lookup key users
// type on the CLI; any change to the slug rule breaks downstream
// tooling, so these are contract-level.
func TestSlugifyTag(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Entity Management", "entity-management"},
		{"OAuth, OIDC Providers", "oauth-oidc-providers"},
		{"Entity Model, Workflow", "entity-model-workflow"},
		{"OAuth, Keys", "oauth-keys"},
		{"Entity, Audit", "entity-audit"},
		{"Entity model", "entity-model"},
		{"Search", "search"},
		{"User, Machine", "user-machine"},
		{"Stream Data", "stream-data"},
		{"User, Account", "user-account"},
		{"SQL-Schema", "sql-schema"},
		{"CQL Execution Statistics", "cql-execution-statistics"},

		// extra normalization invariants
		{"Multiple   Spaces", "multiple-spaces"},
		{"Mixed, CASE , tokens", "mixed-case-tokens"},
		{"  trim  ", "trim"},
	}
	for _, c := range cases {
		if got := SlugifyTag(c.in); got != c.want {
			t.Errorf("SlugifyTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestListOpenAPITags verifies the tags action enumerates every tag
// defined in the embedded spec as slug → canonical-name pairs, sorted
// by slug.
func TestListOpenAPITags(t *testing.T) {
	swagger, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("GetSwagger: %v", err)
	}

	tags := ListOpenAPITags(swagger)
	if len(tags) != len(swagger.Tags) {
		t.Fatalf("ListOpenAPITags returned %d entries; spec has %d tags", len(tags), len(swagger.Tags))
	}
	// Every tag must have a non-empty slug.
	for _, tg := range tags {
		if tg.Slug == "" {
			t.Errorf("tag %q produced empty slug", tg.Canonical)
		}
	}
	// Output must be sorted by slug (deterministic for CLI consumption).
	for i := 1; i < len(tags); i++ {
		if tags[i-1].Slug >= tags[i].Slug {
			t.Errorf("tags not sorted by slug: %q >= %q", tags[i-1].Slug, tags[i].Slug)
		}
	}
	// The 11 canonical names defined in the spec's tags section must all appear.
	// Note: CQL Execution Statistics is excluded — it was never in the spec's
	// top-level tags declaration (only referenced as an exclude-tag in api/config.yaml).
	want := []string{
		"Entity Management",
		"OAuth, OIDC Providers",
		"Entity Model, Workflow",
		"OAuth, Keys",
		"Entity, Audit",
		"Entity model",
		"Search",
		"User, Machine",
		"Stream Data",
		"User, Account",
		"SQL-Schema",
	}
	present := map[string]bool{}
	for _, tg := range tags {
		present[tg.Canonical] = true
	}
	for _, c := range want {
		if !present[c] {
			t.Errorf("expected canonical name %q not present in tag list", c)
		}
	}
}

// TestOpenAPISpecSlugsAreUnique pins that no two tags in the embedded
// spec slugify to the same value. Slug collision would silently shadow
// one tag's operations under another's slug at dispatch time; with this
// check, any future tag addition that collides fails the build
// immediately. Gate 6 — resolve at spec-edit time, not at runtime.
func TestOpenAPISpecSlugsAreUnique(t *testing.T) {
	swagger, _ := genapi.GetSwagger()
	seen := map[string]string{}
	for _, tg := range swagger.Tags {
		slug := SlugifyTag(tg.Name)
		if prev, ok := seen[slug]; ok {
			t.Errorf("tag-slug collision: %q and %q both slugify to %q", prev, tg.Name, slug)
		}
		seen[slug] = tg.Name
	}
}

// TestFilterOpenAPISpecByTag_UnknownSlug pins the error contract.
func TestFilterOpenAPISpecByTag_UnknownSlug(t *testing.T) {
	swagger, _ := genapi.GetSwagger()
	_, err := FilterOpenAPISpecByTag(swagger, "definitely-not-a-tag")
	if err == nil {
		t.Fatalf("FilterOpenAPISpecByTag: expected error for unknown slug, got nil")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-tag") {
		t.Errorf("error should name the bad slug; got %q", err.Error())
	}
}

// TestFilterOpenAPISpecByTag_PathsFilteredToTag — after filtering, every
// operation in the output must carry the target tag and paths without any
// matching operation must be gone.
func TestFilterOpenAPISpecByTag_PathsFilteredToTag(t *testing.T) {
	swagger, _ := genapi.GetSwagger()
	const slug = "entity-management"

	filtered, err := FilterOpenAPISpecByTag(swagger, slug)
	if err != nil {
		t.Fatalf("FilterOpenAPISpecByTag: %v", err)
	}

	canonical := ""
	for _, tg := range swagger.Tags {
		if SlugifyTag(tg.Name) == slug {
			canonical = tg.Name
			break
		}
	}
	if canonical == "" {
		t.Fatalf("test fixture: no tag in spec for slug %q", slug)
	}

	if filtered.Paths.Len() == 0 {
		t.Fatalf("filtered paths empty — tag %q has no operations in this spec?", canonical)
	}

	// Forward: every operation that survived must carry the target tag.
	survived := map[string]bool{}
	for path, pathItem := range filtered.Paths.Map() {
		for method, op := range pathItem.Operations() {
			key := method + " " + path
			survived[key] = true
			if !hasTag(op, canonical) {
				t.Errorf("%s: surviving operation does not carry tag %q (tags: %v)",
					key, canonical, op.Tags)
			}
		}
	}

	// Converse: every source operation carrying the target tag must survive.
	// Without this check, a bug that drops operations matching some other
	// criterion (e.g. all PUTs) would pass the forward check alone.
	for path, pathItem := range swagger.Paths.Map() {
		for method, op := range pathItem.Operations() {
			if !hasTag(op, canonical) {
				continue
			}
			key := method + " " + path
			if !survived[key] {
				t.Errorf("source operation %s carries tag %q but was dropped from the filtered doc", key, canonical)
			}
		}
	}

	// The Tags slice should be reduced to just the filtered tag.
	if len(filtered.Tags) != 1 || filtered.Tags[0].Name != canonical {
		t.Errorf("Tags slice = %+v; want [%s]", filtered.Tags, canonical)
	}
}

// TestFilterOpenAPISpecByTag_EmitsSelfContainedDoc — the output JSON must
// not contain any $ref that points outside its own components. This is
// the "emitted doc is valid" acceptance criterion.
func TestFilterOpenAPISpecByTag_EmitsSelfContainedDoc(t *testing.T) {
	swagger, _ := genapi.GetSwagger()
	tags := ListOpenAPITags(swagger)

	for _, tg := range tags {
		tg := tg
		t.Run(tg.Slug, func(t *testing.T) {
			filtered, err := FilterOpenAPISpecByTag(swagger, tg.Slug)
			if err != nil {
				t.Fatalf("FilterOpenAPISpecByTag(%q): %v", tg.Slug, err)
			}

			raw, err := json.Marshal(filtered)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			refs := extractRefs(raw)
			for _, ref := range refs {
				if !strings.HasPrefix(ref, "#/components/") {
					t.Errorf("unexpected $ref shape %q", ref)
					continue
				}
				if !refExistsInDoc(raw, ref) {
					t.Errorf("tag %q: $ref %q is dangling in the emitted doc", tg.Slug, ref)
				}
			}
		})
	}
}

// TestEmitOpenAPITagsAction — tabular output: `<slug>  <canonical>` per
// line, one tag per line, sorted by slug.
func TestEmitOpenAPITagsAction(t *testing.T) {
	var buf bytes.Buffer
	rc := emitOpenAPITags(&buf)
	if rc != 0 {
		t.Fatalf("emitOpenAPITags rc = %d, want 0", rc)
	}
	out := buf.String()
	// Every line must be `<slug><whitespace><canonical>` — at least 2 cols.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected ≥5 tag lines, got %d:\n%s", len(lines), out)
	}
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Errorf("malformed line %q — want slug then canonical name", line)
		}
	}
	if !strings.Contains(out, "entity-management") {
		t.Errorf("expected output to contain %q; got:\n%s", "entity-management", out)
	}
}

// TestEmitOpenAPIByTagAction_JSONFormat — dispatching to the per-tag
// action for a valid slug emits a valid OpenAPI 3.1 JSON doc.
func TestEmitOpenAPIByTagAction_JSONFormat(t *testing.T) {
	action, ok := lookupOpenAPITagAction("entity-management", "json")
	if !ok {
		t.Fatalf("lookupOpenAPITagAction: slug not resolved")
	}
	var buf bytes.Buffer
	if rc := action(&buf); rc != 0 {
		t.Fatalf("action rc = %d, output:\n%s", rc, buf.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc["openapi"] == nil {
		t.Errorf("emitted doc missing openapi field")
	}
	if doc["paths"] == nil {
		t.Errorf("emitted doc missing paths")
	}
}

// TestLookupOpenAPITagAction_UnknownSlug — unknown slug returns ok=false
// so the CLI dispatch layer can surface a clear error.
func TestLookupOpenAPITagAction_UnknownSlug(t *testing.T) {
	_, ok := lookupOpenAPITagAction("not-a-real-slug", "json")
	if ok {
		t.Fatal("expected ok=false for unknown slug")
	}
}

// --- helpers ---

func hasTag(op *openapi3.Operation, name string) bool {
	for _, t := range op.Tags {
		if t == name {
			return true
		}
	}
	return false
}

var refRe = regexp.MustCompile(`"\$ref":"(#[^"]+)"`)

// extractRefs pulls every $ref target out of a marshaled OpenAPI doc.
func extractRefs(raw []byte) []string {
	ms := refRe.FindAllSubmatch(raw, -1)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, string(m[1]))
	}
	return out
}

// refExistsInDoc returns true iff the named internal $ref resolves
// within the marshaled doc. Walks components.<kind>.<name> precisely —
// a prior version substring-searched `"<name>":` which could false-
// positive on property names colliding with component names (e.g. a
// property `items`).
func refExistsInDoc(raw []byte, ref string) bool {
	// ref shape: "#/components/<kind>/<name>"
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	if len(parts) != 3 || parts[0] != "components" {
		return false
	}
	kind, name := parts[1], parts[2]

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	components, ok := doc["components"].(map[string]any)
	if !ok {
		return false
	}
	section, ok := components[kind].(map[string]any)
	if !ok {
		return false
	}
	_, ok = section[name]
	return ok
}
