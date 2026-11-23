package grpc

import (
	"context"
	"encoding/json"
	"testing"

	events "github.com/cyoda/cyoda-go/api/grpc/events"
	"github.com/cyoda/cyoda-go/internal/domain/entity"
)

// startValueGuardWF rests an entity in OPEN. The automated "afterSubmit" leaves
// OPEN for HIT when the criterion sees transitionForLatestSave "submit". The
// manual "submit" and "touch" both return to OPEN, so every request ends with
// the guard evaluated in OPEN, and the state says what it saw.
const startValueGuardWF = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "start-value-guard", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "afterSubmit", "next": "HIT", "manual": false,
				 "criterion": {"type": "lifecycle", "field": "transitionForLatestSave", "operatorType": "EQUALS", "value": "submit"}},
				{"name": "submit", "next": "OPEN", "manual": true},
				{"name": "touch", "next": "OPEN", "manual": true}
			]},
			"HIT": {}
		}
	}]
}`

const guardData = `{"name":"Test Order","amount":100,"status":"draft"}`

// guardEnv is a gRPC service with startValueGuardWF imported for model.
type guardEnv struct {
	svc   *CloudEventsServiceImpl
	ctx   context.Context
	model string
}

func newGuardEnv(t *testing.T) guardEnv {
	t.Helper()
	svc, wfHandler, ctx := newTestEnvWithWorkflow(t)
	const model = "start-value-guard"
	setupScheduledWorkflowRPCEnv(t, svc, wfHandler, ctx, model, startValueGuardWF)
	return guardEnv{svc: svc, ctx: ctx, model: model}
}

// requireEnvelopeSuccess fails the test unless resp is a successful transaction
// envelope.
func requireEnvelopeSuccess(t *testing.T, what string, success bool, errInfo any) {
	t.Helper()
	if !success {
		t.Fatalf("%s: Success = false, error %+v", what, errInfo)
	}
}

// create creates one entity through EntityCreateRequest and returns its id.
func (g guardEnv) create(t *testing.T) string {
	t.Helper()
	resp, err := g.svc.EntityManage(g.ctx, makeCE(EntityCreateRequest, map[string]any{
		"id": "create", "dataFormat": "JSON",
		"payload": map[string]any{
			"model": map[string]any{"name": g.model, "version": 1},
			"data":  json.RawMessage(guardData),
		},
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	requireEnvelopeSuccess(t, "create", typed.Success, typed.Error)
	return typed.TransactionInfo.EntityIds[0]
}

// savedBySubmit creates an entity and saves it once more by a request naming
// "submit", from a version saved by a create: it rests in OPEN with a stored
// transitionForLatestSave of "submit".
func (g guardEnv) savedBySubmit(t *testing.T) string {
	t.Helper()
	id := g.create(t)
	if ok, e := g.transition(t, id, "submit"); !ok {
		t.Fatalf("seed: submit failed: %+v", e)
	}
	if state, name := g.read(t, id); state != "OPEN" || name != "submit" {
		t.Fatalf("seed: entity in %q saved by %q; want OPEN saved by \"submit\" (the guard sees the start version's value, \"loopback\")", state, name)
	}
	return id
}

// read returns the entity's state and stored transitionForLatestSave.
func (g guardEnv) read(t *testing.T, id string) (state, transition string) {
	t.Helper()
	env, err := g.svc.entityHandler.GetEntity(g.ctx, entity.GetOneEntityInput{EntityID: id})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	state, _ = env.Meta["state"].(string)
	transition, _ = env.Meta["transitionForLatestSave"].(string)
	return state, transition
}

func (g guardEnv) transition(t *testing.T, id, name string) (bool, any) {
	t.Helper()
	resp, err := g.svc.EntityManage(g.ctx, makeCE(EntityTransitionRequest, map[string]any{
		"id": "transition", "entityId": id, "transition": name,
	}))
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	var typed events.EntityTransitionResponseJson
	validateResponse(t, resp, &typed)
	return typed.Success, typed.Error
}

func (g guardEnv) update(t *testing.T, id, name string) (bool, any) {
	t.Helper()
	payload := map[string]any{"entityId": id, "data": json.RawMessage(guardData)}
	if name != "" {
		payload["transition"] = name
	}
	resp, err := g.svc.EntityManage(g.ctx, makeCE(EntityUpdateRequest, map[string]any{
		"id": "update", "dataFormat": "JSON", "payload": payload,
	}))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	return typed.Success, typed.Error
}

func (g guardEnv) patch(t *testing.T, id, name string) (bool, any) {
	t.Helper()
	resp, err := g.svc.EntityManage(g.ctx, makeCE(EntityPatchRequest, map[string]any{
		"id": "patch", "patchFormat": "MERGE_PATCH",
		"payload": map[string]any{"entityId": id, "ifMatch": "*", "transition": name, "patch": map[string]any{"amount": 7}},
	}))
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, resp, &typed)
	return typed.Success, typed.Error
}

// collection sends one EntityManageCollection request and returns its single
// response envelope.
func (g guardEnv) collection(t *testing.T, eventType string, fields map[string]any) events.EntityTransactionResponseJson {
	t.Helper()
	stream := &mockManageStream{ctx: g.ctx}
	if err := g.svc.EntityManageCollection(makeCE(eventType, fields), stream); err != nil {
		t.Fatalf("%s: %v", eventType, err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("%s: %d responses, want 1", eventType, len(stream.sent))
	}
	var typed events.EntityTransactionResponseJson
	validateResponse(t, stream.sent[0], &typed)
	return typed
}

func (g guardEnv) updateCollection(t *testing.T, id, name string) (bool, any) {
	t.Helper()
	item := map[string]any{"entityId": id, "data": json.RawMessage(guardData)}
	if name != "" {
		item["transition"] = name
	}
	typed := g.collection(t, EntityUpdateCollectionRequest, map[string]any{
		"id": "update-collection", "dataFormat": "JSON", "payloads": []any{item},
	})
	return typed.Success, typed.Error
}

// requireGuard asserts the entity's state after a request: HIT when the
// guard saw "submit", OPEN otherwise, and the name the request saved.
func (g guardEnv) requireGuard(t *testing.T, id, wantState, wantSaved, why string) {
	t.Helper()
	state, name := g.read(t, id)
	if state != wantState {
		t.Errorf("state = %q, want %s: %s", state, wantState, why)
	}
	if name != wantSaved {
		t.Errorf("stored transitionForLatestSave = %q, want the request's %q", name, wantSaved)
	}
}

// What a criterion sees in transitionForLatestSave is the name of the version
// the request started from, on every gRPC door: never the name the request
// sends, and none on a create.
func TestDecision6_CriterionSeesStartValue_EachGRPCDoor(t *testing.T) {
	const (
		notOwnName = "the guard sees the start version's \"loopback\", not the request's own \"submit\""
		startName  = "the guard sees the start version's \"submit\""
	)
	type door func(g guardEnv, t *testing.T, id, name string) (bool, any)
	named := []struct {
		name string
		send door
	}{
		{"update", guardEnv.update},
		{"patch", guardEnv.patch},
		{"transition", guardEnv.transition},
		{"collection_update", guardEnv.updateCollection},
	}
	for _, d := range named {
		t.Run(d.name+"/names_submit", func(t *testing.T) {
			g := newGuardEnv(t)
			id := g.create(t)
			ok, e := d.send(g, t, id, "submit")
			requireEnvelopeSuccess(t, d.name, ok, e)
			g.requireGuard(t, id, "OPEN", "submit", notOwnName)
		})
		t.Run(d.name+"/follows_submit", func(t *testing.T) {
			g := newGuardEnv(t)
			id := g.savedBySubmit(t)
			ok, e := d.send(g, t, id, "touch")
			requireEnvelopeSuccess(t, d.name, ok, e)
			g.requireGuard(t, id, "HIT", "touch", startName)
		})
	}
	loopback := []struct {
		name string
		send door
	}{
		{"update_loopback", guardEnv.update},
		{"collection_update_loopback", guardEnv.updateCollection},
	}
	for _, d := range loopback {
		t.Run(d.name+"/follows_submit", func(t *testing.T) {
			g := newGuardEnv(t)
			id := g.savedBySubmit(t)
			ok, e := d.send(g, t, id, "")
			requireEnvelopeSuccess(t, d.name, ok, e)
			g.requireGuard(t, id, "HIT", "loopback", startName)
		})
	}
	t.Run("collection_create", func(t *testing.T) {
		g := newGuardEnv(t)
		typed := g.collection(t, EntityCreateCollectionRequest, map[string]any{
			"id": "create-collection", "dataFormat": "JSON",
			"payloads": []any{map[string]any{
				"model": map[string]any{"name": g.model, "version": 1},
				"data":  json.RawMessage(guardData),
			}},
		})
		requireEnvelopeSuccess(t, "collection create", typed.Success, typed.Error)
		g.requireGuard(t, typed.TransactionInfo.EntityIds[0], "OPEN", "loopback", "a create starts from no version, so the guard sees none")
	})
}
