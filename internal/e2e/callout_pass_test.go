package e2e_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
)

// The exact literal texts the code gives for a cross-tenant pass and for a
// pass naming a transaction that has ended, read verbatim from
// internal/domain/txjoin/txjoin.go's error mapping. common.Operational's
// Message is "<code>: <text>", and WriteError puts that string straight into
// the HTTP problem document's detail and into the gRPC envelope's
// Error.Message — one literal serves both doors.
const (
	forbiddenTenantDetail  = "FORBIDDEN: transaction belongs to a different tenant"
	endedTransactionDetail = "TRANSACTION_NOT_FOUND: transaction not found or no longer active"
)

// passClaims decodes the claims of a recorded pass. The claims are not secret;
// the signature is what protects them.
func passClaims(t *testing.T, pass string) token.Claims {
	t.Helper()
	head, _, ok := strings.Cut(pass, ".")
	if !ok {
		t.Fatal("recorded pass has no signature part")
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		t.Fatalf("decode pass claims: %v", err)
	}
	var c token.Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal pass claims: %v", err)
	}
	return c
}

// tamperSignature returns a COPY of pass with one byte of its signature part
// flipped to a different valid base64url character, so the HMAC no longer
// verifies but the pass still parses structurally. The original pass is
// untouched; the tampered copy is never logged or printed.
func tamperSignature(pass string) string {
	dot := strings.LastIndexByte(pass, '.')
	if dot < 0 || dot+1 >= len(pass) {
		return pass + "A"
	}
	b := []byte(pass)
	if b[dot+1] == 'A' {
		b[dot+1] = 'B'
	} else {
		b[dot+1] = 'A'
	}
	return string(b)
}

// TestCalloutPass (shape F-ended): with processor B's callout in progress and
// processor A's ended, on one open transaction.
func TestCalloutPass(t *testing.T) {
	h := newCalloutHarness(t, calloutTuning(3, 100*time.Millisecond))
	_ = h.token(t)
	const model, secondary, tagA, tagB = "s10-pass", "s10-pass-secondary", "s10-pass-a", "s10-pass-b"
	h.SetupModelWithWorkflow(t, secondary, secondaryWorkflow)
	h.SetupModelWithWorkflow(t, model, chainWorkflowJSON("s10-pass-wf",
		procSpec{"s10-proc-a", "SYNC", map[string]any{"calculationNodesTags": tagA}},
		procSpec{"s10-proc-b", "SYNC", map[string]any{"calculationNodesTags": tagB}}))

	release := make(chan struct{})
	releaseNow := closeOnce(release)
	t.Cleanup(releaseNow)
	a := h.AttachCnode(t, cnodeSpec{name: "a", tags: []string{tagA}})
	b := h.AttachCnode(t, cnodeSpec{name: "b", tags: []string{tagB}, script: scriptHold(release, answerOK())})

	done := make(chan createEntityResult, 1)
	go func() { done <- h.CreateEntityRaw(model, 1, workflowSampleModel) }()
	live := awaitCnodeReceived(t, b, 1, 15*time.Second)[0]
	ended := a.Received()[0]
	claims := passClaims(t, live.Pass())
	if claims.Callout == "" || claims.Major == 0 {
		t.Fatalf("a minted pass carries callout=%q major=%d; want the callout's request id and a number", claims.Callout, claims.Major)
	}
	if claims.Callout != live.RequestID {
		t.Errorf("pass callout = %q; want the callout's request id %q", claims.Callout, live.RequestID)
	}

	t.Run("no-callout-and-number-401", func(t *testing.T) {
		bare := token.Claims{NodeID: claims.NodeID, TxRef: claims.TxRef, ExpiresAt: claims.ExpiresAt}
		pass, err := h.app.TokenSigner().Issue(bare)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		assertRefusedOnAllDoors(t, h, pass, secondary, live.EntityID, http.StatusUnauthorized, "UNAUTHORIZED", "invalid transaction token", pass, claims.NodeID)
	})

	t.Run("expired-410", func(t *testing.T) {
		old := claims
		old.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		pass, err := h.app.TokenSigner().Issue(old)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		assertRefusedOnAllDoors(t, h, pass, secondary, live.EntityID, http.StatusGone, "TRANSACTION_EXPIRED", "", pass, claims.NodeID)
	})

	t.Run("tampered-signature-401", func(t *testing.T) {
		tampered := tamperSignature(live.Pass())
		assertRefusedOnAllDoors(t, h, tampered, secondary, live.EntityID, http.StatusUnauthorized, "UNAUTHORIZED", "invalid transaction token", tampered, claims.NodeID)
	})

	t.Run("malformed-structurally-401", func(t *testing.T) {
		const garbage = "not-a-real-pass-no-separator-at-all"
		assertRefusedOnAllDoors(t, h, garbage, secondary, live.EntityID, http.StatusUnauthorized, "UNAUTHORIZED", "invalid transaction token", claims.NodeID)
	})

	var bearerB string
	t.Run("another-tenant-403", func(t *testing.T) {
		clientB, secretB := h.provisionTenant(t, "s10-tenant-b", "s10-user-b")
		bearerB = h.fetchTokenFor(t, clientB, secretB)
		// Tenant B gets its own copy of the target model, so an entity count
		// in B's space means something (an endpoint erroring on an unknown
		// model would otherwise report "0" for the wrong reason).
		h.setupModelWithWorkflowAs(t, bearerB, secondary, secondaryWorkflow)

		// A current pass and a superseded one: another tenant must be told
		// the SAME nothing by both — never CALLOUT_SUPERSEDED, which would
		// leak that a callout used to exist. Each pass is its own subtest, so
		// the first pass failing does not hide the second, and no failure
		// message ever prints a pass.
		results := map[string]map[string]doorResult{}
		for _, tc := range []struct{ name, pass string }{
			{"current pass", live.Pass()},
			{"ended pass", ended.Pass()},
		} {
			pass := tc.pass
			t.Run(tc.name, func(t *testing.T) {
				results[tc.name] = assertRefusedOnAllDoorsAs(t, h, bearerB, pass, secondary, live.EntityID,
					http.StatusForbidden, "FORBIDDEN", forbiddenTenantDetail, pass, claims.NodeID)
			})
		}

		// The oracle claim, asserted rather than merely reported: on every
		// door, the current pass and the ended pass produce an IDENTICAL
		// refusal (status, errorCode, retryable, detail) — tenant B learns
		// nothing more from a superseded pass than from a live one.
		for _, door := range []string{"http-write", "http-read", "grpc-write", "grpc-read", "grpc-stream-write", "grpc-stream-read"} {
			cur, end := results["current pass"][door], results["ended pass"][door]
			if cur != end {
				t.Errorf("%s: the current pass and the ended pass are not answered identically: %+v vs %+v", door, cur, end)
			}
		}

		if n := h.countEntitiesAs(t, bearerB, secondary); n != 0 {
			t.Errorf("%d entities in tenant B's own space after the refused writes; want 0", n)
		}
	})

	releaseNow()
	if res := awaitCreate(t, done, 15*time.Second); res.status != http.StatusOK {
		t.Fatalf("create: %d %s; want 200 — none of the refused passes touched the operation", res.status, res.body)
	}
	if n := h.countEntities(t, secondary); n != 0 {
		t.Errorf("%d secondary entities committed; want 0", n)
	}

	// The transaction the passes above name has now ended (committed).
	// FINDING (Minor, per review; carried to the security-audit list):
	// presenting the SAME live pass that got 403 while the transaction was
	// open now gets 404 — tenant B can therefore tell "still open" from
	// "ended" for a transaction it already holds a signed pass for. This is
	// not an enumeration or minting capability: B cannot learn of, or mint a
	// pass for, a transaction it was never given one for; the pass's own
	// claims already name the transaction before B presents it either time.
	t.Run("another-tenant-transaction-ended-404", func(t *testing.T) {
		results := map[string]map[string]doorResult{}
		for _, tc := range []struct{ name, pass string }{
			{"current pass", live.Pass()},
			{"ended pass", ended.Pass()},
		} {
			pass := tc.pass
			t.Run(tc.name, func(t *testing.T) {
				results[tc.name] = assertRefusedOnAllDoorsAs(t, h, bearerB, pass, secondary, live.EntityID,
					http.StatusNotFound, "TRANSACTION_NOT_FOUND", endedTransactionDetail, pass, claims.NodeID)
			})
		}
		for _, door := range []string{"http-write", "http-read", "grpc-write", "grpc-read", "grpc-stream-write", "grpc-stream-read"} {
			cur, end := results["current pass"][door], results["ended pass"][door]
			if cur != end {
				t.Errorf("%s: the two passes are not answered identically once the transaction has ended: %+v vs %+v", door, cur, end)
			}
		}
		if n := h.countEntitiesAs(t, bearerB, secondary); n != 0 {
			t.Errorf("%d entities in tenant B's own space after the refused (ended-transaction) requests; want 0", n)
		}
	})
}
