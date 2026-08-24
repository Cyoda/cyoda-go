package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// Task 14: honor transactionTimeoutMs on the five gRPC entity write events
// (EntityCreateRequest, EntityUpdateRequest, EntityPatchRequest,
// EntityCreateCollectionRequest, EntityUpdateCollectionRequest). See
// .superpowers/sdd/2026-08-11-transaction-control-params-plan/task-14-brief.md.

// sampleTimeoutEntityID is a placeholder entity id used by the invalid/joined
// tests below, which never reach the service layer (resolveEventTimeout
// rejects before any entity lookup), so it need not name a real entity.
const sampleTimeoutEntityID = "00000000-0000-0000-0000-000000000001"

// --- table of the 5 in-scope write events ---

// timeoutOp describes one of the 5 event types that decode
// transactionTimeoutMs, and how to build a CloudEvent payload for it.
type timeoutOp struct {
	name       string
	eventType  string
	collection bool // true for EntityManageCollection events (server-streaming)
	payload    func(millis *int) map[string]any
}

func timeoutOps() []timeoutOp {
	ifMatch := "*"
	return []timeoutOp{
		{
			name:      "Create",
			eventType: EntityCreateRequest,
			payload: func(millis *int) map[string]any {
				m := map[string]any{
					"id":         "test",
					"dataFormat": "JSON",
					"payload": map[string]any{
						"model": map[string]any{"name": "person", "version": 1},
						"data":  map[string]any{"name": "Alice"},
					},
				}
				if millis != nil {
					m["transactionTimeoutMs"] = *millis
				}
				return m
			},
		},
		{
			name:      "Update",
			eventType: EntityUpdateRequest,
			payload: func(millis *int) map[string]any {
				m := map[string]any{
					"id":         "test",
					"dataFormat": "JSON",
					"payload": map[string]any{
						"entityId": sampleTimeoutEntityID,
						"data":     map[string]any{"name": "Bob"},
					},
				}
				if millis != nil {
					m["transactionTimeoutMs"] = *millis
				}
				return m
			},
		},
		{
			name:      "Patch",
			eventType: EntityPatchRequest,
			payload: func(millis *int) map[string]any {
				m := map[string]any{
					"id":          "test",
					"patchFormat": "MERGE_PATCH",
					"payload": map[string]any{
						"entityId": sampleTimeoutEntityID,
						"ifMatch":  ifMatch,
						"patch":    map[string]any{"name": "Bob"},
					},
				}
				if millis != nil {
					m["transactionTimeoutMs"] = *millis
				}
				return m
			},
		},
		{
			name:       "CreateCollection",
			eventType:  EntityCreateCollectionRequest,
			collection: true,
			payload: func(millis *int) map[string]any {
				m := map[string]any{
					"id":         "test",
					"dataFormat": "JSON",
					"payloads": []any{
						map[string]any{
							"model": map[string]any{"name": "person", "version": 1},
							"data":  map[string]any{"name": "A"},
						},
					},
				}
				if millis != nil {
					m["transactionTimeoutMs"] = *millis
				}
				return m
			},
		},
		{
			name:       "UpdateCollection",
			eventType:  EntityUpdateCollectionRequest,
			collection: true,
			payload: func(millis *int) map[string]any {
				m := map[string]any{
					"id":         "test",
					"dataFormat": "JSON",
					"payloads": []any{
						map[string]any{
							"entityId": sampleTimeoutEntityID,
							"data":     map[string]any{"name": "B"},
						},
					},
				}
				if millis != nil {
					m["transactionTimeoutMs"] = *millis
				}
				return m
			},
		},
	}
}

// invoke sends op's CloudEvent through the right RPC (unary EntityManage or
// streaming EntityManageCollection) and returns the decoded
// EntityTransactionResponseJson envelope fields common to all 5 cases.
func (op timeoutOp) invoke(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, millis *int) (success bool, errCode, errMsg string, hasError bool) {
	t.Helper()
	ce := makeCE(op.eventType, op.payload(millis))

	if op.collection {
		stream := &mockManageStream{ctx: ctx}
		if err := svc.EntityManageCollection(ce, stream); err != nil {
			t.Fatalf("%s: unexpected gRPC error: %v", op.name, err)
		}
		if len(stream.sent) != 1 {
			t.Fatalf("%s: expected 1 response, got %d", op.name, len(stream.sent))
		}
		var typed events.EntityTransactionResponseJson
		validateResponse(t, stream.sent[0], &typed)
		if typed.Error != nil {
			return typed.Success, typed.Error.Code, typed.Error.Message, true
		}
		return typed.Success, "", "", false
	}

	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("%s: unexpected gRPC error: %v", op.name, err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Error != nil {
		return typed.Success, typed.Error.Code, typed.Error.Message, true
	}
	return typed.Success, "", "", false
}

// --- (a) invalid transactionTimeoutMs -> 400 BAD_REQUEST ---

// TestRPC_TransactionTimeoutMs_Invalid400 pins that every one of the 5
// in-scope write events rejects a non-positive transactionTimeoutMs with a
// CLIENT_ERROR envelope whose message is prefixed BAD_REQUEST:, before any
// domain call is attempted.
func TestRPC_TransactionTimeoutMs_Invalid400(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})

	for _, op := range timeoutOps() {
		for _, bad := range []int{0, -1} {
			bad := bad
			t.Run(fmt.Sprintf("%s/%d", op.name, bad), func(t *testing.T) {
				success, code, msg, hasErr := op.invoke(t, svc, ctx, &bad)
				if success {
					t.Fatal("expected success=false")
				}
				if !hasErr {
					t.Fatal("expected error field to be populated")
				}
				if code != "CLIENT_ERROR" {
					t.Errorf("code = %q, want CLIENT_ERROR", code)
				}
				if !strings.HasPrefix(msg, common.ErrCodeBadRequest+":") {
					t.Errorf("message = %q, want prefix %q", msg, common.ErrCodeBadRequest+":")
				}
			})
		}
	}
}

// --- (b) tx-token'd (joined) request + field set -> same 400 rejection ---

// TestRPC_TransactionTimeoutMs_JoinedRejected400 pins that a request carrying
// a joined transaction (spi.GetTransaction(ctx) != nil — how a routed
// compute-node callback presents at param-resolution time) is rejected with
// CLIENT_ERROR/BAD_REQUEST rather than silently honoring or silently
// ignoring the client-supplied deadline.
func TestRPC_TransactionTimeoutMs_JoinedRejected400(t *testing.T) {
	svc, ctx := newTestEnv(t)
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})
	joinedCtx := spi.WithTransaction(ctx, &spi.TransactionState{ID: "tx-1"})
	millis := 5000

	for _, op := range timeoutOps() {
		t.Run(op.name, func(t *testing.T) {
			success, code, msg, hasErr := op.invoke(t, svc, joinedCtx, &millis)
			if success {
				t.Fatal("expected success=false")
			}
			if !hasErr {
				t.Fatal("expected error field to be populated")
			}
			if code != "CLIENT_ERROR" {
				t.Errorf("code = %q, want CLIENT_ERROR", code)
			}
			if !strings.HasPrefix(msg, common.ErrCodeBadRequest+":") {
				t.Errorf("message = %q, want prefix %q", msg, common.ErrCodeBadRequest+":")
			}
			if !strings.Contains(msg, "joins an open transaction") {
				t.Errorf("message = %q, want mention of the joined transaction", msg)
			}
		})
	}
}

// --- fakes for (c)/(d)/(e): a blocking store and a Commit-hooking tx manager ---

// blockingEntityStore blocks Save until ctx is Done and returns ctx.Err() —
// deterministic by construction, never races a wall-clock sleep against the
// deadline. Mirrors internal/domain/entity's handler_reqtimeout_test.go fake.
type blockingEntityStore struct {
	spi.EntityStore
	mu    sync.Mutex
	saves int
}

func (s *blockingEntityStore) Save(ctx context.Context, _ *spi.Entity) (int64, error) {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	<-ctx.Done()
	return 0, ctx.Err()
}

// blockingStoreFactory wraps a real spi.StoreFactory but always hands out the
// given blockingEntityStore from EntityStore(); every other accessor
// delegates to the wrapped factory.
type blockingStoreFactory struct {
	spi.StoreFactory
	entityStore *blockingEntityStore
}

func (f *blockingStoreFactory) EntityStore(_ context.Context) (spi.EntityStore, error) {
	return f.entityStore, nil
}

// onCommitTxMgr wraps a real spi.TransactionManager, running onCommit
// (if set) synchronously before delegating to the real Commit — lets a test
// observe/manipulate what the commit's own ctx looks like while a commit is
// genuinely in flight, and count commits.
type onCommitTxMgr struct {
	spi.TransactionManager
	mu       sync.Mutex
	commits  int
	onCommit func(ctx context.Context)
}

func (m *onCommitTxMgr) Commit(ctx context.Context, txID string) error {
	m.mu.Lock()
	m.commits++
	m.mu.Unlock()
	if m.onCommit != nil {
		m.onCommit(ctx)
	}
	return m.TransactionManager.Commit(ctx, txID)
}

func (m *onCommitTxMgr) commitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commits
}

// newTimeoutTestUserCtx builds a context carrying a test user/tenant, mirroring
// newTestEnv's user context.
func newTimeoutTestUserCtx() context.Context {
	uc := &spi.UserContext{
		UserID:   "timeout-test-user",
		UserName: "Timeout Test",
		Tenant:   spi.Tenant{ID: "timeout-tenant", Name: "Timeout Tenant"},
		Roles:    []string{"ADMIN"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

// importLockPerson imports and locks the "person" model ({name: String}) via
// h against the given ctx.
func importLockPerson(t *testing.T, h *model.Handler, ctx context.Context) {
	t.Helper()
	dataBytes, err := json.Marshal(map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("marshal sample data: %v", err)
	}
	if _, err := h.ImportModel(ctx, model.ImportModelInput{
		EntityName: "person", ModelVersion: "1", Format: "JSON",
		Converter: "SAMPLE_DATA", Data: dataBytes,
	}); err != nil {
		t.Fatalf("ImportModel: %v", err)
	}
	if _, err := h.LockModel(ctx, "person", "1"); err != nil {
		t.Fatalf("LockModel: %v", err)
	}
}

// --- (c) blocking store -> 408 TRANSACTION_TIMEOUT, Retryable:true ---

// TestRPC_EntityCreate_TransactionTimeoutMs_BlockingStore_408 pins spec D2/D8
// end to end through the gRPC door: a transactionTimeoutMs:1 deadline that
// genuinely expires while Save blocks surfaces as CLIENT_ERROR with a
// TRANSACTION_TIMEOUT: prefixed message and Retryable:true.
func TestRPC_EntityCreate_TransactionTimeoutMs_BlockingStore_408(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realFactory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := realFactory.GetTransactionManager()

	ctx := newTimeoutTestUserCtx()
	modelHandler := model.New(realFactory)
	importLockPerson(t, modelHandler, ctx)

	blocking := &blockingEntityStore{}
	factory := &blockingStoreFactory{StoreFactory: realFactory, entityStore: blocking}

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, _ := factory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New())

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	millis := 1
	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":                   "test",
		"dataFormat":           "JSON",
		"transactionTimeoutMs": millis,
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})

	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if typed.Success {
		t.Fatal("expected success=false")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if typed.Error.Code != "CLIENT_ERROR" {
		t.Errorf("code = %q, want CLIENT_ERROR", typed.Error.Code)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeTransactionTimeout+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeTransactionTimeout+":")
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Errorf("Retryable = %v, want true", typed.Error.Retryable)
	}
}

// --- (d) commit-wins: deadline expires after commit already started -> Success:true ---

// TestRPC_EntityCreate_TransactionTimeoutMs_CommitWins_Success pins that once
// Commit has legitimately started (the pre-commit check passed while the
// request ctx was still live), the commit runs shielded and is not aborted
// by the request deadline firing mid-commit (spec D2's "an interrupted
// commit is an in-doubt outcome, never a rollback-able one").
func TestRPC_EntityCreate_TransactionTimeoutMs_CommitWins_Success(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realFactory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	realTxMgr := realFactory.GetTransactionManager()

	var sawCancellation bool
	rtm := &onCommitTxMgr{TransactionManager: realTxMgr}
	rtm.onCommit = func(ctx context.Context) {
		// Outlives the request deadline below by a wide margin; the shielded
		// commit ctx must show no cancellation regardless of how long this runs.
		time.Sleep(50 * time.Millisecond)
		if ctx.Err() != nil {
			sawCancellation = true
		}
	}

	ctx := newTimeoutTestUserCtx()
	modelHandler := model.New(realFactory)
	importLockPerson(t, modelHandler, ctx)

	engine := workflow.NewEngine(realFactory, common.NewDefaultUUIDGenerator(), rtm)
	searchStore, _ := realFactory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(realFactory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(realFactory, rtm, common.NewDefaultUUIDGenerator(), engine, txgate.New())

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         rtm,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	millis := 10
	ce := makeCE(EntityCreateRequest, map[string]any{
		"id":                   "test",
		"dataFormat":           "JSON",
		"transactionTimeoutMs": millis,
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": "Alice"},
		},
	})

	resp, err := svc.EntityManage(ctx, ce)
	if err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	if sawCancellation {
		t.Fatal("commit ctx observed the request deadline mid-commit; commit must run shielded")
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true, got error: %+v", typed.Error)
	}
	if rtm.commitCount() != 1 {
		t.Fatalf("commits = %d, want 1", rtm.commitCount())
	}
}

// --- (e) EntityUpdateCollectionRequest: ONE deadline for the whole case,
// loop-head ctx.Err() check per spec D9 ---

// TestRPC_EntityUpdateCollection_TransactionTimeoutMs_LoopHeadStopsSecondItem
// pins that EntityUpdateCollectionRequest attaches a single deadline around
// the whole per-item loop (not one per item) and checks ctx.Err() at each
// iteration head: once the (already-committed) first item's commit outlives
// the deadline, the second item must never be attempted, and the response
// routes through the case's existing whole-envelope error path with a
// TRANSACTION_TIMEOUT-coded, retryable CLIENT_ERROR.
func TestRPC_EntityUpdateCollection_TransactionTimeoutMs_LoopHeadStopsSecondItem(t *testing.T) {
	realFactory := memory.NewStoreFactory()
	realFactory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	realTxMgr := realFactory.GetTransactionManager()
	rtm := &onCommitTxMgr{TransactionManager: realTxMgr}

	ctx := newTimeoutTestUserCtx()
	modelHandler := model.New(realFactory)
	importLockPerson(t, modelHandler, ctx)

	engine := workflow.NewEngine(realFactory, common.NewDefaultUUIDGenerator(), rtm)
	searchStore, _ := realFactory.AsyncSearchStore(context.Background())
	searchService := search.NewSearchService(realFactory, common.NewDefaultUUIDGenerator(), searchStore)
	entityHandler := entity.New(realFactory, rtm, common.NewDefaultUUIDGenerator(), engine, txgate.New())

	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         rtm,
		entityHandler: entityHandler,
		modelHandler:  modelHandler,
		searchService: searchService,
	}

	// Seed 2 real entities to update, before the onCommit hook is armed.
	ids := make([]string, 0, 2)
	for _, name := range []string{"orig-0", "orig-1"} {
		createCE := makeCE(EntityCreateRequest, map[string]any{
			"id":         "seed",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": name},
			},
		})
		resp, err := svc.EntityManage(ctx, createCE)
		if err != nil {
			t.Fatalf("seed create %s: %v", name, err)
		}
		payload := parseResponsePayload(t, resp)
		txInfo := payload["transactionInfo"].(map[string]any)
		ids = append(ids, txInfo["entityIds"].([]any)[0].(string))
	}

	// Arm the hook only for the update-collection call under test: on the
	// first commit (item 0), sleep well past the 10ms deadline below, so by
	// the time the loop reaches item 1's iteration head the deadline has
	// genuinely, naturally expired.
	rtm.onCommit = func(_ context.Context) {
		time.Sleep(100 * time.Millisecond)
	}

	millis := 10
	ce := makeCE(EntityUpdateCollectionRequest, map[string]any{
		"id":                   "test",
		"dataFormat":           "JSON",
		"transactionTimeoutMs": millis,
		"payloads": []any{
			map[string]any{"entityId": ids[0], "data": map[string]any{"name": "X"}},
			map[string]any{"entityId": ids[1], "data": map[string]any{"name": "Y"}},
		},
	})

	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("unexpected gRPC error: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false")
	}
	if typed.Error == nil {
		t.Fatal("expected error field to be populated")
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeTransactionTimeout+":") {
		t.Errorf("message = %q, want prefix %q", typed.Error.Message, common.ErrCodeTransactionTimeout+":")
	}
	if typed.Error.Retryable == nil || !*typed.Error.Retryable {
		t.Errorf("Retryable = %v, want true", typed.Error.Retryable)
	}

	// commits == 2 (seed items 0/1 during setup happen on the unwrapped path
	// above... wait: seeding used svc.EntityManage with rtm already wired, so
	// those 2 seed commits count too). Only 1 MORE commit (item 0 of the
	// update) should have happened during the update-collection call itself.
	if got := rtm.commitCount(); got != 3 {
		t.Fatalf("total commits = %d, want 3 (2 seed creates + 1 update-collection item 0); item 1 must never be attempted", got)
	}

	// Decisive: item 0's update is durable, item 1 was never attempted.
	realEntityStore, err := realFactory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	e0, err := realEntityStore.Get(ctx, ids[0])
	if err != nil {
		t.Fatalf("Get(%s): %v", ids[0], err)
	}
	var p0 struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(e0.Data, &p0); err != nil {
		t.Fatalf("decode entity 0: %v", err)
	}
	if p0.Name != "X" {
		t.Fatalf("entity 0 name = %q, want \"X\" (item 0's commit must be durable)", p0.Name)
	}

	e1, err := realEntityStore.Get(ctx, ids[1])
	if err != nil {
		t.Fatalf("Get(%s): %v", ids[1], err)
	}
	var p1 struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(e1.Data, &p1); err != nil {
		t.Fatalf("decode entity 1: %v", err)
	}
	if p1.Name != "orig-1" {
		t.Fatalf("entity 1 name = %q, want \"orig-1\" (item 1 must never have been attempted)", p1.Name)
	}
}
