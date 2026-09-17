package grpc

import (
	"context"
	"testing"
	"time"

	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
)

// ---------------------------------------------------------------------------
// creationDate / lastUpdateTime on the gRPC door carry the commit instant.
//
// Both values are serialised onto the wire by this package (search.go's
// buildEntityMeta and dispatch.go's buildEntityPayload) out of the entity
// envelope, and the existing temporal tests (search_temporal_test.go) only
// exercise creationDate for chronological ORDERING — an assertion that passes
// identically whether a write is dated at its transaction's start or at its
// commit. These tests assert the value itself: it must equal the instant the
// backend recorded for the transaction that wrote the revision, which is what
// GetSubmitTime reports.
// ---------------------------------------------------------------------------

// entityMetaGRPC reads one entity through EntityGetRequest and returns its
// response meta map.
func entityMetaGRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, entityID string) map[string]any {
	t.Helper()
	resp, err := svc.EntitySearch(ctx, makeCE(EntityGetRequest, map[string]any{
		"id":       "ci-get",
		"entityId": entityID,
	}))
	if err != nil {
		t.Fatalf("EntityGetRequest failed: %v", err)
	}
	var typed events.EntityResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true; error: %v", typed.Error)
	}
	meta, ok := typed.Payload.Meta.(map[string]interface{})
	if !ok {
		t.Fatalf("Payload.Meta is not map[string]interface{}: %T", typed.Payload.Meta)
	}
	return meta
}

// metaInstantGRPC parses one RFC3339Nano meta field.
func metaInstantGRPC(t *testing.T, meta map[string]any, key string) time.Time {
	t.Helper()
	s, ok := meta[key].(string)
	if !ok || s == "" {
		t.Fatalf("meta.%s missing or not a string: %v", key, meta)
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse meta.%s %q: %v", key, s, err)
	}
	return ts
}

// createEntityGRPC creates one entity and returns its id and the id of the
// transaction that committed it.
func createEntityGRPC(t *testing.T, svc *CloudEventsServiceImpl, ctx context.Context, model string, data map[string]any) (string, string) {
	t.Helper()
	resp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "ci-create", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": model, "version": 1},
			"data":  data,
		},
	}))
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	if !typed.Success {
		t.Fatalf("expected success=true on create; error: %v", typed.Error)
	}
	if len(typed.TransactionInfo.EntityIds) != 1 {
		t.Fatalf("expected one entity id, got %v", typed.TransactionInfo.EntityIds)
	}
	if typed.TransactionInfo.TransactionID == nil || *typed.TransactionInfo.TransactionID == "" {
		t.Fatal("create response carries no transactionId; the commit instant cannot be resolved")
	}
	return typed.TransactionInfo.EntityIds[0], *typed.TransactionInfo.TransactionID
}

// TestEntityMeta_GRPC_DatesAreTheCommitInstant pins creationDate and
// lastUpdateTime on the gRPC door to the instant their transaction committed,
// as the backend recorded it — not to a clock read while the transaction was
// still open.
func TestEntityMeta_GRPC_DatesAreTheCommitInstant(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-commit-instant"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	entityID, createTx := createEntityGRPC(t, svc, ctx, model, map[string]any{"name": "A"})

	createSubmit, err := svc.txMgr.GetSubmitTime(ctx, createTx)
	if err != nil {
		t.Fatalf("GetSubmitTime(create): %v", err)
	}

	meta := entityMetaGRPC(t, svc, ctx, entityID)
	if got, _ := meta["transactionId"].(string); got != createTx {
		t.Errorf("meta.transactionId = %q, want the creating transaction %q", got, createTx)
	}
	if cd := metaInstantGRPC(t, meta, "creationDate"); !cd.Equal(createSubmit) {
		t.Errorf("meta.creationDate = %s, want the creating transaction's commit instant %s",
			cd.UTC(), createSubmit.UTC())
	}
	if lut := metaInstantGRPC(t, meta, "lastUpdateTime"); !lut.Equal(createSubmit) {
		t.Errorf("meta.lastUpdateTime = %s, want the creating transaction's commit instant %s",
			lut.UTC(), createSubmit.UTC())
	}
}

// TestEntityMeta_GRPC_UpdateMovesLastUpdateTimeOnly pins the two fields apart
// across a second transaction: lastUpdateTime takes the new commit instant
// while creationDate stays at the first one. A stamp taken from any clock
// other than the committing transaction's would have to disagree with one of
// the two.
func TestEntityMeta_GRPC_UpdateMovesLastUpdateTimeOnly(t *testing.T) {
	svc, ctx := newTestEnv(t)
	const model = "grpc-commit-instant-update"
	importAndLockModel(t, svc, ctx, model, "1", map[string]any{"name": "A"})

	entityID, createTx := createEntityGRPC(t, svc, ctx, model, map[string]any{"name": "A"})
	createSubmit, err := svc.txMgr.GetSubmitTime(ctx, createTx)
	if err != nil {
		t.Fatalf("GetSubmitTime(create): %v", err)
	}

	updateResp, err := svc.EntityManage(ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id": "ci-update", "dataFormat": "JSON",
		"payload": map[string]any{
			"entityId": entityID,
			"data":     map[string]any{"name": "B"},
		},
	}))
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	var updated events.EntityTransactionResponseJson
	validateResponse(t, updateResp, &updated)
	if !updated.Success {
		t.Fatalf("expected success=true on update; error: %v", updated.Error)
	}
	if updated.TransactionInfo.TransactionID == nil || *updated.TransactionInfo.TransactionID == "" {
		t.Fatal("update response carries no transactionId")
	}
	updateTx := *updated.TransactionInfo.TransactionID

	updateSubmit, err := svc.txMgr.GetSubmitTime(ctx, updateTx)
	if err != nil {
		t.Fatalf("GetSubmitTime(update): %v", err)
	}
	if !updateSubmit.After(createSubmit) {
		t.Fatalf("the two transactions share an instant (create=%s update=%s); the assertions "+
			"below could not discriminate", createSubmit.UTC(), updateSubmit.UTC())
	}

	meta := entityMetaGRPC(t, svc, ctx, entityID)
	if cd := metaInstantGRPC(t, meta, "creationDate"); !cd.Equal(createSubmit) {
		t.Errorf("meta.creationDate = %s after an update, want the CREATING transaction's commit "+
			"instant %s — an update must not restamp it", cd.UTC(), createSubmit.UTC())
	}
	if lut := metaInstantGRPC(t, meta, "lastUpdateTime"); !lut.Equal(updateSubmit) {
		t.Errorf("meta.lastUpdateTime = %s, want the UPDATING transaction's commit instant %s",
			lut.UTC(), updateSubmit.UTC())
	}
}
