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
		fd, ok := fields[key]
		if !ok {
			return nil, fmt.Errorf("unknown sort field: %q", k.Path)
		}
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
