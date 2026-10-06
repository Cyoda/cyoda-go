package grpc

// consistency_fence_test.go — every gRPC read that takes an instant answers the
// consistency-time fence inside its CloudEvents envelope: a later instant is a
// non-retryable CLIENT_ERROR carrying POINT_IN_TIME_AFTER_CONSISTENCY_TIME, an
// unobtainable consistency time is retryable, and an instant at or before it is
// served. Reads with an instant return the data as at that instant.

import (
	"context"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/consistency"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/domain/workflow"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// fenceEnv is a gRPC service whose consistency time is the instant or error the
// test chose, over a memory store whose clock the test drives.
type fenceEnv struct {
	svc    *CloudEventsServiceImpl
	ctx    context.Context
	clock  *memory.TestClock
	jobs   spi.AsyncSearchStore
	tm     *consistencyTimeTM
	person []string // entity ids saved by save, oldest first
}

var fenceBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newFenceEnv(t *testing.T) *fenceEnv {
	t.Helper()
	clock := memory.NewTestClockAt(fenceBase)
	factory := memory.NewStoreFactory(memory.WithApplyFunc(testSchemaApply), memory.WithClock(clock))
	t.Cleanup(func() { factory.Close() })
	factory.NewTransactionManager(common.NewDefaultUUIDGenerator())
	txMgr := factory.GetTransactionManager()

	tm := &consistencyTimeTM{TransactionManager: txMgr}
	cons := consistency.New(tm)

	engine := workflow.NewEngine(factory, common.NewDefaultUUIDGenerator(), txMgr)
	searchStore, err := factory.AsyncSearchStore(context.Background())
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	svc := &CloudEventsServiceImpl{
		registry:      NewMemberRegistry(),
		txMgr:         txMgr,
		entityHandler: entity.New(factory, txMgr, common.NewDefaultUUIDGenerator(), engine, txgate.New(), cons),
		modelHandler:  model.New(factory),
		searchService: search.NewSearchService(factory, common.NewDefaultUUIDGenerator(), searchStore, cons),
		cons:          cons,
	}
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "fence-user", Kind: spi.PrincipalUser,
		Tenant: spi.Tenant{ID: "fence-tenant", Name: "fence"},
		Roles:  []string{"ADMIN"},
	})
	e := &fenceEnv{svc: svc, ctx: ctx, clock: clock, jobs: searchStore, tm: tm}
	importAndLockModel(t, svc, ctx, "person", "1", map[string]any{"name": "Alice"})
	return e
}

// save creates one person at the store clock's current instant.
func (e *fenceEnv) save(t *testing.T, name string) {
	t.Helper()
	resp, err := e.svc.EntityManage(e.ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "test", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": "person", "version": 1},
			"data":  map[string]any{"name": name},
		},
	}))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	ids := parseResponsePayload(t, resp)["transactionInfo"].(map[string]any)["entityIds"].([]any)
	e.person = append(e.person, ids[0].(string))
}

func (e *fenceEnv) consistencyAt(c time.Time) { e.tm.at, e.tm.err = c, nil }
func (e *fenceEnv) consistencyErr(err error)  { e.tm.at, e.tm.err = time.Time{}, err }

// fenceReq runs one request and returns every envelope payload it sent.
type fenceReq struct {
	name string
	run  func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any
}

func pitField(m map[string]any, pit *time.Time) map[string]any {
	if pit != nil {
		m["pointInTime"] = pit.UTC().Format(time.RFC3339Nano)
	}
	return m
}

func collect(t *testing.T, sent []*cepb.CloudEvent) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(sent))
	for _, ce := range sent {
		out = append(out, parseResponsePayload(t, ce))
	}
	return out
}

func searchStream(t *testing.T, e *fenceEnv, ce *cepb.CloudEvent) []map[string]any {
	t.Helper()
	st := &mockEntityStream{ctx: e.ctx}
	if err := e.svc.EntitySearchCollection(ce, st); err != nil {
		t.Fatalf("failures belong in the envelope, got transport error: %v", err)
	}
	return collect(t, st.sent)
}

func emptyCondition() map[string]any {
	return map[string]any{"type": "group", "operator": "AND", "conditions": []any{}}
}

const (
	reqGet = iota
	reqGetAll
	reqSearch
	reqSnapshot
	reqDeleteAll
	reqStats
	reqStatsByState
	reqChanges
)

func fencedRequests() []fenceReq {
	person := map[string]any{"name": "person", "version": 1}
	return []fenceReq{
		reqGet: {"EntityGetRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			ce, err := e.svc.EntitySearch(e.ctx, makeCE(EntityGetRequest, pitField(map[string]any{
				"id": "test", "entityId": e.person[0]}, pit)))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			return []map[string]any{parseResponsePayload(t, ce)}
		}},
		reqGetAll: {"EntityGetAllRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			return searchStream(t, e, makeCE(EntityGetAllRequest, pitField(map[string]any{
				"id": "test", "model": person}, pit)))
		}},
		reqSearch: {"EntitySearchRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			return searchStream(t, e, makeCE(EntitySearchRequest, pitField(map[string]any{
				"id": "test", "model": person, "condition": emptyCondition()}, pit)))
		}},
		reqSnapshot: {"EntitySnapshotSearchRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			ce, err := e.svc.EntitySearch(e.ctx, makeCE(EntitySnapshotSearchRequest, pitField(map[string]any{
				"id": "test", "model": person, "condition": emptyCondition()}, pit)))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			return []map[string]any{parseResponsePayload(t, ce)}
		}},
		reqDeleteAll: {"EntityDeleteAllRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			st := &mockManageStream{ctx: e.ctx}
			if err := e.svc.EntityManageCollection(makeCE(EntityDeleteAllRequest, pitField(map[string]any{
				"id": "test", "model": person}, pit)), st); err != nil {
				t.Fatalf("transport error: %v", err)
			}
			return collect(t, st.sent)
		}},
		reqStats: {"EntityStatsGetRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			return searchStream(t, e, makeCE(EntityStatsGetRequest, pitField(map[string]any{
				"id": "test", "model": person}, pit)))
		}},
		reqStatsByState: {"EntityStatsByStateGetRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			return searchStream(t, e, makeCE(EntityStatsByStateGetRequest, pitField(map[string]any{
				"id": "test", "model": person}, pit)))
		}},
		reqChanges: {"EntityChangesMetadataGetRequest", func(t *testing.T, e *fenceEnv, pit *time.Time) []map[string]any {
			return searchStream(t, e, makeCE(EntityChangesMetadataGetRequest, pitField(map[string]any{
				"id": "test", "entityId": e.person[0]}, pit)))
		}},
	}
}

// failure reads the error of a failed envelope.
func failure(t *testing.T, envs []map[string]any) (code, msg string, retryable bool) {
	t.Helper()
	if len(envs) == 0 {
		t.Fatal("no envelope was sent")
	}
	env := envs[0]
	if ok, _ := env["success"].(bool); ok {
		t.Fatalf("want a failure envelope, got success: %v", env)
	}
	er, _ := env["error"].(map[string]any)
	if er == nil {
		t.Fatalf("failure envelope has no error: %v", env)
	}
	code, _ = er["code"].(string)
	msg, _ = er["message"].(string)
	retryable, _ = er["retryable"].(bool)
	return code, msg, retryable
}

func requireSuccess(t *testing.T, envs []map[string]any) {
	t.Helper()
	if len(envs) == 0 {
		t.Fatal("no envelope was sent")
	}
	for _, env := range envs {
		if ok, _ := env["success"].(bool); !ok {
			t.Fatalf("want success, got %v", env)
		}
	}
}

func TestFence_PointInTimeAfterConsistencyTime_Refused(t *testing.T) {
	for _, rq := range fencedRequests() {
		t.Run(rq.name, func(t *testing.T) {
			e := newFenceEnv(t)
			e.save(t, "Alice")
			c := fenceBase.Add(time.Hour)
			e.consistencyAt(c)
			pit := c.Add(time.Millisecond)

			code, msg, retryable := failure(t, rq.run(t, e, &pit))
			if code != "CLIENT_ERROR" {
				t.Errorf("Error.Code = %q, want CLIENT_ERROR", code)
			}
			if !strings.HasPrefix(msg, common.ErrCodePointInTimeAfterConsistencyTime+":") {
				t.Errorf("Error.Message = %q, want the %s prefix", msg, common.ErrCodePointInTimeAfterConsistencyTime)
			}
			if !strings.Contains(msg, c.Format(time.RFC3339Nano)) {
				t.Errorf("Error.Message = %q, want it to carry the consistency time %s", msg, c.Format(time.RFC3339Nano))
			}
			if retryable {
				t.Error("Error.Retryable = true, want false")
			}
		})
	}
}

func TestFence_PointInTimeAtConsistencyTime_Served(t *testing.T) {
	for _, rq := range fencedRequests() {
		t.Run(rq.name, func(t *testing.T) {
			e := newFenceEnv(t)
			e.save(t, "Alice")
			c := fenceBase.Add(time.Hour)
			e.consistencyAt(c)
			requireSuccess(t, rq.run(t, e, &c))
		})
	}
}

func TestFence_ConsistencyTimeUnavailable_Retryable(t *testing.T) {
	for _, rq := range fencedRequests() {
		t.Run(rq.name, func(t *testing.T) {
			e := newFenceEnv(t)
			e.save(t, "Alice")
			e.consistencyErr(spi.ErrConsistencyTimeUnavailable)
			far := fenceBase.Add(1000 * time.Hour)

			code, msg, retryable := failure(t, rq.run(t, e, &far))
			if code != "CLIENT_ERROR" {
				t.Errorf("Error.Code = %q, want CLIENT_ERROR", code)
			}
			if !strings.HasPrefix(msg, common.ErrCodeConsistencyTimeUnavailable+":") {
				t.Errorf("Error.Message = %q, want the %s prefix", msg, common.ErrCodeConsistencyTimeUnavailable)
			}
			if !retryable {
				t.Error("Error.Retryable = false, want true")
			}
		})
	}
}

func TestFence_StorageUnavailable_Retryable(t *testing.T) {
	e := newFenceEnv(t)
	e.save(t, "Alice")
	e.consistencyErr(&storageOutageError{detail: storageOutageDetail})
	far := fenceBase.Add(1000 * time.Hour)

	code, msg, retryable := failure(t, fencedRequests()[reqGetAll].run(t, e, &far))
	if code != "CLIENT_ERROR" {
		t.Errorf("Error.Code = %q, want CLIENT_ERROR", code)
	}
	if !strings.HasPrefix(msg, common.ErrCodeStorageUnavailable+":") {
		t.Errorf("Error.Message = %q, want the %s prefix", msg, common.ErrCodeStorageUnavailable)
	}
	if !retryable {
		t.Error("Error.Retryable = false, want true")
	}
	if strings.Contains(msg, "db.internal") {
		t.Errorf("Error.Message leaks operator detail: %q", msg)
	}
}

// twoSaves seeds Alice, then Bob two hours later, and returns an instant between
// them; the consistency time is later than both.
func twoSaves(t *testing.T) (*fenceEnv, time.Time) {
	t.Helper()
	e := newFenceEnv(t)
	e.save(t, "Alice")
	e.clock.Advance(time.Hour)
	between := e.clock.Now()
	e.clock.Advance(time.Hour)
	e.save(t, "Bob")
	e.clock.Advance(time.Hour)
	e.consistencyAt(e.clock.Now())
	return e, between
}

func TestFence_EntityGetAll_ReturnsDataAsAtT(t *testing.T) {
	e, between := twoSaves(t)
	run := fencedRequests()[reqGetAll].run

	if got := run(t, e, &between); len(got) != 1 {
		t.Errorf("as at the earlier instant: %d entities, want 1: %v", len(got), got)
	}
	if got := run(t, e, nil); len(got) != 2 {
		t.Errorf("without an instant: %d entities, want 2", len(got))
	}
}

func TestFence_EntityStats_ReturnsCountAsAtT(t *testing.T) {
	e, between := twoSaves(t)
	for _, idx := range []int{reqStats, reqStatsByState} {
		rq := fencedRequests()[idx]
		t.Run(rq.name, func(t *testing.T) {
			count := func(pit *time.Time) int {
				envs := rq.run(t, e, pit)
				requireSuccess(t, envs)
				n := 0
				for _, env := range envs {
					n += int(env["count"].(float64))
				}
				return n
			}
			if got := count(&between); got != 1 {
				t.Errorf("count as at the earlier instant = %d, want 1", got)
			}
			if got := count(nil); got != 2 {
				t.Errorf("count without an instant = %d, want 2", got)
			}
		})
	}
}

func TestFence_SnapshotSearch_DefaultInstantIsConsistencyTime(t *testing.T) {
	e := newFenceEnv(t)
	e.save(t, "Alice")
	c := fenceBase.Add(90*time.Minute + 123456789*time.Nanosecond)
	e.consistencyAt(c)

	envs := fencedRequests()[reqSnapshot].run(t, e, nil)
	requireSuccess(t, envs)
	id := envs[0]["status"].(map[string]any)["snapshotId"].(string)
	job, err := e.jobs.GetJob(e.ctx, id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !job.PointInTime.Equal(c) {
		t.Errorf("job instant = %v, want the consistency time %v exactly", job.PointInTime, c)
	}
}
