package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	internalapi "github.com/cyoda-platform/cyoda-go/internal/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/entity"
	"github.com/cyoda-platform/cyoda-go/internal/e2e/openapivalidator"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// ---------------------------------------------------------------------------
// Batched delete that never converges -> 409 DELETE_NOT_CONVERGED over HTTP
// ---------------------------------------------------------------------------
//
// The streamed batched delete (transactionSize set, no pointInTime) re-selects
// the matching entities before every batch and finishes when a pass finds
// nothing left. Under sustained concurrent inserts that pass never comes up
// empty, so the loop is capped at a fixed number of selection cycles and fails
// closed: 409 DELETE_NOT_CONVERGED, retryable, with the batches that already
// committed left deleted.
//
// Reaching that cap deterministically means lowering it — the production
// default is sized to be unreachable by any converging delete, and racing a
// real insert storm against it would be neither fast nor reliable. The
// handler's cycle bound is the seam (entity.Handler.WithMaxDeleteCycles); the
// storm shape itself is pinned at domain level by
// internal/domain/entity/delete_progress_guard_test.go.
//
// ISOLATED single-backend (Postgres) e2e, deliberately NOT a parity scenario:
// per .claude/rules/test-coverage.md, concurrency/storm shapes stay out of the
// shared parity suite.

// newLowBudgetDeleteServer mounts the real generated router over an
// entity.Handler built from the running e2e app's own Postgres-backed
// collaborators, with its streamed-delete cycle budget lowered to maxCycles.
// The app wires its own handler with the production budget and exposes no
// accessor to reach it, so the endpoint is re-mounted here rather than
// configured.
//
// The mount mirrors the main suite's: the same generated chi router and
// binding-error handler the app uses, under the same "/api" context path, and
// behind the same OpenAPI conformance validator — so this test's responses are
// checked against api/openapi.yaml and recorded in the error-code matrix
// exactly like the shared server's. Only the auth middleware is replaced (by a
// fixed tenant identity), because the handler under test is the delete, not
// the token path.
func newLowBudgetDeleteServer(t *testing.T, maxCycles int) string {
	t.Helper()

	h := entity.New(
		testApp.StoreFactory(),
		testApp.TransactionManager(),
		common.NewDefaultUUIDGenerator(),
		testApp.WorkflowEngine(),
		txgate.New(),
	).WithMaxDeleteCycles(maxCycles)

	apiServer := internalapi.NewServer()
	apiServer.Entity = h
	apiHandler := genapi.HandlerWithOptions(apiServer, genapi.StdHTTPServerOptions{
		BaseRouter:       internalapi.NewChiMux(),
		ErrorHandlerFunc: internalapi.BindingErrorHandler,
	})

	uc := spi.GetUserContext(intxTenantCtx())
	withTenant := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHandler.ServeHTTP(w, r.WithContext(spi.WithUserContext(r.Context(), uc)))
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", withTenant))

	swagger, err := genapi.GetSwagger()
	if err != nil {
		t.Fatalf("get swagger: %v", err)
	}
	swagger.Servers = openapi3.Servers{{URL: "/api"}}
	validator, err := openapivalidator.NewValidator(swagger)
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}

	srv := httptest.NewServer(openapivalidator.NewMiddleware(validator)(mux))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestDeleteEntities_Batched_NonConvergence_409 pins the HTTP contract: the
// capped batched delete answers 409 DELETE_NOT_CONVERGED, advertises itself
// retryable, and leaves the batch it did commit durably deleted — never a 200
// whose counts describe only the part of the work that fit inside the cap.
func TestDeleteEntities_Batched_NonConvergence_409(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: requires Docker + PostgreSQL")
	}
	const model = "e2e-delcond-noconverge"
	importModelWithSample(t, model, 1, `{"status":"sample","n":0}`)
	lockModelE2E(t, model, 1)

	ids := []string{
		createEntityE2E(t, model, 1, `{"status":"drop","n":1}`),
		createEntityE2E(t, model, 1, `{"status":"drop","n":2}`),
		createEntityE2E(t, model, 1, `{"status":"drop","n":3}`),
	}

	// Budget 1: the first selection cycle deletes one entity, the next
	// re-scan still matches the other two, and the guard trips — the same
	// state a genuine insert storm holds the loop in.
	baseURL := newLowBudgetDeleteServer(t, 1)

	req, err := e2eNewRequest(t, http.MethodDelete,
		fmt.Sprintf("%s/api/entity/%s/1?transactionSize=1", baseURL, model),
		strings.NewReader(`{"type":"simple","jsonPath":"$.status","operatorType":"EQUALS","value":"drop"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", resp.StatusCode, body)
	}
	var pd struct {
		Detail string         `json:"detail"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body=%s", err, body)
	}
	if code, _ := pd.Props["errorCode"].(string); code != common.ErrCodeDeleteNotConverged {
		t.Errorf("errorCode = %q, want %s; body=%s", code, common.ErrCodeDeleteNotConverged, body)
	}
	if retryable, _ := pd.Props["retryable"].(bool); !retryable {
		t.Errorf("%s not advertised retryable; body=%s", common.ErrCodeDeleteNotConverged, body)
	}
	// The detail must name the cause and the remedies — an operator reading
	// only this line has to know what to do next.
	for _, want := range []string{"did not converge", "narrow the condition", "retry"} {
		if !strings.Contains(pd.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", pd.Detail, want)
		}
	}

	// Fail closed applies to the RESPONSE, not to work already committed: the
	// one batch that ran stays deleted.
	deleted := 0
	for _, id := range ids {
		r := doAuth(t, http.MethodGet, "/api/entity/"+id, "")
		status, getBody := r.StatusCode, readBody(t, r)
		switch status {
		case http.StatusNotFound:
			deleted++
		case http.StatusOK:
		default:
			t.Fatalf("GET entity %s: unexpected status %d: %s", id, status, getBody)
		}
	}
	if deleted != 1 {
		t.Errorf("%d of %d entities deleted, want exactly 1 (the single batch that committed before the cap)", deleted, len(ids))
	}
}
