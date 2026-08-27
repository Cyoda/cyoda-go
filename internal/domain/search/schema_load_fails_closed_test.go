package search_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go-spi/predicate"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// A search validates its condition's field paths against the model's schema.
// When that schema cannot be loaded, validateConditionPaths returned nil —
// "validation passed" — and the search answered anyway.
//
// The answer it gives is not merely unvalidated, it is wrong. With no fields
// map, spi.ConditionToFilter stamps an empty Declared on every leaf, and per
// its own godoc that does not degrade every leaf the same way: the eight
// comparison and ordering leaves annihilate to a non-match while the other
// eighteen evaluate normally. So a comparison that should have matched
// silently matches nothing, and the caller receives 200 with a short result
// set indistinguishable from a legitimate one.

// brokenSchemaModelStore answers Get with a descriptor whose schema blob does
// not parse. Model EXISTENCE therefore still resolves — EnsureModelRegistered
// only needs Get to succeed — while every schema-derived view of the model
// fails. That is the split this test needs: the request gets far enough to
// reach path validation, and path validation is what breaks.
//
// It deliberately implements neither RefreshAndGet nor SchemaNode, so
// loadFieldsMap takes its Get + Unmarshal route and the single-refresh retry
// is not in play.
type brokenSchemaModelStore struct {
	ref    spi.ModelRef
	schema []byte
}

func (s *brokenSchemaModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	return &spi.ModelDescriptor{
		Ref:    s.ref,
		State:  spi.ModelLocked,
		Schema: s.schema,
	}, nil
}

func (s *brokenSchemaModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *brokenSchemaModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *brokenSchemaModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *brokenSchemaModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *brokenSchemaModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *brokenSchemaModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *brokenSchemaModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *brokenSchemaModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*brokenSchemaModelStore)(nil)

// TestSearch_SchemaLoadFails_FailsClosedInsteadOfAnswering pins the rule from
// .claude/rules/correctness-over-availability.md: an unavailable dependency a
// correct result requires fails the operation, it does not downgrade it.
//
// The entity stored here DOES satisfy the condition. Before the fix the search
// returned 200 with zero results — a wrong answer the caller cannot tell from
// "nothing matched".
func TestSearch_SchemaLoadFails_FailsClosedInsteadOfAnswering(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	factory := &modelStoreFactory{
		StoreFactory: base,
		modelStore:   &brokenSchemaModelStore{ref: ref, schema: []byte(`{"this is": not valid json`)},
	}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err == nil {
		t.Fatalf("Search returned no error with an unloadable schema; got %d result(s). "+
			"A model-store read failure must not produce a result set.", len(results))
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Status < 500 {
		t.Errorf("appErr.Status = %d, want 5xx: an unloadable schema is a dependency "+
			"failure, not client fault", appErr.Status)
	}
}

// TestSearch_ModelCarriesNoSchema_RejectsTheDataPath covers the sibling arm:
// the descriptor loads cleanly but declares no fields at all, so loadFieldsMap
// returns (nil, nil). That was also treated as "nothing to validate against"
// and the search answered.
//
// A model declaring no fields declares no $.name. Under the rule that a query
// whose path shape contradicts the model is an error — we do not interpret
// model syntax errors flexibly — the path is unknown and the request is client
// fault, not a dependency failure.
func TestSearch_ModelCarriesNoSchema_RejectsTheDataPath(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-1")
	ref := spi.ModelRef{EntityName: "person", ModelVersion: "1"}
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	factory := &modelStoreFactory{
		StoreFactory: base,
		modelStore:   &brokenSchemaModelStore{ref: ref, schema: nil},
	}
	searchStore, _ := base.AsyncSearchStore(context.Background())
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), searchStore)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}

	results, err := svc.Search(ctx, ref, cond, search.SearchOptions{})
	if err == nil {
		t.Fatalf("Search accepted a data path against a model declaring no fields; "+
			"got %d result(s)", len(results))
	}
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if appErr.Code != common.ErrCodeInvalidFieldPath {
		t.Errorf("appErr.Code = %q, want %q", appErr.Code, common.ErrCodeInvalidFieldPath)
	}
}

// switchableModelStore serves a good descriptor until Break is called, then a
// descriptor whose schema blob does not parse. It exists to reach the async
// executor's OWN schema load: submit-time validation and the background job
// are two separate loads, and only the second one can be broken in isolation.
type switchableModelStore struct {
	mu     sync.Mutex
	ref    spi.ModelRef
	good   []byte
	broken bool
}

func (s *switchableModelStore) Break() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = true
}

func (s *switchableModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := s.good
	if s.broken {
		raw = []byte(`{"this is": not valid json`)
	}
	return &spi.ModelDescriptor{Ref: s.ref, State: spi.ModelLocked, Schema: raw}, nil
}

func (s *switchableModelStore) Save(context.Context, *spi.ModelDescriptor) error { return nil }
func (s *switchableModelStore) GetAll(context.Context) ([]spi.ModelRef, error)   { return nil, nil }
func (s *switchableModelStore) Delete(context.Context, spi.ModelRef) error       { return nil }
func (s *switchableModelStore) Lock(context.Context, spi.ModelRef) error         { return nil }
func (s *switchableModelStore) Unlock(context.Context, spi.ModelRef) error       { return nil }
func (s *switchableModelStore) IsLocked(context.Context, spi.ModelRef) (bool, error) {
	return true, nil
}
func (s *switchableModelStore) SetChangeLevel(context.Context, spi.ModelRef, spi.ChangeLevel) error {
	return nil
}
func (s *switchableModelStore) ExtendSchema(context.Context, spi.ModelRef, spi.SchemaDelta) error {
	return nil
}

var _ spi.ModelStore = (*switchableModelStore)(nil)

// breakOnCreateStore breaks the model schema at the moment the job row is
// created — synchronously inside SubmitAsync, after submit-time validation has
// already run and before the worker picks the job up. That makes the window
// deterministic instead of racing the goroutine.
type breakOnCreateStore struct {
	spi.AsyncSearchStore
	ms *switchableModelStore
}

func (s *breakOnCreateStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	s.ms.Break()
	return s.AsyncSearchStore.CreateJob(ctx, job)
}

// TestAsyncSearch_SchemaBreaksAfterSubmit_JobFails covers the executor's own
// load. runAsyncJob discarded the fields-map error outright
// ("fields, _ := loadFieldsMap(...) // best-effort; nil-tolerant") and
// translated the condition against a nil map, so the job completed
// SUCCESSFUL with a result set built from part-annihilated leaves.
func TestAsyncSearch_SchemaBreaksAfterSubmit_JobFails(t *testing.T) {
	base := memory.NewStoreFactory()
	defer base.Close()

	ctx := tenantCtx("tenant-async-broken")
	ref := spi.ModelRef{EntityName: "asyncbroken", ModelVersion: "1"}
	saveEntity(t, ctx, base, ref, "e1", []byte(`{"name":"Alice"}`))

	ms := &switchableModelStore{ref: ref, good: goodStringSchema(t, "name")}
	factory := &modelStoreFactory{StoreFactory: base, modelStore: ms}

	realAsync, err := base.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	store := &breakOnCreateStore{AsyncSearchStore: realAsync, ms: ms}

	pool := search.NewWorkerPool(2, 8)
	t.Cleanup(func() { pool.Drain(context.Background()) })
	svc := search.NewSearchService(factory, common.NewTestUUIDGenerator(), store).WithAsyncPool(pool)

	cond := &predicate.SimpleCondition{
		JsonPath:     "$.name",
		OperatorType: "EQUALS",
		Value:        "Alice",
	}
	jobID, err := svc.SubmitAsync(ctx, ref, cond, search.SearchOptions{})
	if err != nil {
		t.Fatalf("SubmitAsync: %v", err)
	}

	status := pollUntilTerminal(t, svc, ctx, jobID, 5*time.Second)
	if status.Status != "FAILED" {
		t.Fatalf("job status = %q with %d result(s), want FAILED: the executor could "+
			"not load the schema its condition must be typed against, so it must not "+
			"report a result set", status.Status, status.Total)
	}
}

// goodStringSchema marshals an object schema declaring each named field as a
// String leaf.
func goodStringSchema(t *testing.T, fields ...string) []byte {
	t.Helper()
	node := schema.NewObjectNode()
	for _, f := range fields {
		node.SetChild(f, schema.NewLeafNode(schema.String))
	}
	raw, err := schema.Marshal(node)
	if err != nil {
		t.Fatalf("schema.Marshal: %v", err)
	}
	return raw
}
