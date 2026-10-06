package postgres_test

// consistency_role_test.go — the consistency-time functions under a runtime
// role that is not the owner of the plugin's objects and holds exactly the
// grants docs/plugins/POSTGRES.md ("Roles") lists for them. Both functions are
// SECURITY DEFINER, so such a role stamps and computes consistency times
// without any privilege on the stamp floor, and cannot move the floor itself.

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// newDocumentedGrantsRole creates a login role on a database of its own,
// migrated by the owner, and grants it exactly the documented grants for the
// consistency-time objects. It returns the owner's pool and a pool connected
// as the role.
func newDocumentedGrantsRole(t *testing.T) (owner, role *pgxpool.Pool) {
	t.Helper()
	dsn := freshCTDatabase(t)
	owner = newCTPool(t, dsn, 10, nil)
	if err := postgres.Migrate(owner); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	name := "cyoda_ct_role_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	ident := pgx.Identifier{name}.Sanitize()
	for _, stmt := range []string{
		`CREATE ROLE ` + ident + ` LOGIN PASSWORD 'probe' NOSUPERUSER NOCREATEDB NOCREATEROLE`,
		// The documented grants, and nothing else.
		`GRANT USAGE ON SCHEMA public TO ` + ident,
		`GRANT SELECT, INSERT ON consistency_tenant_keys TO ` + ident,
		`GRANT USAGE ON SEQUENCE consistency_tenant_key_seq TO ` + ident,
	} {
		if _, err := owner.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("provision role: %v", err)
		}
	}
	t.Cleanup(func() {
		// Before the database drop (LIFO), while the granted objects exist.
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+ident)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+ident)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", errors.Unwrap(err))
	}
	u.User = url.UserPassword(name, "probe")
	role = newCTPool(t, u.String(), 4, nil)
	return owner, role
}

// A role with only the documented grants allocates a new tenant's key, stamps
// a commit through cyoda_stamp, and gets consistency times on both sides of
// it. It cannot read, advance or set the stamp floor itself.
func TestConsistencyTime_DocumentedGrantsSuffice(t *testing.T) {
	owner, role := newDocumentedGrantsRole(t)
	const tenant spi.TenantID = "ct-role-tenant"
	ctx := ctxWithTenant(tenant)
	f := postgres.NewStoreFactory(role)
	f.InitTransactionManager(newTestUUIDGenerator())
	tm := ctTM(t, f, ctx)

	// Begin allocates the new tenant's key: the lookup's INSERT and nextval
	// under the table's row-level security policy.
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin as the role: %v", err)
	}
	if err := tm.Rollback(txCtx, txID); err != nil {
		t.Fatalf("Rollback as the role: %v", err)
	}
	key := storedTenantKey(t, owner, tenant)
	var before time.Time
	if err := owner.QueryRow(context.Background(),
		`SELECT 'epoch'::timestamptz + last_value * interval '1 microsecond' FROM cyoda_stamp_floor`).Scan(&before); err != nil {
		t.Fatalf("read the floor as the owner: %v", err)
	}

	tx, err := role.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin as the role: %v", err)
	}
	var stamp time.Time
	if err := tx.QueryRow(context.Background(), `SELECT cyoda_stamp($1)`, key).Scan(&stamp); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("cyoda_stamp as the role: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit as the role: %v", err)
	}
	if !stamp.After(before) {
		t.Fatalf("stamp %v is not above the earlier consistency time %v", stamp, before)
	}
	after, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("second ConsistencyTime as the role: %v", err)
	}
	if after.Before(stamp) {
		t.Fatalf("consistency time %v is below the stamp %v committed before it", after, stamp)
	}

	for _, stmt := range []string{
		`SELECT setval('cyoda_stamp_floor', 1)`,
		`SELECT setval('cyoda_stamp_floor', 253370764800000000)`,
		`SELECT nextval('cyoda_stamp_floor')`,
		`SELECT last_value FROM cyoda_stamp_floor`,
	} {
		_, err := role.Exec(context.Background(), stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s as the role: want permission denied (42501), got %v", stmt, err)
		}
	}
}

// The functions resolve their relations — the floor sequence and the
// pg_locks, pg_settings and pg_database catalogs — ahead of the session's
// temporary schema, so a session cannot shadow them with temporary objects of
// the same names: a temporary cyoda_stamp_floor does not move C, and a
// temporary pg_locks does not hide the tenant's markers from the wait.
func TestConsistencyTimeSQL_TemporaryObjectsDoNotShadow(t *testing.T) {
	owner, role := newDocumentedGrantsRole(t)
	const tenant spi.TenantID = "ct-shadow-tenant"
	key := tenantKey(t, owner, tenant)

	conn, err := role.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire role connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `
		CREATE TEMP SEQUENCE cyoda_stamp_floor AS bigint;
		SELECT setval('cyoda_stamp_floor', 253370764800000000);
		CREATE TEMP VIEW pg_locks AS SELECT * FROM pg_catalog.pg_locks WHERE false;`); err != nil {
		t.Fatalf("create the shadowing temporary objects: %v", err)
	}

	var c time.Time
	if err := conn.QueryRow(context.Background(), `SELECT cyoda_consistency_time($1, 1000)`, key).Scan(&c); err != nil {
		t.Fatalf("cyoda_consistency_time with shadowing objects: %v", err)
	}
	if c.After(time.Now().Add(time.Hour)) {
		t.Fatalf("C %v came from a temporary sequence, not the stamp floor", c)
	}

	holdStamp(t, owner, tenant)
	err = conn.QueryRow(context.Background(), `SELECT cyoda_consistency_time($1, 300)`, key).Scan(&c)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("with a held marker and a temporary pg_locks, want the wait to exhaust its budget (55P03), got C=%v err=%v", c, err)
	}
}
