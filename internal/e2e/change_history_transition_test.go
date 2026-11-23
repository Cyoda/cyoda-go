package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
)

// change_history_transition_test.go — which transition name a version
// records, and what a run evaluates in transitionForLatestSave, on every door.
//
//   - A version records the transition its request named: the named
//     transition, "loopback" for a save that named none, the scheduled
//     transition for a scheduled firing — on every version the request
//     commits, the one a COMMIT_BEFORE_DISPATCH boundary commits included.
//   - Workflow selection and criteria see the name of the version the request
//     started from, as its transaction sees it, for the whole run; none on a
//     create.

// chtGuardWF rests an entity in OPEN. The automated "afterSubmit" leaves OPEN
// for HIT when the criterion sees transitionForLatestSave "submit". The manual
// "submit" and "touch" both return to OPEN, so every request ends with the
// guard evaluated in OPEN, and the state says what it saw.
const chtGuardWF = `{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": "%s", "initialState": "OPEN", "active": true,
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

const chtData = `{"name":"cht","amount":1,"status":"quiet"}`

// chtMeta reads the entity's meta: the current version, or the one query
// (pointInTime=…, transactionId=…) selects.
func chtMeta(t *testing.T, id, query string) map[string]any {
	t.Helper()
	path := "/api/entity/" + id
	if query != "" {
		path += "?" + query
	}
	resp := doAuth(t, http.MethodGet, path, "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	var env struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode %s: %v: %s", path, err, body)
	}
	return env.Meta
}

// chtStateAndName returns the entity's current state and stored
// transitionForLatestSave.
func chtStateAndName(t *testing.T, id string) (string, string) {
	t.Helper()
	m := chtMeta(t, id, "")
	state, _ := m["state"].(string)
	name, _ := m["transitionForLatestSave"].(string)
	return state, name
}

// chtVersionNames returns the transitionForLatestSave of each version of the
// entity, newest first, read version by version with ?transactionId=.
func chtVersionNames(t *testing.T, id string) []string {
	t.Helper()
	resp := doAuth(t, http.MethodGet, "/api/entity/"+id+"/changes", "")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET changes %s: %d %s", id, resp.StatusCode, body)
	}
	var changes []struct {
		TransactionID string `json:"transactionId"`
	}
	if err := json.Unmarshal([]byte(body), &changes); err != nil {
		t.Fatalf("decode changes %s: %v: %s", id, err, body)
	}
	names := make([]string, len(changes))
	for i, c := range changes {
		names[i], _ = chtMeta(t, id, "transactionId="+c.TransactionID)["transitionForLatestSave"].(string)
	}
	return names
}

// chtDo sends one request and fails the test unless it answers 200.
func chtDo(t *testing.T, method, path, body string) string {
	t.Helper()
	resp := doAuth(t, method, path, body)
	out := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, out)
	}
	return out
}

// chtFirstID returns the first entity id of a transaction-response array.
func chtFirstID(t *testing.T, body string) string {
	t.Helper()
	var arr []struct {
		EntityIDs []string `json:"entityIds"`
	}
	if err := json.Unmarshal([]byte(body), &arr); err != nil || len(arr) == 0 || len(arr[0].EntityIDs) == 0 {
		t.Fatalf("no entity id in %s (err %v)", body, err)
	}
	return arr[0].EntityIDs[0]
}

// chtUpdateItem is one item of an HTTP collection update; transition "" names
// none.
func chtUpdateItem(id, data, transition string) map[string]any {
	item := map[string]any{"id": id, "payload": data}
	if transition != "" {
		item["transition"] = transition
	}
	return item
}

func chtJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// chtCreateItem is one item of an HTTP collection create.
func chtCreateItem(model, data string) map[string]any {
	return map[string]any{"model": map[string]any{"name": model, "version": 1}, "payload": data}
}

// chtPatch sends a merge patch naming transition, against the entity's
// current transaction id.
func chtPatch(t *testing.T, id, transition string) {
	t.Helper()
	resp := patchEntity(t, fmt.Sprintf("/api/entity/JSON/%s/%s", id, transition),
		"application/merge-patch+json", getEntityTxID(t, id), `{"amount":7}`)
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH %s/%s: %d %s", id, transition, resp.StatusCode, body)
	}
}

// What a criterion sees in transitionForLatestSave is the name of the version
// the request started from, on every HTTP door: never the name the request
// sends, and none on a create.
func TestDecision6_CriterionSeesStartValue_EachHTTPDoor(t *testing.T) {
	model := uniq("e2e-cht-guard")
	setupModelWithWorkflow(t, model, fmt.Sprintf(chtGuardWF, model+"-wf"))

	create := func(t *testing.T) string {
		t.Helper()
		return chtFirstID(t, chtDo(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/1", model), chtData))
	}
	// savedBySubmit rests an entity in OPEN with "submit" stored, saved from
	// a version a create saved.
	savedBySubmit := func(t *testing.T) string {
		t.Helper()
		id := create(t)
		chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/submit", chtData)
		if state, name := chtStateAndName(t, id); state != "OPEN" || name != "submit" {
			t.Fatalf("seed: entity in %q saved by %q; want OPEN saved by \"submit\" (the guard sees the start version's \"loopback\")", state, name)
		}
		return id
	}
	require := func(t *testing.T, id, wantState, wantSaved, why string) {
		t.Helper()
		state, name := chtStateAndName(t, id)
		if state != wantState {
			t.Errorf("state = %q, want %s: %s", state, wantState, why)
		}
		if name != wantSaved {
			t.Errorf("stored transitionForLatestSave = %q, want the request's %q", name, wantSaved)
		}
	}
	const (
		notOwnName = "the guard sees the start version's \"loopback\", not the request's own \"submit\""
		startName  = "the guard sees the start version's \"submit\""
	)

	named := []struct {
		name string
		send func(t *testing.T, id, transition string)
	}{
		{"put", func(t *testing.T, id, transition string) {
			chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/"+transition, chtData)
		}},
		{"patch", chtPatch},
		{"collection_update", func(t *testing.T, id, transition string) {
			chtDo(t, http.MethodPut, "/api/entity/JSON", chtJSON(t, []any{chtUpdateItem(id, chtData, transition)}))
		}},
	}
	for _, d := range named {
		t.Run(d.name+"/names_submit", func(t *testing.T) {
			id := create(t)
			d.send(t, id, "submit")
			require(t, id, "OPEN", "submit", notOwnName)
		})
		t.Run(d.name+"/follows_submit", func(t *testing.T) {
			id := savedBySubmit(t)
			d.send(t, id, "touch")
			require(t, id, "HIT", "touch", startName)
		})
	}

	loopback := []struct {
		name string
		send func(t *testing.T, id string)
	}{
		{"put_loopback", func(t *testing.T, id string) {
			chtDo(t, http.MethodPut, "/api/entity/JSON/"+id, chtData)
		}},
		{"collection_update_loopback", func(t *testing.T, id string) {
			chtDo(t, http.MethodPut, "/api/entity/JSON", chtJSON(t, []any{chtUpdateItem(id, chtData, "")}))
		}},
	}
	for _, d := range loopback {
		t.Run(d.name+"/follows_submit", func(t *testing.T) {
			id := savedBySubmit(t)
			d.send(t, id)
			require(t, id, "HIT", "loopback", startName)
		})
	}

	const noneOnCreate = "a create starts from no version, so the guard sees none"
	t.Run("create", func(t *testing.T) {
		require(t, create(t), "OPEN", "loopback", noneOnCreate)
	})
	t.Run("collection_create", func(t *testing.T) {
		body := chtDo(t, http.MethodPost, "/api/entity/JSON", chtJSON(t, []any{chtCreateItem(model, chtData)}))
		require(t, chtFirstID(t, body), "OPEN", "loopback", noneOnCreate)
	})
}

// Workflow selection sees the name of the version the request started from:
// a request does not choose its workflow by the transition it names.
func TestDecision6_SelectionSeesStartValue(t *testing.T) {
	model := uniq("e2e-cht-select")
	wf := func(name, criterion, next string) map[string]any {
		w := map[string]any{
			"version": "1.1", "name": name, "initialState": "S", "active": true,
			"states": map[string]any{
				"S": map[string]any{"transitions": []any{
					map[string]any{"name": "x", "next": "S", "manual": true},
					map[string]any{"name": "y", "next": next, "manual": true},
				}},
				next: map[string]any{},
			},
		}
		if criterion != "" {
			w["criterion"] = json.RawMessage(criterion)
		}
		return w
	}
	setupModelWithWorkflow(t, model, chtJSON(t, map[string]any{
		"importMode": "REPLACE",
		"workflows": []any{
			wf("saved-by-x", `{"type":"lifecycle","field":"transitionForLatestSave","operatorType":"EQUALS","value":"x"}`, "FIRST_Y"),
			wf("other", "", "SECOND_Y"),
		},
	}))

	id := createEntityE2E(t, model, 1, chtData)
	chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/x", chtData)
	if state, name := chtStateAndName(t, id); state != "S" || name != "x" {
		t.Fatalf("seed: entity in %q saved by %q; want S saved by \"x\"", state, name)
	}

	chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/y", chtData)
	if state, _ := chtStateAndName(t, id); state != "FIRST_Y" {
		t.Errorf("state = %q, want FIRST_Y: selection sees the start version's \"x\" and runs the first definition, not the request's own \"y\"", state)
	}
}

// chtCBDProc is a COMMIT_BEFORE_DISPATCH processor served by the suite's
// local processing service.
func chtCBDProc(name string) string {
	return fmt.Sprintf(`{"type": "externalized", "name": %q, "executionMode": "COMMIT_BEFORE_DISPATCH",
		"config": {"attachEntity": true, "calculationNodesTags": ""}}`, name)
}

// registerCountingProc registers a processor that leaves the entity as it is
// and counts its dispatches.
func registerCountingProc(t *testing.T, name string) *atomic.Int32 {
	t.Helper()
	n := &atomic.Int32{}
	procSvc.RegisterProcessor(name, func(_ context.Context, entity *spi.Entity, _ spi.ProcessorDefinition) (*spi.Entity, error) {
		n.Add(1)
		return entity, nil
	})
	t.Cleanup(procSvc.Reset)
	return n
}

// A criterion after a COMMIT_BEFORE_DISPATCH boundary sees what it saw before
// the boundary: the start version's name.
func TestDecision6_CriterionAfterBoundarySeesStartValue(t *testing.T) {
	model := uniq("e2e-cht-after-boundary")
	proc := model + "-cbd"
	dispatched := registerCountingProc(t, proc)
	setupModelWithWorkflow(t, model, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [
					{"name": "submit", "next": "OPEN", "manual": true},
					{"name": "touch", "next": "MID", "manual": true, "processors": [%s]}
				]},
				"MID": {"transitions": [
					{"name": "afterSubmit", "next": "HIT", "manual": false,
					 "criterion": {"type": "lifecycle", "field": "transitionForLatestSave", "operatorType": "EQUALS", "value": "submit"}}
				]},
				"HIT": {}
			}
		}]
	}`, model, chtCBDProc(proc)))

	id := createEntityE2E(t, model, 1, chtData)
	chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/submit", chtData)
	chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/touch", chtData)

	if n := dispatched.Load(); n != 1 {
		t.Fatalf("the processor was dispatched %d time(s), want 1: the run needs a boundary", n)
	}
	if state, name := chtStateAndName(t, id); state != "HIT" || name != "touch" {
		t.Errorf("entity in %q saved by %q; want HIT saved by \"touch\": the criterion after the boundary sees the start version's \"submit\"", state, name)
	}
}

// The version a COMMIT_BEFORE_DISPATCH boundary commits carries the request's
// name, as the final version does, on the doors whose boundary version had no
// name.
func TestDecision8_BoundaryVersionCarriesRequestName(t *testing.T) {
	model := uniq("e2e-cht-boundary")
	proc := model + "-cbd"
	dispatched := registerCountingProc(t, proc)
	// A create with status "cbd" and a loopback update to status "relay" each
	// reach a transition with the processor; "named" has it too. Status
	// "quiet" reaches none, and neither does "mark", which only gives the
	// loopback update a start version whose name is not "loopback".
	setupModelWithWorkflow(t, model, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%[1]s-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [
					{"name": "init", "next": "OPEN", "manual": false,
					 "criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "cbd"},
					 "processors": [%[2]s]},
					{"name": "plain", "next": "OPEN", "manual": false}
				]},
				"OPEN": {"transitions": [
					{"name": "relay", "next": "DONE", "manual": false,
					 "criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "relay"},
					 "processors": [%[2]s]},
					{"name": "mark", "next": "OPEN", "manual": true},
					{"name": "named", "next": "DONE", "manual": true, "processors": [%[2]s]}
				]},
				"DONE": {}
			}
		}]
	}`, model, chtCBDProc(proc)))

	const (
		cbd   = `{"name":"cht","amount":1,"status":"cbd"}`
		relay = `{"name":"cht","amount":1,"status":"relay"}`
	)
	quiet := func(t *testing.T) string {
		t.Helper()
		return createEntityE2E(t, model, 1, chtData)
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) string
		want []string // newest first
	}{
		{"create", func(t *testing.T) string {
			return createEntityE2E(t, model, 1, cbd)
		}, []string{"loopback", "loopback"}},
		{"loopback_update", func(t *testing.T) string {
			id := quiet(t)
			chtDo(t, http.MethodPut, "/api/entity/JSON/"+id+"/mark", chtData)
			chtDo(t, http.MethodPut, "/api/entity/JSON/"+id, relay)
			return id
		}, []string{"loopback", "loopback", "mark", "loopback"}},
		{"collection_create_item", func(t *testing.T) string {
			return chtFirstID(t, chtDo(t, http.MethodPost, "/api/entity/JSON", chtJSON(t, []any{chtCreateItem(model, cbd)})))
		}, []string{"loopback", "loopback"}},
		{"collection_update_item_named", func(t *testing.T) string {
			id := quiet(t)
			chtDo(t, http.MethodPut, "/api/entity/JSON", chtJSON(t, []any{chtUpdateItem(id, chtData, "named")}))
			return id
		}, []string{"named", "named", "loopback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := dispatched.Load()
			id := tc.run(t)
			if n := dispatched.Load() - before; n != 1 {
				t.Fatalf("the processor was dispatched %d time(s), want 1: the request needs one boundary", n)
			}
			if got := chtVersionNames(t, id); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("version transition names (newest first) = %q, want %q: the boundary version carries the request's name", got, tc.want)
			}
		})
	}
}

// A scheduled firing's version records the scheduled transition, not the name
// of the request before it.
func TestDecision7_ScheduledFiringStampsItsTransition(t *testing.T) {
	h, _ := newSchedulerHarness(t, nil)
	model := uniq("e2e-cht-sched")
	h.SetupModelWithWorkflow(t, model, schedDoc(model+"-wf", map[string]any{
		"Open": map[string]any{"transitions": []any{map[string]any{
			"name": "submit", "next": "Submitted", "manual": true,
		}}},
		"Submitted": map[string]any{"transitions": []any{map[string]any{
			"name": "expire", "next": "Expired", "manual": false, "schedule": map[string]any{"delayMs": 100},
		}}},
		"Expired": map[string]any{},
	}))
	id := createOpen(t, h, model, chtData)
	resp := h.DoAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/submit", chtData, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: %d %s", resp.StatusCode, body)
	}

	awaitCallbackEntityState(t, h, id, "Expired", scheduledFireTimeout)
	resp = h.DoAuth(t, http.MethodGet, "/api/entity/"+id, "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read: %d %s", resp.StatusCode, body)
	}
	var env struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if got := env.Meta["transitionForLatestSave"]; got != "expire" {
		t.Errorf("meta.transitionForLatestSave = %v, want \"expire\": the firing records its own transition, not the earlier \"submit\"", got)
	}
}

// The same entity twice in one collection update — a named item, then a
// loopback item — commits one version carrying the last item's name. The
// second item starts from the first item's save, which its transaction sees.
func TestChangeHistory_CollectionSameEntityTwice_LastItemWins(t *testing.T) {
	model := uniq("e2e-cht-twice")
	setupModelWithWorkflow(t, model, fmt.Sprintf(chtGuardWF, model+"-wf"))
	id := createEntityE2E(t, model, 1, chtData)

	chtDo(t, http.MethodPut, "/api/entity/JSON", chtJSON(t, []any{
		chtUpdateItem(id, chtData, "submit"),
		chtUpdateItem(id, chtData, ""),
	}))

	if got, want := chtVersionNames(t, id), []string{"loopback", "loopback"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("version transition names (newest first) = %q, want %q: one version for the request, with the last item's name", got, want)
	}
	if state, _ := chtStateAndName(t, id); state != "HIT" {
		t.Errorf("state = %q, want HIT: the loopback item's guard sees the \"submit\" the first item saved in the transaction", state)
	}
}

// A request that fails after a COMMIT_BEFORE_DISPATCH boundary answers its
// error; the version the boundary committed stays, with the request's name.
func TestChangeHistory_FailureAfterBoundary_KeepsBoundaryVersionName(t *testing.T) {
	model := uniq("e2e-cht-fail-after")
	cbdProc, boom := model+"-cbd", model+"-boom"
	dispatched := registerCountingProc(t, cbdProc)
	procSvc.RegisterProcessor(boom, func(context.Context, *spi.Entity, spi.ProcessorDefinition) (*spi.Entity, error) {
		return nil, fmt.Errorf("processor failed after the boundary")
	})
	setupModelWithWorkflow(t, model, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%s-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [
					{"name": "named", "next": "DONE", "manual": true, "processors": [%s,
						{"type": "externalized", "name": %q, "executionMode": "SYNC",
						 "config": {"attachEntity": true, "calculationNodesTags": ""}}]}
				]},
				"DONE": {}
			}
		}]
	}`, model, chtCBDProc(cbdProc), boom))
	id := createEntityE2E(t, model, 1, chtData)

	resp := doAuth(t, http.MethodPut, "/api/entity/JSON/"+id+"/named", `{"name":"cht","amount":2,"status":"quiet"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the processor's failure); body: %s", resp.StatusCode, body)
	}
	assertErrorCode(t, body, "WORKFLOW_FAILED")
	if n := dispatched.Load(); n != 1 {
		t.Fatalf("the boundary processor was dispatched %d time(s), want 1", n)
	}

	if got, want := chtVersionNames(t, id), []string{"named", "loopback"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("version transition names (newest first) = %q, want %q: the boundary's committed version carries the request's name", got, want)
	}
}

// A callout that reads the entity under the request's transaction token sees
// the request's own name once the engine has saved the entity in the
// transaction (the save before the callout), not the start version's.
func TestDecision6_JoinedReadSeesRequestName(t *testing.T) {
	h := newCallbackHarness(t)
	model := uniq("e2e-cht-joined")
	proc := model + "-read"
	seen := newSeenLog()
	h.RegisterProc(proc, func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.GetEntity(rc.entityID)
		if err != nil {
			seen.put(rc.entityID, fmt.Sprintf("error: %v", err))
			return nil, nil
		}
		var env struct {
			Meta map[string]any `json:"meta"`
		}
		_ = json.Unmarshal([]byte(res.Body), &env)
		seen.put(rc.entityID, fmt.Sprintf("%d %v", res.StatusCode, env.Meta["transitionForLatestSave"]))
		return nil, nil
	})
	syncProc := fmt.Sprintf(`{"type": "externalized", "name": %q, "executionMode": "SYNC",
		"config": {"attachEntity": true, "calculationNodesTags": ""}}`, proc)
	h.SetupModelWithWorkflow(t, model, fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "%[1]s-wf", "initialState": "OPEN", "active": true,
			"states": {
				"OPEN": {"transitions": [
					{"name": "relay", "next": "DONE", "manual": false,
					 "criterion": {"type": "simple", "jsonPath": "$.status", "operatorType": "EQUALS", "value": "relay"},
					 "processors": [%[2]s]},
					{"name": "mark", "next": "OPEN", "manual": true},
					{"name": "named", "next": "DONE", "manual": true, "processors": [%[2]s]}
				]},
				"DONE": {}
			}
		}]
	}`, model, syncProc))

	put := func(t *testing.T, path, data string) {
		t.Helper()
		resp := h.DoAuth(t, http.MethodPut, path, data, "")
		if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", path, resp.StatusCode, body)
		}
	}
	// The request changes the data, so the engine saves the entity in the
	// transaction before the callout.
	const changed = `{"name":"cht","amount":2,"status":"quiet"}`

	t.Run("named", func(t *testing.T) {
		id := createOpen(t, h, model, chtData)
		put(t, "/api/entity/JSON/"+id+"/named", changed)
		if got := seen.get(t, id); got != "200 named" {
			t.Errorf("joined read saw %q, want \"200 named\": the request's name, not the start version's \"loopback\"", got)
		}
	})
	t.Run("loopback", func(t *testing.T) {
		id := createOpen(t, h, model, chtData)
		put(t, "/api/entity/JSON/"+id+"/mark", chtData)
		put(t, "/api/entity/JSON/"+id, `{"name":"cht","amount":2,"status":"relay"}`)
		if got := seen.get(t, id); got != "200 loopback" {
			t.Errorf("joined read saw %q, want \"200 loopback\": the request's name, not the start version's \"mark\"", got)
		}
	})
}
