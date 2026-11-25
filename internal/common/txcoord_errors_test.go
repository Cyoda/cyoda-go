package common

import (
	"net/http"
	"testing"
)

func TestTxCoordErrors(t *testing.T) {
	cases := []struct {
		name      string
		err       *AppError
		status    int
		code      string
		retryable bool
	}{
		{"busy", TransactionBusy(), http.StatusServiceUnavailable, ErrCodeTransactionBusy, true},
		{"too many joined", TooManyJoinedRequests(), http.StatusServiceUnavailable, ErrCodeTooManyJoinedRequests, true},
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
		})
	}
}
