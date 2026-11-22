package common

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestTxCoordErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       *AppError
		status    int
		code      string
		retryable bool
		rollback  bool
	}{
		{"owner", TransactionRollbackOnly("VALIDATION_FAILED"), http.StatusConflict, ErrCodeTransactionRollbackOnly, false, true},
		{"joined", TransactionRollbackOnlyJoined(), http.StatusConflict, ErrCodeTransactionRollbackOnly, false, true},
		{"busy", TransactionBusy(), http.StatusServiceUnavailable, ErrCodeTransactionBusy, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err.Level != LevelOperational {
				t.Errorf("Level = %v, want operational", c.err.Level)
			}
			if c.err.Status != c.status || c.err.Code != c.code || c.err.Retryable != c.retryable {
				t.Errorf("got %d %s retryable=%v, want %d %s retryable=%v",
					c.err.Status, c.err.Code, c.err.Retryable, c.status, c.code, c.retryable)
			}
			if got := errors.Is(c.err, ErrRollbackOnly); got != c.rollback {
				t.Errorf("errors.Is(ErrRollbackOnly) = %v, want %v", got, c.rollback)
			}
		})
	}
	if m := TransactionRollbackOnly("VALIDATION_FAILED").Message; !strings.Contains(m, "VALIDATION_FAILED") {
		t.Errorf("message %q does not name the failed code", m)
	}
}

// TestTransactionRollbackOnly_UnknownCode: a backend that coordinates the
// transaction itself does not say which code the failed request got; the
// message then names none rather than an empty one.
func TestTransactionRollbackOnly_UnknownCode(t *testing.T) {
	m := TransactionRollbackOnly("").Message
	if strings.Contains(m, "failed with ,") || strings.Contains(m, "with  ") {
		t.Errorf("message %q names an empty code", m)
	}
	if !strings.Contains(m, "can only be rolled back") {
		t.Errorf("message %q does not say the transaction can only be rolled back", m)
	}
}
