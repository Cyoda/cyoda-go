package search

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// resolveOrderBy validates each OrderKey and attaches its ordering class,
// producing the typed OrderSpecs the plugins/comparator consume. Data keys
// must be a scalar (non-array) leaf in the model schema; meta keys must be in
// the canonical allowlist. Any failure returns an error the caller maps to
// 400 INVALID_FIELD_PATH.
func resolveOrderBy(keys []OrderKey, fields map[string]schema.FieldDescriptor) ([]spi.OrderSpec, error) {
	specs := make([]spi.OrderSpec, 0, len(keys))
	for _, k := range keys {
		if k.Source == spi.SourceMeta {
			mf, ok := resolveMetaField(k.Path)
			if !ok {
				return nil, fmt.Errorf("unknown meta sort field: %q", k.Path)
			}
			specs = append(specs, spi.OrderSpec{Path: mf.Path, Source: mf.Source, Desc: k.Desc, Kind: mf.Kind})
			continue
		}
		// normalisePath, not a manual prepend: the HTTP parser strips "$." before
		// building the key (sortparam.go) but gRPC passes the client's path
		// through verbatim, so "$.city" from a gRPC caller became "$.$.city" and
		// a 400 for a field that exists — the two transports disagreeing on the
		// same request.
		key := normalisePath(k.Path)
		// Hold the path to the SCALAR grammar before asking the schema about
		// it. Schema membership alone is not the boundary: a scalar leaf
		// inside an array of objects is recorded under the wildcard key with
		// IsArray FALSE (schema.collectFields recurses into an array's object
		// element with inArray=false), so "$.items[*].name" passed both the
		// lookup and the array guard below. The HTTP parser cannot spell it —
		// isValidSortPath refuses "[" — but gRPC builds an OrderKey from the
		// client's path verbatim, so the two transports answered the same
		// request differently. And an accepted projection has nowhere good to
		// go: the pushdown branch is refused by each plugin's own path
		// validator (400, plus a WARN that the boundary and the plugin
		// disagree), while the in-memory branch hands it to gjson, which has
		// no bracket syntax — every entity misses, all compare equal, and the
		// caller gets 200 with results that are simply not sorted. A
		// wrong-but-available answer is the one outcome this engine does not
		// give.
		//
		// ValidateScalarJSONPath is the check groupBy and aggregation fields
		// take, for the same reason: a projection cannot denote the single
		// scalar an ordering needs. It is applied to the NORMALISED path, so
		// a sort key keeps its bare spelling ("price") where those surfaces
		// require the "$." leader — the leader is the one point on which a
		// sort key differs, and normalisePath supplies it. The diagnostic
		// therefore names the "$."-prefixed form; only a gRPC caller can
		// reach this arm, and HTTP's own parser refuses these paths earlier.
		if err := ValidateScalarJSONPath(key); err != nil {
			return nil, err
		}
		fd, ok := fields[key]
		if !ok {
			return nil, fmt.Errorf("unknown sort field: %q", k.Path)
		}
		// Backstop. Every IsArray:true descriptor is keyed "...[*]"
		// (schema.collectFields sets the flag only for an array of leaves),
		// so the grammar check above already refused that spelling. It stays
		// because the flag, not the spelling, is what "you cannot sort by this
		// field" means — a future FieldsMap that keys an array some other way
		// must not become sortable by omission.
		if fd.IsArray {
			return nil, fmt.Errorf("cannot sort by array field: %q", k.Path)
		}
		// SORT uses sortKindForData, not classifyType: a data-temporal field
		// sorts lexically (OrderText) — its ISO-8601 bytes are chronological and
		// byte-identical across backends — while the FILTER path keeps
		// OrderTemporal via classifyType. Decoupling here is what fixes the
		// cross-backend ORDER BY divergence for data-temporal fields.
		kind, err := sortKindForData(fd.Types)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k.Path, err)
		}
		// Emit the prefix-stripped form the plugins' path validators accept —
		// they reject "$" as an identifier byte. HTTP already stripped before
		// building the key; gRPC did not, so strip here for both.
		specs = append(specs, spi.OrderSpec{Path: strings.TrimPrefix(key, "$."), Source: spi.SourceData, Desc: k.Desc, Kind: kind})
	}
	return specs, nil
}
