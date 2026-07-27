package entity

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// attributionUserCtx builds a plain user ctx for the "user records itself"
// scenario, with Kind explicitly set to spi.PrincipalUser. txJoinTestCtx (used
// elsewhere in this package) predates the attribution work and leaves Kind
// unset — deliberately using a distinct, explicitly-kinded ctx here means
// these tests assert the documented user-kind behavior rather than the
// legacy-unset-kind fallback.
func attributionUserCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "txjoin-user",
		UserName: "TxJoin",
		Kind:     spi.PrincipalUser,
		Tenant:   spi.Tenant{ID: "txjoin-tenant", Name: "TxJoin"},
		Roles:    []string{"user"},
	})
}

// getEntityMeta fetches the given entity's current Meta via a fresh,
// non-transactional context — i.e. it reads the durable, committed row.
func getEntityMeta(t *testing.T, h *Handler, entityID string) spi.EntityMeta {
	t.Helper()
	base := txJoinTestCtx()
	store, err := h.factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e, err := store.Get(base, entityID)
	if err != nil {
		t.Fatalf("Get(%s): %v", entityID, err)
	}
	return e.Meta
}

// TestCreateEntity_StampsAttribution drives CreateEntity through the
// spi.AttributionFor stamp rule (Task 8):
//
//   - (a) a plain user ctx: ChangeUser==userID, ChangeUserKind==user,
//     ChangeExecutor.ID==userID, ChangeExecutor.Kind==user.
//   - (b) a service-kind executor joined onto a tx whose Origin is a user:
//     ChangeUser==origin user, ChangeUserKind==user (inherited),
//     ChangeExecutor.ID==service ID, ChangeExecutor.Kind==service.
func TestCreateEntity_StampsAttribution(t *testing.T) {
	t.Run("plain user ctx records itself", func(t *testing.T) {
		h, _, _, _ := newTxJoinTestHandler(t)
		userCtx := attributionUserCtx()

		res, err := h.CreateEntity(userCtx, sampleWidgetInput())
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		meta := getEntityMeta(t, h, res.EntityIDs[0])

		if meta.ChangeUser != "txjoin-user" {
			t.Errorf("ChangeUser = %q, want %q", meta.ChangeUser, "txjoin-user")
		}
		if meta.ChangeUserKind != spi.PrincipalUser {
			t.Errorf("ChangeUserKind = %q, want %q", meta.ChangeUserKind, spi.PrincipalUser)
		}
		if meta.ChangeExecutor.ID != "txjoin-user" {
			t.Errorf("ChangeExecutor.ID = %q, want %q", meta.ChangeExecutor.ID, "txjoin-user")
		}
		if meta.ChangeExecutor.Kind != spi.PrincipalUser {
			t.Errorf("ChangeExecutor.Kind = %q, want %q", meta.ChangeExecutor.Kind, spi.PrincipalUser)
		}
	})

	t.Run("service executor inherits joined tx origin", func(t *testing.T) {
		h, _, txMgr, base := newTxJoinTestHandler(t)

		ownerTxID, ownerCtx, err := txMgr.Begin(base)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}

		// The plugin does not yet populate Origin at Begin (a later task) —
		// set it directly on the TransactionState to construct the joined-tx
		// scenario under test: a user-attributed causal chain that a
		// service-kind executor later writes into.
		tx := spi.GetTransaction(ownerCtx)
		if tx == nil {
			t.Fatal("expected a TransactionState on ownerCtx")
		}
		tx.Origin = spi.Principal{ID: "origin-user", Kind: spi.PrincipalUser}

		serviceCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
			UserID:   "svc-writer",
			UserName: "ServiceWriter",
			Kind:     spi.PrincipalService,
			Tenant:   spi.Tenant{ID: "txjoin-tenant", Name: "TxJoin"},
		})
		joinedCtx, err := txMgr.Join(serviceCtx, ownerTxID)
		if err != nil {
			t.Fatalf("Join: %v", err)
		}

		res, err := h.CreateEntity(joinedCtx, sampleWidgetInput())
		if err != nil {
			t.Fatalf("create in joined tx: %v", err)
		}

		if err := txMgr.Commit(ownerCtx, ownerTxID); err != nil {
			t.Fatalf("owner commit: %v", err)
		}

		meta := getEntityMeta(t, h, res.EntityIDs[0])

		if meta.ChangeUser != "origin-user" {
			t.Errorf("ChangeUser = %q, want inherited origin %q", meta.ChangeUser, "origin-user")
		}
		if meta.ChangeUserKind != spi.PrincipalUser {
			t.Errorf("ChangeUserKind = %q, want %q (inherited from origin)", meta.ChangeUserKind, spi.PrincipalUser)
		}
		if meta.ChangeExecutor.ID != "svc-writer" {
			t.Errorf("ChangeExecutor.ID = %q, want %q", meta.ChangeExecutor.ID, "svc-writer")
		}
		if meta.ChangeExecutor.Kind != spi.PrincipalService {
			t.Errorf("ChangeExecutor.Kind = %q, want %q", meta.ChangeExecutor.Kind, spi.PrincipalService)
		}
	})
}

// TestUpdateEntity_StampsAttribution mirrors TestCreateEntity_StampsAttribution
// for the UpdateEntity path (service.go update site).
func TestUpdateEntity_StampsAttribution(t *testing.T) {
	t.Run("plain user ctx records itself", func(t *testing.T) {
		h, _, _, _ := newTxJoinTestHandler(t)
		userCtx := attributionUserCtx()

		created, err := h.CreateEntity(userCtx, sampleWidgetInput())
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		entityID := created.EntityIDs[0]

		_, err = h.UpdateEntity(userCtx, UpdateEntityInput{
			EntityID: entityID,
			Format:   "JSON",
			Data:     []byte(`{"name":"w2"}`),
		})
		if err != nil {
			t.Fatalf("UpdateEntity: %v", err)
		}

		meta := getEntityMeta(t, h, entityID)
		if meta.ChangeUser != "txjoin-user" {
			t.Errorf("ChangeUser = %q, want %q", meta.ChangeUser, "txjoin-user")
		}
		if meta.ChangeUserKind != spi.PrincipalUser {
			t.Errorf("ChangeUserKind = %q, want %q", meta.ChangeUserKind, spi.PrincipalUser)
		}
		if meta.ChangeExecutor.ID != "txjoin-user" {
			t.Errorf("ChangeExecutor.ID = %q, want %q", meta.ChangeExecutor.ID, "txjoin-user")
		}
		if meta.ChangeExecutor.Kind != spi.PrincipalUser {
			t.Errorf("ChangeExecutor.Kind = %q, want %q", meta.ChangeExecutor.Kind, spi.PrincipalUser)
		}
	})

	t.Run("service executor inherits joined tx origin", func(t *testing.T) {
		h, _, txMgr, base := newTxJoinTestHandler(t)

		created, err := h.CreateEntity(base, sampleWidgetInput())
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		entityID := created.EntityIDs[0]

		ownerTxID, ownerCtx, err := txMgr.Begin(base)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		tx := spi.GetTransaction(ownerCtx)
		if tx == nil {
			t.Fatal("expected a TransactionState on ownerCtx")
		}
		tx.Origin = spi.Principal{ID: "origin-user", Kind: spi.PrincipalUser}

		serviceCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
			UserID:   "svc-writer",
			UserName: "ServiceWriter",
			Kind:     spi.PrincipalService,
			Tenant:   spi.Tenant{ID: "txjoin-tenant", Name: "TxJoin"},
		})
		joinedCtx, err := txMgr.Join(serviceCtx, ownerTxID)
		if err != nil {
			t.Fatalf("Join: %v", err)
		}

		_, err = h.UpdateEntity(joinedCtx, UpdateEntityInput{
			EntityID: entityID,
			Format:   "JSON",
			Data:     []byte(`{"name":"w3"}`),
		})
		if err != nil {
			t.Fatalf("update in joined tx: %v", err)
		}

		if err := txMgr.Commit(ownerCtx, ownerTxID); err != nil {
			t.Fatalf("owner commit: %v", err)
		}

		meta := getEntityMeta(t, h, entityID)
		if meta.ChangeUser != "origin-user" {
			t.Errorf("ChangeUser = %q, want inherited origin %q", meta.ChangeUser, "origin-user")
		}
		if meta.ChangeUserKind != spi.PrincipalUser {
			t.Errorf("ChangeUserKind = %q, want %q (inherited from origin)", meta.ChangeUserKind, spi.PrincipalUser)
		}
		if meta.ChangeExecutor.ID != "svc-writer" {
			t.Errorf("ChangeExecutor.ID = %q, want %q", meta.ChangeExecutor.ID, "svc-writer")
		}
		if meta.ChangeExecutor.Kind != spi.PrincipalService {
			t.Errorf("ChangeExecutor.Kind = %q, want %q", meta.ChangeExecutor.Kind, spi.PrincipalService)
		}
	})
}
