package match_test

// Cross-module parity smoke test: pins the contract that
// internal/match.MatchFilter and plugins/sqlite.EvaluateFilter agree on
// the same (filter, data, meta) tuple. Drift between the two means
// grouped-stats / streaming-tally / Iterate results would silently
// disagree across backends.
//
// This is NOT an exhaustive parity matrix — that lives in the
// grouped-stats E2E parity suite. This smoke test focuses on cases
// where drift is plausible:
//   - numeric ordering on stringly-typed numeric data (string→float
//     coercion divergence)
//   - LIKE patterns crossing multibyte characters (rune vs byte
//     semantics divergence)
//   - the standard structural cases (Eq, IsNull, NotNull, AND, OR,
//     nested, SourceState, SourceMeta) so a future refactor in either
//     evaluator can't silently break the simple cases either.
//
// The test file lives in the root module (next to MatchFilter) and
// imports plugins/sqlite via its public EvaluateFilter wrapper. The
// sqlite plugin is a separate Go module — internal/match cannot import
// it directly, but a _test.go file may, because root module tests can
// see all transitive deps.

import (
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/match"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func mustParityJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return b
}

func TestMatchFilter_SqliteParity_Smoke(t *testing.T) {
	// A shared meta used across SourceMeta cases.
	meta := spi.EntityMeta{
		ID:               "ent-1",
		State:            "available",
		Version:          7,
		CreationDate:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		LastModifiedDate: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		ChangeType:       "UPDATED",
	}

	// Data fixtures.
	scalar := mustParityJSON(t, map[string]any{
		"variantId": "v1",
		"qty":       42,
		"color":     "red",
	})
	stringyNumeric := mustParityJSON(t, map[string]any{
		"qty": "100",
	})
	multibyte := mustParityJSON(t, map[string]any{
		"name":   "café",
		"leader": "éaf", // leading multibyte rune — exposes byte-vs-rune _ divergence
	})
	withNull := mustParityJSON(t, map[string]any{
		"a": "x",
		"b": nil,
	})

	cases := []struct {
		name string
		f    spi.Filter
		data []byte
		meta spi.EntityMeta
	}{
		// --- Issue 1: numeric ordering on stringly-typed numeric data ---
		// Sqlite returns false (byte-lex "100" vs "42" → "1" < "4"); MatchFilter
		// must agree. Before the fix MatchFilter parsed strings → returned true.
		{
			name: "Gt-on-stringly-numeric",
			f:    spi.Filter{Op: spi.FilterGt, Path: "qty", Source: spi.SourceData, Value: 42},
			data: stringyNumeric,
		},
		{
			name: "Lt-on-stringly-numeric",
			f:    spi.Filter{Op: spi.FilterLt, Path: "qty", Source: spi.SourceData, Value: 42},
			data: stringyNumeric,
		},
		{
			name: "Gte-on-stringly-numeric",
			f:    spi.Filter{Op: spi.FilterGte, Path: "qty", Source: spi.SourceData, Value: 100},
			data: stringyNumeric,
		},
		{
			name: "Between-on-stringly-numeric",
			f:    spi.Filter{Op: spi.FilterBetween, Path: "qty", Source: spi.SourceData, Values: []any{10, 200}},
			data: stringyNumeric,
		},

		// --- Issue 2: LIKE crossing multibyte characters ---
		// "café" has a 2-byte 'é'. Sqlite uses byte-based matchLike, so a
		// pattern like "ca_é" with one underscore can NEVER match because the
		// underscore consumes one byte but "f" is one byte and "é" is two
		// bytes — alignment depends on the exact pattern.
		{
			name: "Like-multibyte-underscore-aligns",
			f:    spi.Filter{Op: spi.FilterLike, Path: "name", Source: spi.SourceData, Value: "ca__%"},
			data: multibyte,
		},
		{
			name: "Like-multibyte-rune-vs-byte-divergence",
			f:    spi.Filter{Op: spi.FilterLike, Path: "name", Source: spi.SourceData, Value: "ca_é"},
			data: multibyte,
		},
		{
			// Hard divergence case: leader is "éaf" (UTF-8 c3 a9 61 66 = 4 bytes).
			// Rune-based "_af": '.' matches rune 'é', "af" matches → true.
			// Byte-based "_af": '_' consumes byte c3, then expects 'a'=0x61 but
			// next byte is a9 → false. Both evaluators must agree (false).
			name: "Like-leading-multibyte-underscore-divergence",
			f:    spi.Filter{Op: spi.FilterLike, Path: "leader", Source: spi.SourceData, Value: "_af"},
			data: multibyte,
		},
		{
			name: "Like-ascii-percent",
			f:    spi.Filter{Op: spi.FilterLike, Path: "name", Source: spi.SourceData, Value: "ca%"},
			data: multibyte,
		},

		// --- Standard scalar cases ---
		{
			name: "Eq-string",
			f:    spi.Filter{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
			data: scalar,
		},
		{
			name: "Eq-numeric",
			f:    spi.Filter{Op: spi.FilterEq, Path: "qty", Source: spi.SourceData, Value: 42},
			data: scalar,
		},
		{
			name: "Ne-string",
			f:    spi.Filter{Op: spi.FilterNe, Path: "variantId", Source: spi.SourceData, Value: "vX"},
			data: scalar,
		},
		{
			name: "Contains",
			f:    spi.Filter{Op: spi.FilterContains, Path: "color", Source: spi.SourceData, Value: "ed"},
			data: scalar,
		},
		{
			name: "StartsWith",
			f:    spi.Filter{Op: spi.FilterStartsWith, Path: "color", Source: spi.SourceData, Value: "re"},
			data: scalar,
		},
		{
			name: "EndsWith",
			f:    spi.Filter{Op: spi.FilterEndsWith, Path: "color", Source: spi.SourceData, Value: "ed"},
			data: scalar,
		},

		// --- Null semantics ---
		{
			name: "IsNull-missing-field",
			f:    spi.Filter{Op: spi.FilterIsNull, Path: "missing", Source: spi.SourceData},
			data: scalar,
		},
		{
			name: "IsNull-explicit-null",
			f:    spi.Filter{Op: spi.FilterIsNull, Path: "b", Source: spi.SourceData},
			data: withNull,
		},
		{
			name: "NotNull-present",
			f:    spi.Filter{Op: spi.FilterNotNull, Path: "a", Source: spi.SourceData},
			data: withNull,
		},
		{
			name: "NotNull-missing",
			f:    spi.Filter{Op: spi.FilterNotNull, Path: "missing", Source: spi.SourceData},
			data: withNull,
		},
		{
			name: "Eq-against-null-field-missing-short-circuits-false",
			f:    spi.Filter{Op: spi.FilterEq, Path: "missing", Source: spi.SourceData, Value: "anything"},
			data: scalar,
		},
		{
			name: "Ne-against-missing-field-vacuously-true",
			f:    spi.Filter{Op: spi.FilterNe, Path: "missing", Source: spi.SourceData, Value: "anything"},
			data: scalar,
		},

		// --- Groups ---
		{
			name: "AND-all-match",
			f: spi.Filter{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
					{Op: spi.FilterGt, Path: "qty", Source: spi.SourceData, Value: 10},
				},
			},
			data: scalar,
		},
		{
			name: "AND-one-fails",
			f: spi.Filter{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
					{Op: spi.FilterGt, Path: "qty", Source: spi.SourceData, Value: 100},
				},
			},
			data: scalar,
		},
		{
			name: "OR-one-matches",
			f: spi.Filter{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "vX"},
					{Op: spi.FilterEq, Path: "color", Source: spi.SourceData, Value: "red"},
				},
			},
			data: scalar,
		},
		{
			name: "OR-all-fail",
			f: spi.Filter{
				Op: spi.FilterOr,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "vX"},
					{Op: spi.FilterEq, Path: "color", Source: spi.SourceData, Value: "blue"},
				},
			},
			data: scalar,
		},
		{
			name: "Nested-AND-OR",
			f: spi.Filter{
				Op: spi.FilterAnd,
				Children: []spi.Filter{
					{Op: spi.FilterEq, Path: "variantId", Source: spi.SourceData, Value: "v1"},
					{
						Op: spi.FilterOr,
						Children: []spi.Filter{
							{Op: spi.FilterGt, Path: "qty", Source: spi.SourceData, Value: 100},
							{Op: spi.FilterEq, Path: "color", Source: spi.SourceData, Value: "red"},
						},
					},
				},
			},
			data: scalar,
		},

		// --- SourceMeta ---
		{
			name: "Meta-state-eq",
			f:    spi.Filter{Op: spi.FilterEq, Path: "state", Source: spi.SourceMeta, Value: "available"},
			meta: meta,
		},
		{
			name: "Meta-state-ne",
			f:    spi.Filter{Op: spi.FilterEq, Path: "state", Source: spi.SourceMeta, Value: "shipped"},
			meta: meta,
		},
		{
			name: "Meta-entity_id-eq",
			f:    spi.Filter{Op: spi.FilterEq, Path: "entity_id", Source: spi.SourceMeta, Value: "ent-1"},
			meta: meta,
		},
		{
			name: "Meta-change_type-eq",
			f:    spi.Filter{Op: spi.FilterEq, Path: "change_type", Source: spi.SourceMeta, Value: "UPDATED"},
			meta: meta,
		},
		{
			name: "Meta-version-gt",
			f:    spi.Filter{Op: spi.FilterGt, Path: "version", Source: spi.SourceMeta, Value: int64(5)},
			meta: meta,
		},

		// --- Case-insensitive ops ---
		{
			name: "IEq-match",
			f:    spi.Filter{Op: spi.FilterIEq, Path: "color", Source: spi.SourceData, Value: "RED"},
			data: scalar,
		},
		{
			name: "IContains-match",
			f:    spi.Filter{Op: spi.FilterIContains, Path: "color", Source: spi.SourceData, Value: "ED"},
			data: scalar,
		},

		// --- Empty groups (identity elements) ---
		{
			name: "Empty-AND-tautology",
			f:    spi.Filter{Op: spi.FilterAnd},
			data: scalar,
		},
		{
			name: "Empty-OR-contradiction",
			f:    spi.Filter{Op: spi.FilterOr},
			data: scalar,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			if data == nil {
				data = []byte("{}")
			}
			ent := &spi.Entity{Meta: tc.meta, Data: data}

			sqliteRes, err := sqlite.EvaluateFilter(tc.f, ent)
			if err != nil {
				t.Fatalf("sqlite.EvaluateFilter errored: %v", err)
			}
			matchRes := match.MatchFilter(tc.f, data, tc.meta)

			if sqliteRes != matchRes {
				t.Fatalf("PARITY DRIFT: sqlite=%v match=%v\n  filter=%+v", sqliteRes, matchRes, tc.f)
			}
		})
	}
}
