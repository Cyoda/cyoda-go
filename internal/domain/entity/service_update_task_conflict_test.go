package entity

import (
	"encoding/json"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/testing/taskconflict"
)

// The reconcile's refusal comes from the statement, as on PostgreSQL. It
// must answer 409 CONFLICT, not 412 ENTITY_MODIFIED: nothing modified the
// entity.
func TestUpdateEntity_Loopback_TaskRowConflict_Is409Conflict(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.ReconcileForEntity, 1)

	_, err := e.h.UpdateEntity(e.ctx, UpdateEntityInput{
		EntityID: ids[0], Format: "JSON", Data: json.RawMessage(`{"name":"Q","age":7}`),
	})
	requireConflict409(t, err)
}

func TestUpdateEntity_NamedTransition_TaskRowConflict_Is409Conflict(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	e.plan.Refuse(taskconflict.ReconcileForEntity, 1)

	_, err := e.h.UpdateEntity(e.ctx, UpdateEntityInput{
		EntityID: ids[0], Format: "JSON", Data: json.RawMessage(`{"name":"Q","age":7}`), Transition: "Leave",
	})
	requireConflict409(t, err)
}

// With an IfMatch the collection door isolates an ENTITY_MODIFIED item and
// carries on. A task-row conflict must not be isolated: on PostgreSQL the
// transaction is already aborted, so the whole request fails with 409.
func TestUpdateEntityCollection_IfMatch_TaskRowConflict_FailsTheRequest(t *testing.T) {
	e := newTaskEnv(t)
	ids := seedPersons(t, e.h, e.ctx, 1)
	store, err := e.real.EntityStore(e.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	cur, err := store.Get(e.ctx, ids[0])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	e.plan.Refuse(taskconflict.ReconcileForEntity, 1)

	res, err := e.h.UpdateEntityCollection(e.ctx, []UpdateCollectionItem{{
		EntityID: ids[0], Payload: json.RawMessage(`{"name":"Q","age":7}`), IfMatch: cur.Meta.TransactionID,
	}})
	if err == nil {
		t.Fatalf("err = nil, result %+v; want the request to fail with 409", res)
	}
	requireConflict409(t, err)
}
