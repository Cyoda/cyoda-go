package e2e_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// attribution_test.go — single-backend (postgres) e2e coverage for follow-on
// action attribution (spec docs/superpowers/specs/2026-07-22-attribute-followon-actions-design.md
// §4, §7, §9). Each scenario drives a real cascade/handover through the full
// HTTP+gRPC stack via the callback harness (callback_harness_test.go) and
// asserts the recorded {attributed, executor} pair via GET /entity/{id}/changes.
//
// Two distinct principals are used to make attribution observable:
//   - a USER token (user_roles claim → Kind=user), minted per-test — this is the
//     "origin" of a causal chain that a cascade must propagate.
//   - the harness's M2M client-credentials token (scopes claim → Kind=service) —
//     the compute member's identity, and the executor of joined cascade writes.
//
// The tx-token is never logged (Gate 3 / spec §8-H10).

const (
	// attrTestIssuer matches newCallbackHarnessConfigured's cfg.IAM.JWTIssuer.
	attrTestIssuer = "cyoda-callback-test"
	// attrTenant matches the harness bootstrap TenantID.
	attrTenant = "test-tenant"
	// attrServiceID is the service principal id the harness's client-credentials
	// token carries (caas_user_id == bootstrap client.UserID). It is the executor
	// id of every joined callback the member makes, and the attributed id when a
	// detached service callback records itself (§4.3).
	attrServiceID = "test-admin"
)

// --- token minting + explicit-bearer request helpers -------------------------

// signingKID recomputes the deterministic KID app.NewAuthService derives from
// the signing key's public part (sha256(SPKI)[:16] hex) so a self-minted token
// validates against the stack's local key source.
func (h *callbackHarness) signingKID(t *testing.T) string {
	t.Helper()
	pubDER, err := x509.MarshalPKIXPublicKey(&h.signKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	sum := sha256.Sum256(pubDER)
	return hex.EncodeToString(sum[:16])
}

// mintUserToken mints an OBO-shaped user JWT (carries the user_roles claim key →
// validator assigns Kind=user) for the given principal id. Signed with the
// stack's own key so it validates first-party.
func (h *callbackHarness) mintUserToken(t *testing.T, userID string, roles ...string) string {
	t.Helper()
	if roles == nil {
		roles = []string{}
	}
	now := time.Now()
	claims := map[string]any{
		"sub":          userID,
		"iss":          attrTestIssuer,
		"caas_user_id": userID,
		"caas_org_id":  attrTenant,
		"user_roles":   roles, // KEY present → Kind=user (even when empty)
		"exp":          now.Add(time.Hour).Unix(),
		"iat":          now.Unix(),
		"jti":          uuid.NewString(),
	}
	tok, err := auth.Sign(claims, h.signKey, h.signingKID(t))
	if err != nil {
		t.Fatalf("mint user token: %v", err)
	}
	return tok
}

// DoAuthBearer issues an HTTP request with an explicit bearer (rather than the
// cached M2M token) so a test can act as a specific user. txToken, when
// non-empty, joins transaction T.
func (h *callbackHarness) DoAuthBearer(t *testing.T, method, path, body, txToken, bearer string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	if txToken != "" {
		req.Header.Set("X-Tx-Token", txToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	return resp
}

// callbackBearer is the goroutine-safe callback counterpart of DoAuthBearer: it
// runs off the test goroutine (from inside a processor) and presents an explicit
// bearer, so a processor can make an OBO (user-token) callback instead of using
// the member's cached service credentials.
func (h *callbackHarness) callbackBearer(method, path, body, txToken, bearer string) (callbackResult, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		return callbackResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	if txToken != "" {
		req.Header.Set("X-Tx-Token", txToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return callbackResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return callbackResult{StatusCode: resp.StatusCode, Body: string(raw)}, nil
}

// createEntityAs creates a primary entity via the client-facing POST presenting
// an explicit bearer (e.g. a user token), returning the entity id + raw result.
func (h *callbackHarness) createEntityAs(t *testing.T, bearer, entityName string, modelVersion int, payload string) (entityID string, status int, body string) {
	t.Helper()
	resp := h.DoAuthBearer(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion), payload, "", bearer)
	body = h.readBody(t, resp)
	status = resp.StatusCode
	if status != http.StatusOK {
		return "", status, body
	}
	return parseCreatedEntityID(body), status, body
}

// parseCreatedEntityID pulls the first created id out of a transaction-response
// array (`[{"entityIds":["..."]}]`).
func parseCreatedEntityID(body string) string {
	var arr []map[string]any
	if json.Unmarshal([]byte(body), &arr) != nil || len(arr) == 0 {
		return ""
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) == 0 {
		return ""
	}
	id, _ := ids[0].(string)
	return id
}

// CreateEntityAs is the reqCtx (processor-side) callback counterpart of
// CreateEntity that presents an explicit bearer instead of the member's cached
// service credentials — used to drive an OBO (user-token) callback write. It
// echoes rc.token so the write joins T when a token is present (joined mode) and
// runs as an ordinary direct request when rc.token is empty (CBD-detached, §4.3).
func (rc *reqCtx) CreateEntityAs(bearer, entityName string, modelVersion int, payload string) (callbackResult, error) {
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion)
	res, err := rc.h.callbackBearer(http.MethodPost, path, payload, rc.token, bearer)
	if err != nil {
		return callbackResult{}, err
	}
	if res.StatusCode == http.StatusOK {
		res.EntityID = parseCreatedEntityID(res.Body)
	}
	return res, nil
}

// DeleteEntity issues a DELETE /entity/{id} callback echoing the tx-token so the
// tombstone is written inside the primary's transaction T (delete cascade, §7).
func (rc *reqCtx) DeleteEntity(entityID string) (callbackResult, error) {
	return rc.h.callback(http.MethodDelete, "/api/entity/"+entityID, "", rc.token)
}

// setupModelSampleWithWorkflow imports+locks a model from a CUSTOM sample (so
// the model schema can declare extra data fields) then imports a workflow —
// the SetupModelWithWorkflow counterpart used when the fixed sample won't do.
func (h *callbackHarness) setupModelSampleWithWorkflow(t *testing.T, entityName, sample, workflowJSON string) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", entityName), sample, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import model %s: %d %s", entityName, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPut, fmt.Sprintf("/api/model/%s/1/lock", entityName), "", "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("lock model %s: %d %s", entityName, resp.StatusCode, body)
	}
	resp = h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/%s/1/workflow/import", entityName), workflowJSON, "")
	if body := h.readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Fatalf("import workflow %s: %d %s", entityName, resp.StatusCode, body)
	}
}

// --- change-metadata assertion helpers ---------------------------------------

// getChanges reads GET /entity/{id}/changes (admin token) → newest-first entries.
func (h *callbackHarness) getChanges(t *testing.T, entityID string) []map[string]any {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, "/api/entity/"+entityID+"/changes", "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET changes %s: %d %s", entityID, resp.StatusCode, body)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("decode changes %s: %v: %s", entityID, err, body)
	}
	return arr
}

// findChangeByType returns the newest change entry of the given canonical
// changeType (CREATE/UPDATE/DELETE), or nil.
func findChangeByType(changes []map[string]any, changeType string) map[string]any {
	for _, c := range changes {
		if ct, _ := c["changeType"].(string); ct == changeType {
			return c
		}
	}
	return nil
}

// assertAttribution asserts the {attributed, executor} pair on a change entry.
// wantUser is the attributed principal id (the "user" field), wantAttrKind the
// attributed kind, wantExecKind the executor kind. wantExecID may be "" to skip
// the executor-id check.
func assertAttribution(t *testing.T, change map[string]any, label, wantUser, wantAttrKind, wantExecKind, wantExecID string) {
	t.Helper()
	if change == nil {
		t.Fatalf("%s: no matching change entry", label)
	}
	if got, _ := change["user"].(string); got != wantUser {
		t.Errorf("%s: attributed user = %q; want %q (full entry: %v)", label, got, wantUser, change)
	}
	if got, _ := change["attributedKind"].(string); got != wantAttrKind {
		t.Errorf("%s: attributedKind = %q; want %q (full entry: %v)", label, got, wantAttrKind, change)
	}
	ex, ok := change["executedBy"].(map[string]any)
	if !ok || ex == nil {
		t.Fatalf("%s: executedBy missing (full entry: %v)", label, change)
	}
	if got, _ := ex["kind"].(string); got != wantExecKind {
		t.Errorf("%s: executedBy.kind = %q; want %q (full entry: %v)", label, got, wantExecKind, change)
	}
	if wantExecID != "" {
		if got, _ := ex["id"].(string); got != wantExecID {
			t.Errorf("%s: executedBy.id = %q; want %q (full entry: %v)", label, got, wantExecID, change)
		}
	}
}

// procCascadeWF builds a single-transition workflow whose init transition runs
// one processor of the given name and execution mode. cfgExtra is spliced into
// the processor config object (e.g. `, "startNewTxOnDispatch": true`).
func procCascadeWF(name, procName, execMode, cfgExtra string) string {
	return fmt.Sprintf(`{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": %q, "initialState": "NONE", "active": true,
			"states": {
				"NONE":   {"transitions": [{"name": "init", "next": "ACTIVE", "manual": false,
					"processors": [{"type": "calculator", "name": %q, "executionMode": %q,
						"config": {"attachEntity": true, "calculationNodesTags": ""%s}}]
				}]},
				"ACTIVE": {}
			}
		}]
	}`, name+"-wf", procName, execMode, cfgExtra)
}

// --- Scenario 1: cascade — service processor writes Y in the user's tx --------

// TestAttribution_CascadeServiceProcessor: a USER (alice) creates X; X's SYNC
// processor — dispatched to the compute member and calling back with the
// member's SERVICE credentials (no user token anywhere on the callback) —
// creates Y inside the joined transaction. Y must attribute to alice (origin),
// executed by the service (§4, §7).
func TestAttribution_CascadeServiceProcessor(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "attr-casc-primary"
	const secondary = "attr-casc-secondary"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

	yIDs := make(chan string, 1)
	h.RegisterProc("attr-casc-proc", func(rc *reqCtx) (map[string]any, error) {
		// No user token: uses the member's cached SERVICE bearer, echoing the
		// tx-token so the write JOINS alice's transaction.
		res, err := rc.CreateEntity(secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("cascade create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cascade create status=%d body=%s", res.StatusCode, res.Body)
		}
		yIDs <- res.EntityID
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-casc", "attr-casc-proc", "SYNC", ""))

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	_, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create X as alice: %d %s", status, body)
	}

	var yID string
	select {
	case yID = <-yIDs:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: cascade processor did not create Y")
	}

	change := findChangeByType(h.getChanges(t, yID), "CREATE")
	assertAttribution(t, change, "Y cascade write", "alice", "user", "service", attrServiceID)
}

// --- Scenario 2: two-hop segmented cascade X→Y→Z (CBD startNewTx=true) --------

// TestAttribution_TwoHopCascade: alice creates X; X's CBD (startNewTx=true)
// processor creates Y in the continued segment; Y's SYNC processor creates Z in
// that segment. Origin (alice) must survive the CBD commit boundary and the
// nested join so Z still attributes to alice (§4.4).
func TestAttribution_TwoHopCascade(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "attr-2hop-primary"
	const secondary = "attr-2hop-secondary"
	const tertiary = "attr-2hop-tertiary"
	h.SetupModelWithWorkflow(t, tertiary, secondaryWorkflow)

	zIDs := make(chan string, 1)
	// Y's processor (hop 2): joined SYNC write of Z.
	h.RegisterProc("attr-2hop-y", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(tertiary, 1, `{"name":"z","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("hop2 create Z: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("hop2 create Z status=%d body=%s", res.StatusCode, res.Body)
		}
		select {
		case zIDs <- res.EntityID:
		default:
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, secondary, procCascadeWF("attr-2hop-y", "attr-2hop-y", "SYNC", ""))

	// X's processor (hop 1): CBD startNewTx=true creates Y in the continued segment.
	h.RegisterProc("attr-2hop-x", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.CreateEntity(secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("hop1 create Y: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("hop1 create Y status=%d body=%s", res.StatusCode, res.Body)
		}
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-2hop-x", "attr-2hop-x", "COMMIT_BEFORE_DISPATCH", `, "startNewTxOnDispatch": true`))

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	_, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create X as alice: %d %s", status, body)
	}

	var zID string
	select {
	case zID = <-zIDs:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout: two-hop cascade did not reach Z")
	}

	// Z is written in a segment continued from X; wait for the segment to commit.
	var change map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, code := h.GetEntityState(t, zID); code == http.StatusOK && st != "" {
			change = findChangeByType(h.getChanges(t, zID), "CREATE")
			if change != nil {
				break
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	assertAttribution(t, change, "Z two-hop write", "alice", "user", "service", attrServiceID)
}

// --- Scenario 3: CBD detached handover (startNewTx=false), §4.3 ---------------

// TestAttribution_CBDDetachedHandover: a CBD-default (startNewTx=false)
// processor callback is an ordinary DIRECT request — no joined tx, no origin
// carrier. Attribution is handed to whatever identity the callback presents:
//   - service credentials → attributed = executor = service;
//   - an OBO user identity → that user (D3).
func TestAttribution_CBDDetachedHandover(t *testing.T) {
	t.Run("ServiceCreds", func(t *testing.T) {
		h := newCallbackHarness(t)
		const primary = "attr-cbd-svc-primary"
		const secondary = "attr-cbd-svc-secondary"
		h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

		yIDs := make(chan string, 1)
		h.RegisterProc("attr-cbd-svc-proc", func(rc *reqCtx) (map[string]any, error) {
			// rc.token is empty in CBD-detached mode → direct request, service creds.
			res, err := rc.CreateEntity(secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
			if err != nil {
				return nil, fmt.Errorf("detached create: %w", err)
			}
			if res.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("detached create status=%d body=%s", res.StatusCode, res.Body)
			}
			yIDs <- res.EntityID
			return nil, nil
		})
		h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-cbd-svc", "attr-cbd-svc-proc", "COMMIT_BEFORE_DISPATCH", ""))

		alice := h.mintUserToken(t, "alice", "ROLE_USER")
		if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
			t.Fatalf("create X as alice: %d %s", status, body)
		}

		var yID string
		select {
		case yID = <-yIDs:
		case <-time.After(10 * time.Second):
			t.Fatal("timeout: detached processor did not create Y")
		}
		// Detached: no origin inheritance — records the service itself.
		change := findChangeByType(h.getChanges(t, yID), "CREATE")
		assertAttribution(t, change, "Y detached service write", attrServiceID, "service", "service", attrServiceID)
	})

	t.Run("OBOUser", func(t *testing.T) {
		h := newCallbackHarness(t)
		const primary = "attr-cbd-obo-primary"
		const secondary = "attr-cbd-obo-secondary"
		h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)

		bob := h.mintUserToken(t, "bob", "ROLE_USER")
		yIDs := make(chan string, 1)
		h.RegisterProc("attr-cbd-obo-proc", func(rc *reqCtx) (map[string]any, error) {
			// Detached direct request presenting an OBO user identity (bob).
			res, err := rc.CreateEntityAs(bob, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
			if err != nil {
				return nil, fmt.Errorf("obo create: %w", err)
			}
			if res.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("obo create status=%d body=%s", res.StatusCode, res.Body)
			}
			yIDs <- res.EntityID
			return nil, nil
		})
		h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-cbd-obo", "attr-cbd-obo-proc", "COMMIT_BEFORE_DISPATCH", ""))

		alice := h.mintUserToken(t, "alice", "ROLE_USER")
		if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
			t.Fatalf("create X as alice: %d %s", status, body)
		}

		var yID string
		select {
		case yID = <-yIDs:
		case <-time.After(10 * time.Second):
			t.Fatal("timeout: OBO processor did not create Y")
		}
		// Handover to bob's presented identity — records bob, not alice, not service.
		change := findChangeByType(h.getChanges(t, yID), "CREATE")
		assertAttribution(t, change, "Y detached OBO write", "bob", "user", "user", "bob")
	})
}

// --- Scenario 4: D3 — OBO user token joined into another user's tx ------------

// TestAttribution_D3_OBOKeepsOwnUser: a processor callback presenting an OBO
// user token (bob) AND echoing the tx-token JOINS alice's transaction. Per D3
// (§7 stamp rule) a user-kind executor records ITSELF — so Y records bob, not
// the chain origin alice. A scheduled timer armed on Y inside that same tx,
// however, carries the CHAIN ORIGIN (alice) as ArmedBy (§5.2 divergence),
// verified by durable-task inspection.
func TestAttribution_D3_OBOKeepsOwnUser(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "attr-d3-primary"
	const secondary = "attr-d3-secondary"
	// Y's workflow: init → Open, with a far-future scheduled AutoClose so the
	// task stays armed (never due) while we inspect ArmedBy.
	secondaryWF := `{
		"importMode": "REPLACE",
		"workflows": [{
			"version": "1.1", "name": "attr-d3-y-wf", "initialState": "NONE", "active": true,
			"states": {
				"NONE": {"transitions": [{"name": "init", "next": "Open", "manual": false}]},
				"Open": {"transitions": [{"name": "AutoClose", "next": "Closed", "manual": false, "schedule": {"delayMs": 600000}}]},
				"Closed": {}
			}
		}]
	}`
	h.SetupModelWithWorkflow(t, secondary, secondaryWF)

	bob := h.mintUserToken(t, "bob", "ROLE_USER")
	yIDs := make(chan string, 1)
	h.RegisterProc("attr-d3-proc", func(rc *reqCtx) (map[string]any, error) {
		// Joined (rc.token present) BUT presenting bob's user token → D3.
		res, err := rc.CreateEntityAs(bob, secondary, 1, `{"name":"y","amount":1,"status":"new"}`)
		if err != nil {
			return nil, fmt.Errorf("d3 create: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("d3 create status=%d body=%s", res.StatusCode, res.Body)
		}
		yIDs <- res.EntityID
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-d3", "attr-d3-proc", "SYNC", ""))

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice: %d %s", status, body)
	}

	var yID string
	select {
	case yID = <-yIDs:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: D3 processor did not create Y")
	}

	// Write attribution: bob (the presented user), NOT alice (chain origin).
	change := findChangeByType(h.getChanges(t, yID), "CREATE")
	assertAttribution(t, change, "Y D3 write", "bob", "user", "user", "bob")

	// Timer arming: the AutoClose task armed on Y inside alice's tx carries the
	// CHAIN ORIGIN (alice), not the executor bob (§5.2). Inspect the durable row.
	var armedID, armedKind string
	if err := dbPool.QueryRow(context.Background(),
		`SELECT armed_by_id, armed_by_kind FROM scheduled_tasks WHERE entity_id=$1`, yID,
	).Scan(&armedID, &armedKind); err != nil {
		t.Fatalf("inspect scheduled_task for Y=%s: %v", yID, err)
	}
	if armedID != "alice" || armedKind != "user" {
		t.Errorf("armed timer principal = {%q,%q}; want {alice,user} (chain origin, not executor bob)", armedID, armedKind)
	}
}

// --- Scenario 5: delete cascade ----------------------------------------------

// TestAttribution_DeleteCascade: alice creates X; X's SYNC processor deletes a
// pre-existing entity (victim) inside the joined tx using SERVICE credentials.
// The tombstone must attribute to alice (origin), executed by the service —
// the executor is the STAGER, not the committer (§7 rows 6-9).
func TestAttribution_DeleteCascade(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "attr-del-primary"
	const victimModel = "attr-del-victim"
	h.SetupModelWithWorkflow(t, victimModel, secondaryWorkflow)

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	// Pre-create the victim (any principal; the delete's origin comes from X's tx).
	victimID, status, body := h.createEntityAs(t, alice, victimModel, 1, `{"name":"victim","amount":5,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create victim: %d %s", status, body)
	}

	deleted := make(chan bool, 1)
	h.RegisterProc("attr-del-proc", func(rc *reqCtx) (map[string]any, error) {
		res, err := rc.DeleteEntity(victimID)
		if err != nil {
			return nil, fmt.Errorf("cascade delete: %w", err)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cascade delete status=%d body=%s", res.StatusCode, res.Body)
		}
		deleted <- true
		return nil, nil
	})
	h.SetupModelWithWorkflow(t, primary, procCascadeWF("attr-del", "attr-del-proc", "SYNC", ""))

	if _, status, body := h.createEntityAs(t, alice, primary, 1, `{"name":"x","amount":100,"status":"new"}`); status != http.StatusOK {
		t.Fatalf("create X as alice: %d %s", status, body)
	}
	select {
	case <-deleted:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: cascade delete did not run")
	}

	change := findChangeByType(h.getChanges(t, victimID), "DELETE")
	assertAttribution(t, change, "victim tombstone", "alice", "user", "service", attrServiceID)
}

// --- Scenario 6: negative — spoofed request/response fields are ignored -------

// TestAttribution_NoRequestFieldSetsOrigin: neither the entity save body nor the
// processor callout RESPONSE can set the attributed principal. Spoofed
// user/attributedKind/executedBy/armedBy fields are treated as ordinary data (or
// ignored) and never override the platform-set attribution (§9). alice's direct
// create records alice/user/user regardless of the spoof.
func TestAttribution_NoRequestFieldSetsOrigin(t *testing.T) {
	h := newCallbackHarness(t)

	const primary = "attr-neg-primary"

	// The model deliberately DECLARES fields named like the attribution surface
	// (user/attributedKind/executedBy/armedBy) as ordinary DATA, so a caller can
	// legitimately populate them — proving the engine never sources attribution
	// from the entity body/response. (A locked model otherwise rejects unknown
	// top-level fields, which would mask the point.)
	sample := `{"name":"x","amount":100,"status":"new",` +
		`"user":"","attributedKind":"","executedBy":{"id":"","kind":""},"armedBy":{"id":"","kind":""}}`

	// Processor returns applyData carrying spoofed attribution keys — these are
	// ordinary data; they must not touch the change stamp.
	h.RegisterProc("attr-neg-proc", func(rc *reqCtx) (map[string]any, error) {
		out := cloneData(rc.entityData)
		out["user"] = "attacker"
		out["attributedKind"] = "service"
		out["executedBy"] = map[string]any{"id": "evil", "kind": "system"}
		out["armedBy"] = map[string]any{"id": "evil", "kind": "system"}
		return out, nil
	})
	h.setupModelSampleWithWorkflow(t, primary, sample, procCascadeWF("attr-neg", "attr-neg-proc", "SYNC", ""))

	alice := h.mintUserToken(t, "alice", "ROLE_USER")
	// Save body ALSO carries spoofed attribution fields (as data).
	spoofBody := `{"name":"x","amount":100,"status":"new",` +
		`"user":"attacker","attributedKind":"service",` +
		`"executedBy":{"id":"evil","kind":"system"},"armedBy":{"id":"evil","kind":"system"}}`
	xID, status, body := h.createEntityAs(t, alice, primary, 1, spoofBody)
	if status != http.StatusOK {
		t.Fatalf("create X as alice (spoofed body): %d %s", status, body)
	}

	// Direct user create → attributed = executor = alice, regardless of spoof in
	// either the save body or the processor's applyData response.
	change := findChangeByType(h.getChanges(t, xID), "CREATE")
	assertAttribution(t, change, "X spoof-resistant write", "alice", "user", "user", "alice")

	// Sanity: the spoofed values WERE accepted as data (not rejected), so the
	// test proves "accepted-but-ignored for attribution", not "rejected".
	if got, _ := h.GetEntityData(t, xID)["user"].(string); got != "attacker" {
		t.Errorf("spoofed data.user = %q; want %q (should be stored as plain data)", got, "attacker")
	}
}
