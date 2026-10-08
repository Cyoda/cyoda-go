package postgres_test

// consistency_role_test.go — the consistency-time functions under a runtime
// role that is not the owner of the plugin's objects and holds exactly the
// grants docs/plugins/POSTGRES.md ("Roles") lists for them. Both functions run
// with the caller's rights, so those grants include the stamp floor.

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
		`GRANT SELECT, UPDATE ON cyoda_stamp_floor TO ` + ident,
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
// it.
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
	before, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("ConsistencyTime as the role: %v", err)
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
		t.Fatalf("stamp %v is not above the consistency time %v given before it", stamp, before)
	}
	after, err := tm.ConsistencyTime(ctx)
	if err != nil {
		t.Fatalf("second ConsistencyTime as the role: %v", err)
	}
	if after.Before(stamp) {
		t.Fatalf("consistency time %v is below the stamp %v committed before it", after, stamp)
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

// The plugin's calls name their argument types with pg_catalog. Bare `int4` is
// not a keyword: it resolves through the search path, so on a path that puts
// public ahead of pg_catalog a domain public.int4 would turn `$1::int4` into a
// cast to the domain, and an overload taking that domain would then be the
// exact match. A role granted CREATE on public after the plugin started
// plants the domain and such overloads of both functions, each recording its
// call; a commit, a non-transactional save and ConsistencyTime through the
// plugin call none of them.
func TestConsistencyTime_PlantedDomainOverloadsAreNeverCalled(t *testing.T) {
	dsn := freshCTDatabase(t)
	pathFirst := func(c *pgxpool.Config) { c.ConnConfig.RuntimeParams["search_path"] = "public, pg_catalog" }
	owner := newCTPool(t, dsn, 10, pathFirst)
	if err := postgres.Migrate(owner); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	name := "cyoda_ct_domain_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	ident := pgx.Identifier{name}.Sanitize()
	for _, stmt := range []string{
		`CREATE ROLE ` + ident + ` LOGIN PASSWORD 'probe' NOSUPERUSER`,
		`GRANT CREATE ON SCHEMA public TO ` + ident,
	} {
		if _, err := owner.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("provision role: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+ident+` CASCADE`)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+ident)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", errors.Unwrap(err))
	}
	u.User = url.UserPassword(name, "probe")
	planter := newCTPool(t, u.String(), 1, pathFirst)
	if _, err := planter.Exec(context.Background(), `
		CREATE TABLE public.ct_planted_calls (fn pg_catalog.text);
		GRANT INSERT ON public.ct_planted_calls TO PUBLIC;
		CREATE DOMAIN public.int4 AS pg_catalog.int4;
		CREATE FUNCTION public.cyoda_stamp(public.int4) RETURNS pg_catalog.timestamptz LANGUAGE plpgsql AS $f$
		BEGIN
		  INSERT INTO public.ct_planted_calls VALUES ('cyoda_stamp');
		  RETURN '1999-01-01T00:00:00Z';
		END $f$;
		CREATE FUNCTION public.cyoda_consistency_time(public.int4, pg_catalog.int8) RETURNS pg_catalog.timestamptz LANGUAGE plpgsql AS $f$
		BEGIN
		  INSERT INTO public.ct_planted_calls VALUES ('cyoda_consistency_time');
		  RETURN '2999-01-01T00:00:00Z';
		END $f$;`); err != nil {
		t.Fatalf("plant the domain and overloads: %v", err)
	}

	f := postgres.NewStoreFactory(owner)
	f.InitTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant(ctTenant)
	tm := ctTM(t, f, ctx)
	planted := func(after string) {
		t.Helper()
		rows, err := owner.Query(context.Background(), `SELECT fn FROM public.ct_planted_calls`)
		if err != nil {
			t.Fatalf("read the planted calls: %v", err)
		}
		called, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the planted calls: %v", err)
		}
		if len(called) != 0 {
			t.Fatalf("after %s, planted overloads ran with the plugin's rights: %v", after, called)
		}
	}

	before, err := tm.ConsistencyTime(ctx)
	planted("ConsistencyTime")
	if err != nil {
		t.Fatalf("ConsistencyTime: %v", err)
	}
	if _, err := commitOneEntityErr(t, f, ctx); err != nil {
		planted("a transactional commit")
		t.Fatalf("transactional Commit: %v", err)
	}
	planted("a transactional commit")
	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		planted("a non-transactional save")
		t.Fatalf("non-transactional Save: %v", err)
	}
	planted("a non-transactional save")
	after, err := tm.ConsistencyTime(ctx)
	planted("the second ConsistencyTime")
	if err != nil {
		t.Fatalf("second ConsistencyTime: %v", err)
	}
	if after.Before(before) || after.After(time.Now().Add(time.Hour)) {
		t.Fatalf("consistency times %v then %v are not real", before, after)
	}
}
