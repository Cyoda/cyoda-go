package grpc

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// trackingReadSearcherEntityStore wraps a real memory EntityStore and records
// the spi.SearchOptions passed to every Search call, so tests can assert what
// the gRPC handler threaded through from the CloudEvent payload down to the
// SPI boundary — the same DTO-to-SearchOptions boundary the HTTP handler test
// (internal/domain/search/handler_tracking_read_test.go) exercises.
type trackingReadSearcherEntityStore struct {
	spi.EntityStore
	captured *spi.SearchOptions
}

func (s *trackingReadSearcherEntityStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	*s.captured = opts
	searcher, ok := s.EntityStore.(spi.Searcher)
	if !ok {
		return nil, nil
	}
	return searcher.Search(ctx, filter, opts)
}

// trackingReadSearcherFactory wraps a StoreFactory and returns the spy
// EntityStore above from EntityStore(ctx).
type trackingReadSearcherFactory struct {
	spi.StoreFactory
	entityStore *trackingReadSearcherEntityStore
}

func (f *trackingReadSearcherFactory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	real, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	f.entityStore.EntityStore = real
	return f.entityStore, nil
}

// newTrackingReadTestEnv builds a CloudEventsServiceImpl wired to a real
// in-memory store via a spy factory that captures the spi.SearchOptions
// passed to every Search call. The returned closure reads the most recently
// captured options.
func newTrackingReadTestEnv(t *testing.T) (*CloudEventsServiceImpl, context.Context, func() spi.SearchOptions) {
	t.Helper()

	base := memory.NewStoreFactory()
	base.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := base.GetTransactionManager()

	var captured spi.SearchOptions
	spyStore := &trackingReadSearcherEntityStore{captured: &captured}
	factory := &trackingReadSearcherFactory{StoreFactory: base, entityStore: spyStore}

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

	ctx := spi.WithUserContext(context.Background(), uc)
	return svc, ctx, func() spi.SearchOptions { return captured }
}

// TestEntitySearch_DirectSearch_TrackingReadTrue_ReachesSearchOptions verifies
// that a sync (direct) gRPC search request carrying trackingRead:true in the
// CloudEvent payload maps through to spi.SearchOptions.TrackingRead == true at
// the SearchService.DirectSearch call — the sync, tx-token-join-reachable
// search path. The async snapshot-submit path is detached (background
// ctx, no tx) and does not expose trackingRead — mirrors the HTTP rule.
func TestEntitySearch_DirectSearch_TrackingReadTrue_ReachesSearchOptions(t *testing.T) {
	svc, ctx, capturedOpts := newTrackingReadTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "tr-true-1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
		"trackingRead": true,
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if !capturedOpts().TrackingRead {
		t.Errorf("capturedOpts().TrackingRead = false, want true (trackingRead:true on CloudEvent payload)")
	}
}

// TestEntitySearch_DirectSearch_TrackingReadAbsent_DefaultsFalse verifies the
// default: when trackingRead is absent from the CloudEvent payload,
// spi.SearchOptions.TrackingRead must be false.
func TestEntitySearch_DirectSearch_TrackingReadAbsent_DefaultsFalse(t *testing.T) {
	svc, ctx, capturedOpts := newTrackingReadTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"surname": "Smith"})

	ce := makeCE(EntitySearchRequest, map[string]any{
		"id":    "tr-absent-1",
		"model": map[string]any{"name": "person", "version": 1},
		"condition": map[string]any{
			"type": "group", "operator": "AND", "conditions": []any{},
		},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if capturedOpts().TrackingRead {
		t.Errorf("capturedOpts().TrackingRead = true, want false (trackingRead absent from CloudEvent payload)")
	}
}
