package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// ownerNode bundles a real (memory-backed) CloudEventsServiceImpl with the
// concrete store factory + transaction manager behind it, so a test can open a
// transaction on this node, buffer an uncommitted write into it, and later join
// that live transaction the way the tx-route interceptor's local-join branch
// does. It models "node A" — the transaction owner.
type ownerNode struct {
	svc     *CloudEventsServiceImpl
	factory *memory.StoreFactory
	txMgr   spi.TransactionManager
	uc      *spi.UserContext
}

func newOwnerNode(t *testing.T) *ownerNode {
	t.Helper()

	factory := memory.NewStoreFactory()
	factory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := factory.GetTransactionManager()

	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "Test User",
		Tenant:   spi.Tenant{ID: "test-tenant", Name: "Test Tenant"},
		Roles:    []string{"ADMIN"},
	}

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), searchService)
	modelHandler := model.New(factory)

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}
	return &ownerNode{svc: svc, factory: factory, txMgr: txMgr, uc: uc}
}

func (o *ownerNode) ctx() context.Context {
	return spi.WithUserContext(context.Background(), o.uc)
}

// TestTxRouteInterceptor_SearchForwardReturnsOwnerUncommittedWrites is the
// multi-node co-location proof for in-transaction search.
//
// INVARIANT: an in-tx Searcher.Search requires co-location with the tx owner.
// A transaction's buffered (uncommitted) writes live only in the owner node's
// TransactionState; no other node can see them. Forwarding the search to the
// owner — the tx-route interceptor's B→A proxy branch — is what makes
// read-your-own-writes hold across nodes. Without the forward, node B would run
// the search against its own store (which has neither the committed row nor the
// owner's buffer) and return an empty/committed-only result — a correctness
// violation, not a mere availability degradation.
//
// This test composes the REAL pieces end-to-end, stubbing only the network hop:
//
//	node B interceptor (non-owner) — recognises the token names node A, proxies
//	  └─ forwardSearchStream seam (stands in for the gRPC wire to node A)
//	       └─ txjoin.JoinFromToken(node A's txMgr)  — owner joins the LIVE tx
//	            └─ node A's real EntitySearchCollection → overlay Searcher.Search
//	                 └─ returns the owner's UNCOMMITTED buffered row (RYW)
//
// It asserts the RYW row crosses the B→A→B boundary — not merely that a forward
// happened. The negative control (a committed-only search on node A sees
// nothing) proves the row is genuinely uncommitted and only visible through the
// owner's live-tx overlay.
func TestTxRouteInterceptor_SearchForwardReturnsOwnerUncommittedWrites(t *testing.T) {
	signer, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	// --- node A (owner): register a model, open a tx, buffer an uncommitted write.
	owner := newOwnerNode(t)
	importAndLockModel(t, owner.svc, owner.ctx(), "person", "1", map[string]any{"surname": "Smith"})

	txID, txCtx, err := owner.txMgr.Begin(owner.ctx())
	if err != nil {
		t.Fatalf("Begin tx on owner: %v", err)
	}

	bufferedID := uuid.NewString()
	store, err := owner.factory.EntityStore(txCtx)
	if err != nil {
		t.Fatalf("owner EntityStore: %v", err)
	}
	if _, err := store.Save(txCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID:       bufferedID,
			TenantID: spi.TenantID(owner.uc.Tenant.ID),
			ModelRef: spi.ModelRef{EntityName: "person", ModelVersion: "1"},
			State:    "active",
		},
		Data: []byte(`{"surname":"Buffered"}`),
	}); err != nil {
		t.Fatalf("buffer uncommitted write: %v", err)
	}
	// NOTE: no Commit — the write lives only in the owner's live tx buffer.

	// A direct in-tx search whose CloudEvent matches every person in the model.
	searchCE := makeCE(EntitySearchRequest, map[string]any{
		"id":    "ryw-search-1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
	})

	// --- negative control: a committed-only search on node A (no tx joined) must
	// NOT see the buffered row. This pins that the row is genuinely uncommitted,
	// so any later sighting of it can only come from the owner's live-tx overlay.
	committedOnly := &mockEntityStream{ctx: owner.ctx()}
	if err := owner.svc.EntitySearchCollection(searchCE, committedOnly); err != nil {
		t.Fatalf("committed-only search: %v", err)
	}
	if len(committedOnly.sent) != 0 {
		t.Fatalf("committed-only search must return 0 rows (write is uncommitted), got %d", len(committedOnly.sent))
	}

	// --- token names node A; the referenced tx is the one we just buffered into.
	tok, err := signer.Issue("node-A", txID, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue token: %v", err)
	}

	// --- node A owner-side forward target: join the LIVE tx exactly as the
	// interceptor's local-join branch does (txjoin.JoinFromToken with node A's
	// txMgr), then run node A's REAL search handler on that tx-joined context.
	// This is the co-location: the overlay Searcher only sees the buffer because
	// it executes on the node that holds the tx.
	ownerHandle := func(ce *cepb.CloudEvent) (*fakeClientStream, error) {
		ownerCtx, jerr := txjoin.JoinFromToken(owner.ctx(), signer, owner.txMgr, tok)
		if jerr != nil {
			return nil, jerr
		}
		if spi.GetTransaction(ownerCtx) == nil {
			t.Fatal("owner must join the live tx from the token (co-location precondition)")
		}
		cap := &mockEntityStream{ctx: ownerCtx}
		if herr := owner.svc.EntitySearchCollection(ce, cap); herr != nil {
			return nil, herr
		}
		return &fakeClientStream{frames: cap.sent}, nil
	}

	// --- node B (non-owner): interceptor with its OWN empty txMgr (it does not
	// and cannot hold the owner's tx). Its registry knows node A is alive, so a
	// node-A token proxies. The forwardSearchStream seam stands in for the gRPC
	// wire hop to node A and returns node A's overlay frames.
	regB := fakeRouteRegistry{nodes: map[string]contract.NodeInfo{
		"node-A": {NodeID: "node-A", Addr: "http://node-a:8080", Alive: true},
	}}
	nodeB := newTxRouteInterceptor(signer, regB, "node-B", fakeJoinTM{}, 9090, true)

	var forwardedAddr string
	nodeB.forwardSearchStream = func(_ context.Context, _ *proxy.ClientPool, addr string, ce *cepb.CloudEvent) (googlegrpc.ServerStreamingClient[cepb.CloudEvent], error) {
		forwardedAddr = addr
		return ownerHandle(ce)
	}

	baseCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("tx-token", tok))
	inbound := &fakeServerStream{ctx: baseCtx, recv: []*cepb.CloudEvent{searchCE}}

	handler := func(any, googlegrpc.ServerStream) error {
		t.Fatal("node B must forward the in-tx search to the owner, not run it locally")
		return nil
	}
	if err := nodeB.stream()(nil, inbound, entitySearchCollectionInfo(), handler); err != nil {
		t.Fatalf("node B stream interceptor: %v", err)
	}

	// The forward must have targeted node A's derived gRPC addr.
	if forwardedAddr != "node-a:9090" {
		t.Fatalf("expected forward to node A's gRPC addr %q, got %q", "node-a:9090", forwardedAddr)
	}

	// RYW ASSERTION: the owner's uncommitted buffered row crossed B→A→B and was
	// streamed back verbatim to node B's caller. A canned/mock forward or a
	// local (non-owner) execution would fail here.
	if len(inbound.sent) != 1 {
		t.Fatalf("expected exactly 1 forwarded RYW row, got %d", len(inbound.sent))
	}
	resp := decodeEntityRespCE(t, inbound.sent[0])
	if !resp.Success {
		t.Fatalf("expected Success=true, got error %+v", resp.Error)
	}
	meta, _ := resp.Payload.Meta.(map[string]any)
	gotID, _ := meta["id"].(string)
	if gotID != bufferedID {
		t.Fatalf("forwarded row id = %q, want the owner's uncommitted buffered id %q", gotID, bufferedID)
	}
	data, _ := resp.Payload.Data.(map[string]any)
	if data == nil || data["surname"] != "Buffered" {
		t.Fatalf("forwarded row must carry the owner's buffered data {surname:Buffered}, got %+v", resp.Payload.Data)
	}
}
