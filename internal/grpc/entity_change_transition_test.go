package grpc

import (
	"testing"

	events "github.com/cyoda/cyoda-go/api/grpc/events"
)

// TestEntityChangesMetadata_Transition: the gRPC change history carries the
// transition each version recorded as changeMeta.transition, newest first,
// and omits it on the DELETE tombstone.
func TestEntityChangesMetadata_Transition(t *testing.T) {
	const modelName = "grpc-change-transition"
	svc, wfHandler, ctx := newTestEnvWithWorkflow(t)
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, modelName, `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "cht-wf", "initialState": "OPEN", "active": true,
			"states": {"OPEN": {"transitions": [{"name": "approve", "next": "OPEN", "manual": true}]}}
		}]
	}`)

	createResp, err := svc.EntityManage(ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "c", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": modelName, "version": 1},
			"data":  map[string]any{"name": "Test Order", "amount": 100, "status": "draft"},
		},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entityID := parseResponsePayload(t, createResp)["transactionInfo"].(map[string]any)["entityIds"].([]any)[0].(string)

	steps := []struct {
		typ    string
		fields map[string]any
	}{
		{EntityUpdateRequest, map[string]any{"id": "u", "dataFormat": "JSON", "payload": map[string]any{
			"entityId": entityID, "data": map[string]any{"name": "Alicia"}}}},
		{EntityTransitionRequest, map[string]any{"id": "t", "entityId": entityID, "transition": "approve"}},
		{EntityDeleteRequest, map[string]any{"id": "d", "entityId": entityID}},
	}
	for _, s := range steps {
		resp, err := svc.EntityManage(ctx, makeCE(s.typ, s.fields))
		if err != nil {
			t.Fatalf("%s: %v", s.typ, err)
		}
		if p := parseResponsePayload(t, resp); p["success"] != true {
			t.Fatalf("%s: not successful: %v", s.typ, p)
		}
	}

	stream := &mockEntityStream{ctx: ctx}
	if err := svc.EntitySearchCollection(makeCE(EntityChangesMetadataGetRequest, map[string]any{
		"id": "g", "entityId": entityID,
	}), stream); err != nil {
		t.Fatalf("changes: %v", err)
	}
	// Newest first: delete (none), approve, loopback update, create.
	want := []string{"", "approve", "loopback", "loopback"}
	if len(stream.sent) != len(want) {
		t.Fatalf("got %d entries, want %d", len(stream.sent), len(want))
	}
	for i, w := range want {
		var typed events.EntityChangesMetadataResponseJson
		validateResponse(t, stream.sent[i], &typed)
		got := typed.ChangeMeta.Transition
		switch {
		case w == "" && got != nil:
			t.Errorf("entry[%d]: transition = %q, want absent", i, *got)
		case w != "" && (got == nil || *got != w):
			t.Errorf("entry[%d]: transition = %v, want %q", i, got, w)
		}
	}
}
