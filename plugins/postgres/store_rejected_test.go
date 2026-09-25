package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func TestClassifyError_DeterministicRejectionsCarryErrStoreRejected(t *testing.T) {
	for _, code := range []string{"22021", "22001", "22P02", "23502", "23505", "23514", "42P01", "42703"} {
		err := classifyError(fmt.Errorf("statement: %w", &pgconn.PgError{Code: code}))
		if !errors.Is(err, spi.ErrStoreRejected) {
			t.Errorf("SQLSTATE %s: %v does not carry ErrStoreRejected", code, err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("SQLSTATE %s: the PgError left the chain: %v", code, err)
		}
	}
	for _, code := range []string{"40001", "40P01", "55P03", "57014", "25P03", "08006", "53300"} {
		if err := classifyError(&pgconn.PgError{Code: code}); errors.Is(err, spi.ErrStoreRejected) {
			t.Errorf("SQLSTATE %s is retryable but carries ErrStoreRejected: %v", code, err)
		}
	}
	twice := classifyError(classifyError(&pgconn.PgError{Code: "22021"}))
	if n := strings.Count(twice.Error(), spi.ErrStoreRejected.Error()); n != 1 {
		t.Errorf("classifying twice marked %d times: %v", n, twice)
	}
}
