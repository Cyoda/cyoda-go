package e2e_test

import (
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// ---------------------------------------------------------------------------
// Paged listing inside a joined transaction: read-set FOOTPRINT
// ---------------------------------------------------------------------------
//
// These tests pin the contract the CHANGELOG advertises and design §5 states:
// a paged entity listing performed inside a joined transaction records ONLY
// THE RETURNED PAGE into that transaction's conflict read-set — not the whole
// model. A concurrent committed write to a same-model entity that never
// appeared on the page therefore does NOT abort the commit.
//
// Before task E5, ListEntities materialised the model via
// spi.EntityStore.GetAll, whose recording IS model-wide; it now pages via
// GetPage, which records the returned page only (plugins/postgres/
// entity_store.go, getPageCurrent). The narrowing is the behaviour change,
// and it is what (a) below asserts.
//
// Three cases, each an executable half of the footprint claim:
//
//	(a) off-page write  -> COMMITS   — the narrowing itself.
//	(b) on-page  write  -> CONFLICTS — proves (a) does not pass merely because
//	    nothing is recorded at all. Without this, deleting the recording loop
//	    outright would leave (a) green.
//	(c) whole-model read (GetAll, the pre-narrowing shape) + the SAME off-page
//	    write -> CONFLICTS. This is the empirical "it fails if the narrowing is
//	    reverted": it runs the old footprint against the exact scenario (a)
//	    uses and observes the abort (a) would then suffer.
//
// ISOLATED single-backend (Postgres) e2e, deliberately NOT in the shared
// parity suite — per .claude/rules/test-coverage.md, concurrency scenarios
// assert consistency in an isolated single-backend test and never in the
// shared parity suite. Determinism matches search_intx_tracking_test.go's:
// first-committer-wins validates VERSION NUMBERS, and every step here runs in
// sequential program order, so tx B always commits before tx A regardless of
// wall-clock timing.

// newListHandler builds an entity.Handler over the running e2e app's own
// store factory, transaction manager and workflow engine. app.App exposes no
// entity-handler accessor, and ListEntities is the surface under test here —
// the engine call that pages at the store — so the handler is assembled from
// the app's real collaborators rather than reaching for the store directly.
func newListHandler(t *testing.T) *entity.Handler {
	t.Helper()
	return entity.New(
		testApp.StoreFactory(),
		testApp.TransactionManager(),
		common.NewDefaultUUIDGenerator(),
		testApp.WorkflowEngine(),
		txgate.New(),
	)
}

// TestListIntxReadSet_UnreturnedEntity_Commits is case (a): a listing inside a
// transaction records only the page it returned, so a concurrent committed
// write to a same-model entity that was NOT on that page leaves the commit
// unaffected.
func TestListIntxReadSet_UnreturnedEntity_Commits(t *testing.T) {
	const model = "e2e-list-readset-unreturned"
	setupSearchModel(t, model)
	seeded := []string{
		createEntityE2E(t, model, 1, `{"name":"L1","amount":1,"status":"active"}`),
		createEntityE2E(t, model, 1, `{"name":"L2","amount":2,"status":"active"}`),
		createEntityE2E(t, model, 1, `{"name":"L3","amount":3,"status":"active"}`),
	}

	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()
	h := newListHandler(t)

	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txA) }()

	page, err := h.ListEntities(txCtxA, model, "1", entity.PaginationParams{PageSize: 1, PageNumber: 0}, nil)
	if err != nil {
		t.Fatalf("tx A ListEntities: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("tx A page: want exactly 1 entity, got %d", len(page))
	}
	onPage := envelopeID(t, page[0])
	offPage := otherThan(t, seeded, onPage)

	time.Sleep(10 * time.Millisecond)

	// tx B commits a write to an entity of the SAME MODEL that tx A's page
	// did not return.
	commitConcurrentWrite(t, model, offPage, `{"name":"Loff","amount":99,"status":"active"}`)

	// tx A commits OK: only the returned page (onPage) is in its read-set.
	if err := tm.Commit(ctx, txA); err != nil {
		t.Fatalf("tx A commit: want success (%s was never on the returned page, so not tracked), got %v", offPage, err)
	}
}

// TestListIntxReadSet_ReturnedEntity_ConflictsOnCommit is case (b): the page
// that WAS returned is tracked, so a concurrent committed write to it aborts
// the commit. Negative control for case (a) — it proves the page is recorded
// at all, so (a)'s success is narrowing and not silence.
func TestListIntxReadSet_ReturnedEntity_ConflictsOnCommit(t *testing.T) {
	const model = "e2e-list-readset-returned"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"L1","amount":1,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"L2","amount":2,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"L3","amount":3,"status":"active"}`)

	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()
	h := newListHandler(t)

	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txA) }()

	page, err := h.ListEntities(txCtxA, model, "1", entity.PaginationParams{PageSize: 1, PageNumber: 0}, nil)
	if err != nil {
		t.Fatalf("tx A ListEntities: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("tx A page: want exactly 1 entity, got %d", len(page))
	}
	onPage := envelopeID(t, page[0])

	time.Sleep(10 * time.Millisecond)

	commitConcurrentWrite(t, model, onPage, `{"name":"Lon","amount":98,"status":"active"}`)

	err = tm.Commit(ctx, txA)
	if err == nil {
		t.Fatalf("tx A commit: expected spi.ErrConflict (%s was on the returned page, so tracked), got nil", onPage)
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("tx A commit: want spi.ErrConflict, got %v", err)
	}
}

// TestListIntxReadSet_WholeModelRead_ConflictsOnUnreturnedEntity is case (c),
// the revert control: it performs the PRE-NARROWING read — GetAll, which
// records the whole model — and then runs case (a)'s exact scenario. The
// commit must abort. That is the observed failure case (a) would produce if
// ListEntities were rewired back onto a model-wide read, which is what makes
// case (a) a real regression guard rather than a test that cannot fail.
func TestListIntxReadSet_WholeModelRead_ConflictsOnUnreturnedEntity(t *testing.T) {
	const model = "e2e-list-readset-wholemodel"
	setupSearchModel(t, model)
	seeded := []string{
		createEntityE2E(t, model, 1, `{"name":"L1","amount":1,"status":"active"}`),
		createEntityE2E(t, model, 1, `{"name":"L2","amount":2,"status":"active"}`),
		createEntityE2E(t, model, 1, `{"name":"L3","amount":3,"status":"active"}`),
	}
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}

	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()
	h := newListHandler(t)

	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txA) }()

	// Which entity would page 0 return? Ask the narrowed path, in a
	// throwaway non-transactional context, so the off-page entity chosen
	// below is exactly the one case (a) would choose.
	probe, err := h.ListEntities(ctx, model, "1", entity.PaginationParams{PageSize: 1, PageNumber: 0}, nil)
	if err != nil {
		t.Fatalf("probe ListEntities: %v", err)
	}
	if len(probe) != 1 {
		t.Fatalf("probe page: want exactly 1 entity, got %d", len(probe))
	}
	offPage := otherThan(t, seeded, envelopeID(t, probe[0]))

	// The pre-narrowing read: materialise the whole model inside tx A.
	store, err := testApp.StoreFactory().EntityStore(txCtxA)
	if err != nil {
		t.Fatalf("tx A EntityStore: %v", err)
	}
	all, err := store.GetAll(txCtxA, ref)
	if err != nil {
		t.Fatalf("tx A GetAll: %v", err)
	}
	if len(all) != len(seeded) {
		t.Fatalf("tx A GetAll: want %d entities, got %d", len(seeded), len(all))
	}

	time.Sleep(10 * time.Millisecond)

	commitConcurrentWrite(t, model, offPage, `{"name":"Loff","amount":97,"status":"active"}`)

	err = tm.Commit(ctx, txA)
	if err == nil {
		t.Fatalf("tx A commit: expected spi.ErrConflict — a whole-model read tracks %s too, so the concurrent write must abort it", offPage)
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("tx A commit: want spi.ErrConflict, got %v", err)
	}
}

// envelopeID extracts meta.id from a listing envelope.
func envelopeID(t *testing.T, e entity.EntityEnvelope) string {
	t.Helper()
	id, ok := e.Meta["id"].(string)
	if !ok || id == "" {
		t.Fatalf("listing envelope carries no meta.id: %+v", e.Meta)
	}
	return id
}

// otherThan returns a seeded id that is not exclude.
func otherThan(t *testing.T, seeded []string, exclude string) string {
	t.Helper()
	for _, id := range seeded {
		if id != exclude {
			return id
		}
	}
	t.Fatalf("no seeded entity other than %s", exclude)
	return ""
}
