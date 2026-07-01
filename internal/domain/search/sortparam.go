package search

import (
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ParseSortParam parses repeatable `sort` query values into OrderKeys.
// Grammar: [@]path[:asc|:desc]. Bare ⇒ data; leading '@' ⇒ meta (flat name).
// A leading "$." on a data path is tolerated. Direction defaults to asc.
// Duplicate paths and >maxKeys keys are rejected. Semantic validation
// (schema scalar-leaf, meta allowlist) happens later in the service.
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

// isValidSortPath allows dotted identifiers (letters/digits/_/-), no empty
// segments — the same safe subset filters use.
func isValidSortPath(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(p, ".") {
		if seg == "" {
			return false
		}
		for _, c := range seg {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
				c >= '0' && c <= '9', c == '_', c == '-':
			default:
				return false
			}
		}
	}
	return true
}
