package parity

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda/cyoda-go/e2e/parity/client"
)

// startValueData is the data of RunDecision6CriterionSeesStartValue's
// entities.
const startValueData = `{"name":"s","amount":1,"status":"new"}`

// startValueGuardWorkflow rests a created entity in OPEN. Two automated
// transitions leave OPEN by what a criterion sees in transitionForLatestSave:
// "sawSubmit" for "submit", "sawLoopback" for "loopback". The manual "submit"
// and "touch" lead to OPEN from every state, so every request ends with both
// criteria evaluated in OPEN (a loopback update in OPEN too), and the state
// says what they saw: OPEN for neither.
const startValueGuardWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "start-value-guard-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [
				{"name": "sawSubmit", "next": "SAW_SUBMIT", "manual": false,
				 "criterion": {"type": "lifecycle", "field": "transitionForLatestSave", "operatorType": "EQUALS", "value": "submit"}},
				{"name": "sawLoopback", "next": "SAW_LOOPBACK", "manual": false,
				 "criterion": {"type": "lifecycle", "field": "transitionForLatestSave", "operatorType": "EQUALS", "value": "loopback"}},
				{"name": "submit", "next": "OPEN", "manual": true},
				{"name": "touch", "next": "OPEN", "manual": true}
			]},
			"SAW_SUBMIT": {"transitions": [
				{"name": "submit", "next": "OPEN", "manual": true},
				{"name": "touch", "next": "OPEN", "manual": true}
			]},
			"SAW_LOOPBACK": {"transitions": [
				{"name": "submit", "next": "OPEN", "manual": true},
				{"name": "touch", "next": "OPEN", "manual": true}
			]}
		}
	}]
}`

// startValueScheduledWorkflow: the manual "submit" leads from OPEN to
// SUBMITTED, where the scheduled "expire" fires after 300 ms into EXPIRED.
// Two automated transitions leave EXPIRED by what a criterion sees in
// transitionForLatestSave: "sawSubmit" for "submit", "sawExpire" for
// "expire".
const startValueScheduledWorkflow = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "start-value-scheduled-wf", "initialState": "OPEN", "active": true,
		"states": {
			"OPEN": {"transitions": [{"name": "submit", "next": "SUBMITTED", "manual": true}]},
			"SUBMITTED": {"transitions": [{"name": "expire", "next": "EXPIRED", "manual": false,
				"schedule": {"delayMs": 300}}]},
			"EXPIRED": {"transitions": [
				{"name": "sawSubmit", "next": "SAW_SUBMIT", "manual": false,
				 "criterion": {"type": "lifecycle", "field": "transitionForLatestSave", "operatorType": "EQUALS", "value": "submit"}},
				{"name": "sawExpire", "next": "SAW_EXPIRE", "manual": false,
				 "criterion": {"type": "lifecycle", "field": "transitionForLatestSave", "operatorType": "EQUALS", "value": "expire"}}
			]},
			"SAW_SUBMIT": {},
			"SAW_EXPIRE": {}
		}
	}]
}`

// RunDecision6CriterionSeesStartValue proves, on every backend, that a
// criterion on transitionForLatestSave sees the name of the version the
// request started from — never the name the request itself sends, and none
// on a create — while the version the request saves records the request's
// own name:
//
//   - create: the criteria see none; the version records "loopback".
//   - named update: a PUT naming "touch" after a create sees "loopback"; a
//     PUT naming "submit" after "touch" sees "touch" (neither guard); a PUT
//     naming "touch" after "submit" sees "submit".
//   - loopback update: a PUT naming no transition after "submit" sees
//     "submit"; the version records "loopback".
//   - scheduled firing: the firing of "expire" after "submit" sees
//     "submit"; its version records "expire".
func RunDecision6CriterionSeesStartValue(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	const guardModel = "start-value-guard"
	setupModelWithWorkflow(t, c, guardModel, 1, startValueData, startValueGuardWorkflow)

	// require reads the entity and requires its state and the name its
	// current version records.
	require := func(t *testing.T, id uuid.UUID, wantState, wantSaved, why string) {
		t.Helper()
		e, err := c.GetEntity(t, id)
		if err != nil {
			t.Fatalf("GetEntity: %v", err)
		}
		if e.Meta.State != wantState {
			t.Errorf("state = %q, want %s: %s", e.Meta.State, wantState, why)
		}
		if e.Meta.TransitionForLatestSave != wantSaved {
			t.Errorf("meta.transitionForLatestSave = %q, want the request's %q", e.Meta.TransitionForLatestSave, wantSaved)
		}
		if t.Failed() {
			t.FailNow()
		}
	}
	create := func(t *testing.T) uuid.UUID {
		t.Helper()
		id, err := c.CreateEntity(t, guardModel, 1, startValueData)
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		return id
	}
	put := func(t *testing.T, id uuid.UUID, transition string) {
		t.Helper()
		if err := c.UpdateEntity(t, id, transition, startValueData); err != nil {
			t.Fatalf("PUT %s: %v", transition, err)
		}
	}
	// savedBySubmit rests an entity in OPEN with "submit" stored: create, PUT
	// "touch", PUT "submit".
	savedBySubmit := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := create(t)
		put(t, id, "touch")
		put(t, id, "submit")
		require(t, id, "OPEN", "submit", "seed: the PUT naming \"submit\" sees the start version's \"touch\"")
		return id
	}

	t.Run("create", func(t *testing.T) {
		require(t, create(t), "OPEN", "loopback", "a create starts from no version, so the criteria see none")
	})

	t.Run("named_update", func(t *testing.T) {
		id := create(t)
		put(t, id, "touch")
		require(t, id, "SAW_LOOPBACK", "touch", "the criteria see the start version's \"loopback\", not the request's own \"touch\"")
		put(t, id, "submit")
		require(t, id, "OPEN", "submit", "the criteria see the start version's \"touch\", not the request's own \"submit\"")
		put(t, id, "touch")
		require(t, id, "SAW_SUBMIT", "touch", "the criteria see the start version's \"submit\"")
	})

	t.Run("loopback_update", func(t *testing.T) {
		id := savedBySubmit(t)
		if err := c.UpdateEntityData(t, id, startValueData); err != nil {
			t.Fatalf("PUT without a transition: %v", err)
		}
		require(t, id, "SAW_SUBMIT", "loopback", "the criteria see the start version's \"submit\"")
	})

	t.Run("scheduled_firing", func(t *testing.T) {
		const model = "start-value-scheduled"
		setupModelWithWorkflow(t, c, model, 1, startValueData, startValueScheduledWorkflow)
		id, err := c.CreateEntity(t, model, 1, startValueData)
		if err != nil {
			t.Fatalf("CreateEntity: %v", err)
		}
		if err := c.UpdateEntity(t, id, "submit", startValueData); err != nil {
			t.Fatalf("PUT submit: %v", err)
		}
		// The firing's run commits once, at the end of its cascade.
		AwaitScheduler(t, fixture, 15*time.Second, fmt.Sprintf("entity %s past SUBMITTED", id), func() (bool, string) {
			e, err := c.GetEntity(t, id)
			if err != nil {
				t.Fatalf("GetEntity while awaiting the firing: %v", err)
			}
			return e.Meta.State != "SUBMITTED", fmt.Sprintf("state %q", e.Meta.State)
		})
		require(t, id, "SAW_SUBMIT", "expire", "the criteria after the firing see the start version's \"submit\", not the scheduled \"expire\"")
	})
}
