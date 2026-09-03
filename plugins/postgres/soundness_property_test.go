package postgres_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// Task 13 — per-backend pushdown soundness property (postgres, Docker-gated
// via gsNewStore -> setupEntityTest -> testDBURL).
//
// Mirrors plugins/sqlite/soundness_property_test.go's contract exactly: the
// SQL WHERE fragment planQuery produces must return a SUPERSET of
// spi.Prepare/PreparedFilter.Match's true matches (never under-select); the
// postFilter kernel re-check then narrows that candidate set back down to
// the exact result. See that file's doc comment for the full rationale
// (superset assertion + "backend result == memory backend result" equality
// proxy, and why temporal PUSH-soundness coverage uses SourceMeta
// "creationDate" rather than a SourceData path — a data temporal comparison
// is routed to the residual by isLeafPushable, so it never exercises a
// pushed WHERE fragment).
//
// Determinism note: unlike sqlite (which has an injectable Clock —
// sqlite.TestClock — for exact-instant control), postgres stamps
// meta.CreationDate from the database's own CURRENT_TIMESTAMP
// (entity_store.go), which is not test-injectable. The corpus below is
// therefore seeded with small sleeps to guarantee a strict creationDate
// ordering, and the oracle is built by READING BACK each entity's persisted
// Meta.CreationDate via store.Get — so the oracle always compares against
// the exact value the backend itself will compare against, with zero
// hand-alignment guesswork. The temporal boundary condition below uses one
// corpus entity's own (real, read-back) creationDate as the operand, which
// is exactly on the SOUND-SUPERSET boundary (Gt relaxed to >=) the same way
// the "amount" numeric conditions are boundary-stressed with a
// caller-controlled value.
var soundnessCorpusRows = []struct {
	id    string
	state string
	data  map[string]any
}{
	{"e5", "available", map[string]any{"amount": 0.0, "name": "", "code": ""}},
	{"e7", "shipped", map[string]any{"amount": -50.0, "name": "Negative", "code": "C300"}},
	{"e3", "available", map[string]any{"amount": 99.9999999, "name": "Gadget", "code": "B200"}},
	{"e1", "available", map[string]any{"amount": 100.0, "name": "Widget", "code": "A100"}},
	{"e8", "available", map[string]any{"amount": 100.0, "name": "Widget", "code": "A100"}},
	{"e2", "available", map[string]any{"amount": 100.0000001, "name": "widget-pro", "code": 42}},
	{"e10", "available", map[string]any{"amount": 150.0, "name": "MidRange", "code": "D400"}},
	{"e4", "shipped", map[string]any{"amount": 250.0, "name": "SuperWidget", "code": 250}},
	{"e9", "shipped", map[string]any{"amount": 300.0, "name": "zzTop", "code": 300}},
	{"e6", "available", map[string]any{"name": "NoAmount"}},
}

// buildSoundnessCorpus seeds soundnessCorpusRows into store (sleeping
// briefly between saves to guarantee a strict creationDate order) and
// returns the corpus as []*spi.Entity, read back via store.Get so the
// in-process kernel oracle sees the EXACT Meta.CreationDate the backend
// persisted (not an estimate).
func buildSoundnessCorpus(t *testing.T, ctx context.Context, store spi.EntityStore) []*spi.Entity {
	t.Helper()
	for i, row := range soundnessCorpusRows {
		if i > 0 {
			time.Sleep(2 * time.Millisecond)
		}
		gsSave(t, ctx, store, row.id, row.state, row.data)
	}
	out := make([]*spi.Entity, 0, len(soundnessCorpusRows))
	for _, row := range soundnessCorpusRows {
		e, err := store.Get(ctx, row.id)
		if err != nil {
			t.Fatalf("Get %s: %v", row.id, err)
		}
		out = append(out, e)
	}
	return out
}

// --- filter builders (deterministic condition-table helpers) ---

func fStr(op spi.FilterOp, path string, value any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path, Value: value, Declared: []spi.DataType{spi.String}}
}

func fNum(op spi.FilterOp, path string, value any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path, Value: value, Declared: []spi.DataType{spi.Double}}
}

func fNumBetween(op spi.FilterOp, path string, lo, hi any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path, Values: []any{lo, hi}, Declared: []spi.DataType{spi.Double}}
}

// fMetaTemporal builds a SourceMeta creationDate leaf — the pushable temporal
// shape. See the file doc comment for why a SourceData temporal comparison is
// excluded from push-soundness coverage (isLeafPushable routes it to the
// residual, so it never produces a pushed WHERE fragment to probe).
func fMetaTemporal(op spi.FilterOp, value any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceMeta, Path: "creationDate", Value: value, Coercion: spi.CoerceTemporal, Declared: []spi.DataType{spi.ZonedDateTime}}
}

// fPoly builds a leaf against the polymorphic "code" field, which holds a
// mix of string and integer values across the corpus (union type — Declared
// carries BOTH candidate types so the kernel's type-directed EvalLeaf can
// engage whichever bucket the stored value and operand agree on; see
// eval_leaf.go bucketDeclared/expandCompare).
func fPoly(op spi.FilterOp, path string, value any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path, Value: value, Declared: []spi.DataType{spi.Integer, spi.String}}
}

// fStringOp builds a Contains/StartsWith/EndsWith/Like leaf. These ops
// ignore Declared entirely (kindStringOp in eval_leaf.go) — no need to set it.
func fStringOp(op spi.FilterOp, path string, value string) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path, Value: value}
}

func fUnary(op spi.FilterOp, path string) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceData, Path: path}
}

// oracleIDs computes the TRUE match set directly via
// spi.Prepare/PreparedFilter.Match over the in-process corpus — exactly the
// memory backend's Iterate/Search algorithm (no SQL, no narrowing).
func oracleIDs(t *testing.T, corpus []*spi.Entity, f spi.Filter) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	pf, err := spi.Prepare(f)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	for _, e := range corpus {
		if pf.Match(e.Data, e.Meta) {
			out[e.Meta.ID] = true
		}
	}
	return out
}

func idSetFromEntities(es []*spi.Entity) map[string]bool {
	out := make(map[string]bool, len(es))
	for _, e := range es {
		out[e.Meta.ID] = true
	}
	return out
}

func idSetFromStrings(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findByID returns the corpus entity with the given ID, failing the test if
// absent — used to pull a real, backend-persisted timestamp for the
// temporal boundary condition.
func findByID(t *testing.T, corpus []*spi.Entity, id string) *spi.Entity {
	t.Helper()
	for _, e := range corpus {
		if e.Meta.ID == id {
			return e
		}
	}
	t.Fatalf("corpus entity %q not found", id)
	return nil
}

// TestPostgresPushdownSoundnessProperty is the Task 13 property test: for
// every condition in the table below, the raw SQL candidate set must be a
// superset of the kernel's true matches, and the full Search() pipeline
// result must equal the kernel's true matches exactly.
func TestPostgresPushdownSoundnessProperty(t *testing.T) {
	factory, store, ctx := gsNewStore(t)
	corpus := buildSoundnessCorpus(t, ctx, store)
	searcher := store
	pool := postgres.PoolForTest(factory)

	// e1's own persisted creationDate — an exact SOUND-SUPERSET boundary
	// operand (Gt relaxed to >=; e1 sits exactly on it), mirroring how the
	// numeric "amount" conditions below stress amount==100.0 exactly.
	e1CreationDate := findByID(t, corpus, "e1").Meta.CreationDate.Format(time.RFC3339Nano)

	conditions := []struct {
		name string
		f    spi.Filter
	}{
		{"eq_string", fStr(spi.FilterEq, "name", "Widget")},
		{"ne_string", fStr(spi.FilterNe, "name", "Widget")},
		{"gt_boundary", fNum(spi.FilterGt, "amount", 100.0)},
		{"lt_boundary", fNum(spi.FilterLt, "amount", 100.0)},
		{"gte_boundary", fNum(spi.FilterGte, "amount", 100.0)},
		{"lte_boundary", fNum(spi.FilterLte, "amount", 100.0)},
		{"between_exclusive_boundary", fNumBetween(spi.FilterBetween, "amount", 0.0, 100.0)},
		{"between_inclusive_boundary", fNumBetween(spi.FilterBetweenInclusive, "amount", 0.0, 100.0)},
		{"contains_substring", fStringOp(spi.FilterContains, "name", "idget")},
		{"starts_with", fStringOp(spi.FilterStartsWith, "name", "Wi")},
		{"ends_with", fStringOp(spi.FilterEndsWith, "name", "get")},
		{"like_literal_no_wildcard", fStringOp(spi.FilterLike, "name", "Widget")},
		{"isnull_amount", fUnary(spi.FilterIsNull, "amount")},
		{"notnull_amount", fUnary(spi.FilterNotNull, "amount")},
		{"eq_polymorphic_string_variant", fPoly(spi.FilterEq, "code", "A100")},
		{"eq_polymorphic_number_variant", fPoly(spi.FilterEq, "code", json.Number("250"))},
		{"temporal_gt_boundary", fMetaTemporal(spi.FilterGt, e1CreationDate)},
		{"temporal_lte_boundary", fMetaTemporal(spi.FilterLte, e1CreationDate)},
		{
			"and_exact_and_sound_superset",
			spi.Filter{Op: spi.FilterAnd, Children: []spi.Filter{
				fUnary(spi.FilterNotNull, "amount"),
				fNum(spi.FilterGt, "amount", 100.0),
			}},
		},
		{
			"or_fully_pushable_not_exact",
			spi.Filter{Op: spi.FilterOr, Children: []spi.Filter{
				fStr(spi.FilterEq, "name", "Widget"),
				fNum(spi.FilterGt, "amount", 200.0),
			}},
		},
		{
			"or_with_nonpushable_child_forces_full_residual",
			spi.Filter{Op: spi.FilterOr, Children: []spi.Filter{
				fStr(spi.FilterEq, "name", "Widget"),
				fNum(spi.FilterNe, "amount", 100.0),
			}},
		},
	}

	for _, tc := range conditions {
		t.Run(tc.name, func(t *testing.T) {
			oracle := oracleIDs(t, corpus, tc.f)

			// Assertion 1: SQL pre-recheck candidates ⊇ kernel matches (no
			// under-select survives to the re-check stage).
			candidateIDs, err := postgres.SearchCandidateIDsForTest(pool, ctx, "gs-tenant", gsModel.EntityName, gsModel.ModelVersion, tc.f)
			if err != nil {
				t.Fatalf("SearchCandidateIDsForTest: %v", err)
			}
			candidates := idSetFromStrings(candidateIDs)
			for id := range oracle {
				if !candidates[id] {
					t.Errorf("UNDER-SELECT: kernel matches %q but the raw SQL candidate set does not contain it\n  filter=%+v\n  oracle=%v\n  candidates=%v",
						id, tc.f, sortedKeys(oracle), sortedKeys(candidates))
				}
			}

			// Assertion 2: full pipeline (WHERE + postFilter re-check) == kernel
			// matches exactly — the "backend result == memory backend result"
			// equality proxy.
			results, err := searcher.Search(ctx, tc.f, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion, Limit: len(soundnessCorpusRows) + 1})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			actual := idSetFromEntities(results)
			for id := range oracle {
				if !actual[id] {
					t.Errorf("UNDER-SELECT (survived re-check): kernel matches %q, backend Search() did not return it\n  filter=%+v", id, tc.f)
				}
			}
			for id := range actual {
				if !oracle[id] {
					t.Errorf("OVER-SELECT (re-check failed to narrow): backend Search() returned %q, kernel does not match it\n  filter=%+v", id, tc.f)
				}
			}
		})
	}
}

// TestPostgresPushdownSoundness_EndsWithUnderSelects_KNOWNBUG pinned a SECOND,
// independent, and more severe real soundness violation discovered while
// building the property test above: plugins/postgres/query_planner.go's
// leafToSQL, case spi.FilterEndsWith, pushed
// `substr(col, -length(value)) = value` — copying sqlite's negative-substr
// idiom verbatim. In SQLite, substr(X, -N) means "the last N characters",
// so this works there. In PostgreSQL, substr's semantics with a negative (or
// non-positive) start position are ENTIRELY DIFFERENT: substr does not count
// from the end at all — a non-positive start simply shifts the (1-indexed)
// window to include characters before position 1, which for a call with no
// length argument returns the ENTIRE remaining string starting at position 1
// (clamped). Concretely: substr('Widget', -3) returned 'Widget' in postgres,
// not 'get'. So the pushed condition degenerated to (roughly) `col = value`,
// which is false for any stored value longer than the ENDS_WITH suffix —
// i.e. FilterEndsWith on postgres returned essentially ZERO rows for any
// real-world use, a total failure of the operator, not a boundary-only
// under-select.
//
// Fixed by switching the pushed SQL to `right(col, char_length($N)) = $N`
// — postgres's actual "last N characters" primitive — mirroring how
// StartsWith binds its length argument once and its comparison value once.
// FilterEndsWith stays pushable and SOUND-SUPERSET (still re-checked by the
// kernel for non-text stringification), now correctly.
func TestPostgresPushdownSoundness_EndsWithUnderSelects_KNOWNBUG(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "e1", "available", map[string]any{"name": "Widget"})

	filter := spi.Filter{Op: spi.FilterEndsWith, Source: spi.SourceData, Path: "name", Value: "get"}

	oraclePF, err := spi.Prepare(filter)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	oracle := oraclePF.Match([]byte(`{"name":"Widget"}`), spi.EntityMeta{})
	if !oracle {
		t.Fatalf("test setup invalid: kernel oracle must match ENDS_WITH 'get' against 'Widget'")
	}

	searcher := store
	results, err := searcher.Search(ctx, filter, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion, Limit: len(soundnessCorpusRows) + 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("UNDER-SELECT: kernel matches ENDS_WITH 'get' against 'Widget', but backend Search() found 0 rows (postgres substr(col, -N) does not mean \"last N characters\")")
	}
}

// TestPostgresPushdownSoundness_LikeWildcardUnderSelects_KNOWNBUG mirrors
// plugins/sqlite/soundness_property_test.go's KNOWNBUG test: pinned the same
// REAL soundness violation for postgres. plugins/postgres/query_planner.go's
// leafToSQL, case spi.FilterLike, had the byte-for-byte identical
// escapeLike() call escaping every '%'/'_' before binding to
// `LIKE $N ESCAPE '\'` — turning a genuine wildcard pattern into a literal
// string match at the SQL layer, while the kernel
// (spi.Prepare/PreparedFilter.Match -> like_pattern.go) treats those
// characters as real wildcards.
//
// Fixed identically to sqlite: FilterLike removed from isPushable, so Like
// is now residual-only and the kernel evaluates it directly with the
// correct wildcard semantics. See the sqlite KNOWNBUG test's doc comment
// for the full argument.
func TestPostgresPushdownSoundness_LikeWildcardUnderSelects_KNOWNBUG(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "e1", "available", map[string]any{"desc": "foobarbaz"})

	filter := spi.Filter{Op: spi.FilterLike, Source: spi.SourceData, Path: "desc", Value: "foo%baz"}

	oraclePF, err := spi.Prepare(filter)
	if err != nil {
		t.Fatalf("spi.Prepare: %v", err)
	}
	oracle := oraclePF.Match([]byte(`{"desc":"foobarbaz"}`), spi.EntityMeta{})
	if !oracle {
		t.Fatalf("test setup invalid: kernel oracle must match wildcard pattern 'foo%%baz' against 'foobarbaz'")
	}

	searcher := store
	results, err := searcher.Search(ctx, filter, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion, Limit: len(soundnessCorpusRows) + 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("UNDER-SELECT: kernel matches wildcard pattern 'foo%%baz' against 'foobarbaz', but backend Search() found 0 rows (escapeLike turned the wildcard into a literal-string match)")
	}
}
