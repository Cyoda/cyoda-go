package postgres_test

// consistency_role_test.go — the consistency-time functions under a runtime
// role that is not the owner of the plugin's objects and holds exactly the
// grants docs/plugins/POSTGRES.md ("Roles") lists for them. Both functions are
// SECURITY DEFINER, so such a role stamps and computes consistency times
// without any privilege on the stamp floor, and cannot move the floor itself.

import (
	"context"
	"errors"
	"fmt"
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
// consistency-time objects, plus any extra grants given. It returns the
// owner's pool, a pool connected as the role, and the role's name.
func newDocumentedGrantsRole(t *testing.T, extra ...string) (owner, role *pgxpool.Pool, name string) {
	t.Helper()
	dsn := freshCTDatabase(t)
	owner = newCTPool(t, dsn, 10, nil)
	if err := postgres.Migrate(owner); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	name = "cyoda_ct_role_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
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
	for _, grant := range extra {
		if _, err := owner.Exec(context.Background(), grant+` TO `+ident); err != nil {
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
	return owner, role, name
}

// A role with only the documented grants allocates a new tenant's key, stamps
// a commit through cyoda_stamp, and gets consistency times on both sides of
// it. It cannot read, advance or set the stamp floor itself.
func TestConsistencyTime_DocumentedGrantsSuffice(t *testing.T) {
	owner, role, _ := newDocumentedGrantsRole(t)
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
	owner, role, _ := newDocumentedGrantsRole(t)
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

// Both functions are SECURITY DEFINER, so whatever they resolve runs as the
// migration role. A runtime role that may create objects in the functions'
// schema — PostgreSQL 14's default for public — plants a better-matching
// setval or an exact-match *(bigint, interval) operator there, each of which
// would make the caller SUPERUSER if it ran with the definer's privileges.
// The functions resolve everything but the plugin's own objects in
// pg_catalog, and those by schema-qualified name, so neither planted object
// runs: both calls give correct results and the role stays an ordinary role.
func TestConsistencyTimeSQL_PlantedObjectsDoNotRunAsTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant string // run as the role; %[1]s is the role's quoted name
	}{
		{"setval", `
			CREATE FUNCTION public.setval(text, bigint, boolean) RETURNS bigint LANGUAGE plpgsql AS $f$
			BEGIN
			  EXECUTE 'ALTER ROLE %[1]s SUPERUSER';
			  RETURN pg_catalog.setval($1::regclass, $2, $3);
			END $f$;`},
		{"operator *(bigint, interval)", `
			CREATE FUNCTION public.ct_planted_mul(bigint, interval) RETURNS interval LANGUAGE plpgsql AS $f$
			BEGIN
			  EXECUTE 'ALTER ROLE %[1]s SUPERUSER';
			  RETURN $1::float8 * $2;
			END $f$;
			CREATE OPERATOR public.* (LEFTARG = bigint, RIGHTARG = interval, FUNCTION = public.ct_planted_mul);`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, role, name := newDocumentedGrantsRole(t, `GRANT CREATE ON SCHEMA public`)
			const tenant spi.TenantID = "ct-planted-tenant"
			key := tenantKey(t, owner, tenant)
			if _, err := role.Exec(context.Background(),
				fmt.Sprintf(tc.plant, pgx.Identifier{name}.Sanitize())); err != nil {
				t.Fatalf("plant the object as the role: %v", err)
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
			var c time.Time
			if err := role.QueryRow(context.Background(), `SELECT cyoda_consistency_time($1, 1000)`, key).Scan(&c); err != nil {
				t.Fatalf("cyoda_consistency_time as the role: %v", err)
			}

			var super bool
			if err := owner.QueryRow(context.Background(),
				`SELECT rolsuper FROM pg_roles WHERE rolname = $1`, name).Scan(&super); err != nil {
				t.Fatalf("read the role's attributes: %v", err)
			}
			if super {
				t.Fatal("the planted object ran with the functions' owner's privileges: the role made itself SUPERUSER")
			}
			if c.Before(stamp) {
				t.Fatalf("C %v is below the stamp %v committed before it", c, stamp)
			}
			var floor int64
			if err := owner.QueryRow(context.Background(), `SELECT last_value FROM cyoda_stamp_floor`).Scan(&floor); err != nil {
				t.Fatalf("read the floor: %v", err)
			}
			if floor < c.UnixMicro() {
				t.Fatalf("the floor %d is below the C %d the call returned: the call did not move the real floor", floor, c.UnixMicro())
			}
		})
	}
}

// The migration creates its objects in whatever schema it runs in, which is
// not necessarily public: run with a search_path that names another schema
// first, the functions land there, refer to that schema's floor, and stamp and
// return consistency times through the plugin.
func TestConsistencyTimeMigration_RunsInANonPublicSchema(t *testing.T) {
	const schema = "ct_other_schema"
	dsn := freshCTDatabase(t)
	setup := newCTPool(t, dsn, 2, nil)
	if _, err := setup.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	pool := newCTPool(t, dsn, 10, func(c *pgxpool.Config) {
		c.ConnConfig.RuntimeParams["search_path"] = schema
	})
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate into %s: %v", schema, err)
	}

	var inSchema, inPublic int
	if err := setup.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE n.nspname = $1), count(*) FILTER (WHERE n.nspname = 'public')
		   FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE p.proname IN ('cyoda_stamp', 'cyoda_consistency_time')`, schema).Scan(&inSchema, &inPublic); err != nil {
		t.Fatalf("find the functions: %v", err)
	}
	if inSchema != 2 || inPublic != 0 {
		t.Fatalf("%d functions in %s and %d in public, want 2 and 0", inSchema, schema, inPublic)
	}

	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant(ctTenant)
	tm := ctTM(t, f, ctx)
	before, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("ConsistencyTime: %v", err)
	}
	txID := commitOneEntity(t, f, ctx)
	submit, err := tm.GetSubmitTime(ctx, txID)
	if err != nil {
		t.Fatalf("GetSubmitTime: %v", err)
	}
	if !submit.After(before) {
		t.Fatalf("a commit after C was stamped %v, not above C %v", submit, before)
	}
	after, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("second ConsistencyTime: %v", err)
	}
	if after.Before(submit) {
		t.Fatalf("C %v is below the commit %v before it", after, submit)
	}
	var floor int64
	if err := setup.QueryRow(context.Background(),
		`SELECT last_value FROM `+schema+`.cyoda_stamp_floor`).Scan(&floor); err != nil {
		t.Fatalf("read the floor in %s: %v", schema, err)
	}
	if floor < after.UnixMicro() {
		t.Fatalf("the floor in %s (%d) is below the C returned (%d)", schema, floor, after.UnixMicro())
	}
}

// The plugin calls cyoda_stamp and cyoda_consistency_time by unqualified name
// on its own connection, which runs with the owner's rights. A role that may
// create objects in the functions' schema (PostgreSQL 14's default for
// public) can add an overload of either function, which is legal because the
// signature differs. Unless the plugin's call names the argument types, an
// overload whose type PostgreSQL prefers for an untyped parameter would be
// chosen — and run with the plugin's rights — or make the call ambiguous or
// unencodable, failing every commit. With typed calls the real functions are
// exact matches, which no overload beats. Each case plants overloads of both
// functions that make the planter SUPERUSER, record that they ran and return a
// forged instant, then drives a transactional commit, a non-transactional
// save and ConsistencyTime through the plugin on the owner's pool.
func TestConsistencyTime_PlantedOverloadsAreNeverCalled(t *testing.T) {
	for _, typ := range []string{"float8", "numeric", "text"} {
		t.Run(typ, func(t *testing.T) {
			owner, role, name := newDocumentedGrantsRole(t, `GRANT CREATE ON SCHEMA public`)
			plant := fmt.Sprintf(`
				CREATE TABLE public.ct_planted_calls (fn text);
				GRANT INSERT ON public.ct_planted_calls TO PUBLIC;
				CREATE FUNCTION public.cyoda_stamp(%[1]s) RETURNS timestamptz LANGUAGE plpgsql AS $f$
				BEGIN
				  INSERT INTO public.ct_planted_calls VALUES ('cyoda_stamp');
				  EXECUTE 'ALTER ROLE %[2]s SUPERUSER';
				  RETURN '1999-01-01T00:00:00Z';
				END $f$;
				CREATE FUNCTION public.cyoda_consistency_time(%[1]s, %[1]s) RETURNS timestamptz LANGUAGE plpgsql AS $f$
				BEGIN
				  INSERT INTO public.ct_planted_calls VALUES ('cyoda_consistency_time');
				  EXECUTE 'ALTER ROLE %[2]s SUPERUSER';
				  RETURN '2999-01-01T00:00:00Z';
				END $f$;`, typ, pgx.Identifier{name}.Sanitize())
			if _, err := role.Exec(context.Background(), plant); err != nil {
				t.Fatalf("plant the overloads as the role: %v", err)
			}

			f := postgres.NewStoreFactory(owner)
			f.InitTransactionManager(newTestUUIDGenerator())
			ctx := ctxWithTenant(ctTenant)
			tm := ctTM(t, f, ctx)
			notPlanted := func(after string) {
				t.Helper()
				var calls int
				var super bool
				if err := owner.QueryRow(context.Background(),
					`SELECT (SELECT count(*) FROM public.ct_planted_calls),
					        (SELECT rolsuper FROM pg_roles WHERE rolname = $1)`, name).Scan(&calls, &super); err != nil {
					t.Fatalf("read the planted calls: %v", err)
				}
				if calls != 0 || super {
					t.Fatalf("after %s, a planted overload ran %d times with the plugin's rights (planter SUPERUSER: %v)",
						after, calls, super)
				}
			}

			before, err := tm.ConsistencyTime(ctx)
			notPlanted("ConsistencyTime")
			if err != nil {
				t.Fatalf("ConsistencyTime: %v", err)
			}
			txID, err := commitOneEntityErr(t, f, ctx)
			notPlanted("a transactional commit")
			if err != nil {
				t.Fatalf("transactional Commit: %v", err)
			}
			submit, err := tm.GetSubmitTime(ctx, txID)
			if err != nil {
				t.Fatalf("GetSubmitTime: %v", err)
			}
			es, err := f.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			id := uuid.NewString()
			if _, err := es.Save(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
			}); err != nil {
				notPlanted("a non-transactional save")
				t.Fatalf("non-transactional Save: %v", err)
			}
			notPlanted("a non-transactional save")
			var saved time.Time
			if err := owner.QueryRow(context.Background(),
				`SELECT transaction_time FROM entity_versions WHERE tenant_id = $1 AND entity_id = $2`,
				string(ctTenant), id).Scan(&saved); err != nil {
				t.Fatalf("read the save's stamp: %v", err)
			}
			after, err := tm.ConsistencyTime(ctx)
			notPlanted("the second ConsistencyTime")
			if err != nil {
				t.Fatalf("second ConsistencyTime: %v", err)
			}
			if !submit.After(before) || !saved.After(submit) || after.Before(saved) {
				t.Fatalf("stamps and consistency times out of order: C %v, commit %v, save %v, C %v",
					before, submit, saved, after)
			}
			if after.After(time.Now().Add(time.Hour)) {
				t.Fatalf("C %v is not a real consistency time", after)
			}
		})
	}
}
