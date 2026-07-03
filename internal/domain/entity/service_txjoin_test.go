package entity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	wfengine "github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// newTxJoinTestHandler wires a Handler onto a fresh in-memory backend with a
// locked "Widget/1" model (schema {name:String}, no workflow, no unique keys).
// A create against this model runs the engine to a forced-success no-op (no
// segmentation), so finalTxID == entry txID — the plain single-segment shape a
// joined callback exercises.
func newTxJoinTestHandler(t *testing.T) (*Handler, spi.StoreFactory, spi.TransactionManager, context.Context) {
	t.Helper()
	factory := memory.NewStoreFactory()
	ctx := txJoinTestCtx()

	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	engine := wfengine.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	h := New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())

	registerWidgetModel(t, ctx, factory)
	return h, factory, txMgr, ctx
}

func txJoinTestCtx() context.Context {
	return spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID:   "txjoin-user",
		UserName: "TxJoin",
		Tenant:   spi.Tenant{ID: "txjoin-tenant", Name: "TxJoin"},
		Roles:    []string{"user"},
	})
}

func registerWidgetModel(t *testing.T, ctx context.Context, factory spi.StoreFactory) {
	t.Helper()
	node := schema.NewObjectNode()
	node.SetChild("name", schema.NewLeafNode(schema.String))
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	modelStore, err := factory.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if err := modelStore.Save(ctx, &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Widget", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
	}); err != nil {
		t.Fatalf("ModelStore.Save: %v", err)
	}
}

func sampleWidgetInput() CreateEntityInput {
	return CreateEntityInput{
		EntityName:   "Widget",
		ModelVersion: "1",
		Format:       "JSON",
		Data:         json.RawMessage(`{"name":"w"}`),
	}
}

// visibleOutsideTx reports whether an entity is readable through a fresh,
// non-transactional context (i.e. it has been committed to the durable store).
func visibleOutsideTx(t *testing.T, h *Handler, entityID string) bool {
	t.Helper()
	base := txJoinTestCtx()
	store, err := h.factory.EntityStore(base)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	_, err = store.Get(base, entityID)
	if err == nil {
		return true
	}
	if errors.Is(err, spi.ErrNotFound) {
		return false
	}
	t.Fatalf("unexpected Get error: %v", err)
	return false
}

// TestCreateEntity_ParticipatesInJoinedTx is the crux of #287: when ctx already
// carries a joined tx (a routed compute-node callback), CreateEntity must NOT
// open a new tx and must NOT commit — the write stays in the joined tx's buffer
// for the OWNER to commit, and the reported txID is the owner's.
func TestCreateEntity_ParticipatesInJoinedTx(t *testing.T) {
	h, _, txMgr, base := newTxJoinTestHandler(t)

	ownerTxID, ownerCtx, err := txMgr.Begin(base)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Simulate a callback joining the owner tx.
	joinedCtx, err := txMgr.Join(base, ownerTxID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	res, err := h.CreateEntity(joinedCtx, sampleWidgetInput())
	if err != nil {
		t.Fatalf("create in joined tx: %v", err)
	}
	if res.TransactionID != ownerTxID {
		t.Fatalf("participating write should report owner txID %q, got %q", ownerTxID, res.TransactionID)
	}
	if len(res.EntityIDs) != 1 {
		t.Fatalf("expected 1 entity ID, got %d", len(res.EntityIDs))
	}

	// Before the owner commits, the entity is NOT visible outside the tx.
	if visibleOutsideTx(t, h, res.EntityIDs[0]) {
		t.Fatal("joined-callback write leaked before owner commit")
	}

	// Owner commits -> now visible.
	if err := txMgr.Commit(ownerCtx, ownerTxID); err != nil {
		t.Fatalf("owner commit: %v", err)
	}
	if !visibleOutsideTx(t, h, res.EntityIDs[0]) {
		t.Fatal("write not visible after owner commit")
	}
}

// TestCreateEntity_NormalPath_BeginsAndCommits is the regression guard: when
// there is NO joined tx on ctx, CreateEntity Begins its own tx, commits it, and
// the entity is immediately visible — unchanged pre-#287 behavior.
func TestCreateEntity_NormalPath_BeginsAndCommits(t *testing.T) {
	h, _, _, base := newTxJoinTestHandler(t)

	res, err := h.CreateEntity(base, sampleWidgetInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.TransactionID == "" {
		t.Fatal("expected a non-empty owned txID")
	}
	if len(res.EntityIDs) != 1 {
		t.Fatalf("expected 1 entity ID, got %d", len(res.EntityIDs))
	}
	// Owned path commits inline -> immediately visible.
	if !visibleOutsideTx(t, h, res.EntityIDs[0]) {
		t.Fatal("owned create should be visible immediately after commit")
	}
}
