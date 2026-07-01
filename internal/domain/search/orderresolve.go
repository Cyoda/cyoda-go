package search

import (
	"fmt"

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
		fd, ok := fields["$."+k.Path]
		if !ok {
			return nil, fmt.Errorf("unknown sort field: %q", k.Path)
		}
		if fd.IsArray {
			return nil, fmt.Errorf("cannot sort by array field: %q", k.Path)
		}
		kind, err := classifyType(fd.Types)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k.Path, err)
		}
		specs = append(specs, spi.OrderSpec{Path: k.Path, Source: spi.SourceData, Desc: k.Desc, Kind: kind})
	}
	return specs, nil
}
