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
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// testResponseMax is the joined-answer ceiling these tests build a Joiner with
// — the shipped default of CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES. Nothing in
// this package writes an answer, so the figure only has to be a valid one.
const testResponseMax = 10 << 20

// testMaxWaiters is the queue cap these tests build a Joiner with — the shipped
// default of CYODA_CALLOUT_JOINED_MAX_WAITERS. It has to be comfortably above
// what the parallel-callbacks test queues on one transaction.
const testMaxWaiters = 128

// fakeTM satisfies spi.TransactionManager by embedding the interface and
// overriding only Join. Unimplemented methods panic if unexpectedly called.
type fakeTM struct {
	spi.TransactionManager
	joinErr error
	// origin is the attribution root Join puts on the transaction it returns,
	// as a real manager carries the one recorded at Begin.
	origin spi.Principal
	// noTx makes Join succeed but hand back the context with no transaction
	// on it — a manager that broke its contract.
	noTx bool
}

func (f fakeTM) Join(ctx context.Context, txID string) (context.Context, error) {
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	if f.noTx {
		return ctx, nil
	}
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: txID, Origin: f.origin}), nil
}

// make32 returns a deterministic 32-byte HMAC secret for tests.
func make32(t *testing.T) []byte {
	t.Helper()
	return []byte("test-secret-key-at-least-32byte!")
}

// noCalloutCoord returns a coordination that knows no callout: every pass is
// refused.
func noCalloutCoord() txcoord.Coordination { return txcoord.New(fakeTM{}) }

// liveCoord returns a coordination on which calloutID is in progress on txID
// at major 1, and the pass claims that name it.
func liveCoord(t *testing.T, calloutID, txID string) (txcoord.Coordination, token.Claims) {
	t.Helper()
	c := txcoord.New(fakeTM{})
	_, end := c.BeginCallout(context.Background(), txID, calloutID)
	t.Cleanup(end)
	c.Advance(calloutID, 1)
	return c, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
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

// joinFromToken is the entry of a joined request as Run runs it, short of the
// handler: verify the pass, then join, admit and take the gate (enter). The
// scope is opened outside the transaction (no savepoint, which the fake
// manager does not take) and ended at once; the returned context keeps the
// transaction and the pairs.
func joinFromToken(ctx context.Context, s *token.Signer, txMgr spi.TransactionManager, c txcoord.Coordination, tok string) (context.Context, error) {
	// NewJoiner fails only when the meter cannot register its counter, and the
	// no-op meter cannot.
	j, _ := NewJoiner(s, txMgr, c, testResponseMax, testMaxWaiters, nil)
	pass, err := j.Verify(tok)
	if err != nil || pass == nil {
		return ctx, err
	}
	scope, sctx, err := j.enter(ctx, pass, txcoord.Outside)
	if err != nil {
		return sctx, err
	}
	scope.End(nil)
	return sctx, nil
}

func TestVerify_EmptyPassIsNotAJoinedRequest(t *testing.T) {
	ctx := context.Background()
	got, err := joinFromToken(ctx, nil, fakeTM{}, noCalloutCoord(), "")
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

func TestJoin_ValidPassJoinsTheTransaction(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	f, claims := liveCoord(t, "req-tx-1", "tx-1")
	tok, err := s.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ctx, err := joinFromToken(context.Background(), s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tx := spi.GetTransaction(ctx)
	if tx == nil || tx.ID != "tx-1" {
		t.Fatalf("expected joined tx tx-1, got %+v", tx)
	}
}

func TestVerify_ExpiredMaps410(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-exp", ExpiresAt: time.Now().Add(-time.Second).Unix(), Callout: "req-tx-exp", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = joinFromToken(context.Background(), s, fakeTM{}, noCalloutCoord(), tok)
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

func TestVerify_ForgedMaps401(t *testing.T) {
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
	_, err = joinFromToken(context.Background(), s, fakeTM{}, noCalloutCoord(), tok)
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

func TestJoin_NotFoundMaps404(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = joinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxNotFound}, noCalloutCoord(), tok)
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

func TestJoin_RolledBackMaps404(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = joinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxRolledBack}, noCalloutCoord(), tok)
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

func TestJoin_AlreadyCommittedMaps404(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = joinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxAlreadyCommitted}, noCalloutCoord(), tok)
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

func TestJoin_UnknownJoinErrorMaps5xx(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	unknownErr := errors.New("db unavailable")
	_, err = joinFromToken(context.Background(), s, fakeTM{joinErr: unknownErr}, noCalloutCoord(), tok)
	var op *common.AppError
	if !errors.As(err, &op) {
		t.Fatalf("expected *common.AppError, got %T: %v", err, err)
	}
	if op.Status != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error, got %d", op.Status)
	}
}

func TestJoin_TenantMismatchMaps403(t *testing.T) {
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = joinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, noCalloutCoord(), tok)
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

func TestJoin_AdmitsUnderTheCurrentNumber(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveCoord(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	ctx, err := joinFromToken(context.Background(), s, fakeTM{}, f, tok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx := spi.GetTransaction(ctx); tx == nil || tx.ID != "tx-1" {
		t.Fatalf("expected joined tx-1, got %+v", tx)
	}
	if got := txcoord.Pairs(ctx); len(got) != 1 || got[0] != (txcoord.Pair{Callout: "req-1", Major: 1}) {
		t.Fatalf("Pairs = %v", got)
	}
}

func TestJoin_CalloutEnded_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f := noCalloutCoord()
	_, end := f.BeginCallout(context.Background(), "tx-1", "req-1")
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	base := context.Background()
	ctx, err := joinFromToken(base, s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
	if ctx != base {
		t.Fatal("a refused join must hand back the caller's context")
	}
}

func TestJoin_ReplacedWhileCalloutInProgress_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveCoord(t, "req-1", "tx-1")
	tok, _ := s.Issue(claims)
	f.Advance("req-1", 2)
	_, err := joinFromToken(context.Background(), s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
}

func TestJoin_EnclosingCalloutNotCurrent_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveCoord(t, "req-inner", "tx-1")
	claims.Outer = []token.Pair{{Callout: "req-outer", Major: 1}} // never begun
	tok, _ := s.Issue(claims)
	_, err := joinFromToken(context.Background(), s, fakeTM{}, f, tok)
	assertAppErr(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded)
}

// The transaction is looked up, and its tenant checked, BEFORE the fence is
// consulted.
func TestJoin_JoinComesBeforeTheFence(t *testing.T) {
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
			f := noCalloutCoord()
			tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
			_, err := joinFromToken(context.Background(), s, fakeTM{joinErr: tc.joinErr}, f, tok)
			assertAppErr(t, err, tc.status, tc.code)
		})
	}
}

// A pass presented by the wrong tenant is answered 403 whatever the fence knows
// about the callout it names — current, superseded or never registered — so a
// stolen pass tells another tenant nothing about which callouts exist.
func TestJoin_TenantMismatchIsOneAnswerWhateverTheFenceKnows(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) (txcoord.Coordination, token.Claims){
		"callout current": func(t *testing.T) (txcoord.Coordination, token.Claims) {
			return liveCoord(t, "req-1", "tx-1")
		},
		"callout superseded": func(t *testing.T) (txcoord.Coordination, token.Claims) {
			f, claims := liveCoord(t, "req-1", "tx-1")
			f.Advance("req-1", 2)
			return f, claims
		},
		"callout unknown": func(t *testing.T) (txcoord.Coordination, token.Claims) {
			return noCalloutCoord(), token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, claims := setup(t)
			tok, _ := s.Issue(claims)
			_, err := joinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, f, tok)
			assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
		})
	}
}

// A stolen pass absorbs nothing: another tenant presenting a higher minor must
// not shut the rightful cnode out.
func TestJoin_TenantMismatch_AbsorbsNothing(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveCoord(t, "req-1", "tx-1")
	rightful := rightfulRequest(t, s, f, claims)
	claims.Minor = 9
	tok, _ := s.Issue(claims)
	_, err := joinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, f, tok)
	assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
	if err := txcoord.Check(rightful); err != nil {
		t.Fatalf("a refused join changed the fence: %v", err)
	}
}

// rightfulRequest is a request of the rightful compute node admitted under
// claims at minor 1: Check on its context reads whether its pass is still
// current.
func rightfulRequest(t *testing.T, s *token.Signer, c txcoord.Coordination, claims token.Claims) context.Context {
	t.Helper()
	claims.Minor = 1
	tok, _ := s.Issue(claims)
	ctx, err := joinFromToken(context.Background(), s, fakeTM{}, c, tok)
	if err != nil {
		t.Fatalf("the rightful request was refused: %v", err)
	}
	return ctx
}

func TestVerify_NoCalloutAndNumber_401(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	_, err := joinFromToken(context.Background(), s, fakeTM{}, noCalloutCoord(), tok)
	assertAppErr(t, err, http.StatusUnauthorized, common.ErrCodeUnauthorized)
	var appErr *common.AppError
	_ = errors.As(err, &appErr)
	if appErr.Message != "UNAUTHORIZED: invalid transaction token" {
		t.Fatalf("message = %q", appErr.Message)
	}
}

// An on-behalf-of request joins only a transaction begun for its own user: the
// pass binds no caller, so a compute node presenting it with another user's
// on-behalf-of token would otherwise be recorded as that user inside the first
// user's transaction. A service client — a compute node's own token — joins
// whatever transaction its pass names, and its writes attribute to that
// transaction's origin.
func TestJoin_OBOJoinsOnlyItsOwnUsersTransaction(t *testing.T) {
	alice := spi.Principal{ID: "alice", Kind: spi.PrincipalUser}
	oboClient := &spi.Principal{ID: "app-client", Kind: spi.PrincipalService}
	tests := []struct {
		name   string
		origin spi.Principal
		noTx   bool
		uc     *spi.UserContext
		refuse bool
	}{
		{"OBOOwnUser_Admitted", alice, false,
			&spi.UserContext{UserID: "alice", Kind: spi.PrincipalUser, Executor: oboClient}, false},
		{"OBOOtherUser_403", alice, false,
			&spi.UserContext{UserID: "bob", Kind: spi.PrincipalUser, Executor: oboClient}, true},
		{"OBOSameIDServiceOrigin_403", spi.Principal{ID: "alice", Kind: spi.PrincipalService}, false,
			&spi.UserContext{UserID: "alice", Kind: spi.PrincipalUser, Executor: oboClient}, true},
		{"OBONoOrigin_403", spi.Principal{}, false,
			&spi.UserContext{UserID: "alice", Kind: spi.PrincipalUser, Executor: oboClient}, true},
		// Fails closed: a join that hands back no transaction has no origin to
		// match.
		{"OBONoTransaction_403", alice, true,
			&spi.UserContext{UserID: "alice", Kind: spi.PrincipalUser, Executor: oboClient}, true},
		{"ServiceClient_UserOrigin_Admitted", alice, false,
			&spi.UserContext{UserID: "compute-client", Kind: spi.PrincipalService}, false},
		{"ServiceClient_ServiceOrigin_Admitted", spi.Principal{ID: "app-svc", Kind: spi.PrincipalService}, false,
			&spi.UserContext{UserID: "compute-client", Kind: spi.PrincipalService}, false},
		{"ServiceClient_NoOrigin_Admitted", spi.Principal{}, false,
			&spi.UserContext{UserID: "compute-client", Kind: spi.PrincipalService}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, claims := liveCoord(t, "req-1", "tx-1")
			tok, _ := s.Issue(claims)
			base := spi.WithUserContext(context.Background(), tc.uc)
			got, err := joinFromToken(base, s, fakeTM{origin: tc.origin, noTx: tc.noTx}, f, tok)
			if !tc.refuse {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tx := spi.GetTransaction(got); tx == nil || tx.ID != "tx-1" {
					t.Fatalf("expected joined tx-1, got %+v", tx)
				}
				return
			}
			assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
			var appErr *common.AppError
			_ = errors.As(err, &appErr)
			if appErr.Message != "FORBIDDEN: an on-behalf-of request may join only its own user's transaction" {
				t.Fatalf("message = %q", appErr.Message)
			}
			if got != base {
				t.Fatal("a refused join must hand back the caller's context")
			}
		})
	}
}

// A refused on-behalf-of join absorbs nothing: it is refused before the fence,
// so a higher minor on its pass does not shut the rightful compute node out.
func TestJoin_OBOOtherUser_AbsorbsNothing(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, claims := liveCoord(t, "req-1", "tx-1")
	rightful := rightfulRequest(t, s, f, claims)
	claims.Minor = 9
	tok, _ := s.Issue(claims)
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "bob", Kind: spi.PrincipalUser,
		Executor: &spi.Principal{ID: "app-client", Kind: spi.PrincipalService},
	})
	_, err := joinFromToken(ctx, s, fakeTM{origin: spi.Principal{ID: "alice", Kind: spi.PrincipalUser}}, f, tok)
	assertAppErr(t, err, http.StatusForbidden, common.ErrCodeForbidden)
	if err := txcoord.Check(rightful); err != nil {
		t.Fatalf("a refused join changed the fence: %v", err)
	}
}
