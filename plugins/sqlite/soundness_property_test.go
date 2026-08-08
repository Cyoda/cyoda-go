package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// Task 13 — per-backend pushdown soundness property.
//
// The contract under test (query_planner.go's doc comment on sqlPlan): the
// SQL WHERE fragment planQuery produces is a best-effort NARROWING that must
// return a SUPERSET of spi.MatchFilter's true matches (never under-select);
// the kernel re-check (postFilter, applied inside Search/Iterate) then
// narrows that candidate set back down to the exact result. This file proves
// the invariant holds, isolated to the sqlite backend, over a fixed corpus
// and a fixed (deterministic — no randomness) condition table covering
// eq/ne/ordering/between/contains/like/isnull across numeric, string,
// temporal, and polymorphic (multi-Declared-type) fields.
//
// Two assertions per condition:
//  1. candidateIDs (the raw SQL WHERE result, BEFORE any Go-side re-check,
//     via sqlite.SearchCandidateIDsForTest) ⊇ oracleIDs (the kernel's true
//     matches, computed directly via spi.MatchFilter over the corpus — this
//     is exactly what the memory backend's Iterate/Search does, since memory
//     has no SQL layer at all: plugins/memory/searcher.go calls
//     spi.MatchFilter per entity, uncached, unnarrowed). No false negatives
//     survive to the re-check stage.
//  2. store.Search(...) (the FULL pipeline: WHERE narrowing + postFilter
//     kernel re-check) == oracleIDs exactly. This is the "backend result ==
//     memory backend result" equality proxy the task calls out as the
//     acceptable substitute when the raw candidate set isn't accessible —
//     here it is accessible too (assertion 1), so this is additional,
//     stronger evidence, not a substitute.
//
// Temporal PUSH-soundness coverage uses spi.SourceMeta "creationDate"
// (Coercion: CoerceTemporal), NOT a SourceData path. A SourceData temporal
// field IS stamped CoerceTemporal today (model discovery content-sniffs
// ISO-8601 sample strings into a temporal subtype — see schema.InferDataType),
// but a data temporal COMPARISON leaf is deliberately NOT pushed: isLeafPushable
// routes it to the residual so the kernel (which performs temporal-subtype
// resolution the flat epoch-ms push cannot reproduce) is authoritative. It
// therefore never exercises the pushed-WHERE soundness property probed here.
// spi.SourceMeta "creationDate" IS pushed (search.lifecycleToFilter stamps it
// on every LifecycleCondition against a temporal meta field) and its storage
// convention is fixed: the meta JSON blob's creation_date key is a microsecond
// epoch integer (entity_store.go marshalEntityMeta), matching temporalLeafToSQL's
// `col / 1000` assumption — the reachable, pushable temporal shape.
//
// A genuine soundness bug turned up while building this property test (see
// TestSqlitePushdownSoundness_LikeWildcardUnderSelects_KNOWNBUG below) — now
// fixed (FilterLike is residual-only; see that test's doc comment).

// soundnessCorpusRow is one corpus entity. creationDateAdvance is the
// duration to advance the shared TestClock by immediately BEFORE this row is
// saved (0 means "save at the clock's current instant" — used to give two
// rows the exact same creationDate, exercising the temporal boundary case
// the same way the numeric "amount" boundary is exercised). Rows MUST be
// listed in non-decreasing creationDate order — sqlite.TestClock.Advance
// only moves forward.
var soundnessCorpusRows = []struct {
	id                  string
	state               string
	creationDateAdvance time.Duration
	data                map[string]any
}{
	// t0 = 1970-01-01T00:00:00Z (TestClock start, no advance needed).
	{"e5", "available", 0, map[string]any{"amount": 0.0, "name": "", "code": ""}},
	{"e7", "shipped", dateDelta("1970-01-01T00:00:00Z", "2020-01-01T00:00:00Z"), map[string]any{"amount": -50.0, "name": "Negative", "code": "C300"}},
	{"e3", "available", dateDelta("2020-01-01T00:00:00Z", "2021-01-01T00:00:00Z"), map[string]any{"amount": 99.9999999, "name": "Gadget", "code": "B200"}},
	{"e1", "available", dateDelta("2021-01-01T00:00:00Z", "2021-06-15T00:00:00Z"), map[string]any{"amount": 100.0, "name": "Widget", "code": "A100"}},
	{"e8", "available", 0, map[string]any{"amount": 100.0, "name": "Widget", "code": "A100"}}, // same creationDate instant as e1
	{"e2", "available", 1 * time.Millisecond, map[string]any{"amount": 100.0000001, "name": "widget-pro", "code": 42}},
	{"e10", "available", dateDelta("2021-06-15T00:00:00.001Z", "2021-06-15T12:00:00Z"), map[string]any{"amount": 150.0, "name": "MidRange", "code": "D400"}},
	{"e4", "shipped", dateDelta("2021-06-15T12:00:00Z", "2022-12-31T23:59:59Z"), map[string]any{"amount": 250.0, "name": "SuperWidget", "code": 250}},
	{"e9", "shipped", dateDelta("2022-12-31T23:59:59Z", "2023-05-05T05:05:05Z"), map[string]any{"amount": 300.0, "name": "zzTop", "code": 300}},
	{"e6", "available", 0, map[string]any{"name": "NoAmount"}}, // amount/code/creationDate-comparability all absent by design
}

// dateDelta computes the duration between two RFC3339 instants, for
// readable, self-documenting Advance() calls in soundnessCorpusRows above.
// Panics on a malformed literal — these are fixed test-file constants, never
// user input.
func dateDelta(from, to string) time.Duration {
	f, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		panic(err)
	}
	t, err := time.Parse(time.RFC3339Nano, to)
	if err != nil {
		panic(err)
	}
	return t.Sub(f)
}

// newSoundnessStore creates a fresh sqlite store factory backed by a
// TestClock pinned at 1970-01-01T00:00:00Z, so buildSoundnessCorpus can
// assign every entity a deterministic, reproducible meta.CreationDate by
// advancing the clock before each Save. The clock is returned alongside the
// store so the caller can pass the SAME instance into buildSoundnessCorpus.
func newSoundnessStore(t *testing.T) (*sqlite.StoreFactory, spi.EntityStore, context.Context, *sqlite.TestClock) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "soundness_test.db")
	clock := sqlite.NewTestClockAt(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath, sqlite.WithClock(clock))
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	t.Cleanup(func() { factory.Close() })
	ctx := testCtx("tenant-1")
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	return factory, store, ctx, clock
}

// buildSoundnessCorpus seeds soundnessCorpusRows into store (advancing
// clock deterministically per row, per creationDateAdvance) and returns the
// same corpus as []*spi.Entity for the in-process kernel oracle. The
// returned entities' Meta.CreationDate mirrors exactly what Save() assigned
// (both read the same clock instant), so the oracle and the backend agree on
// what "creationDate" means for every row.
func buildSoundnessCorpus(t *testing.T, ctx context.Context, store spi.EntityStore, clock *sqlite.TestClock) []*spi.Entity {
	t.Helper()
	out := make([]*spi.Entity, 0, len(soundnessCorpusRows))
	for _, row := range soundnessCorpusRows {
		if row.creationDateAdvance > 0 {
			clock.Advance(row.creationDateAdvance)
		}
		wantCreation := clock.Now()

		raw, err := json.Marshal(row.data)
		if err != nil {
			t.Fatalf("marshal %s: %v", row.id, err)
		}
		e := &spi.Entity{
			Meta: spi.EntityMeta{ID: row.id, ModelRef: gsModel, State: row.state},
			Data: raw,
		}
		if _, err := store.Save(ctx, e); err != nil {
			t.Fatalf("save %s: %v", row.id, err)
		}
		// The oracle entity's meta must reflect exactly what Save() persisted
		// (CreationDate stamped from the store's clock) so spi.MatchFilter
		// evaluates the SAME creationDate the backend's SQL sees.
		out = append(out, &spi.Entity{
			Meta: spi.EntityMeta{ID: row.id, ModelRef: gsModel, State: row.state, CreationDate: wantCreation},
			Data: raw,
		})
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

// fMetaTemporal builds a SourceMeta creationDate leaf — the reachable-today
// temporal shape (see the file doc comment for why SourceData+CoerceTemporal
// is excluded).
func fMetaTemporal(op spi.FilterOp, value any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceMeta, Path: "creationDate", Value: value, Coercion: spi.CoerceTemporal, Declared: []spi.DataType{spi.ZonedDateTime}}
}

func fMetaTemporalBetween(op spi.FilterOp, lo, hi any) spi.Filter {
	return spi.Filter{Op: op, Source: spi.SourceMeta, Path: "creationDate", Values: []any{lo, hi}, Coercion: spi.CoerceTemporal, Declared: []spi.DataType{spi.ZonedDateTime}}
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

// soundnessConditions is the fixed, deterministic condition table. Every
// entry is built from the table above (no randomness, fully reproducible).
// Coverage: eq/ne (string + polymorphic), ordering (gt/lt/gte/lte, all
// boundary-value stressed), between (exclusive + inclusive, boundary
// stressed), contains/starts_with/ends_with/like, isnull/notnull, temporal
// ordering + between (meta creationDate), and AND/OR combinations mixing an
// EXACT leaf with a SOUND-SUPERSET leaf (to prove group dissection stays
// sound).
var soundnessConditions = []struct {
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
	{"temporal_gt_boundary", fMetaTemporal(spi.FilterGt, "2021-06-15T00:00:00Z")},
	{"temporal_lte_boundary", fMetaTemporal(spi.FilterLte, "2021-06-15T00:00:00Z")},
	{"temporal_between_inclusive_boundary", fMetaTemporalBetween(spi.FilterBetweenInclusive, "2020-01-01T00:00:00Z", "2021-06-15T00:00:00Z")},
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

// oracleIDs computes the TRUE match set directly via spi.MatchFilter over
// the in-process corpus — exactly the memory backend's Iterate/Search
// algorithm (no SQL, no narrowing).
func oracleIDs(corpus []*spi.Entity, f spi.Filter) map[string]bool {
	out := map[string]bool{}
	pf := spi.Prepare(f)
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

// TestSqlitePushdownSoundnessProperty is the Task 13 property test: for
// every condition in soundnessConditions, the raw SQL candidate set must be
// a superset of the kernel's true matches, and the full Search() pipeline
// result must equal the kernel's true matches exactly.
func TestSqlitePushdownSoundnessProperty(t *testing.T) {
	factory, store, ctx, clock := newSoundnessStore(t)
	corpus := buildSoundnessCorpus(t, ctx, store, clock)
	searcher := store.(spi.Searcher)

	for _, tc := range soundnessConditions {
		t.Run(tc.name, func(t *testing.T) {
			oracle := oracleIDs(corpus, tc.f)

			// Assertion 1: SQL pre-recheck candidates ⊇ kernel matches (no
			// under-select survives to the re-check stage).
			candidateIDs, err := sqlite.SearchCandidateIDsForTest(factory, ctx, "tenant-1", gsModel.EntityName, gsModel.ModelVersion, tc.f)
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
			results, err := searcher.Search(ctx, tc.f, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion})
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

// TestSqlitePushdownSoundness_PolymorphicIntStringUnderSelects pins the
// polymorphic-field under-select: a field declared [INTEGER, STRING] holding
// both an int-30 row and a string-"30" row. EQUALS "30" expands (kernel) into
// an int branch (matches int-30) and a string branch (matches string-"30") —
// both rows are true matches. Pre-fix, sqlite bound "30" as TEXT and
// json_extract returned INTEGER 30 for the int row, so `30 = '30'` excluded it
// from the SQL candidate set entirely (before the kernel re-check could see
// it). The fix makes polymorphic comparison leaves residual-only on sqlite, so
// the kernel evaluates all branches and both rows match on every backend.
func TestSqlitePushdownSoundness_PolymorphicIntStringUnderSelects(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "int30", "available", map[string]any{"code": 30})
	gsSave(t, ctx, store, "str30", "available", map[string]any{"code": "30"})
	gsSave(t, ctx, store, "int99", "available", map[string]any{"code": 99})

	filter := fPoly(spi.FilterEq, "code", "30")

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx, filter, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := idSetFromEntities(results)
	want := map[string]bool{"int30": true, "str30": true}
	if len(got) != len(want) || !got["int30"] || !got["str30"] {
		t.Errorf("polymorphic EQUALS \"30\": got %v, want both int-30 and string-\"30\" (under-select if int-30 missing)", sortedKeys(got))
	}
}

// TestSqlitePushdownSoundness_MonomorphicStringNumericOperand pins the
// monomorphic-STRING under-select: a STRING-declared field holding the string
// "30", queried with a numeric-looking operand that arrives as a json.Number.
// Pre-fix, the json.Number bound numerically (30), so `json_extract = 30`
// (numeric) missed the text-stored "30". The fix TEXT-binds on a non-numeric
// declared field, so it matches on every backend.
func TestSqlitePushdownSoundness_MonomorphicStringNumericOperand(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "s30", "available", map[string]any{"code": "30"})
	gsSave(t, ctx, store, "s99", "available", map[string]any{"code": "99"})

	// Declared STRING, operand a json.Number (as the predicate parser produces
	// for a numeric JSON condition value).
	filter := spi.Filter{Op: spi.FilterEq, Source: spi.SourceData, Path: "code", Value: json.Number("30"), Declared: []spi.DataType{spi.String}}

	// Kernel oracle: the string field holds "30", the operand normalizes to the
	// text "30" -> a match.
	if !spi.Prepare(filter).Match([]byte(`{"code":"30"}`), spi.EntityMeta{}) {
		t.Fatalf("test setup invalid: kernel must match STRING \"30\" against numeric-looking operand 30")
	}

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx, filter, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := idSetFromEntities(results)
	if len(got) != 1 || !got["s30"] {
		t.Errorf("monomorphic STRING EQUALS json.Number(30): got %v, want exactly [s30] (under-select if the numeric bind missed the text-stored \"30\")", sortedKeys(got))
	}
}

// TestSqlitePushdownSoundness_LikeWildcardUnderSelects_KNOWNBUG pinned a REAL
// soundness violation discovered while building the property test above:
// plugins/sqlite/query_planner.go's leafToSQL, case spi.FilterLike, escaped
// EVERY '%'/'_' in the operand via escapeLike before binding it to SQL
// `LIKE ? ESCAPE '\'` — turning a genuine wildcard pattern into a literal
// string match at the SQL layer. The kernel (spi.MatchFilter -> eval_leaf.go
// likeToRegex) does the opposite: it treats an unescaped '%' as "match any
// run of characters" and '_' as "match any one character" — the standard
// LIKE-wildcard reading.
//
// Fixed by removing FilterLike from isPushable: Like is now residual-only,
// so the kernel (spi.MatchFilter) evaluates it directly with the correct
// wildcard semantics — no SQL WHERE narrowing, no under-select risk. A sound
// SQL-LIKE translation that aligns SQL LIKE to Cloud's LIKE grammar (so Like
// can be pushed again) is deferred to a dedicated follow-up; leafToSQL's
// FilterLike branch is kept in query_planner.go, unreachable via isPushable
// like Ne, for mirror totality with postgres.
func TestSqlitePushdownSoundness_LikeWildcardUnderSelects_KNOWNBUG(t *testing.T) {
	_, store, ctx := gsNewStore(t)
	gsSave(t, ctx, store, "e1", "available", map[string]any{"desc": "foobarbaz"})

	filter := spi.Filter{Op: spi.FilterLike, Source: spi.SourceData, Path: "desc", Value: "foo%baz"}

	oracle := spi.Prepare(filter).Match([]byte(`{"desc":"foobarbaz"}`), spi.EntityMeta{})
	if !oracle {
		t.Fatalf("test setup invalid: kernel oracle must match wildcard pattern 'foo%%baz' against 'foobarbaz'")
	}

	searcher := store.(spi.Searcher)
	results, err := searcher.Search(ctx, filter, spi.SearchOptions{ModelName: gsModel.EntityName, ModelVersion: gsModel.ModelVersion})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("UNDER-SELECT: kernel matches wildcard pattern 'foo%%baz' against 'foobarbaz', but backend Search() found 0 rows (escapeLike turned the wildcard into a literal-string match)")
	}
}
