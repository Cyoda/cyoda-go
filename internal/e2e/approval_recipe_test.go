package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	cyodapb "github.com/cyoda/cyoda-go/api/grpc/cyoda"
	internalgrpc "github.com/cyoda/cyoda-go/internal/grpc"
)

// approval_recipe_test.go — the segregation-of-duties recipe of
// `cyoda help auth integration` ("the submitter may not approve"), run as a
// compute node runs it: a function criterion of the manual approve
// transition reads the entity's change history with the compute node's own
// client and approves only when
//
//   - the callout is a user's request from the backend's OBO client,
//   - the newest change-history entry is the submit, by a user other than
//     the approver (authid), and
//   - the version that entry's transaction committed has the data the
//     criterion's payload carries.
//
// It refuses in every other case: a read that fails, a payload without data.

// arWorkflow: DRAFT --submit--> SUBMITTED --approve--> APPROVED, both manual;
// approve is guarded by the function criterion named fn. attachEntity is
// given only when attach is non-empty ("" leaves the default).
func arWorkflow(wf, fn, attach string) string {
	cfg := `"calculationNodesTags":""`
	if attach != "" {
		cfg += `,"attachEntity":` + attach
	}
	return fmt.Sprintf(`{
	"importMode": "REPLACE",
	"workflows": [{
		"version": "1.1", "name": %q, "initialState": "DRAFT", "active": true,
		"states": {
			"DRAFT": {"transitions": [{"name": "submit", "next": "SUBMITTED", "manual": true}]},
			"SUBMITTED": {"transitions": [
				{"name": "approve", "next": "APPROVED", "manual": true,
				 "criterion": {"type": "function", "function": {"name": %q, "config": {%s}}}}
			]},
			"APPROVED": {}
		}
	}]
}`, wf, fn, cfg)
}

// arVerdicts records, per entity, why the check refused or that it approved.
type arVerdicts struct {
	mu sync.Mutex
	m  map[string]string
}

func (v *arVerdicts) put(entityID, verdict string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[entityID] = verdict
}

func (v *arVerdicts) get(entityID string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.m[entityID]
}

const arApproved = "approved"

// arChange is the part of a change-history entry the check reads.
type arChange struct {
	Transition     string `json:"transition"`
	AttributedKind string `json:"attributedKind"`
	User           string `json:"user"`
	TransactionID  string `json:"transactionId"`
}

// arGet is a read with the compute node's own client, outside any
// transaction (no transaction token, no pointInTime). It runs on the member's
// handler goroutine, so it returns errors instead of failing the test.
func arGet(h *callbackHarness, bearer, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(h.ownerCtx(), http.MethodGet, h.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return raw, nil
}

// arCheck is the recipe's check. backendClient is the backend's OBO client id
// (the executor allow-list); computeBearer is the compute node's own client.
// Every refusal is recorded with its reason; the answer is never an error,
// so a refusal is a criterion that does not match.
func arCheck(h *callbackHarness, computeBearer, backendClient string, verdicts *arVerdicts) callbackCrit {
	return func(rc *reqCtx) (bool, error) {
		refuse := func(why string) (bool, error) {
			verdicts.put(rc.entityID, why)
			return false, nil
		}
		if rc.attrs["authtype"] != "user" {
			return refuse("not a user")
		}
		if rc.attrs["authexectype"] != "service" || rc.attrs["authexecid"] != backendClient {
			return refuse("not the backend's OBO client")
		}
		if rc.entityData == nil {
			return refuse("no data in the payload")
		}
		raw, err := arGet(h, computeBearer, "/api/entity/"+rc.entityID+"/changes")
		if err != nil {
			return refuse("history read failed")
		}
		var changes []arChange
		if err := json.Unmarshal(raw, &changes); err != nil || len(changes) == 0 {
			return refuse("history unreadable")
		}
		newest := changes[0]
		if newest.Transition != "submit" {
			return refuse("newest entry is not the submit")
		}
		if newest.AttributedKind != "user" {
			return refuse("submit not by a user")
		}
		if newest.User == rc.attrs["authid"] {
			return refuse("approver is the submitter")
		}
		raw, err = arGet(h, computeBearer, "/api/entity/"+rc.entityID+"?transactionId="+newest.TransactionID)
		if err != nil {
			return refuse("submitted version read failed")
		}
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Data == nil {
			return refuse("submitted version unreadable")
		}
		if !reflect.DeepEqual(env.Data, rc.entityData) {
			return refuse("data differs from the submitted version")
		}
		verdicts.put(rc.entityID, arApproved)
		return true, nil
	}
}

// arFixture is one stack with the recipe's workflow on two models: one whose
// criterion takes the default attachEntity, one with attachEntity false.
type arFixture struct {
	h             *callbackHarness
	model, noData string
	alice, bob    string // OBO tokens of the backend's OBO client
	verdicts      *arVerdicts
}

func newARFixture(t *testing.T) *arFixture {
	t.Helper()
	h := newCallbackHarness(t)
	f := &arFixture{
		h:        h,
		model:    uniq("ar-approve"),
		noData:   uniq("ar-approve-nodata"),
		alice:    oboTokenOn(t, h.baseURL, h.token(t), "alice"),
		bob:      oboTokenOn(t, h.baseURL, h.token(t), "bob"),
		verdicts: &arVerdicts{m: map[string]string{}},
	}
	fn, fnNoData := uniq("ar-check"), uniq("ar-check-nodata")
	check := arCheck(h, h.computeBearer(t), oboClientOf(t, f.alice), f.verdicts)
	h.RegisterCriteria(fn, check)
	h.RegisterCriteria(fnNoData, check)
	h.SetupModelWithWorkflow(t, f.model, arWorkflow(f.model+"-wf", fn, ""))
	h.SetupModelWithWorkflow(t, f.noData, arWorkflow(f.noData+"-wf", fnNoData, "false"))
	return f
}

// patch sends a merge patch as token, naming transition ("" names none),
// with the given If-Match, and returns the status and body.
func (f *arFixture) patch(t *testing.T, token, id, transition, ifMatch, body string) (int, string) {
	t.Helper()
	path := "/api/entity/JSON/" + id
	if transition != "" {
		path += "/" + transition
	}
	req, err := e2eNewRequest(t, http.MethodPatch, f.h.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("If-Match", ifMatch)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	return resp.StatusCode, f.h.readBody(t, resp)
}

// txID returns the entity's current meta.transactionId.
func (f *arFixture) txID(t *testing.T, id string) string {
	t.Helper()
	resp := f.h.DoAuth(t, http.MethodGet, "/api/entity/"+id, "", "")
	body := f.h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", id, resp.StatusCode, body)
	}
	var env struct {
		Meta struct {
			TransactionID string `json:"transactionId"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Meta.TransactionID == "" {
		t.Fatalf("no transactionId in %s (err %v)", body, err)
	}
	return env.Meta.TransactionID
}

// submitted creates an entity of model and has alice submit it, as the
// backend does: PATCH {} naming submit, If-Match the version she read.
func (f *arFixture) submitted(t *testing.T, model string) string {
	t.Helper()
	id, status, body := f.h.CreateEntity(t, model, 1, `{"name":"order","amount":1,"status":"draft"}`)
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	if code, out := f.patch(t, f.alice, id, "submit", f.txID(t, id), `{}`); code != http.StatusOK {
		t.Fatalf("alice submits: %d %s", code, out)
	}
	f.requireState(t, id, "SUBMITTED")
	return id
}

func (f *arFixture) requireState(t *testing.T, id, want string) {
	t.Helper()
	if state, status := f.h.GetEntityState(t, id); state != want {
		t.Fatalf("state = %q (GET %d), want %s", state, status, want)
	}
}

// requireRefused asserts that the approval answered 400 WORKFLOW_FAILED, the
// entity stayed SUBMITTED, and the check refused for the reason given.
func (f *arFixture) requireRefused(t *testing.T, id string, code int, body, reason string) {
	t.Helper()
	if code != http.StatusBadRequest || !strings.Contains(body, "WORKFLOW_FAILED") {
		t.Fatalf("approval: %d %s; want 400 WORKFLOW_FAILED", code, body)
	}
	f.requireState(t, id, "SUBMITTED")
	if got := f.verdicts.get(id); got != reason {
		t.Fatalf("check verdict = %q, want %q", got, reason)
	}
}

func TestApprovalRecipe(t *testing.T) {
	f := newARFixture(t)

	t.Run("Passes", func(t *testing.T) {
		t.Run("PatchWithIfMatch", func(t *testing.T) {
			id := f.submitted(t, f.model)
			code, body := f.patch(t, f.bob, id, "approve", f.txID(t, id), `{}`)
			if code != http.StatusOK {
				t.Fatalf("bob approves: %d %s (check verdict %q)", code, body, f.verdicts.get(id))
			}
			f.requireState(t, id, "APPROVED")
			if got := f.verdicts.get(id); got != arApproved {
				t.Fatalf("check verdict = %q, want %q", got, arApproved)
			}
		})
		t.Run("GRPCTransition", func(t *testing.T) {
			id := f.submitted(t, f.model)
			reqCE, err := internalgrpc.NewCloudEvent(internalgrpc.EntityTransitionRequest, map[string]any{
				"id": "ar-approve", "entityId": id, "transition": "approve",
			})
			if err != nil {
				t.Fatalf("build transition request: %v", err)
			}
			respCE, err := cyodapb.NewCloudEventsServiceClient(f.h.apiConn).EntityManage(f.h.grpcCtxAs(f.bob, ""), reqCE)
			if err != nil {
				t.Fatalf("EntityManage: %v", err)
			}
			env, err := parseTxEnvelope(respCE)
			if err != nil || !env.Success {
				t.Fatalf("bob approves over gRPC: err=%v envelope=%+v (check verdict %q)", err, env, f.verdicts.get(id))
			}
			f.requireState(t, id, "APPROVED")
			if got := f.verdicts.get(id); got != arApproved {
				t.Fatalf("check verdict = %q, want %q", got, arApproved)
			}
		})
	})

	t.Run("SameUserRefused", func(t *testing.T) {
		id := f.submitted(t, f.model)
		code, body := f.patch(t, f.alice, id, "approve", f.txID(t, id), `{}`)
		f.requireRefused(t, id, code, body, "approver is the submitter")
	})

	t.Run("WriteBetweenRefused", func(t *testing.T) {
		id := f.submitted(t, f.model)
		if code, out := f.patch(t, f.alice, id, "", "*", `{}`); code != http.StatusOK {
			t.Fatalf("loopback update: %d %s", code, out)
		}
		code, body := f.patch(t, f.bob, id, "approve", f.txID(t, id), `{}`)
		f.requireRefused(t, id, code, body, "newest entry is not the submit")
	})

	t.Run("DataChangeRefused", func(t *testing.T) {
		id := f.submitted(t, f.model)
		code, body := f.patch(t, f.bob, id, "approve", f.txID(t, id), `{"amount":2}`)
		f.requireRefused(t, id, code, body, "data differs from the submitted version")
	})

	t.Run("NoDataRefused", func(t *testing.T) {
		id := f.submitted(t, f.noData)
		code, body := f.patch(t, f.bob, id, "approve", f.txID(t, id), `{}`)
		f.requireRefused(t, id, code, body, "no data in the payload")
	})
}
