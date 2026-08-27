package search

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// ParseSortParam parses repeatable `sort` query values into OrderKeys.
// Grammar: [@]path[:asc|:desc]. Bare ⇒ data; leading '@' ⇒ meta (flat name).
// A leading "$." on a data path is tolerated. Direction defaults to asc.
// Duplicate paths and >maxKeys keys are rejected. The path GRAMMAR is checked
// here too, but it is not this parser's to own: resolveOrderBy applies it to
// every key whatever the transport, because gRPC builds an OrderKey without
// passing through here. Semantic validation (schema scalar-leaf, meta
// allowlist) happens there as well.
func ParseSortParam(values []string, maxKeys int) ([]OrderKey, error) {
	keys := make([]OrderKey, 0, len(values))
	for _, raw := range values {
		k, err := parseSortToken(raw)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return capAndDedupOrderKeys(keys, maxKeys)
}

// capAndDedupOrderKeys enforces the per-request sort-key cap and rejects
// duplicate keys (same source+path). Shared by the HTTP grammar parser and
// the service-layer resolver so every entry point (HTTP, gRPC, sync, async)
// is bounded uniformly.
func capAndDedupOrderKeys(keys []OrderKey, maxKeys int) ([]OrderKey, error) {
	if len(keys) > maxKeys {
		return nil, fmt.Errorf("too many sort keys: %d (max %d)", len(keys), maxKeys)
	}
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		dedup := string(k.Source) + ":" + k.Path
		if _, dup := seen[dedup]; dup {
			return nil, fmt.Errorf("duplicate sort key: %q", k.Path)
		}
		seen[dedup] = struct{}{}
	}
	return keys, nil
}

func parseSortToken(raw string) (OrderKey, error) {
	tok := raw
	desc := false
	if i := strings.LastIndexByte(tok, ':'); i >= 0 {
		switch tok[i+1:] {
		case "asc":
			desc = false
		case "desc":
			desc = true
		default:
			return OrderKey{}, fmt.Errorf("invalid sort direction in %q", raw)
		}
		tok = tok[:i]
	}
	source := spi.SourceData
	if strings.HasPrefix(tok, "@") {
		source = spi.SourceMeta
		tok = tok[1:]
		if strings.ContainsRune(tok, '.') {
			return OrderKey{}, fmt.Errorf("meta sort field must be a flat name: %q", raw)
		}
	} else {
		tok = strings.TrimPrefix(tok, "$.")
	}
	if tok == "" {
		return OrderKey{}, fmt.Errorf("empty sort path in %q", raw)
	}
	if !isValidSortPath(tok) {
		return OrderKey{}, fmt.Errorf("malformed sort path: %q", raw)
	}
	return OrderKey{Path: tok, Source: source, Desc: desc}, nil
}

// isValidSortPath allows dotted segments, no empty ones, each drawn from the
// one segment charset [schema.IsSegmentName] defines — the same charset the
// jsonPath grammar and model import hold field names to, so a field is
// sortable exactly when it is addressable.
func isValidSortPath(p string) bool {
	for _, seg := range strings.Split(p, ".") {
		if !schema.IsSegmentName(seg) {
			return false
		}
	}
	return true
}
