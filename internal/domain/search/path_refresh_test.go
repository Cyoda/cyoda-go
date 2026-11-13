package search_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// heldReadModelStore is a spi.ModelStore whose Get can be held after it has
// read the stored descriptor: the caller then returns what it read before the
// hold, so a write that lands during the hold is not visible to that read.
type heldReadModelStore struct {
	mu      sync.Mutex
	current *spi.ModelDescriptor
	hold    chan struct{} // when non-nil, the next Get blocks on it after reading
	reached chan struct{} // closed by the held Get once it has read
}

func (s *heldReadModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	read := s.current
	hold, reached := s.hold, s.reached
	s.hold, s.reached = nil, nil
	s.mu.Unlock()
	if hold != nil {
		close(reached)
		<-hold
	}
	return read, nil
}

func (s *heldReadModelStore) armHold() (release func(), reached <-chan struct{}) {
	h, r := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.hold, s.reached = h, r
	s.mu.Unlock()
	return func() { close(h) }, r
}

// setCurrent replaces the stored descriptor, as a committed extension does.
func (s *heldReadModelStore) setCurrent(d *spi.ModelDescriptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = d
}

// ExtendSchema does not interpret the delta; the test sets the new schema itself.
func (s *heldReadModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}
func (s *heldReadModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *heldReadModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *heldReadModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *heldReadModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *heldReadModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *heldReadModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *heldReadModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}

var _ spi.ModelStore = (*heldReadModelStore)(nil)

// TestSearch_OwnNewField_AfterRacingSearch_Found pins that a search naming a
// field the client's own write has just added is answered from storage, not
// from a remembered refusal or a stale fill. Search S1 names the field before
// it exists and is held after its refresh has read storage; the extension then
// adds the field and invalidates. The cache drops S1's straddling fill, so S1's
// recheck reads the extended schema and finds the field. Search S2 on the same
// field must find it too.
func TestSearch_OwnNewField_AfterRacingSearch_Found(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"p":"hello"}`))

	inner := &heldReadModelStore{current: buildSearchDescriptor(t, ref, "a")}
	caching := modelcache.NewCachingStoreFactory(&modelStoreFactory{StoreFactory: base, modelStore: inner}, nil, nil, time.Hour)

	uuids := common.NewTestUUIDGenerator()
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(caching, uuids, searchStore, newTestConsistency(t, caching), platformclock.Real())

	cond := &predicate.SimpleCondition{JsonPath: "$.p", OperatorType: "EQUALS", Value: "hello"}

	// Warm the cache with the schema that lacks p.
	ms, err := caching.ModelStore(ctx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if _, err := ms.Get(ctx, ref); err != nil {
		t.Fatalf("warm Get: %v", err)
	}

	release, reached := inner.armHold()
	s1 := make(chan error, 1)
	go func() {
		_, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
		s1 <- err
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("S1 never reached the held storage read")
	}

	// The extension lands and invalidates while S1's read is held.
	inner.setCurrent(buildSearchDescriptor(t, ref, "a", "p"))
	if err := ms.ExtendSchema(ctx, ref, spi.SchemaDelta("{}")); err != nil {
		t.Fatalf("ExtendSchema: %v", err)
	}
	release()

	if err := <-s1; err != nil {
		t.Fatalf("S1: its refresh straddled the extension, so the fill is dropped and the recheck reads the field; got %v", err)
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("S2: the field was added, want it found, got %v", err)
	}
	if len(results) != 1 || results[0].Meta.ID != "e1" {
		t.Fatalf("S2: want e1, got %d results", len(results))
	}
}

// TestRefusal_NamesOnlyPathsTheRefreshedSchemaLacks pins that a refusal is
// decided on the refreshed schema: the cached schema lacks x and y, the
// refreshed one has x, so the 400 names y alone and never a path the model has.
func TestRefusal_NamesOnlyPathsTheRefreshedSchemaLacks(t *testing.T) {
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}

	newSvc := func(t *testing.T) (*search.SearchService, *refreshingModelStore) {
		t.Helper()
		base := memory.NewStoreFactory()
		t.Cleanup(func() { base.Close() })
		ms := &refreshingModelStore{
			getQueue:     []*spi.ModelDescriptor{buildSearchDescriptor(t, ref, "a"), buildSearchDescriptor(t, ref, "a")},
			refreshQueue: []*spi.ModelDescriptor{buildSearchDescriptor(t, ref, "a", "x")},
		}
		factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}
		searchStore, _ := base.AsyncSearchStore(context.Background())
		return search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore, newTestConsistency(t, factory), platformclock.Real()), ms
	}
	refusal := func(t *testing.T, err error) string {
		t.Helper()
		var appErr *common.AppError
		if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeInvalidFieldPath {
			t.Fatalf("want INVALID_FIELD_PATH, got %v", err)
		}
		return appErr.Message
	}

	t.Run("condition", func(t *testing.T) {
		svc, _ := newSvc(t)
		cond := &predicate.GroupCondition{Operator: "AND", Conditions: []predicate.Condition{
			&predicate.SimpleCondition{JsonPath: "$.x", OperatorType: "EQUALS", Value: "1"},
			&predicate.SimpleCondition{JsonPath: "$.y", OperatorType: "EQUALS", Value: "1"},
		}}
		_, err := svc.Search(tenantCtx("tenant-1"), ref, cond, search.SearchOptions{Limit: 10})
		msg := refusal(t, err)
		if !strings.Contains(msg, "$.y") || strings.Contains(msg, "$.x") {
			t.Errorf("refusal must name $.y only, got %q", msg)
		}
	})

	t.Run("sort", func(t *testing.T) {
		svc, _ := newSvc(t)
		keys := []search.OrderKey{{Path: "$.x", Source: spi.SourceData}, {Path: "$.y", Source: spi.SourceData}}
		_, err := svc.ResolveSortKeysForTest(tenantCtx("tenant-1"), ref, keys)
		msg := refusal(t, err)
		if !strings.Contains(msg, "y") || strings.Contains(msg, "x") {
			t.Errorf("refusal must name y only, got %q", msg)
		}
	})

	t.Run("ValidateKnownPaths", func(t *testing.T) {
		_, ms := newSvc(t)
		fields := map[string]schema.FieldDescriptor{}
		_, err := search.ValidateKnownPaths(context.Background(), ms, ref, []string{"$.x", "$.y"}, fields)
		msg := refusal(t, err)
		if !strings.Contains(msg, "$.y") || strings.Contains(msg, "$.x") {
			t.Errorf("refusal must name $.y only, got %q", msg)
		}
	})
}
