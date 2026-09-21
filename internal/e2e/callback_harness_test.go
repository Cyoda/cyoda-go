package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	cepb "github.com/cyoda-platform/cyoda-go/api/grpc/cloudevents"
	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	"github.com/cyoda-platform/cyoda-go/app"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// callback_harness_test.go builds a callback-capable in-process compute member
// for the callback-join contract (compute-node callbacks join the originating transaction).
//
// Unlike the localproc in-process ExternalProcessingService used by the other
// workflow E2E tests, this harness stands up a SEPARATE full cyoda-go stack
// (real Postgres via the shared testcontainer, HTTP + gRPC) whose workflow
// engine dispatches processors over the REAL gRPC bidi stream. A real gRPC
// calculation member connects, receives EntityProcessorCalculationRequests
// carrying the signed cyodatxtoken CloudEvent attribute, echoes that token as
// the X-Tx-Token header on HTTP callbacks into the same node, and thereby
// exercises the Joiner.Run -> participate path end-to-end.
//
// The localproc harness cannot exercise this: it bypasses gRPC entirely, so no
// token is ever minted, transmitted, echoed, or joined. This is the harness the
// skipped TestWorkflowProc_UpdateWithCBD_TrueBranch_SecondaryEntityWritten
// pointed at, and it is reused by the later callback E2E tasks.
//
// Reusable entry points (all package-internal to e2e_test):
//   - newCallbackHarness(t)                -> *callbackHarness (full stack + member)
//   - (*callbackHarness).RegisterProc      -> register a processor implemented on the member
//   - (*callbackHarness).SetupModelWithWorkflow
//   - (*callbackHarness).CreateEntity / GetEntityState / GetEntityData / DoAuth
//   - reqCtx.CreateEntity / GetEntity      -> token-echoing callbacks from inside a processor
//
// The token value is never logged (Gate 3 / spec §8-H10).

// reqCtx is handed to a processor implementation running on the compute member.
// It carries the per-request tx-token (echoed on callbacks) and the attached
// (uncommitted) primary-entity snapshot the engine shipped with the calc request.
type reqCtx struct {
	token      string         // cyodatxtoken from the calc request; echoed as X-Tx-Token
	requestID  string         // calc request id (echoed on the response)
	entityID   string         // primary (cascade-anchor) entity id
	entityData map[string]any // attached primary data (uncommitted, from the dispatch payload)
	entityMeta map[string]any // attached primary meta (state, transactionId, ...)
	h          *callbackHarness
}

// callbackResult is the HTTP outcome of a callback made from inside a processor.
type callbackResult struct {
	StatusCode int
	Body       string
	// EntityID is populated for a successful create callback.
	EntityID string
}

// CreateEntity issues a POST /entity callback echoing the tx-token, creating a
// secondary entity that must join the primary's transaction T. Returns the HTTP
// result. A network error fails the harness by panicking on the member
// goroutine only via the returned error (caller decides).
func (rc *reqCtx) CreateEntity(entityName string, modelVersion int, payload string) (callbackResult, error) {
	path := fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion)
	res, err := rc.h.callback(http.MethodPost, path, payload, rc.token)
	if err != nil {
		return callbackResult{}, err
	}
	if res.StatusCode == http.StatusOK {
		// Parse the created entity id out of the transaction-response array.
		var arr []map[string]any
		if json.Unmarshal([]byte(res.Body), &arr) == nil && len(arr) > 0 {
			if ids, ok := arr[0]["entityIds"].([]any); ok && len(ids) > 0 {
				if id, ok := ids[0].(string); ok {
					res.EntityID = id
				}
			}
		}
	}
	return res, nil
}

// GetEntity issues a GET /entity/{id} callback echoing the tx-token, reading an
// entity within the primary's transaction T (read-your-own-writes). Returns the
// HTTP result; the joined read sees T's uncommitted writes.
func (rc *reqCtx) GetEntity(entityID string) (callbackResult, error) {
	path := fmt.Sprintf("/api/entity/%s", entityID)
	return rc.h.callback(http.MethodGet, path, "", rc.token)
}

// callbackProc is a processor implemented on the compute member. It runs on a
// per-request handler goroutine while the engine blocks on the dispatch
// response, so it MUST NOT call t.Fatal (record into test-owned state and assert
// on the main goroutine instead). Returning a non-nil error fails the transition
// (and, for a SYNC processor, rolls back T). The returned map, when non-nil, is
// applied as the primary entity's new data.
type callbackProc func(rc *reqCtx) (applyData map[string]any, err error)

// callbackCrit is a FUNCTION criterion implemented on the compute member. Like
// callbackProc it runs on a per-request handler goroutine and may issue joined
// callbacks (e.g. create an entity in T as a side effect) before returning its
// boolean match. Used to exercise the criterion-dispatch txgate seam.
type callbackCrit func(rc *reqCtx) (matches bool, err error)

// callbackFunc is a generic Function callout implemented on the compute
// member (spi.ScheduleFunction, e.g. a scheduled-transition arm-time timing
// computation). Returns the response's resultKind discriminator
// and result payload (marshalled as the response's "result" object), or an
// error to have the harness reply with a failed EntityFunctionCalculationResponse.
type callbackFunc func(rc *reqCtx) (resultKind string, result map[string]any, err error)

// callbackHarness is a full HTTP+gRPC cyoda-go stack (real Postgres) with a
// connected gRPC compute member. Reused across the callback E2E tests.
type callbackHarness struct {
	app     *app.App
	baseURL string // e.g. http://127.0.0.1:PORT

	// grpcAddr is the stack's gRPC listener address; cnodes dial it.
	grpcAddr string
	// apiConn carries the harness's own gRPC API calls (EntityManage,
	// EntitySearch, …). It belongs to no cnode, so it works on a stack with no
	// cnode attached and survives a cnode closing its stream.
	apiConn *grpc.ClientConn
	// member is the default cnode; nil on a harness built by newCalloutHarness.
	member *computeMember

	// callouts is the harness-wide record of what every scripted cnode
	// received, in arrival order. The zero value is ready.
	callouts calloutLog

	// signKey is this stack's JWT signing key (same key app.New parsed from
	// cfg.IAM.JWTSigningKey). Exposed so attribution tests can mint tokens for
	// DISTINCT principals — a user token (user_roles claim → Kind=user) vs the
	// M2M client-credentials token (scopes claim → Kind=service). Both validate
	// against the same local key source (deterministic KID). Never logged.
	signKey *rsa.PrivateKey

	mu    sync.Mutex
	procs map[string]callbackProc
	crits map[string]callbackCrit
	funcs map[string]callbackFunc

	// bearerVal caches the client-credentials JWT for this stack (ROLE_ADMIN,ROLE_M2M).
	// atomic.Value synchronises the writer (test goroutine, bearerOnce.Do) and the
	// reader (member goroutine, callback()), keeping go test -race clean.
	bearerOnce sync.Once
	bearerVal  atomic.Value // stores string
}

// newCallbackHarness stands up the full stack + connected member and registers
// t.Cleanup to tear everything down. It shares the package Postgres testcontainer
// (the CYODA_POSTGRES_* env vars set by TestMain are still in effect during the
// run), so callers must use per-test-unique model names to stay isolated.
func newCallbackHarness(t *testing.T) *callbackHarness {
	t.Helper()
	return newCallbackHarnessConfigured(t, nil)
}

// newCalloutHarness stands up the full stack with NO cnode attached. Tests
// that script their own cnodes — several of them, attached and detached
// mid-test — start here. configure may be nil.
func newCalloutHarness(t *testing.T, configure func(*app.Config)) *callbackHarness {
	t.Helper()

	// Fresh JWT signing key for this stack (self-contained OAuth + JWKS).
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))

	cfg := app.DefaultConfig()
	cfg.ContextPath = "/api"
	cfg.StorageBackend = "postgres" // CYODA_POSTGRES_URL is set by TestMain and still live.
	cfg.IAM.Mode = "jwt"
	cfg.IAM.JWTSigningKey = keyPEM
	cfg.IAM.JWTIssuer = "cyoda-callback-test"
	cfg.IAM.JWTExpiry = 3600
	cfg.Bootstrap = app.BootstrapConfig{
		ClientID:     "testclient",
		ClientSecret: "testsecret",
		TenantID:     "test-tenant",
		UserID:       "test-admin",
		Roles:        "ROLE_ADMIN,ROLE_M2M",
	}
	// IMPORTANT: do NOT set cfg.ExternalProcessing — leaving it nil selects the
	// owner's loop over the real dispatcher, which mints and attaches the cyodatxtoken.

	// Discover the HTTP port before constructing the app (the JWKS validator URL
	// is built from cfg.HTTPPort and must match the live server).
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	h := &callbackHarness{baseURL: srv.URL, signKey: rsaKey, procs: map[string]callbackProc{}, crits: map[string]callbackCrit{}, funcs: map[string]callbackFunc{}}
	t.Cleanup(srv.Close)

	srvPort := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg.HTTPPort = srvPort

	// gRPC listener for the calc-member stream.
	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	cfg.GRPC.Port = grpcLis.Addr().(*net.TCPAddr).Port

	if configure != nil {
		configure(&cfg)
	}

	a := app.New(cfg)
	h.app = a
	srv.Config.Handler = a.Handler()

	go func() { _ = a.GRPCServer().Serve(grpcLis) }()
	t.Cleanup(func() { _ = a.Close() })
	// t.Cleanup runs LIFO: this Shutdown (stops the scheduler and TTL/tx
	// reapers) is registered after Close so it runs BEFORE Close tears down
	// the store pool. Without it the scheduler's 1s scan loop keeps ticking
	// against a closed pool and spams ERROR logs for the rest of the test
	// binary's life (mirrors the app.New/Shutdown/Close ordering used by
	// cors_e2e_test.go and iam_gated_fixtures_test.go).
	t.Cleanup(a.Shutdown)

	h.grpcAddr = grpcLis.Addr().String()
	apiConn, err := grpc.NewClient(h.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC API connection: %v", err)
	}
	h.apiConn = apiConn
	t.Cleanup(func() { _ = apiConn.Close() })

	// Seed the cached bearer on the test goroutine: callback() and grpcCtx()
	// read it from other goroutines and cannot fetch it themselves.
	h.token(t)
	return h
}

// newCallbackHarnessConfigured is newCallbackHarness with an optional cfg
// mutator applied to app.DefaultConfig() just before app.New — e.g. Task
// 9.2's expiry-elapsed-before-scan scenario disables this stack's built-in
// scheduler (cfg.Scheduler.Enabled = false) so it can drive its own
// bespoke, precisely-timed scheduler.Service instead (mirrors
// TestE2E_ScheduledTransition_RestartDurability's approach), eliminating
// the race window a live default-cadence scheduler ticking mid-flight would
// otherwise create. configure may be nil (identical to newCallbackHarness).
func newCallbackHarnessConfigured(t *testing.T, configure func(*app.Config)) *callbackHarness {
	t.Helper()
	h := newCalloutHarness(t, configure)
	// Tagged "sched-fn" so a schedule.function callout — whose
	// calculationNodesTags is validated non-empty at import — can route to it.
	// Processor/criteria tests configure calculationNodesTags:"" which matches
	// any cnode of the tenant.
	h.member = h.AttachCnode(t, cnodeSpec{name: "default", tags: []string{"sched-fn"}, script: h.registeredScript}).m
	return h
}

// RegisterProc registers a processor implementation on the compute member.
func (h *callbackHarness) RegisterProc(name string, fn callbackProc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.procs[name] = fn
}

func (h *callbackHarness) lookupProc(name string) (callbackProc, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn, ok := h.procs[name]
	return fn, ok
}

// RegisterCriteria registers a FUNCTION criterion implementation on the member.
func (h *callbackHarness) RegisterCriteria(name string, fn callbackCrit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.crits[name] = fn
}

func (h *callbackHarness) lookupCrit(name string) (callbackCrit, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn, ok := h.crits[name]
	return fn, ok
}

// RegisterFunction registers a generic Function callout implementation on the
// member (spi.ScheduleFunction — the scheduled-transition arm-time
// timing computation, and reusable by any future Function-typed callout).
func (h *callbackHarness) RegisterFunction(name string, fn callbackFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.funcs[name] = fn
}

func (h *callbackHarness) lookupFunc(name string) (callbackFunc, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn, ok := h.funcs[name]
	return fn, ok
}

// token returns a cached client-credentials bearer for this stack.
func (h *callbackHarness) token(t *testing.T) string {
	t.Helper()
	h.bearerOnce.Do(func() { h.bearerVal.Store(h.fetchToken(t)) })
	tok, _ := h.bearerVal.Load().(string)
	if tok == "" {
		t.Fatal("callbackHarness: empty bearer token")
	}
	return tok
}

func (h *callbackHarness) fetchToken(t *testing.T) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequest(http.MethodPost, h.baseURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("testclient", "testsecret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint returned %d: %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	tok, _ := out["access_token"].(string)
	return tok
}

// DoAuth performs an authenticated request against this stack's HTTP API. When
// txToken is non-empty it is echoed as the X-Tx-Token header (joining T).
func (h *callbackHarness) DoAuth(t *testing.T, method, path, body, txToken string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token(t))
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

// callback is the goroutine-safe HTTP call issued from inside a processor. It
// does not take *testing.T (it runs off the test goroutine).
func (h *callbackHarness) callback(method, path, body, txToken string) (callbackResult, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		return callbackResult{}, err
	}
	tok, _ := h.bearerVal.Load().(string)
	req.Header.Set("Authorization", "Bearer "+tok)
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

// --- HTTP API convenience (against this stack) ---

func (h *callbackHarness) readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// SetupModelWithWorkflow imports+locks a model and imports a workflow.
func (h *callbackHarness) SetupModelWithWorkflow(t *testing.T, entityName, workflowJSON string) {
	t.Helper()
	// Import model (SAMPLE_DATA converter, same sample as the other workflow tests).
	// workflowSampleModel is defined in workflow_test.go
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/model/import/JSON/SAMPLE_DATA/%s/1", entityName), workflowSampleModel, "")
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

// CreateEntity creates a primary entity via the client-facing POST (no token).
// It returns the entity id and the raw POST response (status + body) so callers
// can assert on both success and failure of the cascade.
func (h *callbackHarness) CreateEntity(t *testing.T, entityName string, modelVersion int, payload string) (entityID string, status int, body string) {
	t.Helper()
	h.token(t) // seed the cached bearer token that CreateEntityRaw reads
	res := h.CreateEntityRaw(entityName, modelVersion, payload)
	if res.err != nil {
		t.Fatalf("%v", res.err)
	}
	return res.entityID, res.status, res.body
}

// createEntityResult is the outcome of a client-facing entity POST, captured
// so it can be asserted on the test goroutine.
type createEntityResult struct {
	entityID string
	status   int
	body     string
	err      error
}

// CreateEntityRaw is the goroutine-safe form of CreateEntity: it returns the
// outcome instead of aborting the test, so callers driving the cascade from a
// goroutine can assert after joining. It reuses h.callback, which reads the
// cached bearer token seeded on the test goroutine during setup.
func (h *callbackHarness) CreateEntityRaw(entityName string, modelVersion int, payload string) createEntityResult {
	res, err := h.callback(http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion), payload, "")
	if err != nil {
		return createEntityResult{status: -1, err: fmt.Errorf("createEntity %s: %w", entityName, err)}
	}
	out := createEntityResult{status: res.StatusCode, body: res.Body}
	if out.status != http.StatusOK {
		return out
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(res.Body), &arr); err != nil || len(arr) == 0 {
		out.err = fmt.Errorf("createEntity %s: unparseable response: %s", entityName, res.Body)
		return out
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) == 0 {
		out.err = fmt.Errorf("createEntity %s: no entityIds: %s", entityName, res.Body)
		return out
	}
	out.entityID, _ = ids[0].(string)
	return out
}

// GetEntityState returns an entity's state, or "" (with the status) when the GET
// is non-200 (e.g. 404 for an entity that was rolled back).
func (h *callbackHarness) GetEntityState(t *testing.T, entityID string) (state string, status int) {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s", entityID), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var m map[string]any
	json.Unmarshal([]byte(body), &m)
	meta, _ := m["meta"].(map[string]any)
	s, _ := meta["state"].(string)
	return s, resp.StatusCode
}

// GetEntityData returns an entity's data map (fails if the GET is non-200).
func (h *callbackHarness) GetEntityData(t *testing.T, entityID string) map[string]any {
	t.Helper()
	resp := h.DoAuth(t, http.MethodGet, fmt.Sprintf("/api/entity/%s", entityID), "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getEntity %s: %d %s", entityID, resp.StatusCode, body)
	}
	var m map[string]any
	json.Unmarshal([]byte(body), &m)
	data, _ := m["data"].(map[string]any)
	return data
}

// GetSMAuditEvents retrieves an entity's StateMachine audit events from this
// stack — the callback-harness counterpart of getSMAuditEvents
// (workflow_proc_test.go), which queries the shared package-level testApp
// and therefore cannot see entities created on a callback harness's own
// separate stack.
func (h *callbackHarness) GetSMAuditEvents(t *testing.T, entityID string) []map[string]any {
	t.Helper()
	return h.getAuditEvents(t, entityID, "StateMachine")
}

// GetAllAuditEvents retrieves every audit event an entity has, of every type
// the door reports by default (StateMachine and EntityChange) — the
// unfiltered sibling of GetSMAuditEvents.
func (h *callbackHarness) GetAllAuditEvents(t *testing.T, entityID string) []map[string]any {
	t.Helper()
	return h.getAuditEvents(t, entityID, "")
}

// getAuditEvents is the shared audit-door call and decode behind
// GetSMAuditEvents and GetAllAuditEvents: eventType filters the query when
// non-empty, else the door's default set is returned.
func (h *callbackHarness) getAuditEvents(t *testing.T, entityID, eventType string) []map[string]any {
	t.Helper()
	path := fmt.Sprintf("/api/audit/entity/%s", entityID)
	if eventType != "" {
		path += "?eventType=" + eventType
	}
	resp := h.DoAuth(t, http.MethodGet, path, "", "")
	body := h.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit GET %s: expected 200, got %d: %s", entityID, resp.StatusCode, body)
	}
	var auditResp map[string]any
	json.Unmarshal([]byte(body), &auditResp)
	items, _ := auditResp["items"].([]any)
	var events []map[string]any
	for _, item := range items {
		if ev, ok := item.(map[string]any); ok {
			events = append(events, ev)
		}
	}
	return events
}

// --- compute member (real gRPC calc member) ---

// The three kinds of callout a cnode receives.
const (
	calloutProcessor = "processor"
	calloutCriterion = "criterion"
	calloutFunction  = "function"
)

// calcRequest is one calculation request as a cnode received it.
type calcRequest struct {
	kind      string // calloutProcessor | calloutCriterion | calloutFunction
	name      string // processor / criterion / function name
	requestID string // the payload's requestId, exactly as sent
	replyID   string // what the reply echoes: requestId, else the payload id
	eventID   string // the CloudEvent id
	rc        *reqCtx
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseCalcRequest decodes a calculation request of any kind. The pass rides
// as a CloudEvent attribute and lands in rc.token; it is never logged.
func (h *callbackHarness) parseCalcRequest(evtType string, ce *cepb.CloudEvent, payload []byte) calcRequest {
	var body struct {
		RequestID     string `json:"requestId"`
		ID            string `json:"id"`
		EntityID      string `json:"entityId"`
		ProcessorName string `json:"processorName"`
		ProcessorID   string `json:"processorId"`
		CriteriaName  string `json:"criteriaName"`
		CriteriaID    string `json:"criteriaId"`
		FunctionName  string `json:"functionName"`
		FunctionID    string `json:"functionId"`
		Payload       *struct {
			Data json.RawMessage `json:"data"`
			Meta map[string]any  `json:"meta"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(payload, &body)

	req := calcRequest{
		requestID: body.RequestID,
		replyID:   firstNonEmpty(body.RequestID, body.ID),
		eventID:   ce.GetId(),
	}
	switch evtType {
	case internalgrpc.EntityCriteriaCalculationRequest:
		req.kind, req.name = calloutCriterion, firstNonEmpty(body.CriteriaName, body.CriteriaID)
	case internalgrpc.EntityFunctionCalculationRequest:
		req.kind, req.name = calloutFunction, firstNonEmpty(body.FunctionName, body.FunctionID)
	default:
		req.kind, req.name = calloutProcessor, firstNonEmpty(body.ProcessorName, body.ProcessorID)
	}
	req.rc = &reqCtx{
		token:     internalgrpc.TxTokenFromCloudEvent(ce),
		requestID: req.replyID,
		entityID:  body.EntityID,
		h:         h,
	}
	if body.Payload != nil {
		req.rc.entityMeta = body.Payload.Meta
		var d map[string]any
		if json.Unmarshal(body.Payload.Data, &d) == nil {
			req.rc.entityData = d
		}
	}
	return req
}

type cnodeReplyKind int

const (
	replyOK cnodeReplyKind = iota
	replyFail
	replySilent
	replyCloseStream
	replyMalformed
)

// cnodeReply is what a cnode does with one callout.
type cnodeReply struct {
	kind       cnodeReplyKind
	data       map[string]any // replyOK, processor: the entity's new data; nil = unchanged
	matches    bool           // replyOK, criterion
	resultKind string         // replyOK, function
	result     map[string]any // replyOK, function
	message    string         // replyFail
	retryable  *bool          // replyFail: the cnode's verdict; nil = none given
	noSuccess  bool           // replyOK: leave the `success` key off the wire entirely
}

// answerOK answers success: a processor leaves the entity unchanged, a
// criterion matches. A function needs answerResult — it has no neutral answer.
func answerOK() cnodeReply                      { return cnodeReply{kind: replyOK, matches: true} }
func answerData(data map[string]any) cnodeReply { return cnodeReply{kind: replyOK, data: data} }
func answerMatches(m bool) cnodeReply           { return cnodeReply{kind: replyOK, matches: m} }
func answerResult(resultKind string, result map[string]any) cnodeReply {
	return cnodeReply{kind: replyOK, resultKind: resultKind, result: result}
}

// answerDataNoSuccessKey answers with the entity's new data and no `success`
// key at all — the shape the published schema calls a success, the field being
// optional with the default `true`.
func answerDataNoSuccessKey(data map[string]any) cnodeReply {
	return cnodeReply{kind: replyOK, data: data, noSuccess: true}
}

// answerFail answers success=false with no verdict on retrying.
func answerFail(msg string) cnodeReply { return cnodeReply{kind: replyFail, message: msg} }

// answerFailVerdict answers success=false and states whether a retry is worthwhile.
func answerFailVerdict(msg string, retryable bool) cnodeReply {
	return cnodeReply{kind: replyFail, message: msg, retryable: &retryable}
}

// neverAnswer takes the work and stays silent; the stream stays open.
func neverAnswer() cnodeReply { return cnodeReply{kind: replySilent} }

// closeStream closes the cnode's stream on receiving the work, without answering.
func closeStream() cnodeReply { return cnodeReply{kind: replyCloseStream} }

// answerMalformedPayload answers success=true with a payload that is a JSON
// string, not an object: the one failure a cnode can cause that would fail
// identically on any other cnode.
func answerMalformedPayload() cnodeReply { return cnodeReply{kind: replyMalformed} }

// cloudEvent builds the reply for req, or (nil, nil) when nothing is sent.
func (r cnodeReply) cloudEvent(req calcRequest) (*cepb.CloudEvent, error) {
	if r.kind == replySilent || r.kind == replyCloseStream {
		return nil, nil
	}
	respType := internalgrpc.EntityProcessorCalculationResponse
	switch req.kind {
	case calloutCriterion:
		respType = internalgrpc.EntityCriteriaCalculationResponse
	case calloutFunction:
		respType = internalgrpc.EntityFunctionCalculationResponse
	}
	body := map[string]any{"requestId": req.replyID, "success": r.kind == replyOK}
	if r.kind == replyFail {
		e := map[string]any{"message": r.message}
		if r.retryable != nil {
			e["retryable"] = *r.retryable
		}
		body["error"] = e
		return internalgrpc.NewCloudEvent(respType, body)
	}
	if r.kind == replyMalformed {
		body["success"] = true
		body["payload"] = "not-an-object"
		return internalgrpc.NewCloudEvent(respType, body)
	}
	switch req.kind {
	case calloutCriterion:
		body["matches"] = r.matches
	case calloutFunction:
		body["resultKind"] = r.resultKind
		body["result"] = r.result
	default:
		if r.data != nil {
			body["payload"] = map[string]any{"data": r.data}
		}
	}
	if r.noSuccess {
		delete(body, "success")
	}
	return internalgrpc.NewCloudEvent(respType, body)
}

// sendReply puts reply on the wire. A reply that cannot be built is reported
// to the server as a failure rather than dropped.
func sendReply(send func(*cepb.CloudEvent) error, req calcRequest, reply cnodeReply) {
	ce, err := reply.cloudEvent(req)
	if err != nil {
		ce, _ = answerFail(fmt.Sprintf("failed to build response: %v", err)).cloudEvent(req)
	}
	if ce != nil {
		_ = send(ce)
	}
}

// registeredReply runs the closure registered under name (RegisterProc /
// RegisterCriteria / RegisterFunction) and turns its outcome into a reply.
func (h *callbackHarness) registeredReply(kind, name string, rc *reqCtx) cnodeReply {
	switch kind {
	case calloutCriterion:
		fn, ok := h.lookupCrit(name)
		if !ok {
			return answerFail(fmt.Sprintf("no callback criterion registered for %q", name))
		}
		matches, err := fn(rc)
		if err != nil {
			return answerFail(err.Error())
		}
		return answerMatches(matches)
	case calloutFunction:
		fn, ok := h.lookupFunc(name)
		if !ok {
			return answerFail(fmt.Sprintf("no callback function registered for %q", name))
		}
		resultKind, result, err := fn(rc)
		if err != nil {
			return answerFail(err.Error())
		}
		return answerResult(resultKind, result)
	default:
		fn, ok := h.lookupProc(name)
		if !ok {
			return answerFail(fmt.Sprintf("no callback processor registered for %q", name))
		}
		data, err := fn(rc)
		if err != nil {
			return answerFail(err.Error())
		}
		return answerData(data)
	}
}

// calcHandler handles one calculation request a cnode received. It runs on a
// goroutine of its own, so it may block and must not call t.Fatal.
type calcHandler func(m *computeMember, send func(*cepb.CloudEvent) error, req calcRequest)

// memberSpec says how a cnode joins and what it does with work.
type memberSpec struct {
	bearer string   // M2M bearer to join with; "" = the harness's own tenant
	tags   []string // join tags
	handle calcHandler
}

type computeMember struct {
	id     string // member id the server gave in the greet
	conn   *grpc.ClientConn
	ctx    context.Context // ends when the cnode stops or closes its stream
	cancel context.CancelFunc
	done   chan struct{}

	// sendMu serialises stream.Send — gRPC bidi streams are not safe for
	// concurrent Send, and calc requests are handled on concurrent goroutines
	// (a depth-2 nested cascade needs the cnode to run the inner processor
	// while the outer processor's callback is still in flight).
	sendMu sync.Mutex
	// handlers tracks in-flight handlers so teardown can drain them.
	handlers sync.WaitGroup
}

// closeStream ends the cnode's stream from the client side, as a crashed or
// partitioned compute program would. The server sees the stream end and evicts
// the member; work it was given and has not answered fails as disconnected.
func (m *computeMember) closeStream() { m.cancel() }

// newComputeMember dials the stack's gRPC server, opens StartStreaming with
// spec.bearer, joins with spec.tags, waits for the greet, then runs a receive
// loop handing each calculation request to spec.handle on its own goroutine.
func newComputeMember(t *testing.T, h *callbackHarness, spec memberSpec) *computeMember {
	t.Helper()
	bearer := spec.bearer
	if bearer == "" {
		bearer = h.token(t)
	}

	conn, err := grpc.NewClient(h.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.StartStreaming(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bearer))
	if err != nil {
		cancel()
		conn.Close()
		t.Fatalf("StartStreaming: %v", err)
	}

	// joinedLegalEntityId is left empty: the server takes the tenant from the
	// bearer, so one join shape serves every tenant.
	joinCE, err := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberJoinEvent, map[string]any{
		"id":                  "callback-member-join",
		"tags":                spec.tags,
		"joinedLegalEntityId": "",
	})
	if err != nil {
		cancel()
		conn.Close()
		t.Fatalf("build join event: %v", err)
	}
	if err := stream.Send(joinCE); err != nil {
		cancel()
		conn.Close()
		t.Fatalf("send join: %v", err)
	}

	greeted := make(chan struct{})
	m := &computeMember{conn: conn, ctx: ctx, cancel: cancel, done: make(chan struct{})}

	send := func(ce *cepb.CloudEvent) error {
		m.sendMu.Lock()
		defer m.sendMu.Unlock()
		return stream.Send(ce)
	}

	go func() {
		defer close(m.done)
		var greetOnce sync.Once
		for {
			ce, err := stream.Recv()
			if err != nil {
				return // stream closed / context cancelled
			}
			evtType, payload, perr := internalgrpc.ParseCloudEvent(ce)
			if perr != nil {
				continue
			}
			switch evtType {
			case internalgrpc.CalculationMemberGreetEvent:
				greetOnce.Do(func() {
					var greet struct {
						MemberID string `json:"memberId"`
					}
					_ = json.Unmarshal(payload, &greet)
					// Written before greeted closes and before any handler
					// goroutine starts: the server writes the greet first.
					m.id = greet.MemberID
					close(greeted)
				})
			case internalgrpc.CalculationMemberKeepAliveEvent:
				ka, kerr := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberKeepAliveEvent, map[string]any{
					"id":      ce.Id,
					"success": true,
				})
				if kerr == nil {
					_ = send(ka)
				}
			case internalgrpc.EntityProcessorCalculationRequest,
				internalgrpc.EntityCriteriaCalculationRequest,
				internalgrpc.EntityFunctionCalculationRequest:
				// Handled concurrently: a handler may block on a callback that
				// drives a further callout to this same cnode.
				req := h.parseCalcRequest(evtType, ce, payload)
				m.handlers.Add(1)
				go func() {
					defer m.handlers.Done()
					spec.handle(m, send, req)
				}()
			default:
				// ignore other server events
			}
		}
	}()

	select {
	case <-greeted:
	case <-time.After(10 * time.Second):
		cancel()
		conn.Close()
		t.Fatal("compute member: timed out waiting for greet")
	}
	return m
}

func (m *computeMember) stop() {
	m.cancel()
	m.conn.Close()
	select {
	case <-m.done:
		// The receive loop has exited, so no further handlers.Add can race the
		// Wait below. Drain any in-flight concurrent calc handlers (their Sends
		// now no-op on the closed stream) so none outlives the test.
		drained := make(chan struct{})
		go func() { m.handlers.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
		}
	case <-time.After(5 * time.Second):
		// Receive loop hung (already a failing test); skip the drain rather than
		// race handlers.Add against Wait.
	}
}

// cloneData returns a shallow copy of an entity data map (nil-safe), so a
// processor can extend the attached primary data without mutating the snapshot.
func cloneData(src map[string]any) map[string]any {
	out := make(map[string]any, len(src)+2)
	for k, v := range src {
		out[k] = v
	}
	return out
}

// entityDataField extracts data.<field> (as a string) from an entity envelope
// response body (`{"meta":{...},"data":{...}}`). Returns "" when absent or when
// the body is not a 200 entity envelope (e.g. a 404 error body).
func entityDataField(body, field string) string {
	var env struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	s, _ := env.Data[field].(string)
	return s
}
