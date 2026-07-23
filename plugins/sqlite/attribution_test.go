package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

// attrCtx returns a context carrying a UserContext with an explicit
// PrincipalKind, for attribution tests that need deterministic control over
// Kind (unlike testCtx/extTestCtx elsewhere in this package, which leave Kind
// at its zero value).
func attrCtx(tenant, userID string, kind spi.PrincipalKind) context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: userID,
		Kind:   kind,
		Tenant: spi.Tenant{ID: spi.TenantID(tenant), Name: tenant},
	})
}

// newAttrFactory creates a fresh StoreFactory backed by a tempdir sqlite file
// and returns it alongside its TransactionManager.
func newAttrFactory(t *testing.T) (*sqlite.StoreFactory, spi.TransactionManager) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "attribution.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	tm, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	return f, tm
}

// TestBeginCapturesOriginFromUserContext verifies that Begin resolves
// TransactionState.Origin via spi.ResolveOrigin from the caller's
// UserContext, and initialises DeleteAttribution.
func TestBeginCapturesOriginFromUserContext(t *testing.T) {
	_, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "alice", spi.PrincipalUser)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()

	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		t.Fatal("expected Begin to populate TransactionState in the returned context")
	}
	want := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	if tx.Origin != want {
		t.Errorf("tx.Origin = %+v, want %+v", tx.Origin, want)
	}
	if tx.DeleteAttribution == nil {
		t.Error("expected Begin to initialise tx.DeleteAttribution")
	}
}

// TestBeginCapturesAmbientOrigin verifies that Begin honours an
// ambient-seeded origin (WithAmbientOrigin) over the caller's own
// UserContext-derived Principal when no parent tx exists — the
// scheduled-fire case, per ResolveOrigin's documented precedence.
func TestBeginCapturesAmbientOrigin(t *testing.T) {
	_, tm := newAttrFactory(t)
	base := attrCtx("tenant-A", "ambient-caller", spi.PrincipalUser)
	seed := spi.Principal{ID: "scheduler", Kind: spi.PrincipalSystem}
	ctx := spi.WithAmbientOrigin(base, seed)

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	defer func() { _ = tm.Rollback(txCtx, txID) }()

	tx := spi.GetTransaction(txCtx)
	if tx == nil {
		t.Fatal("expected Begin to populate TransactionState in the returned context")
	}
	if tx.Origin != seed {
		t.Errorf("tx.Origin = %+v, want ambient seed %+v", tx.Origin, seed)
	}
}

// TestCommitDeleteAttribution_StagerNotCommitter is the central regression
// test for the delete-attribution bug: a delete staged by a joined,
// service-kind caller inside a user-origin transaction must be attributed to
// the tx's origin user with an Executor recording the *stager* (service),
// regardless of which context is later used to Commit. Before the fix,
// Commit's flush stamped the tombstone's user_id column from ” — always
// blank, never any actor.
func TestCommitDeleteAttribution_StagerNotCommitter(t *testing.T) {
	factory, tm := newAttrFactory(t)
	rootCtx := attrCtx("tenant-A", "root-user", spi.PrincipalUser)

	store, err := factory.EntityStore(rootCtx)
	if err != nil {
		t.Fatalf("EntityStore failed: %v", err)
	}
	if _, err := store.Save(rootCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-del",
			TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(rootCtx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	wantOrigin := spi.Principal{ID: "root-user", Kind: spi.PrincipalUser}
	if tx.Origin != wantOrigin {
		t.Fatalf("tx.Origin = %+v, want %+v", tx.Origin, wantOrigin)
	}

	// Join as a distinct, service-kind actor and stage the delete through it.
	svcCtx := attrCtx("tenant-A", "svc-x", spi.PrincipalService)
	joinedCtx, err := tm.Join(svcCtx, txID)
	if err != nil {
		t.Fatalf("Join failed: %v", err)
	}
	joinedStore, err := factory.EntityStore(joinedCtx)
	if err != nil {
		t.Fatalf("EntityStore(joinedCtx) failed: %v", err)
	}
	if err := joinedStore.Delete(joinedCtx, "e-del"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// DeleteAttribution must be captured immediately at stage time, under
	// the joined (service) ctx — not deferred to Commit.
	wantExecutor := spi.Principal{ID: "svc-x", Kind: spi.PrincipalService}
	attr, ok := tx.DeleteAttribution["e-del"]
	if !ok {
		t.Fatal("expected tx.DeleteAttribution to record the staged delete immediately")
	}
	if attr.Attributed != wantOrigin {
		t.Errorf("staged Attributed = %+v, want tx.Origin %+v", attr.Attributed, wantOrigin)
	}
	if attr.Executor != wantExecutor {
		t.Errorf("staged Executor = %+v, want stager %+v", attr.Executor, wantExecutor)
	}

	// Commit using the ROOT ctx (the committer), which is a *different*
	// actor from the stager. The fix must not re-derive attribution from
	// this ctx.
	if err := tm.Commit(rootCtx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	history, err := store.GetVersionHistory(rootCtx, "e-del")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	if tomb.User != wantOrigin.ID {
		t.Errorf("tombstone User = %q, want origin user %q", tomb.User, wantOrigin.ID)
	}
	if tomb.AttributedKind != wantOrigin.Kind {
		t.Errorf("tombstone AttributedKind = %v, want %v", tomb.AttributedKind, wantOrigin.Kind)
	}
	if tomb.Executor != wantExecutor {
		t.Errorf("tombstone Executor = %+v, want stager %+v (not the committer)", tomb.Executor, wantExecutor)
	}
}

// TestCommitFlushesDeletes_FallbackAttribution: when a delete is staged
// directly on tx.Deletes without a corresponding tx.DeleteAttribution entry
// (simulating a caller that bypassed EntityStore.Delete), Commit's flush
// must fall back to spi.AttributionFor(ctx) evaluated at commit time.
func TestCommitFlushesDeletes_FallbackAttribution(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "test-user", spi.PrincipalUser)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:         "e-del-fallback",
			TenantID:   "tenant-A",
			ChangeType: "CREATED",
		},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	// Stage the delete directly, bypassing EntityStore.Delete — no
	// DeleteAttribution entry is recorded.
	tx.Deletes["e-del-fallback"] = true
	tx.WriteSet["e-del-fallback"] = true
	if _, ok := tx.DeleteAttribution["e-del-fallback"]; ok {
		t.Fatal("test setup error: DeleteAttribution must be absent to exercise the fallback path")
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-del-fallback")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	want := spi.Principal{ID: "test-user", Kind: spi.PrincipalUser}
	if tomb.User != want.ID {
		t.Errorf("tombstone User = %q, want fallback (commit ctx) user %q", tomb.User, want.ID)
	}
	if tomb.Executor != want {
		t.Errorf("tombstone Executor = %+v, want fallback %+v", tomb.Executor, want)
	}
}

// TestDelete_NonTx_AttributionIsCaller verifies that a non-transactional
// Delete stamps the tombstone's attribution from the caller's own context
// (attributed == executor == caller) via spi.AttributionFor.
func TestDelete_NonTx_AttributionIsCaller(t *testing.T) {
	factory, _ := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "alice", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	if _, err := store.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       "e-del-nontx",
			TenantID: "tenant-A",
			ModelRef: spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		},
		Data: []byte(`{}`),
	}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := store.Delete(ctx, "e-del-nontx"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-del-nontx")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	want := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	if tomb.User != want.ID {
		t.Errorf("tombstone User = %q, want %q", tomb.User, want.ID)
	}
	if tomb.AttributedKind != want.Kind {
		t.Errorf("tombstone AttributedKind = %v, want %v", tomb.AttributedKind, want.Kind)
	}
	if tomb.Executor != want {
		t.Errorf("tombstone Executor = %+v, want %+v (non-tx: attributed == executor == caller)", tomb.Executor, want)
	}
}

// TestDeleteAll_NonTx_Attribution verifies that non-transactional DeleteAll
// stamps every tombstone's attribution from the caller's context, same as
// single-entity Delete.
func TestDeleteAll_NonTx_Attribution(t *testing.T) {
	factory, _ := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "bob", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	modelRef := spi.ModelRef{EntityName: "m-delall", ModelVersion: "1"}
	for _, id := range []string{"e-da-1", "e-da-2"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-A", ModelRef: modelRef},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("Save(%s) failed: %v", id, err)
		}
	}

	if err := store.DeleteAll(ctx, modelRef); err != nil {
		t.Fatalf("DeleteAll failed: %v", err)
	}

	want := spi.Principal{ID: "bob", Kind: spi.PrincipalUser}
	for _, id := range []string{"e-da-1", "e-da-2"} {
		history, err := store.GetVersionHistory(ctx, id)
		if err != nil {
			t.Fatalf("GetVersionHistory(%s) failed: %v", id, err)
		}
		tomb := history[len(history)-1]
		if !tomb.Deleted {
			t.Fatalf("expected %s's last version to be the DELETE tombstone", id)
		}
		if tomb.Executor != want {
			t.Errorf("%s tombstone Executor = %+v, want %+v", id, tomb.Executor, want)
		}
		if tomb.AttributedKind != want.Kind {
			t.Errorf("%s tombstone AttributedKind = %v, want %v", id, tomb.AttributedKind, want.Kind)
		}
	}
}

// TestSave_NonTx_StampsUserIDColumn verifies that a non-transactional Save
// writes Entity.Meta.ChangeUser into the version row's user_id COLUMN — the
// path GetVersionHistory reads EntityVersion.User from — distinct from
// AttributedKind/Executor, which are sourced from the meta BLOB and already
// covered by TestSaveAndDelete_ExecutorRoundTrip. Regression test for
// saveDirectly having hardcoded user_id to '' instead of cp.Meta.ChangeUser.
func TestSave_NonTx_StampsUserIDColumn(t *testing.T) {
	factory, _ := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "caller", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	wantUser := "origin-user"
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:             "e-userid-col",
			TenantID:       "tenant-A",
			ModelRef:       spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			ChangeType:     "CREATED",
			ChangeUser:     wantUser,
			ChangeUserKind: spi.PrincipalUser,
			ChangeExecutor: spi.Principal{ID: "svc-1", Kind: spi.PrincipalService},
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(ctx, entity); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-userid-col")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 version, got %d", len(history))
	}
	if got := history[0].User; got != wantUser {
		t.Errorf("version.User = %q, want ChangeUser %q (user_id column not stamped)", got, wantUser)
	}
}

// TestDeleteAll_Tx_AttributionStaged verifies that a transactional DeleteAll
// stages DeleteAttribution for every affected entity ID, paired with
// Deletes, under the caller's context at stage time.
func TestDeleteAll_Tx_AttributionStaged(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "carol", spi.PrincipalUser)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	modelRef := spi.ModelRef{EntityName: "m-delall-tx", ModelVersion: "1"}
	for _, id := range []string{"e-dat-1", "e-dat-2"} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: id, TenantID: "tenant-A", ModelRef: modelRef},
			Data: []byte(`{}`),
		}); err != nil {
			t.Fatalf("Save(%s) failed: %v", id, err)
		}
	}

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	txStore, err := factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("EntityStore(txCtx) failed: %v", err)
	}
	if err := txStore.DeleteAll(txCtx, modelRef); err != nil {
		t.Fatalf("DeleteAll failed: %v", err)
	}

	tx := spi.GetTransaction(txCtx)
	want := spi.Principal{ID: "carol", Kind: spi.PrincipalUser}
	for _, id := range []string{"e-dat-1", "e-dat-2"} {
		if !tx.Deletes[id] {
			t.Fatalf("expected tx.Deletes[%s] to be staged", id)
		}
		attr, ok := tx.DeleteAttribution[id]
		if !ok {
			t.Fatalf("expected tx.DeleteAttribution[%s] to be staged alongside tx.Deletes", id)
		}
		if attr.Attributed != want || attr.Executor != want {
			t.Errorf("DeleteAttribution[%s] = %+v, want Attributed==Executor==%+v", id, attr, want)
		}
	}

	if err := tm.Commit(ctx, txID); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	for _, id := range []string{"e-dat-1", "e-dat-2"} {
		history, err := store.GetVersionHistory(ctx, id)
		if err != nil {
			t.Fatalf("GetVersionHistory(%s) failed: %v", id, err)
		}
		tomb := history[len(history)-1]
		if !tomb.Deleted {
			t.Fatalf("expected %s's last version to be the DELETE tombstone", id)
		}
		if tomb.Executor != want {
			t.Errorf("%s tombstone Executor = %+v, want %+v", id, tomb.Executor, want)
		}
	}
}

// TestSaveAndDelete_ExecutorRoundTrip verifies that ChangeUser/ChangeUserKind/
// ChangeExecutor stamped on Entity.Meta before Save round-trip through
// GetVersionHistory as EntityVersion.AttributedKind/Executor — including for
// a DELETED version, whose Executor must be readable without Entity (nil for
// tombstones).
func TestSaveAndDelete_ExecutorRoundTrip(t *testing.T) {
	factory, _ := newAttrFactory(t)
	ctx := attrCtx("tenant-A", "test-user", spi.PrincipalUser)
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}

	wantExecutor := spi.Principal{ID: "svc-1", Kind: spi.PrincipalService}
	entity := &spi.Entity{
		Meta: spi.EntityMeta{
			ID:             "e-exec-1",
			TenantID:       "tenant-A",
			ModelRef:       spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
			ChangeType:     "CREATED",
			ChangeUser:     "origin-user",
			ChangeUserKind: spi.PrincipalUser,
			ChangeExecutor: wantExecutor,
		},
		Data: []byte(`{}`),
	}
	if _, err := store.Save(ctx, entity); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	delCtx := attrCtx("tenant-A", "del-user", spi.PrincipalUser)
	if err := store.Delete(delCtx, "e-exec-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	history, err := store.GetVersionHistory(ctx, "e-exec-1")
	if err != nil {
		t.Fatalf("GetVersionHistory failed: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 versions (CREATE + DELETE), got %d", len(history))
	}

	created := history[0]
	if created.AttributedKind != spi.PrincipalUser {
		t.Errorf("CREATE version AttributedKind = %v, want %v", created.AttributedKind, spi.PrincipalUser)
	}
	if created.Executor != wantExecutor {
		t.Errorf("CREATE version Executor = %+v, want %+v", created.Executor, wantExecutor)
	}

	tomb := history[len(history)-1]
	if !tomb.Deleted {
		t.Fatal("expected last version to be the DELETE tombstone")
	}
	if tomb.Entity != nil {
		t.Errorf("expected nil Entity on a DELETED version, got %+v", tomb.Entity)
	}
	wantDel := spi.Principal{ID: "del-user", Kind: spi.PrincipalUser}
	if tomb.Executor != wantDel {
		t.Errorf("DELETE version Executor = %+v, want %+v (readable without Entity)", tomb.Executor, wantDel)
	}
	if tomb.AttributedKind != spi.PrincipalUser {
		t.Errorf("DELETE version AttributedKind = %v, want %v", tomb.AttributedKind, spi.PrincipalUser)
	}
}
