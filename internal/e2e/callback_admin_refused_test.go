package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_admin_refused_test.go — model and workflow administration never
// runs inside a transaction.
//
// A compute member's callback carries the transaction token of the operation
// that called it out. Every entity operation joins that transaction; a request
// that would change a model or its workflows — import, delete, change level,
// lock, unlock, unique keys, workflow import, and the gRPC EntityModelManage
// requests that do the same — is refused with 400
// MODEL_ADMIN_IN_JOINED_TRANSACTION before the token is verified, before any
// transaction lock is taken and before anything is read or written. The same
// request without the token is an ordinary administration request and works.

const adminRefusedCode = "MODEL_ADMIN_IN_JOINED_TRANSACTION"

// adminOutcome is what one door answered, recorded on the member goroutine
// and asserted on the test goroutine.
type adminOutcome struct {
	door   string
	status int    // HTTP status; 0 on the gRPC door
	code   string // problem errorCode, or the gRPC envelope's message prefix
	body   string
	err    error
}

// modelManageGRPC sends one EntityModelManage request of eventType under
// pass, and returns its envelope.
func (h *callbackHarness) modelManageGRPC(eventType string, body map[string]any, pass string) (txEnvelope, error) {
	reqCE, err := internalgrpc.NewCloudEvent(eventType, body)
	if err != nil {
		return txEnvelope{}, err
	}
	respCE, err := cyodapb.NewCloudEventsServiceClient(h.apiConn).EntityModelManage(h.grpcCtx(pass), reqCE)
	if err != nil {
		return txEnvelope{}, err
	}
	return parseTxEnvelope(respCE)
}

func httpAdminOutcome(door string, res callbackResult, err error) adminOutcome {
	if err != nil {
		return adminOutcome{door: door, err: err}
	}
	var pd problemDoc
	_ = json.Unmarshal([]byte(res.Body), &pd)
	code, _ := pd.Properties["errorCode"].(string)
	return adminOutcome{door: door, status: res.StatusCode, code: code, body: res.Body}
}

func grpcAdminOutcome(door string, env txEnvelope, err error) adminOutcome {
	if err != nil {
		return adminOutcome{door: door, err: err}
	}
	if env.Success || env.Error == nil {
		return adminOutcome{door: door, body: "success"}
	}
	code, _, _ := strings.Cut(env.Error.Message, ":")
	retryable := env.Error.Retryable != nil && *env.Error.Retryable
	return adminOutcome{door: door, code: code, body: fmt.Sprintf("%s %s retryable=%v", env.Error.Code, env.Error.Message, retryable)}
}

// TestCallback_ModelAdministration_RefusedUnderToken makes every
// administration request from inside a running SYNC processor, under the
// callout's real token, on both doors — and, from the same processor, an
// entity create under the token (which joins) and a workflow import without
// the token (which works) — then checks that nothing the refused requests
// asked for happened.
func TestCallback_ModelAdministration_RefusedUnderToken(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "cb-adm-primary"     // the entity whose transition calls the member out
	const target = "cb-adm-target"       // the model the refused requests aim at: imported and locked, no workflow
	const secondary = "cb-adm-secondary" // the joined entity create's model
	const plain = "cb-adm-plain"         // the model administered without the token, from inside the processor

	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", target), workflowSampleModel, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", target, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", target), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", target, resp.StatusCode, body)
	}

	type report struct {
		refused  []adminOutcome
		joined   callbackResult
		joinErr  error
		plainOps []adminOutcome
	}
	done := make(chan report, 1)
	h.RegisterProc("cb-adm-proc", func(rc *reqCtx) (map[string]any, error) {
		var r report
		record := func(door string, res callbackResult, err error) {
			r.refused = append(r.refused, httpAdminOutcome(door, res, err))
		}
		base := fmt.Sprintf("/api/model/%s/1", target)
		res, err := rc.h.callback(http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", target), workflowSampleModel, rc.token)
		record("importEntityModel", res, err)
		res, err = rc.h.callback(http.MethodDelete, base, "", rc.token)
		record("deleteEntityModel", res, err)
		res, err = rc.h.callback(http.MethodPost, base+"/changeLevel/STRUCTURAL", "", rc.token)
		record("setEntityModelChangeLevel", res, err)
		res, err = rc.h.callback(http.MethodPut, base+"/lock", "", rc.token)
		record("lockEntityModel", res, err)
		res, err = rc.h.callback(http.MethodPut, base+"/unlock", "", rc.token)
		record("unlockEntityModel", res, err)
		res, err = rc.h.callback(http.MethodPut, base+"/unique-keys", `[{"id":"uk","fields":["$.name"]}]`, rc.token)
		record("setEntityModelUniqueKeys", res, err)
		res, err = rc.h.callback(http.MethodPost, base+"/workflow/import", secondaryWorkflow, rc.token)
		record("importEntityModelWorkflow", res, err)

		model := map[string]any{"name": target, "version": 1}
		for _, g := range []struct {
			door, eventType string
			body            map[string]any
		}{
			{"grpc EntityModelImportRequest", internalgrpc.EntityModelImportRequest, map[string]any{
				"id": "adm-import", "model": model, "dataFormat": "JSON", "converter": "SAMPLE_DATA",
				"payload": map[string]any{"name": "x"}}},
			{"grpc EntityModelTransitionRequest", internalgrpc.EntityModelTransitionRequest, map[string]any{
				"id": "adm-unlock", "model": model, "transition": "UNLOCK"}},
			{"grpc EntityModelDeleteRequest", internalgrpc.EntityModelDeleteRequest, map[string]any{
				"id": "adm-delete", "model": model}},
			{"grpc EntityModelSetUniqueKeysRequest", internalgrpc.EntityModelSetUniqueKeysRequest, map[string]any{
				"id": "adm-keys", "model": model, "uniqueKeys": []any{map[string]any{"id": "uk", "fields": []string{"$.name"}}}}},
		} {
			env, err := rc.h.modelManageGRPC(g.eventType, g.body, rc.token)
			r.refused = append(r.refused, grpcAdminOutcome(g.door, env, err))
		}

		// An entity operation under the token still joins the transaction.
		r.joined, r.joinErr = rc.CreateEntity(secondary, 1, `{"name":"child","amount":1,"status":"new"}`)

		// Administration without the token is an ordinary request and works,
		// while the callout is in progress.
		res, err = rc.h.callback(http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", plain), workflowSampleModel, "")
		r.plainOps = append(r.plainOps, httpAdminOutcome("importEntityModel", res, err))
		res, err = rc.h.callback(http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", plain), "", "")
		r.plainOps = append(r.plainOps, httpAdminOutcome("lockEntityModel", res, err))
		res, err = rc.h.callback(http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", plain), secondaryWorkflow, "")
		r.plainOps = append(r.plainOps, httpAdminOutcome("importEntityModelWorkflow", res, err))

		done <- r
		return nil, nil
	})
	h.setupModelSampleWithWorkflow(t, primary, workflowSampleModel, chainWorkflowJSON("cb-adm-wf",
		procSpec{name: "cb-adm-proc", mode: "SYNC", config: map[string]any{"calculationNodesTags": ""}}))

	primaryID, status, body := h.CreateEntity(t, primary, 1, `{"name":"parent","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("primary create: status=%d body=%s", status, body)
	}
	var r report
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: the processor did not run")
	}

	if len(r.refused) != 11 {
		t.Fatalf("outcomes = %d, want 11 (7 HTTP + 4 gRPC)", len(r.refused))
	}
	for _, o := range r.refused {
		if o.err != nil {
			t.Errorf("%s: transport error: %v", o.door, o.err)
			continue
		}
		if strings.HasPrefix(o.door, "grpc ") {
			if o.code != adminRefusedCode || !strings.Contains(o.body, "CLIENT_ERROR") || !strings.Contains(o.body, "retryable=false") {
				t.Errorf("%s: %s; want a CLIENT_ERROR envelope prefixed %s, not retryable", o.door, o.body, adminRefusedCode)
			}
			continue
		}
		if o.status != http.StatusBadRequest || o.code != adminRefusedCode {
			t.Errorf("%s: status=%d code=%q body=%s; want 400 %s", o.door, o.status, o.code, o.body, adminRefusedCode)
		}
		if !strings.Contains(o.body, "cannot run inside a transaction") || strings.Contains(o.body, `"retryable":true`) {
			t.Errorf("%s: body = %s; want the reason, not retryable", o.door, o.body)
		}
	}

	// The entity create under the token joined the primary's transaction.
	if r.joinErr != nil || r.joined.StatusCode != http.StatusOK || r.joined.EntityID == "" {
		t.Fatalf("joined create: err=%v status=%d body=%s", r.joinErr, r.joined.StatusCode, r.joined.Body)
	}
	if primTx, secTx := extractTxIDFromAudit(t, h, primaryID), extractTxIDFromAudit(t, h, r.joined.EntityID); primTx != secTx {
		t.Errorf("joined create txID %q != primary txID %q: entity operations must still join", secTx, primTx)
	}

	// Administration without the token worked.
	for _, o := range r.plainOps {
		if o.err != nil || o.status != http.StatusOK {
			t.Errorf("%s without a token: err=%v status=%d body=%s; want 200", o.door, o.err, o.status, o.body)
		}
	}

	// Nothing the refused requests asked for happened: the target model is
	// still there, still locked, without a workflow or a unique key.
	resp = h.DoAuth(t, http.MethodGet, "/api/model/", "", "")
	models := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/model/: %d %s", resp.StatusCode, models)
	}
	var list []struct {
		ModelName    string `json:"modelName"`
		CurrentState string `json:"currentState"`
	}
	if err := json.Unmarshal([]byte(models), &list); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	found := false
	for _, m := range list {
		if m.ModelName == target {
			found = true
			if m.CurrentState != "LOCKED" {
				t.Errorf("target model state = %q, want LOCKED", m.CurrentState)
			}
		}
	}
	if !found {
		t.Errorf("target model %s is gone: a refused delete must not delete", target)
	}
	resp = h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/model/%s/1/workflow/export", target), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusNotFound {
		t.Errorf("workflow export of the target: %d %s; want 404, a refused import must not import", resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/model/export/SIMPLE_VIEW/%s/1", target), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK || strings.Contains(body, `"uk"`) {
		t.Errorf("model export of the target: %d %s; want 200 without the refused unique key", resp.StatusCode, body)
	}
}

// TestModelAdministration_TokenRefused400 pins the refusal on each operation
// of the shared server, behind the OpenAPI conformance validator. The token's
// value is not consulted: the refusal precedes its verification, so a value
// that names nothing is answered the same way a real one is.
func TestModelAdministration_TokenRefused400(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const name = "adm-refused-http"
	if resp := doAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", name), workflowSampleModel); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model: %d", resp.StatusCode)
	}
	base := fmt.Sprintf("/api/model/%s/1", name)
	for _, op := range []struct{ id, method, path, body string }{
		{"importEntityModel", http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", name), workflowSampleModel},
		{"deleteEntityModel", http.MethodDelete, base, ""},
		{"setEntityModelChangeLevel", http.MethodPost, base + "/changeLevel/STRUCTURAL", ""},
		{"lockEntityModel", http.MethodPut, base + "/lock", ""},
		{"unlockEntityModel", http.MethodPut, base + "/unlock", ""},
		{"setEntityModelUniqueKeys", http.MethodPut, base + "/unique-keys", `[{"id":"uk","fields":["$.name"]}]`},
		{"importEntityModelWorkflow", http.MethodPost, base + "/workflow/import", secondaryWorkflow},
	} {
		t.Run(op.id, func(t *testing.T) {
			req, err := authRequestRaw(e2eCtx(t), op.method, op.path, strings.NewReader(op.body))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("X-Tx-Token", "not-a-transaction-token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", op.method, op.path, err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s: status=%d, want 400", op.method, op.path, resp.StatusCode)
			}
			commontest.ExpectErrorCode(t, resp, adminRefusedCode)
		})
	}
	// The model is untouched: still unlocked, so a lock without the token succeeds.
	if resp := doAuth(t, http.MethodPut, base+"/lock", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock without a token after the refusals: %d, want 200", resp.StatusCode)
	}
}
