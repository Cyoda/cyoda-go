package e2e_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

// TestAuditEvents_TxTokenRefusal_410 sends a transaction routing token to an
// operation the published contract declares no `default` response for, and
// proves the join layer's refusal is a status that operation declares.
//
// searchEntityAuditEvents is one such operation, and one a compute member can
// genuinely call back into: a callout reading the audit trail of the entity it
// was handed. It runs on the suite's shared stack, which is the one behind the
// conformance validator — an undeclared status here is not absorbed by a
// `default` response, it is a recorded mismatch that fails
// TestOpenAPIConformanceReport.
//
// The token is HMAC-valid (minted by the server's own signer) but past its
// expiry, so the join layer refuses it in Verify, before the handler runs and
// without reaching the transaction. The token value is never logged.
func TestAuditEvents_TxTokenRefusal_410(t *testing.T) {
	signer := testApp.TokenSigner()
	if signer == nil {
		t.Fatal("testApp has no transaction token signer")
	}

	txRef := "audit-tx-" + randSuffix(t)
	tok, err := signer.Issue(token.Claims{
		NodeID:    "local",
		TxRef:     txRef,
		ExpiresAt: time.Now().Add(-10 * time.Second).Unix(),
		Callout:   "req-" + txRef,
		Major:     1,
	})
	if err != nil {
		t.Fatalf("issue transaction token: %v", err)
	}

	req := authRequest(t, http.MethodGet, "/api/audit/entity/"+uuid.NewString(), nil)
	req.Header.Set("X-Tx-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET audit events failed: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(raw)

	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d; want 410 (body: %s)", resp.StatusCode, body)
	}
	if code := problemErrorCode(body); code != "TRANSACTION_EXPIRED" {
		t.Fatalf("errorCode = %q; want TRANSACTION_EXPIRED (body: %s)", code, body)
	}
}

// TestWorkflowFinishedEvent_RepeatedTxTokenHeader_400 proves the status that
// declaring the header made reachable.
//
// A header parameter the contract declares is one the generated router binds,
// and binding rejects a header sent more than once —
// TooManyValuesForParamError, which BindingErrorHandler renders as a 400
// BAD_REQUEST. getStateMachineFinishedEvent declared neither 400 nor a
// `default`, so that 400 was undeclared on it, and the conformance validator
// records an undeclared status as a mismatch.
//
// The first copy of the header is empty, which is how the request reaches the
// binding at all: the join layer reads the first value, finds it empty, and
// treats the request as unjoined, so the duplicate is answered by the
// generated router rather than refused earlier as a bad pass. The second copy
// carries a real token shape, so this is a genuinely duplicated header and not
// an empty-header case.
func TestWorkflowFinishedEvent_RepeatedTxTokenHeader_400(t *testing.T) {
	signer := testApp.TokenSigner()
	if signer == nil {
		t.Fatal("testApp has no transaction token signer")
	}

	txRef := "finished-tx-" + randSuffix(t)
	tok, err := signer.Issue(token.Claims{
		NodeID:    "local",
		TxRef:     txRef,
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
		Callout:   "req-" + txRef,
		Major:     1,
	})
	if err != nil {
		t.Fatalf("issue transaction token: %v", err)
	}

	path := "/api/audit/entity/" + uuid.NewString() + "/workflow/" + uuid.NewString() + "/finished"
	req := authRequest(t, http.MethodGet, path, nil)
	req.Header.Add("X-Tx-Token", "")
	req.Header.Add("X-Tx-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET workflow finished event failed: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(raw)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body: %s)", resp.StatusCode, body)
	}
	if code := problemErrorCode(body); code != "BAD_REQUEST" {
		t.Fatalf("errorCode = %q; want BAD_REQUEST (body: %s)", code, body)
	}
}
