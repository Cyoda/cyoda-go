package e2e_test

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
)

// ---------------------------------------------------------------------------
// In-transaction TrackingRead conflict footprint (isolated, single-backend)
// ---------------------------------------------------------------------------
//
// These tests pin the end-to-end contract of the tx-aware search pushdown
// THROUGH THE ENGINE: search.SearchService.Search (now de-guarded to route an
// in-tx search to the plugin Searcher) threads SearchOptions.TrackingRead down
// to the store, which records the RETURNED entities into the active
// transaction's read-set so commit-time first-committer-wins (FCW) validates
// them.
//
// The value over the per-plugin searcher_tx_test.go (which drives the plugin
// Searcher directly) is that these exercise the real
// engine -> plugin -> read-set -> FCW path: SearchService.Search with a real
// SearchOptions{TrackingRead:...}, a real TransactionManager, and a real
// concurrent committed writer.
//
// This is an ISOLATED single-backend e2e test (Postgres only, the backend the
// e2e harness runs). It is deliberately NOT in the shared cross-backend parity
// suite: per .claude/rules/concurrency-tests-not-in-parity.md, concurrency
// tests assert CONSISTENCY (one coherent outcome — conflict vs no-conflict),
// never a precise interleave, and belong as isolated single-backend tests.
//
// Determinism: FCW is COMMIT-ORDER based, not a data race. We always commit the
// concurrent writer (tx B) BEFORE committing the tracked reader (tx A), so the
// outcome is a deterministic function of commit order, not of goroutine timing.
// tx B's submit time is naturally after tx A's snapshot (A begins first, B
// commits after several intervening operations); a short sleep makes the
// ordering robust against millisecond-granularity timestamps.

const intxTrackingTenant = "test-tenant"

// intxTenantCtx builds a background context carrying the bootstrap tenant's
// user context, matching the tenant that createEntityE2E writes under so the
// in-process store/search sees the same rows.
func intxTenantCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "test-admin",
		UserName: "Test Admin",
		Tenant:   spi.Tenant{ID: intxTrackingTenant, Name: intxTrackingTenant},
		Roles:    []string{"ROLE_ADMIN"},
	})
}

// commitConcurrentWrite begins a fresh transaction, updates the given entity's
// data, and commits it — the "first committer" in the FCW race. Asserts the
// write commits cleanly (it must win: it has no read-set dependency on tx A).
func commitConcurrentWrite(t *testing.T, model, entityID, newData string) {
	t.Helper()
	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("concurrent writer Begin: %v", err)
	}
	store, err := testApp.StoreFactory().EntityStore(txCtx)
	if err != nil {
		_ = tm.Rollback(ctx, txID)
		t.Fatalf("concurrent writer EntityStore: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       entityID,
			ModelRef: spi.ModelRef{EntityName: model, ModelVersion: "1"},
			State:    "CREATED",
		},
		Data: []byte(newData),
	}); err != nil {
		_ = tm.Rollback(ctx, txID)
		t.Fatalf("concurrent writer Save %s: %v", entityID, err)
	}
	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("concurrent writer Commit must win (no read-set): %v", err)
	}
}

// nameEquals builds a condition matching a single seeded row by its name field.
func nameEquals(name string) predicate.Condition {
	return &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        name,
	}
}

// TestSearchIntxTracking_TrueRecordsReturned_ConflictsOnCommit is case (a):
// with TrackingRead=true, tx A's in-tx search returns entity X, a concurrent
// tx B commits a write to X first, and tx A's commit must fail with
// spi.ErrConflict (the API maps this to a retryable 409).
func TestSearchIntxTracking_TrueRecordsReturned_ConflictsOnCommit(t *testing.T) {
	const model = "e2e-intx-tracking-conflict"
	setupSearchModel(t, model)
	xID := createEntityE2E(t, model, 1, `{"name":"Xrow","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Yrow","amount":200,"status":"active"}`)

	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}

	// tx A: begin, then a TRACKING in-tx search that returns only X.
	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txA) }()

	got, err := testApp.SearchService().Search(txCtxA, ref, nameEquals("Xrow"),
		search.SearchOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("tx A tracking Search: %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != xID {
		t.Fatalf("tx A search: want exactly [%s], got %d rows (%v)", xID, len(got), idsOfEntities(got))
	}

	// Make tx B's commit strictly later than tx A's snapshot.
	time.Sleep(10 * time.Millisecond)

	// tx B: commit a write to the RETURNED entity X first (it wins).
	commitConcurrentWrite(t, model, xID, `{"name":"Xrow","amount":101,"status":"active"}`)

	// tx A's commit must lose: X is in A's read-set and was modified after A's
	// snapshot.
	err = tm.Commit(ctx, txA)
	if err == nil {
		t.Fatal("tx A commit: expected spi.ErrConflict (returned entity was tracked and modified), got nil")
	}
	if !errors.Is(err, spi.ErrConflict) {
		t.Fatalf("tx A commit: want spi.ErrConflict, got %v", err)
	}
}

// TestSearchIntxTracking_TrueUnreturnedEntity_Commits is case (b): with
// TrackingRead=true, only RETURNED rows are tracked. tx B commits a write to a
// NON-returned entity Y, so tx A commits OK — Y never entered A's read-set.
func TestSearchIntxTracking_TrueUnreturnedEntity_Commits(t *testing.T) {
	const model = "e2e-intx-tracking-unreturned"
	setupSearchModel(t, model)
	createEntityE2E(t, model, 1, `{"name":"Xrow","amount":100,"status":"active"}`)
	yID := createEntityE2E(t, model, 1, `{"name":"Yrow","amount":200,"status":"active"}`)

	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}

	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txA) }()

	// Tracking search that returns ONLY X (Y is excluded by the condition).
	got, err := testApp.SearchService().Search(txCtxA, ref, nameEquals("Xrow"),
		search.SearchOptions{TrackingRead: true})
	if err != nil {
		t.Fatalf("tx A tracking Search: %v", err)
	}
	if len(got) != 1 || got[0].Meta.Version == 0 {
		t.Fatalf("tx A search: want exactly 1 row (X), got %d (%v)", len(got), idsOfEntities(got))
	}

	time.Sleep(10 * time.Millisecond)

	// tx B commits a write to the NON-returned entity Y.
	commitConcurrentWrite(t, model, yID, `{"name":"Yrow","amount":201,"status":"active"}`)

	// tx A commits OK: only returned rows (X) are tracked; Y is not in the
	// read-set, so B's write to Y does not conflict.
	if err := tm.Commit(ctx, txA); err != nil {
		t.Fatalf("tx A commit: want success (Y was not returned, so not tracked), got %v", err)
	}
}

// TestSearchIntxTracking_FalseRecordsNothing_Commits is case (c): with
// TrackingRead=false the in-tx search is a plain snapshot read that records no
// read dependency. tx B commits a write to the RETURNED entity X, yet tx A
// still commits OK — nothing was tracked. This is the negative control proving
// case (a)'s conflict is caused by read-set recording, not by the search alone.
func TestSearchIntxTracking_FalseRecordsNothing_Commits(t *testing.T) {
	const model = "e2e-intx-tracking-nontracking"
	setupSearchModel(t, model)
	xID := createEntityE2E(t, model, 1, `{"name":"Xrow","amount":100,"status":"active"}`)
	createEntityE2E(t, model, 1, `{"name":"Yrow","amount":200,"status":"active"}`)

	ctx := intxTenantCtx()
	tm := testApp.TransactionManager()
	ref := spi.ModelRef{EntityName: model, ModelVersion: "1"}

	txA, txCtxA, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("tx A Begin: %v", err)
	}
	defer func() { _ = tm.Rollback(ctx, txA) }()

	// Non-tracking search returns X but records NOTHING into the read-set.
	got, err := testApp.SearchService().Search(txCtxA, ref, nameEquals("Xrow"),
		search.SearchOptions{TrackingRead: false})
	if err != nil {
		t.Fatalf("tx A snapshot Search: %v", err)
	}
	if len(got) != 1 || got[0].Meta.ID != xID {
		t.Fatalf("tx A search: want exactly [%s], got %d rows (%v)", xID, len(got), idsOfEntities(got))
	}

	time.Sleep(10 * time.Millisecond)

	// tx B commits a write to the RETURNED entity X.
	commitConcurrentWrite(t, model, xID, `{"name":"Xrow","amount":102,"status":"active"}`)

	// tx A commits OK: a snapshot read recorded no dependency on X.
	if err := tm.Commit(ctx, txA); err != nil {
		t.Fatalf("tx A commit: want success (snapshot read recorded nothing), got %v", err)
	}
}

// idsOfEntities extracts entity IDs for diagnostics.
func idsOfEntities(entities []*spi.Entity) []string {
	ids := make([]string, len(entities))
	for i, e := range entities {
		ids[i] = e.Meta.ID
	}
	return ids
}
