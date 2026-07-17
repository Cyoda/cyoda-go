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
// for feature #287 (compute-node callbacks join the originating transaction).
//
// Unlike the localproc in-process ExternalProcessingService used by the other
// workflow E2E tests, this harness stands up a SEPARATE full cyoda-go stack
// (real Postgres via the shared testcontainer, HTTP + gRPC) whose workflow
// engine dispatches processors over the REAL gRPC bidi stream. A real gRPC
// calculation member connects, receives EntityProcessorCalculationRequests
// carrying the signed cyodatxtoken CloudEvent attribute, echoes that token as
// the X-Tx-Token header on HTTP callbacks into the same node, and thereby
// exercises the JoinFromToken -> participate path end-to-end.
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

// callbackHarness is a full HTTP+gRPC cyoda-go stack (real Postgres) with a
// connected gRPC compute member. Reused across the #287 callback E2E tests.
type callbackHarness struct {
	app     *app.App
	baseURL string // e.g. http://127.0.0.1:PORT
	member  *computeMember

	mu    sync.Mutex
	procs map[string]callbackProc
	crits map[string]callbackCrit

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
	// real gRPC ProcessorDispatcher, which mints and attaches the cyodatxtoken.

	// Discover the HTTP port before constructing the app (the JWKS validator URL
	// is built from cfg.HTTPPort and must match the live server).
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	h := &callbackHarness{baseURL: srv.URL, procs: map[string]callbackProc{}, crits: map[string]callbackCrit{}}
	t.Cleanup(srv.Close)

	srvPort := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg.HTTPPort = srvPort

	// gRPC listener for the calc-member stream.
	grpcLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grpc listen: %v", err)
	}
	cfg.GRPC.Port = grpcLis.Addr().(*net.TCPAddr).Port

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

	// Connect the compute member and wait for it to be ready.
	h.member = newComputeMember(t, h, grpcLis.Addr().String())
	t.Cleanup(h.member.stop)

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
	resp := h.DoAuth(t, http.MethodPost, fmt.Sprintf("/api/entity/JSON/%s/%d", entityName, modelVersion), payload, "")
	body = h.readBody(t, resp)
	status = resp.StatusCode
	if status != http.StatusOK {
		return "", status, body
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil || len(arr) == 0 {
		t.Fatalf("createEntity %s: unparseable response: %s", entityName, body)
	}
	ids, _ := arr[0]["entityIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("createEntity %s: no entityIds: %s", entityName, body)
	}
	entityID, _ = ids[0].(string)
	return entityID, status, body
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

// --- compute member (real gRPC calc member) ---

type computeMember struct {
	conn   *grpc.ClientConn
	cancel context.CancelFunc
	done   chan struct{}

	// sendMu serialises stream.Send — gRPC bidi streams are not safe for
	// concurrent Send, and calc requests are now dispatched to concurrent
	// handler goroutines (mirroring a real compute node's thread pool, which a
	// depth-2 nested cascade requires: the member must run the inner processor
	// while the outer processor's callback is still in flight).
	sendMu sync.Mutex
	// handlers tracks in-flight concurrent calc handlers so teardown can drain
	// them before closing the connection.
	handlers sync.WaitGroup
}

// newComputeMember dials the stack's gRPC server, opens StartStreaming with an
// M2M bearer, joins, waits for the greet, then runs a receive loop dispatching
// EntityProcessorCalculationRequests to the harness's registered processors.
func newComputeMember(t *testing.T, h *callbackHarness, grpcAddr string) *computeMember {
	t.Helper()

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}
	client := cyodapb.NewCloudEventsServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+h.token(t))

	stream, err := client.StartStreaming(ctx)
	if err != nil {
		cancel()
		conn.Close()
		t.Fatalf("StartStreaming: %v", err)
	}

	// Send the join event.
	joinCE, err := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberJoinEvent, map[string]any{
		"id":                  "callback-member-join",
		"tags":                []string{},
		"joinedLegalEntityId": "test-tenant",
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

	// Wait for the greet (proves registration completed) before returning.
	greeted := make(chan struct{})
	m := &computeMember{conn: conn, cancel: cancel, done: make(chan struct{})}

	// send serialises all outbound frames (keep-alive replies + concurrent calc
	// responses) so no two goroutines call stream.Send at once.
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
				greetOnce.Do(func() { close(greeted) })
			case internalgrpc.CalculationMemberKeepAliveEvent:
				// Reply so the server refreshes our LastSeen (avoids keep-alive timeout).
				ka, kerr := internalgrpc.NewCloudEvent(internalgrpc.CalculationMemberKeepAliveEvent, map[string]any{
					"id":      ce.Id,
					"success": true,
				})
				if kerr == nil {
					_ = send(ka)
				}
			case internalgrpc.EntityProcessorCalculationRequest:
				// Dispatch concurrently: a processor callback may block on an HTTP
				// call that drives a further dispatch to this same member (depth-2
				// cascade). Handling inline on the receive loop would deadlock the
				// member before the txgate is ever exercised.
				m.handlers.Add(1)
				go func(ce *cepb.CloudEvent, payload []byte) {
					defer m.handlers.Done()
					h.handleCalcRequest(send, ce, payload)
				}(ce, payload)
			case internalgrpc.EntityCriteriaCalculationRequest:
				// Same concurrency rationale as processors: a FUNCTION criterion
				// may block on a joined callback that drives a further dispatch.
				m.handlers.Add(1)
				go func(ce *cepb.CloudEvent, payload []byte) {
					defer m.handlers.Done()
					h.handleCriteriaRequest(send, ce, payload)
				}(ce, payload)
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

// handleCalcRequest runs the registered processor for an inbound calc request
// and replies with an EntityProcessorCalculationResponse. It is dispatched on a
// per-request goroutine; send serialises the reply against other concurrent
// handlers and the receive loop's keep-alive replies.
func (h *callbackHarness) handleCalcRequest(send func(*cepb.CloudEvent) error, ce *cepb.CloudEvent, payload []byte) {
	var req struct {
		RequestID     string `json:"requestId"`
		ID            string `json:"id"`
		EntityID      string `json:"entityId"`
		ProcessorName string `json:"processorName"`
		ProcessorID   string `json:"processorId"`
		Payload       *struct {
			Data json.RawMessage `json:"data"`
			Meta map[string]any  `json:"meta"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(payload, &req)
	reqID := req.RequestID
	if reqID == "" {
		reqID = req.ID
	}
	procName := req.ProcessorName
	if procName == "" {
		procName = req.ProcessorID
	}

	sendErr := func(msg string) {
		resp, _ := internalgrpc.NewCloudEvent(internalgrpc.EntityProcessorCalculationResponse, map[string]any{
			"requestId": reqID,
			"success":   false,
			"error":     map[string]any{"message": msg},
		})
		_ = send(resp)
	}

	fn, ok := h.lookupProc(procName)
	if !ok {
		sendErr(fmt.Sprintf("no callback processor registered for %q", procName))
		return
	}

	rc := &reqCtx{
		token:     internalgrpc.TxTokenFromCloudEvent(ce),
		requestID: reqID,
		entityID:  req.EntityID,
		h:         h,
	}
	if req.Payload != nil {
		rc.entityMeta = req.Payload.Meta
		var d map[string]any
		if json.Unmarshal(req.Payload.Data, &d) == nil {
			rc.entityData = d
		}
	}

	applyData, procErr := fn(rc)
	if procErr != nil {
		sendErr(procErr.Error())
		return
	}

	respPayload := map[string]any{
		"requestId": reqID,
		"success":   true,
	}
	if applyData != nil {
		respPayload["payload"] = map[string]any{"data": applyData}
	}
	resp, err := internalgrpc.NewCloudEvent(internalgrpc.EntityProcessorCalculationResponse, respPayload)
	if err != nil {
		sendErr(fmt.Sprintf("failed to build response: %v", err))
		return
	}
	_ = send(resp)
}

// handleCriteriaRequest runs the registered FUNCTION criterion for an inbound
// criteria calc request and replies with an EntityCriteriaCalculationResponse
// (success + matches). Dispatched on a per-request goroutine; send serialises
// the reply.
func (h *callbackHarness) handleCriteriaRequest(send func(*cepb.CloudEvent) error, ce *cepb.CloudEvent, payload []byte) {
	var req struct {
		RequestID    string `json:"requestId"`
		ID           string `json:"id"`
		EntityID     string `json:"entityId"`
		CriteriaName string `json:"criteriaName"`
		CriteriaID   string `json:"criteriaId"`
		Payload      *struct {
			Data json.RawMessage `json:"data"`
			Meta map[string]any  `json:"meta"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(payload, &req)
	reqID := req.RequestID
	if reqID == "" {
		reqID = req.ID
	}
	critName := req.CriteriaName
	if critName == "" {
		critName = req.CriteriaID
	}

	sendErr := func(msg string) {
		resp, _ := internalgrpc.NewCloudEvent(internalgrpc.EntityCriteriaCalculationResponse, map[string]any{
			"requestId": reqID,
			"success":   false,
			"error":     map[string]any{"message": msg},
		})
		_ = send(resp)
	}

	fn, ok := h.lookupCrit(critName)
	if !ok {
		sendErr(fmt.Sprintf("no callback criterion registered for %q", critName))
		return
	}

	rc := &reqCtx{
		token:     internalgrpc.TxTokenFromCloudEvent(ce),
		requestID: reqID,
		entityID:  req.EntityID,
		h:         h,
	}
	if req.Payload != nil {
		rc.entityMeta = req.Payload.Meta
		var d map[string]any
		if json.Unmarshal(req.Payload.Data, &d) == nil {
			rc.entityData = d
		}
	}

	matches, critErr := fn(rc)
	if critErr != nil {
		sendErr(critErr.Error())
		return
	}
	resp, err := internalgrpc.NewCloudEvent(internalgrpc.EntityCriteriaCalculationResponse, map[string]any{
		"requestId": reqID,
		"success":   true,
		"matches":   matches,
	})
	if err != nil {
		sendErr(fmt.Sprintf("failed to build response: %v", err))
		return
	}
	_ = send(resp)
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
