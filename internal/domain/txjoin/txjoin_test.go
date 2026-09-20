package txjoin

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// fakeTM satisfies spi.TransactionManager by embedding the interface and
// overriding only Join. Unimplemented methods panic if unexpectedly called.
type fakeTM struct {
	spi.TransactionManager
	joinErr error
}

func (f fakeTM) Join(ctx context.Context, txID string) (context.Context, error) {
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: txID}), nil
}

// make32 returns a deterministic 32-byte HMAC secret for tests.
func make32(t *testing.T) []byte {
	t.Helper()
	return []byte("test-secret-key-at-least-32byte!")
}

// noCalloutFence returns a fence that knows no callout: every pass is refused.
func noCalloutFence() *fence.Fence { return fence.New(txgate.New()) }

// liveFence returns a fence on which calloutID is in progress on txID at
// major 1, and the pass claims that name it.
func liveFence(t *testing.T, calloutID, txID string) (*fence.Fence, token.Claims) {
	t.Helper()
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), calloutID, txID, nil)
	t.Cleanup(end)
	f.Advance(calloutID, 1)
	return f, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
}

func assertAppErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v (%T); want *common.AppError", err, err)
	}
	if appErr.Status != status || appErr.Code != code {
		t.Fatalf("err = %d %s; want %d %s", appErr.Status, appErr.Code, status, code)
	}
}

func TestJoinFromToken_EmptyPassThrough(t *testing.T) {
	ctx := context.Background()
	got, err := JoinFromToken(ctx, nil, fakeTM{}, noCalloutFence(), "")
	if err != nil {
		t.Fatalf("empty token must not error; err=%v", err)
	}
	if spi.GetTransaction(got) != nil {
		t.Fatal("empty token must not inject a transaction into ctx")
	}
	if got != ctx {
		t.Fatal("empty token must return the original ctx unchanged")
	}
}

func TestJoinFromToken_JoinsValid(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	f, claims := liveFence(t, "req-tx-1", "tx-1")
	tok, err := s.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ctx, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tx := spi.GetTransaction(ctx)
	if tx == nil || tx.ID != "tx-1" {
		t.Fatalf("expected joined tx tx-1, got %+v", tx)
	}
}

func TestJoinFromToken_ExpiredMaps410(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-exp", ExpiresAt: time.Now().Add(-time.Second).Unix(), Callout: "req-tx-exp", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d", op.Status)
	}
	if op.Code != common.ErrCodeTransactionExpired {
		t.Fatalf("expected TRANSACTION_EXPIRED, got %q", op.Code)
	}
}

func TestJoinFromToken_ForgedMaps401(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Issue with one signer, verify with a different signer → tampered.
	s2, err := token.NewSigner([]byte("different-secret-key-at-least-32b!"))
	if err != nil {
		t.Fatalf("NewSigner s2: %v", err)
	}
	tok, err := s2.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", op.Status)
	}
	if op.Code != common.ErrCodeUnauthorized {
		t.Fatalf("expected UNAUTHORIZED, got %q", op.Code)
	}
}

func TestJoinFromToken_NotFoundMaps404(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxNotFound}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", op.Status)
	}
	if op.Code != common.ErrCodeTransactionNotFound {
		t.Fatalf("expected TRANSACTION_NOT_FOUND, got %q", op.Code)
	}
}

func TestJoinFromToken_RolledBackMaps404(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxRolledBack}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", op.Status)
	}
	if op.Code != common.ErrCodeTransactionNotFound {
		t.Fatalf("expected TRANSACTION_NOT_FOUND, got %q", op.Code)
	}
}

func TestJoinFromToken_AlreadyCommittedMaps404(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxAlreadyCommitted}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", op.Status)
	}
	if op.Code != common.ErrCodeTransactionNotFound {
		t.Fatalf("expected TRANSACTION_NOT_FOUND, got %q", op.Code)
	}
}

func TestJoinFromToken_UnknownJoinErrorMaps5xx(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	unknownErr := errors.New("db unavailable")
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: unknownErr}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error, got %d", op.Status)
	}
}

func TestJoinFromToken_TenantMismatchMaps403(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, noCalloutFence(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden, got %d", op.Status)
	}
	if op.Code != common.ErrCodeForbidden {
		t.Fatalf("expected FORBIDDEN, got %q", op.Code)
	}
}

func TestJoinFromToken_AdmitsUnderTheCurrentNumber(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	ctx, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx := spi.GetTransaction(ctx); tx == nil || tx.ID != "tx-1" {
		t.Fatalf("expected joined tx-1, got %+v", tx)
	}
	if got := fence.Pairs(ctx); len(got) != 1 || got[0] != (fence.Pair{Callout: "req-1", Major: 1}) {
		t.Fatalf("Pairs = %v", got)
	}
}

func TestJoinFromToken_CalloutEnded_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), "req-1", "tx-1", nil)
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	base := context.Background()
	ctx, err := JoinFromToken(base, s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
	if ctx != base {
		t.Fatal("a refused join must hand back the caller's context")
	}
}

func TestJoinFromToken_ReplacedWhileCalloutInProgress_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	f.Advance("req-1", 2)
	_, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
}

func TestJoinFromToken_EnclosingCalloutNotCurrent_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-inner", "tx-1")
	claims.Outer = []token.Pair{{Callout: "req-outer", Major: 1}} // never begun
	tok, _ := s.Issue(claims)
	_, err := JoinFromToken(context.Background(), s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
}

// The transaction is looked up, and its tenant checked, BEFORE the fence is
// consulted.
func TestJoinFromToken_JoinComesBeforeTheFence(t *testing.T) {
	tests := []struct {
		name    string
		joinErr error
		status  int
		code    string
	}{
		{"TxGoneBeatsCalloutEnded_404", spi.ErrTxNotFound, http.StatusNotFound, common.ErrCodeTransactionNotFound},
		{"TenantMismatchBeatsTheFence_403", spi.ErrTxTenantMismatch, http.StatusForbidden, common.ErrCodeForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			// Callout ended: the fence alone would answer 410.
			f := fence.New(txgate.New())
			tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
			_, err := JoinFromToken(context.Background(), s, fakeTM{joinErr: tc.joinErr}, f, tok)
			assertAppErr(t, err, tc.status, tc.code)
		})
	}
}

// A pass presented by the wrong tenant is answered 403 whatever the fence knows
// about the callout it names — current, superseded or never registered — so a
// stolen pass tells another tenant nothing about which callouts exist.
func TestJoinFromToken_TenantMismatchIsOneAnswerWhateverTheFenceKnows(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) (*fence.Fence, token.Claims){
		"callout current": func(t *testing.T) (*fence.Fence, token.Claims) {
			return liveFence(t, "req-1", "tx-1")
		},
		"callout superseded": func(t *testing.T) (*fence.Fence, token.Claims) {
			f, claims := liveFence(t, "req-1", "tx-1")
			f.Advance("req-1", 2)
			return f, claims
		},
		"callout unknown": func(t *testing.T) (*fence.Fence, token.Claims) {
			_, claims := liveFence(t, "req-1", "tx-1")
			return noCalloutFence(), claims
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, claims := setup(t)
			tok, _ := s.Issue(claims)
			_, err := JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, f, tok)
			assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
		})
	}
}

// A stolen pass absorbs nothing: another tenant presenting a higher minor must
// not shut the rightful cnode out.
func TestJoinFromToken_TenantMismatch_AbsorbsNothing(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	rightful, err := f.Admit(context.Background(), []fence.Pair{{Callout: "req-1", Major: 1, Minor: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	claims.Minor = 9
	tok, _ := s.Issue(claims)
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, f, tok)
	assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
	if err := fence.Check(rightful); err != nil {
		t.Fatalf("a refused join changed the fence: %v", err)
	}
}

func TestJoinFromToken_NoCalloutAndNumber_401(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	_, err := JoinFromToken(context.Background(), s, fakeTM{}, fence.New(txgate.New()), tok)
	assertAppErr(t, err, http.StatusUnauthorized, common.ErrCodeUnauthorized)
	var appErr *common.AppError
	_ = errors.As(err, &appErr)
	if appErr.Message != "UNAUTHORIZED: invalid transaction token" {
		t.Fatalf("message = %q", appErr.Message)
	}
}

// A cnode that disconnects in the middle of its callback cancels the request's
// context; the joined request must not see it.
func TestJoinFromToken_JoinedContextIsNotCancelledByItsClient(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveFence(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	request, disconnect := context.WithCancel(context.Background())
	joined, err := JoinFromToken(request, s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	disconnect()
	if joined.Err() != nil {
		t.Fatalf("the joined context was cancelled by its client: %v", joined.Err())
	}
	if spi.GetTransaction(joined) == nil || len(fence.Pairs(joined)) != 1 {
		t.Fatal("detaching cancellation must keep the transaction and the pairs")
	}
}
