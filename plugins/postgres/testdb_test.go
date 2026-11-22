package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CreateTestDatabase creates an empty database named prefix + a random suffix
// on the server base points at, and returns its DSN and a func that drops it.
// It takes no *testing.T, so a fixture called from outside a test's own
// goroutine (the conformance harness's ClockFactory) can use it; every
// t-bound helper wraps it. Errors carry the database name, never the DSN,
// which may hold a password. Test-only.
func CreateTestDatabase(base, prefix string) (dsn string, drop func() error, err error) {
	if base == "" {
		return "", nil, errors.New("CYODA_TEST_DB_URL is empty — TestMain must provision a database")
	}
	u, err := url.Parse(base)
	if err != nil {
		// errors.Unwrap: *url.Error renders the URL, which may carry a password.
		return "", nil, fmt.Errorf("parse CYODA_TEST_DB_URL: %w", errors.Unwrap(err))
	}
	name := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quoted := pgx.Identifier{name}.Sanitize()

	// admin runs one statement on its own connection and closes it.
	admin := func(stmt string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			return fmt.Errorf("open admin connection: %w", err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, err = conn.Exec(ctx, stmt)
		return err
	}
	if err := admin("CREATE DATABASE " + quoted); err != nil {
		return "", nil, fmt.Errorf("create %s: %w", name, err)
	}
	drop = func() error {
		if err := admin("DROP DATABASE IF EXISTS " + quoted + " WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
	u.Path = "/" + name
	return u.String(), drop, nil
}
