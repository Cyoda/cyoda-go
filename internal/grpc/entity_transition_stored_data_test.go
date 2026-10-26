package grpc

import (
	"context"
	"encoding/json"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// storedOrder is entity data whose member order a decode and re-encode would
// change (encoding/json writes map keys sorted), so it shows whether the
// transition kept the stored bytes or rewrote them.
const storedOrder = `{"zname":"x","aname":"y"}`

// firstReadFactory runs onFirstRead once, right after the first read of
// entity id armed, before that read returns: a write committed by someone else
// between this request's read and its own write.
type firstReadFactory struct {
	spi.StoreFactory
	armed       string
	onFirstRead func()
}

func (f *firstReadFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	s, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	return &firstReadStore{EntityStore: s, f: f}, nil
}

type firstReadStore struct {
	spi.EntityStore
	f *firstReadFactory
}

func (s *firstReadStore) Get(ctx context.Context, id string) (*spi.Entity, error) {
	e, err := s.EntityStore.Get(ctx, id)
	if s.f.armed != "" && id == s.f.armed {
		s.f.armed = ""
		s.f.onFirstRead()
	}
	return e, err
}

// newTransitionTestEnv is newTestEnv over a firstReadFactory, which it also
// returns so a test can arm it and read the stored entity.
func newTransitionTestEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context, *firstReadFactory) {
	t.Helper()
	base := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply))
	base.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := base.GetTransactionManager()
	factory := &firstReadFactory{StoreFactory: base}

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), newTestConsistency(t, factory)),
		modelHandler:  model.New(factory),
		searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore, newTestConsistency(t, factory)),
	}
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "test-user", UserName: "Test User",
		Tenant: spi.Tenant{ID: "test-tenant", Name: "Test Tenant"},
		Roles:  []string{"ADMIN"},
	})
	importAndLockModel(t, svc, ctx, "ordered", "1", map[string]any{"zname": "x", "aname": "y"})
	return svc, ctx, factory
}

// createStored creates an entity whose stored bytes are exactly storedOrder.
func createStored(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, f *firstReadFactory) string {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "create", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "ordered", "version": 1},
			"data":  json.RawMessage(storedOrder),
		},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	txInfo := parseResponsePayload(t, resp)["transactionInfo"].(map[string]any)
	id := txInfo["entityIds"].([]any)[0].(string)
	if got := storedData(t, ctx, f, id); got != storedOrder {
		t.Fatalf("precondition: create stored %s, want %s", got, storedOrder)
	}
	return id
}

func storedData(t *testing.T, ctx context.Context, f *firstReadFactory, id string) string {
	t.Helper()
	s, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("entity store: %v", err)
	}
	e, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("read stored entity: %v", err)
	}
	return string(e.Data)
}

func transition(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, id string) events.EntityTransitionResponseJson {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityTransitionRequest, map[string]any{
		"id": "transition", "entityId": id, "transition": "UPDATE",
	}))
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	return typed
}

// A transition changes the state, not the data: the stored bytes stay as they
// are, not decoded and encoded again.
func TestRPC_EntityTransition_KeepsStoredBytes(t *testing.T) {
	svc, ctx, f := newTransitionTestEnv(t)
	id := createStored(t, svc, ctx, f)

	if typed := transition(t, svc, ctx, id); !typed.Success {
		t.Fatalf("transition failed: %+v", typed.Error)
	}
	if got := storedData(t, ctx, f, id); got != storedOrder {
		t.Errorf("stored data after transition = %s, want the stored bytes %s", got, storedOrder)
	}
}

// A write another request commits while the transition runs is not lost: the
// transition reads the data it keeps inside its own transaction, so it either
// keeps the competing write's data or fails its commit.
func TestRPC_EntityTransition_ConcurrentWriteNotLost(t *testing.T) {
	svc, ctx, f := newTransitionTestEnv(t)
	id := createStored(t, svc, ctx, f)

	const competing = `{"zname":"competitor","aname":"y"}`
	f.armed = id
	f.onFirstRead = func() {
		if _, err := svc.entityHandler.UpdateEntity(ctx, entity.UpdateEntityInput{
			EntityID: id, Format: "JSON", Data: json.RawMessage(competing),
		}); err != nil {
			t.Fatalf("competing write: %v", err)
		}
	}

	typed := transition(t, svc, ctx, id)
	if f.armed != "" {
		t.Fatal("the competing write never ran")
	}
	if !typed.Success && (typed.Error == nil || typed.Error.Retryable == nil || !*typed.Error.Retryable) {
		t.Fatalf("transition failed with %+v; want success or a retryable conflict", typed.Error)
	}
	if got := storedData(t, ctx, f, id); got != competing {
		t.Errorf("stored data = %s, want the competing write %s (success=%v)", got, competing, typed.Success)
	}
}
