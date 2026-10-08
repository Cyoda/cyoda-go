package grpc

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedDeleteAllPersonIDs mirrors seedDeleteAllTxSizePersons but returns the
// created ids, in creation order, so a test can compare them with the
// response's entityIds. It imports+locks "person" on the first call only.
func seedDeleteAllPersonIDs(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, n int, importModel bool) []string {
	t.Helper()
	if importModel {
		seedDeleteAllTxSizePersons(t, svc, ctx, 0)
	}
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ce := makeCE(EntityCreateRequest, map[string]any{
			"id":         "seed",
			"dataFormat": "JSON",
			"payload": map[string]any{
				"model": map[string]any{"name": "person", "version": 1},
				"data":  map[string]any{"name": "Alice"},
			},
		})
		resp, err := svc.EntityManage(ctx, ce)
		if err != nil {
			t.Fatalf("seed create[%d]: %v", i, err)
		}
		payload := parseResponsePayload(t, resp)
		txInfo := payload["transactionInfo"].(map[string]any)
		ids = append(ids, txInfo["entityIds"].([]any)[0].(string))
	}
	return ids
}

func deleteAllViaGRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, fields map[string]any) events.EntityDeleteAllResponseJson {
	t.Helper()
	req := map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
	}
	for k, v := range fields {
		req[k] = v
	}
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(makeCE(EntityDeleteAllRequest, req), stream); err != nil {
		t.Fatalf("EntityManageCollection: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected 1 response, got %d", len(stream.sent))
	}
	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	return typed
}

func countPersonsViaGRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context) int {
	t.Helper()
	ce := makeCE(EntityGetAllRequest, map[string]any{
		"id":    "test",
		"model": map[string]any{"name": "person", "version": 1},
	})
	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(ce, stream); err != nil {
		t.Fatalf("EntitySearchCollection: %v", err)
	}
	return len(stream.sent)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// pointInTime on the delete-all event selects the committed state as at
// that instant: entities created after it survive, and the whole-model
// DeleteAll fast path is bypassed.
func TestRPC_EntityDeleteAll_PointInTime_SparesLaterCreates(t *testing.T) {
	svc, ctx, spyStore, _ := newDeleteAllTxSizeEnv(t)
	before := seedDeleteAllPersonIDs(t, svc, ctx, 3, true)
	time.Sleep(2 * time.Millisecond)
	pit := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	seedDeleteAllPersonIDs(t, svc, ctx, 2, false)

	typed := deleteAllViaGRPC(t, svc, ctx, map[string]any{
		"pointInTime": pit.Format(time.RFC3339Nano),
		"verbose":     true,
	})
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.NumDeleted != 3 {
		t.Errorf("NumDeleted = %d, want 3", typed.NumDeleted)
	}
	if got, want := sortedCopy(typed.EntityIds), sortedCopy(before); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("EntityIds = %v, want %v", got, want)
	}
	if spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must NOT be called when pointInTime is set")
	}
	if n := countPersonsViaGRPC(t, svc, ctx); n != 2 {
		t.Errorf("entities remaining = %d, want the 2 created after the instant", n)
	}
}

// An id selected at the pointInTime instant but already deleted by the time
// the delete-all runs is reported per id in errorsById, while still
// appearing in the attempted entityIds set alongside the survivors.
func TestRPC_EntityDeleteAll_PointInTime_AlreadyGoneIDInErrorsByID(t *testing.T) {
	svc, ctx, _, _ := newDeleteAllTxSizeEnv(t)
	before := seedDeleteAllPersonIDs(t, svc, ctx, 3, true)
	time.Sleep(2 * time.Millisecond)
	pit := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	seedDeleteAllPersonIDs(t, svc, ctx, 2, false)

	goneID := before[0]
	deleteCE := makeCE(EntityDeleteRequest, map[string]any{
		"id":       "test",
		"entityId": goneID,
	})
	if _, err := svc.EntityManage(ctx, deleteCE); err != nil {
		t.Fatalf("delete %s before delete-all: %v", goneID, err)
	}

	typed := deleteAllViaGRPC(t, svc, ctx, map[string]any{
		"pointInTime": pit.Format(time.RFC3339Nano),
		"verbose":     true,
	})
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.NumDeleted != 2 {
		t.Errorf("NumDeleted = %d, want 2 (the already-gone id doesn't count)", typed.NumDeleted)
	}
	if _, ok := typed.ErrorsByID[goneID]; !ok {
		t.Errorf("ErrorsByID = %v, want an entry for the already-gone id %s", typed.ErrorsByID, goneID)
	}
	if got, want := sortedCopy(typed.EntityIds), sortedCopy(before); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("EntityIds = %v, want the attempted set %v (including the already-gone id)", got, want)
	}
}

func TestRPC_EntityDeleteAll_Verbose_ListsAttemptedIDs_SingleTx(t *testing.T) {
	svc, ctx, spyStore, rtm := newDeleteAllTxSizeEnv(t)
	ids := seedDeleteAllPersonIDs(t, svc, ctx, 3, true)
	commitsBefore := rtm.commitCount()

	typed := deleteAllViaGRPC(t, svc, ctx, map[string]any{"verbose": true})
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.NumDeleted != 3 {
		t.Errorf("NumDeleted = %d, want 3", typed.NumDeleted)
	}
	if got, want := sortedCopy(typed.EntityIds), sortedCopy(ids); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("EntityIds = %v, want %v", got, want)
	}
	if spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must NOT be called when verbose is true (ids must be enumerated)")
	}
	if commits := rtm.commitCount() - commitsBefore; commits != 1 {
		t.Errorf("commits = %d, want exactly 1 (single transaction)", commits)
	}
}

func TestRPC_EntityDeleteAll_Verbose_ListsAttemptedIDs_Batched(t *testing.T) {
	svc, ctx, _, _ := newDeleteAllTxSizeEnv(t)
	ids := seedDeleteAllPersonIDs(t, svc, ctx, 5, true)

	typed := deleteAllViaGRPC(t, svc, ctx, map[string]any{"verbose": true, "transactionSize": 2})
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.NumDeleted != 5 {
		t.Errorf("NumDeleted = %d, want 5", typed.NumDeleted)
	}
	if got, want := sortedCopy(typed.EntityIds), sortedCopy(ids); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("EntityIds = %v, want %v", got, want)
	}
}

// Without verbose the required entityIds field is present and empty, and
// the fast path is still taken — the existing single-tx contract.
func TestRPC_EntityDeleteAll_NotVerbose_EmptyEntityIds_FastPath(t *testing.T) {
	svc, ctx, spyStore, _ := newDeleteAllTxSizeEnv(t)
	seedDeleteAllPersonIDs(t, svc, ctx, 2, true)

	typed := deleteAllViaGRPC(t, svc, ctx, nil)
	if !typed.Success {
		t.Fatalf("expected success=true, error=%+v", typed.Error)
	}
	if typed.EntityIds == nil || len(typed.EntityIds) != 0 {
		t.Errorf("EntityIds = %#v, want present and empty", typed.EntityIds)
	}
	if !spyStore.wasCalled() {
		t.Error("EntityStore.DeleteAll must be called on a plain delete-all")
	}
}

func TestRPC_EntityDeleteAll_PointInTime_ModelNotFound_Envelope(t *testing.T) {
	svc, ctx := newTestEnv(t)
	// A far-future instant would be refused by the fence, so the envelope
	// asserted below also pins that the model is resolved before the fence.
	pit := time.Now().UTC().Add(1000 * time.Hour).Format(time.RFC3339Nano)
	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":          "test",
		"model":       map[string]any{"name": "nosuchmodel", "version": 1},
		"pointInTime": pit,
	})
	stream := &mockManageStream{ctx: ctx}
	if err := svc.EntityManageCollection(ce, stream); err != nil {
		t.Fatalf("EntityManageCollection: %v", err)
	}
	var typed events.EntityDeleteAllResponseJson
	validateResponse(t, stream.sent[0], &typed)
	if typed.Success {
		t.Fatal("expected success=false for an unknown model")
	}
	if typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
		t.Fatalf("Error = %+v, want code CLIENT_ERROR", typed.Error)
	}
	if !strings.HasPrefix(typed.Error.Message, common.ErrCodeModelNotFound+":") {
		t.Errorf("Error.Message = %q, want prefix %q", typed.Error.Message, common.ErrCodeModelNotFound+":")
	}
}

func TestRPC_EntityDeleteAll_MalformedPointInTime_InvalidArgument(t *testing.T) {
	svc, ctx := newTestEnv(t)
	ce := makeCE(EntityDeleteAllRequest, map[string]any{
		"id":          "test",
		"model":       map[string]any{"name": "person", "version": 1},
		"pointInTime": "not-a-timestamp",
	})
	stream := &mockManageStream{ctx: ctx}
	err := svc.EntityManageCollection(ce, stream)
	if err == nil {
		t.Fatal("expected a gRPC status error for a malformed pointInTime")
	}
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument {
		t.Errorf("status code = %v, want InvalidArgument (%v)", st.Code(), err)
	}
}
