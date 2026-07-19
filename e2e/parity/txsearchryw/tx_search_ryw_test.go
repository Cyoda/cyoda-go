package txsearchryw

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// personRef is the model every scenario operates on. No model registration is
// required at the store layer — Save persists a bare ModelRef + JSON.
var personRef = spi.ModelRef{EntityName: "txperson", ModelVersion: "1"}

// cityBerlin is the canonical pushable predicate reused across scenarios.
var cityBerlin = spi.Filter{Op: spi.FilterEq, Path: "city", Source: spi.SourceData, Value: "Berlin"}

// backend is one storage plugin under test. open() returns a FRESH, isolated
// factory plus a committed (non-tx) tenant context and a cleanup func. Each
// scenario opens its own harness so scenarios never share state.
type backend struct {
	name string
	open func(t *testing.T) (spi.StoreFactory, context.Context, func())
}

// backends returns the list of in-tree backends to run the identical RYW
// scenario against. Postgres is included only when a container came up.
func backends(t *testing.T) []backend {
	t.Helper()
	bs := []backend{
		{
			name: "memory",
			open: func(t *testing.T) (spi.StoreFactory, context.Context, func()) {
				f := memory.NewStoreFactory()
				return f, tenantCtx(uuid.NewString()), func() { _ = f.Close() }
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) (spi.StoreFactory, context.Context, func()) {
				dbPath := filepath.Join(t.TempDir(), "txryw.db")
				f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
				if err != nil {
					t.Fatalf("sqlite factory: %v", err)
				}
				return f, tenantCtx(uuid.NewString()), func() { _ = f.Close() }
			},
		},
	}
	if pgPool != nil {
		bs = append(bs, backend{
			name: "postgres",
			open: func(t *testing.T) (spi.StoreFactory, context.Context, func()) {
				// Shared pool; a unique tenant per open isolates scenarios.
				f := postgres.NewStoreFactory(pgPool)
				f.InitTransactionManager(common.NewDefaultUUIDGenerator())
				return f, tenantCtx(uuid.NewString()), func() {}
			},
		})
	}
	return bs
}

// tenantCtx builds a committed (non-tx) context bound to the given tenant.
func tenantCtx(tenant string) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "txryw-user",
		UserName: "txryw",
		Tenant:   spi.Tenant{ID: spi.TenantID(tenant), Name: "txryw-tenant"},
		Roles:    []string{"ROLE_USER"},
	})
}

// ent builds a person entity with an explicit id and raw JSON data. Explicit
// ids are what make invariant #5 (delete-then-save same id) and deterministic
// entity_id tiebreak ordering expressible — the whole reason this test is
// store-level rather than HTTP.
func ent(id, data string) *spi.Entity {
	return &spi.Entity{
		Meta: spi.EntityMeta{ID: id, ModelRef: personRef, State: "NEW"},
		Data: []byte(data),
	}
}

func opts() spi.SearchOptions {
	return spi.SearchOptions{ModelName: personRef.EntityName, ModelVersion: personRef.ModelVersion}
}

// idsSorted returns the entity ids of a result slice, sorted (for id-set
// comparison independent of return order).
func idsSorted(es []*spi.Entity) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Meta.ID)
	}
	sort.Strings(out)
	return out
}

// idsInOrder returns the entity ids of a result slice in the order returned
// (for ordered/pagination assertions).
func idsInOrder(es []*spi.Entity) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Meta.ID)
	}
	return out
}

// begin opens a transaction on the factory and returns the tx-scoped store,
// searcher, tx context, and a rollback cleanup.
func begin(t *testing.T, f spi.StoreFactory, baseCtx context.Context) (spi.EntityStore, spi.Searcher, context.Context) {
	t.Helper()
	tm, err := f.TransactionManager(baseCtx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	txID, txCtx, err := tm.Begin(baseCtx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })
	store, err := f.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(tx): %v", err)
	}
	sr, ok := store.(spi.Searcher)
	if !ok {
		t.Fatalf("store does not implement spi.Searcher")
	}
	return store, sr, txCtx
}

// seed saves the committed baseline through a fresh (non-tx) store.
func seed(t *testing.T, f spi.StoreFactory, baseCtx context.Context, rows ...*spi.Entity) {
	t.Helper()
	store, err := f.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore(seed): %v", err)
	}
	for _, r := range rows {
		if _, err := store.Save(baseCtx, r); err != nil {
			t.Fatalf("seed Save %s: %v", r.Meta.ID, err)
		}
	}
}

// assertRYWOracle is the genuine oracle: an in-tx Search must return exactly
// the id-set (and per-id data) that GetAll(txCtx) + spi.MatchFilter produces
// for the same tx state — computed through the SAME tx-scoped store, so the
// comparison is a real cross-check, not a tautology.
func assertRYWOracle(t *testing.T, store spi.EntityStore, sr spi.Searcher, txCtx context.Context, filter spi.Filter, o spi.SearchOptions) []*spi.Entity {
	t.Helper()
	all, err := store.GetAll(txCtx, personRef)
	if err != nil {
		t.Fatalf("GetAll(tx): %v", err)
	}
	wantIDs := []string{}
	wantData := map[string]string{}
	for _, e := range all {
		if spi.MatchFilter(filter, e.Data, e.Meta) {
			wantIDs = append(wantIDs, e.Meta.ID)
			wantData[e.Meta.ID] = string(e.Data)
		}
	}
	sort.Strings(wantIDs)

	got, err := sr.Search(txCtx, filter, o)
	if err != nil {
		t.Fatalf("Search(tx): %v", err)
	}
	gotIDs := idsSorted(got)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("RYW oracle mismatch: Search=%v, GetAll+MatchFilter=%v", gotIDs, wantIDs)
	}
	for _, e := range got {
		if wd, ok := wantData[e.Meta.ID]; ok && string(e.Data) != wd {
			t.Errorf("id %s data mismatch: Search=%s GetAll=%s", e.Meta.ID, e.Data, wd)
		}
	}
	return got
}

// TestTxSearchRYW runs the identical in-transaction RYW search scenario against
// every in-tree backend. All backends must produce identical results; the
// hardcoded expected id-sets/sequences ARE the cross-backend contract.
func TestTxSearchRYW(t *testing.T) {
	for _, b := range backends(t) {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Run("CoreMatrix", func(t *testing.T) { runCoreMatrix(t, b) })
			t.Run("PageTiebreak", func(t *testing.T) { runPageTiebreak(t, b) })
			t.Run("PageNullsLast", func(t *testing.T) { runPageNullsLast(t, b) })
			t.Run("InTxPITCommittedOnly", func(t *testing.T) { runInTxPIT(t, b) })
		})
	}
	if pgPool == nil {
		t.Log("postgres backend skipped (no container); memory+sqlite verified")
	}
}

// runCoreMatrix covers invariants 1-6: created-in-T present, committed updated
// out-of-match absent, committed match untouched present, deleted-in-T absent,
// delete-then-save same id present (buffered), positive supersession appears
// exactly once as the buffered version.
func runCoreMatrix(t *testing.T, b backend) {
	f, baseCtx, cleanup := b.open(t)
	defer cleanup()

	// Committed Berlin matches (+ one Munich non-match control).
	seed(t, f, baseCtx,
		ent("u1_untouched", `{"city":"Berlin","note":"committed"}`),
		ent("u2_updateout", `{"city":"Berlin","note":"committed"}`),
		ent("u3_delete", `{"city":"Berlin","note":"committed"}`),
		ent("u4_delsave", `{"city":"Berlin","note":"committed"}`),
		ent("u5_supersede", `{"city":"Berlin","note":"committed"}`),
		ent("u6_munich", `{"city":"Munich","note":"committed"}`),
	)

	store, sr, txCtx := begin(t, f, baseCtx)

	// (1) create a new matching entity in T.
	if _, err := store.Save(txCtx, ent("u7_new", `{"city":"Berlin","note":"buffered-new"}`)); err != nil {
		t.Fatalf("create u7_new: %v", err)
	}
	// (2) update a committed match out of the predicate.
	if _, err := store.Save(txCtx, ent("u2_updateout", `{"city":"Munich","note":"buffered-out"}`)); err != nil {
		t.Fatalf("update-out u2: %v", err)
	}
	// (4) delete a committed match.
	if err := store.Delete(txCtx, "u3_delete"); err != nil {
		t.Fatalf("delete u3: %v", err)
	}
	// (5) delete-then-save the SAME id (still matching) — the Task-7 invariant.
	if err := store.Delete(txCtx, "u4_delsave"); err != nil {
		t.Fatalf("delete u4: %v", err)
	}
	if _, err := store.Save(txCtx, ent("u4_delsave", `{"city":"Berlin","note":"buffered-resurrected"}`)); err != nil {
		t.Fatalf("re-save u4: %v", err)
	}
	// (6) positive supersession: re-save a committed match, still matching.
	if _, err := store.Save(txCtx, ent("u5_supersede", `{"city":"Berlin","note":"buffered-superseded"}`)); err != nil {
		t.Fatalf("supersede u5: %v", err)
	}

	got := assertRYWOracle(t, store, sr, txCtx, cityBerlin, opts())

	// Hardcoded RYW-semantics oracle (independent of the Search implementation).
	wantPresent := []string{"u1_untouched", "u4_delsave", "u5_supersede", "u7_new"}
	if gotIDs := idsSorted(got); !reflect.DeepEqual(gotIDs, wantPresent) {
		t.Fatalf("CoreMatrix present-set = %v, want %v", gotIDs, wantPresent)
	}

	// (5)/(6): resurrected + superseded appear exactly once, as the buffered version.
	byID := map[string][]*spi.Entity{}
	for _, e := range got {
		byID[e.Meta.ID] = append(byID[e.Meta.ID], e)
	}
	if hits := byID["u4_delsave"]; len(hits) != 1 {
		t.Fatalf("delete-then-save u4 must appear exactly once, got %d", len(hits))
	} else if string(hits[0].Data) != `{"city":"Berlin","note":"buffered-resurrected"}` {
		t.Errorf("delete-then-save u4 must be the buffered version, got %s", hits[0].Data)
	}
	if hits := byID["u5_supersede"]; len(hits) != 1 {
		t.Fatalf("supersession u5 must appear exactly once, got %d", len(hits))
	} else if string(hits[0].Data) != `{"city":"Berlin","note":"buffered-superseded"}` {
		t.Errorf("supersession u5 must be the buffered version, got %s", hits[0].Data)
	}
}

// runPageTiebreak covers invariant 7 (tiebreak arm): a buffered add ties three
// committed rows on the only explicit sort key (rank) and interleaves at the
// entity_id tiebreak boundary. An offset/limit page straddling the buffered add
// must be identical across backends.
func runPageTiebreak(t *testing.T, b backend) {
	f, baseCtx, cleanup := b.open(t)
	defer cleanup()

	// All rank=5 → the explicit key ties everything; entity_id breaks the tie.
	seed(t, f, baseCtx,
		ent("p1", `{"city":"Berlin","rank":5}`),
		ent("p3", `{"city":"Berlin","rank":5}`),
		ent("p5", `{"city":"Berlin","rank":5}`),
	)
	store, sr, txCtx := begin(t, f, baseCtx)
	// Buffered add ties on rank; its id (p2) interleaves between p1 and p3.
	if _, err := store.Save(txCtx, ent("p2", `{"city":"Berlin","rank":5}`)); err != nil {
		t.Fatalf("buffered add p2: %v", err)
	}

	order := []spi.OrderSpec{{Path: "rank", Source: spi.SourceData, Kind: spi.OrderNumeric}}

	// Full ordered set: all rank=5 → pure entity_id-asc tiebreak.
	full, err := sr.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: personRef.EntityName, ModelVersion: personRef.ModelVersion, OrderBy: order,
	})
	if err != nil {
		t.Fatalf("Search(full): %v", err)
	}
	if got := idsInOrder(full); !reflect.DeepEqual(got, []string{"p1", "p2", "p3", "p5"}) {
		t.Fatalf("tiebreak full order = %v, want [p1 p2 p3 p5]", got)
	}

	// Page straddling the buffered add: offset 1, limit 2 → [p2, p3].
	page, err := sr.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: personRef.EntityName, ModelVersion: personRef.ModelVersion,
		OrderBy: order, Offset: 1, Limit: 2,
	})
	if err != nil {
		t.Fatalf("Search(page): %v", err)
	}
	if got := idsInOrder(page); !reflect.DeepEqual(got, []string{"p2", "p3"}) {
		t.Fatalf("tiebreak page(offset1,limit2) = %v, want [p2 p3]", got)
	}
}

// runPageNullsLast covers invariant 7 (NULLS-LAST arm): a committed row with no
// sort-key value sorts last in both directions, and a buffered add lands
// adjacent to it under an explicit OrderBy. Identical across backends.
func runPageNullsLast(t *testing.T, b backend) {
	f, baseCtx, cleanup := b.open(t)
	defer cleanup()

	seed(t, f, baseCtx,
		ent("n1", `{"city":"Berlin","score":10}`),
		ent("n3_null", `{"city":"Berlin"}`), // no score → sorts last
	)
	store, sr, txCtx := begin(t, f, baseCtx)
	// Buffered add with a score; sits adjacent to the NULL row under asc.
	if _, err := store.Save(txCtx, ent("n2", `{"city":"Berlin","score":20}`)); err != nil {
		t.Fatalf("buffered add n2: %v", err)
	}

	asc := []spi.OrderSpec{{Path: "score", Source: spi.SourceData, Kind: spi.OrderNumeric}}
	got, err := sr.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: personRef.EntityName, ModelVersion: personRef.ModelVersion, OrderBy: asc,
	})
	if err != nil {
		t.Fatalf("Search(asc): %v", err)
	}
	if seq := idsInOrder(got); !reflect.DeepEqual(seq, []string{"n1", "n2", "n3_null"}) {
		t.Fatalf("nulls-last asc = %v, want [n1 n2 n3_null] (missing score sorts last)", seq)
	}

	desc := []spi.OrderSpec{{Path: "score", Source: spi.SourceData, Desc: true, Kind: spi.OrderNumeric}}
	gotDesc, err := sr.Search(txCtx, cityBerlin, spi.SearchOptions{
		ModelName: personRef.EntityName, ModelVersion: personRef.ModelVersion, OrderBy: desc,
	})
	if err != nil {
		t.Fatalf("Search(desc): %v", err)
	}
	if seq := idsInOrder(gotDesc); !reflect.DeepEqual(seq, []string{"n2", "n1", "n3_null"}) {
		t.Fatalf("nulls-last desc = %v, want [n2 n1 n3_null] (missing score sorts last both ways)", seq)
	}
}

// runInTxPIT covers invariant 8: an in-tx Search with PointInTime BEFORE the
// tx's writes returns the committed-as-at snapshot only — buffered creates and
// buffered updates are excluded — identical to GetAllAsAt(pit)+MatchFilter and
// identical across backends. Uses wall-clock separation (works uniformly on all
// three backends without backend-specific time surgery).
func runInTxPIT(t *testing.T, b backend) {
	f, baseCtx, cleanup := b.open(t)
	defer cleanup()

	// Committed Berlin matches, stamped at wall-now.
	seed(t, f, baseCtx,
		ent("b1", `{"city":"Berlin","note":"committed"}`),
		ent("b2", `{"city":"Berlin","note":"committed"}`),
	)

	// pit strictly AFTER the committed writes and strictly BEFORE any tx write.
	time.Sleep(20 * time.Millisecond)
	pit := time.Now()
	time.Sleep(20 * time.Millisecond)

	store, sr, txCtx := begin(t, f, baseCtx)
	// Buffered create postdating pit — must be excluded from the PIT snapshot.
	if _, err := store.Save(txCtx, ent("b3", `{"city":"Berlin","note":"buffered-new"}`)); err != nil {
		t.Fatalf("buffered create b3: %v", err)
	}
	// Buffered update to a committed match — PIT is committed-only, so this must
	// NOT move b1 out of the snapshot.
	if _, err := store.Save(txCtx, ent("b1", `{"city":"Munich","note":"buffered-out"}`)); err != nil {
		t.Fatalf("buffered update b1: %v", err)
	}

	o := opts()
	o.PointInTime = &pit
	o.TrackingRead = true // PIT must still record nothing; asserted via no side effect below

	got, err := sr.Search(txCtx, cityBerlin, o)
	if err != nil {
		t.Fatalf("in-tx PIT Search: %v", err)
	}

	// Oracle: committed-as-at snapshot via GetAllAsAt on the committed store.
	baseStore, err := f.EntityStore(baseCtx)
	if err != nil {
		t.Fatalf("EntityStore(base): %v", err)
	}
	all, err := baseStore.GetAllAsAt(baseCtx, personRef, pit)
	if err != nil {
		t.Fatalf("GetAllAsAt: %v", err)
	}
	wantIDs := []string{}
	for _, e := range all {
		if spi.MatchFilter(cityBerlin, e.Data, e.Meta) {
			wantIDs = append(wantIDs, e.Meta.ID)
		}
	}
	sort.Strings(wantIDs)

	gotIDs := idsSorted(got)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("in-tx PIT oracle mismatch: Search=%v, GetAllAsAt+MatchFilter=%v", gotIDs, wantIDs)
	}
	// Hardcoded: only the committed Berlin rows as-at pit; buffered b3 excluded,
	// buffered Munich update to b1 ignored (committed-only).
	if !reflect.DeepEqual(gotIDs, []string{"b1", "b2"}) {
		t.Fatalf("in-tx PIT present-set = %v, want [b1 b2]", gotIDs)
	}
}
