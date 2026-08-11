package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// errQuerier is a stub whose every method reports the configured error,
// standing in for a pgx.Tx whose statements fail server-side.
type errQuerier struct{ err error }

func (q errQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, q.err
}
func (q errQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, q.err
}
func (q errQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{err: q.err}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// TestClassifiedQuerier_ClassifiesStatementErrors — ExtendSchema's
// self-wrap transaction routes its statements through classifiedQuerier
// so its errors carry the same classification the ambient path gets from
// ctxQuerier: a serialization abort must surface as spi.ErrConflict, and
// an idle-in-transaction reclaim must carry the storage-unavailable
// marker. A raw pgx.Tx would bypass classification entirely and turn
// retryable outcomes into opaque 500s.
func TestClassifiedQuerier_ClassifiesStatementErrors(t *testing.T) {
	ctx := context.Background()
	conflict := &pgconn.PgError{Code: pgerrcode.SerializationFailure}
	q := classifiedQuerier{inner: errQuerier{err: conflict}}

	if _, err := q.Exec(ctx, "UPDATE models SET doc = doc"); !errors.Is(err, spi.ErrConflict) {
		t.Errorf("Exec: 40001 classified as %v, want spi.ErrConflict", err)
	}
	if _, err := q.Query(ctx, "SELECT 1"); !errors.Is(err, spi.ErrConflict) {
		t.Errorf("Query: 40001 classified as %v, want spi.ErrConflict", err)
	}
	if err := q.QueryRow(ctx, "SELECT 1").Scan(); !errors.Is(err, spi.ErrConflict) {
		t.Errorf("QueryRow.Scan: 40001 classified as %v, want spi.ErrConflict", err)
	}
}
