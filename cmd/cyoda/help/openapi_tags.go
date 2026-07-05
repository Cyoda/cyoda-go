package help

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"

	genapi "github.com/cyoda-platform/cyoda-go/api"
)

// TagEntry pairs a tag's URL-safe slug with its canonical name as it
// appears in the OpenAPI spec. The slug is the lookup key on the CLI;
// the canonical name is shown to humans in `cyoda help openapi tags`.
type TagEntry struct {
	Slug      string
	Canonical string
}

// SlugifyTag converts a tag name to a URL-safe slug. The rule:
// lowercase, collapse any run of [whitespace , _] to a single '-',
// and trim leading/trailing '-'. Two different tag names can produce
// the same slug in principle; if that ever happens, ListOpenAPITags
// still returns both entries — callers see both and the ambiguity is
// surfaced at spec-review time rather than silently hidden.
func SlugifyTag(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	lower = slugSepRe.ReplaceAllString(lower, "-")
	lower = slugDoubleRe.ReplaceAllString(lower, "-")
	return strings.Trim(lower, "-")
}

var (
	slugSepRe    = regexp.MustCompile(`[\s,_]+`)
	slugDoubleRe = regexp.MustCompile(`-{2,}`)
)

// ListOpenAPITags returns every tag in the embedded OpenAPI spec as
// a TagEntry, sorted deterministically by slug.
func ListOpenAPITags(swagger *openapi3.T) []TagEntry {
	out := make([]TagEntry, 0, len(swagger.Tags))
	for _, t := range swagger.Tags {
		out = append(out, TagEntry{Slug: SlugifyTag(t.Name), Canonical: t.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// FilterOpenAPISpecByTag returns a fresh *openapi3.T scoped to the
// single tag identified by slug. Paths are filtered to operations
// carrying that tag; path-level parameters are kept if any operation
// on the path survives. The components map is pruned to only the
// members transitively referenced by the surviving paths — the
// emitted document is self-contained (every $ref resolves internally)
// and minimal.
//
// An unknown slug returns a descriptive error naming the slug.
func FilterOpenAPISpecByTag(swagger *openapi3.T, slug string) (*openapi3.T, error) {
	var target *openapi3.Tag
	for _, t := range swagger.Tags {
		if SlugifyTag(t.Name) == slug {
			target = t
			break
		}
	}
	if target == nil {
		valid := make([]string, 0, len(swagger.Tags))
		for _, t := range swagger.Tags {
			valid = append(valid, SlugifyTag(t.Name))
		}
		sort.Strings(valid)
		return nil, fmt.Errorf("unknown tag slug %q; valid: %s", slug, strings.Join(valid, ", "))
	}

	// Filter paths: keep operations that carry the target tag.
	newPaths := openapi3.NewPaths()
	for path, pathItem := range swagger.Paths.Map() {
		newItem := &openapi3.PathItem{
			Ref:         pathItem.Ref,
			Summary:     pathItem.Summary,
			Description: pathItem.Description,
			Servers:     pathItem.Servers,
			Parameters:  pathItem.Parameters,
			Extensions:  pathItem.Extensions,
		}
		kept := false
		for method, op := range pathItem.Operations() {
			if opHasTag(op, target.Name) {
				newItem.SetOperation(method, op)
				kept = true
			}
		}
		if kept {
			newPaths.Set(path, newItem)
		}
	}

	// Compute the transitive closure of $refs under the filtered paths,
	// plus the path-level parameters. The closure then drives the
	// pruned Components map.
	reachable, err := transitiveRefClosure(newPaths, swagger.Components)
	if err != nil {
		return nil, fmt.Errorf("compute transitive refs: %w", err)
	}
	newComponents := pruneComponents(swagger.Components, reachable)

	return &openapi3.T{
		OpenAPI:      swagger.OpenAPI,
		Info:         swagger.Info,
		Servers:      swagger.Servers,
		Paths:        newPaths,
		Components:   newComponents,
		Tags:         openapi3.Tags{target},
		Security:     swagger.Security,
		ExternalDocs: swagger.ExternalDocs,
		Extensions:   swagger.Extensions,
	}, nil
}

// opHasTag returns true if op.Tags contains name.
func opHasTag(op *openapi3.Operation, name string) bool {
	if op == nil {
		return false
	}
	for _, t := range op.Tags {
		if t == name {
			return true
		}
	}
	return false
}

// refPattern matches any JSON `"$ref":"#/components/<kind>/<name>"`
// reference. We walk marshaled bytes rather than reflecting across
// kin-openapi's *Ref types because the latter needs a type-switch per
// component kind; the byte walk works uniformly and is well-tested
// (extractRefs in the test file uses the same shape).
var refPattern = regexp.MustCompile(`"\$ref":\s*"(#/components/[^"]+)"`)

// transitiveRefClosure returns the set of fully-qualified internal
// refs (e.g. "#/components/schemas/EntityMeta") that are reachable
// from the given paths, transitively. Each component is marshaled,
// its refs extracted, and the walk continues until the set stops
// growing.
func transitiveRefClosure(paths *openapi3.Paths, comps *openapi3.Components) (map[string]bool, error) {
	seen := map[string]bool{}
	queue := []string{}

	seed, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("marshal paths: %w", err)
	}
	for _, m := range refPattern.FindAllSubmatch(seed, -1) {
		queue = append(queue, string(m[1]))
	}

	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if seen[ref] {
			continue
		}
		seen[ref] = true

		raw, err := marshalComponent(comps, ref)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			// Ref points outside components (unlikely for our spec) or
			// the component isn't present. Skip; the test for
			// self-contained-ness will flag a genuine dangle.
			continue
		}
		for _, m := range refPattern.FindAllSubmatch(raw, -1) {
			child := string(m[1])
			if !seen[child] {
				queue = append(queue, child)
			}
		}
	}
	return seen, nil
}

// marshalComponent resolves a "#/components/<kind>/<name>" ref against
// the given Components and returns the marshaled bytes of the target,
// or nil if the component kind/name is unknown.
func marshalComponent(comps *openapi3.Components, ref string) ([]byte, error) {
	if comps == nil {
		return nil, nil
	}
	// ref shape: #/components/<kind>/<name>
	parts := strings.SplitN(strings.TrimPrefix(ref, "#/components/"), "/", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	kind, name := parts[0], parts[1]
	var v any
	switch kind {
	case "schemas":
		v = comps.Schemas[name]
	case "parameters":
		v = comps.Parameters[name]
	case "headers":
		v = comps.Headers[name]
	case "requestBodies":
		v = comps.RequestBodies[name]
	case "responses":
		v = comps.Responses[name]
	case "securitySchemes":
		v = comps.SecuritySchemes[name]
	case "examples":
		v = comps.Examples[name]
	case "links":
		v = comps.Links[name]
	case "callbacks":
		v = comps.Callbacks[name]
	default:
		return nil, nil
	}
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// pruneComponents returns a new Components containing only the members
// named in the reachable set. Each kind's map is filtered in isolation.
func pruneComponents(src *openapi3.Components, reachable map[string]bool) *openapi3.Components {
	if src == nil {
		return nil
	}
	out := &openapi3.Components{
		SecuritySchemes: map[string]*openapi3.SecuritySchemeRef{},
		Schemas:         openapi3.Schemas{},
		Parameters:      openapi3.ParametersMap{},
		Headers:         openapi3.Headers{},
		RequestBodies:   openapi3.RequestBodies{},
		Responses:       openapi3.ResponseBodies{},
		Examples:        openapi3.Examples{},
		Links:           openapi3.Links{},
		Callbacks:       openapi3.Callbacks{},
		Extensions:      src.Extensions,
	}

	keep := func(kind, name string) bool {
		return reachable["#/components/"+kind+"/"+name]
	}
	for n, v := range src.Schemas {
		if keep("schemas", n) {
			out.Schemas[n] = v
		}
	}
	for n, v := range src.Parameters {
		if keep("parameters", n) {
			out.Parameters[n] = v
		}
	}
	for n, v := range src.Headers {
		if keep("headers", n) {
			out.Headers[n] = v
		}
	}
	for n, v := range src.RequestBodies {
		if keep("requestBodies", n) {
			out.RequestBodies[n] = v
		}
	}
	for n, v := range src.Responses {
		if keep("responses", n) {
			out.Responses[n] = v
		}
	}
	for n, v := range src.Examples {
		if keep("examples", n) {
			out.Examples[n] = v
		}
	}
	for n, v := range src.Links {
		if keep("links", n) {
			out.Links[n] = v
		}
	}
	for n, v := range src.Callbacks {
		if keep("callbacks", n) {
			out.Callbacks[n] = v
		}
	}
	// SecuritySchemes don't participate in $ref-walking from paths —
	// their references flow via `security: [{<name>: [...]}]` entries,
	// not $refs. Keep the full map so Security still applies.
	for n, v := range src.SecuritySchemes {
		out.SecuritySchemes[n] = v
	}
	return out
}

// --- CLI actions ---

// emitOpenAPITags is the static "tags" action. Emits `<slug>  <canonical>`
// per line, sorted by slug, aligned via tabwriter.
func emitOpenAPITags(w io.Writer) int {
	swagger, err := genapi.GetSwagger()
	if err != nil {
		fmt.Fprintf(w, "cyoda help openapi tags: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, tg := range ListOpenAPITags(swagger) {
		fmt.Fprintf(tw, "%s\t%s\n", tg.Slug, tg.Canonical)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "cyoda help openapi tags: flush: %v\n", err)
		return 1
	}
	return 0
}

// lookupOpenAPITagAction is the dynamic resolver consulted by CLI
// dispatch when the action name under "openapi" is not a static
// registry entry. If slug names a known tag, returns a closure that
// emits the filtered spec in the requested format; otherwise returns
// ok=false so the caller can surface "unknown action" with a hint.
func lookupOpenAPITagAction(slug, format string) (ActionFunc, bool) {
	swagger, err := genapi.GetSwagger()
	if err != nil {
		// Surface the swagger-load failure directly rather than faking
		// "unknown action" — otherwise the user sees an inconsistent
		// error (`unknown action`) here while `cyoda help openapi tags`
		// reports the real swagger-load error for the same underlying
		// condition. The closure reports via w, returns rc=1.
		return func(w io.Writer) int {
			fmt.Fprintf(w, "cyoda help openapi %s: load embedded spec: %v\n", slug, err)
			return 1
		}, true
	}
	found := false
	for _, t := range swagger.Tags {
		if SlugifyTag(t.Name) == slug {
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	return func(w io.Writer) int {
		filtered, err := FilterOpenAPISpecByTag(swagger, slug)
		if err != nil {
			fmt.Fprintf(w, "cyoda help openapi %s: %v\n", slug, err)
			return 1
		}
		return emitFilteredOpenAPI(w, filtered, slug, format)
	}, true
}

// emitFilteredOpenAPI serializes the filtered spec as JSON (default)
// or YAML. YAML round-trips via JSON because openapi3.T carries json
// tags only — same pattern as emitOpenAPIYAML in actions.go.
//
// The RunHelp dispatcher's --format flag defaults to "auto" (chosen
// for topic rendering — text vs markdown); for this action "auto" and
// "" both mean JSON, the only sensible default for an OpenAPI emit.
func emitFilteredOpenAPI(w io.Writer, spec *openapi3.T, slug, format string) int {
	switch format {
	case "", "auto", "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(spec); err != nil {
			fmt.Fprintf(w, "cyoda help openapi %s: encode: %v\n", slug, err)
			return 1
		}
		return 0
	case "yaml":
		raw, err := json.Marshal(spec)
		if err != nil {
			fmt.Fprintf(w, "cyoda help openapi %s: marshal: %v\n", slug, err)
			return 1
		}
		var tree any
		if err := yaml.Unmarshal(raw, &tree); err != nil {
			fmt.Fprintf(w, "cyoda help openapi %s: build tree: %v\n", slug, err)
			return 1
		}
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		if err := enc.Encode(tree); err != nil {
			fmt.Fprintf(w, "cyoda help openapi %s: encode yaml: %v\n", slug, err)
			return 1
		}
		_ = enc.Close()
		return 0
	default:
		fmt.Fprintf(w, "cyoda help openapi %s: unknown format %q (want json or yaml)\n", slug, format)
		return 2
	}
}
