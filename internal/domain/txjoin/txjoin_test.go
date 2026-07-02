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

func TestJoinFromToken_EmptyPassThrough(t *testing.T) {
	ctx := context.Background()
	got, err := JoinFromToken(ctx, nil, fakeTM{}, "")
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
	tok, err := s.Issue("local", "tx-1", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ctx, err := JoinFromToken(context.Background(), s, fakeTM{}, tok)
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
	tok, err := s.Issue("local", "tx-exp", time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{}, tok)
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
	tok, err := s2.Issue("local", "tx-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{}, tok)
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
	tok, err := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxNotFound}, tok)
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
	tok, err := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxRolledBack}, tok)
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
	tok, err := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxAlreadyCommitted}, tok)
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
	tok, err := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	unknownErr := errors.New("db unavailable")
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: unknownErr}, tok)
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
	tok, err := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = JoinFromToken(context.Background(), s, fakeTM{joinErr: spi.ErrTxTenantMismatch}, tok)
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
