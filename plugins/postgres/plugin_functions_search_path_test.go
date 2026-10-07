package postgres_test

// plugin_functions_search_path_test.go — the plugin's helper functions
// cyoda_epoch_millis and cyoda_try_float8 name every function, operator and
// type in their bodies with pg_catalog, so nothing planted in a schema on the
// caller's search_path can stand in for them. They carry no SET clause: a
// per-call search_path would save and restore a setting on every row of a
// sort or grouped stat.
//
// The startup check refuses a start while a role other than a schema's owner
// may create in a schema on the search path. So the role here is granted
// CREATE only after the plugin has migrated and started, which is the case the
// check cannot see: what stops the plant is the qualified names.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// Both functions keep the signature and attributes they were created with
// (migrations 000002 and 000005), carry no settings, and nothing in the schema
// depends on them — no index, view, default or constraint, which recreating
// them could otherwise affect.
func TestPluginFunctions_AttributesUnchangedAndNoSettings(t *testing.T) {
	pool := newCTPool(t, freshCTDatabase(t), 2, nil)
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, tc := range []struct{ fn, returns string }{
		{"cyoda_epoch_millis(text)", "bigint"},
		{"cyoda_try_float8(text)", "double precision"},
	} {
		var (
			config                 []string
			lang, returns, args    string
			volatile, parallel     string
			strict, definer, leaky bool
			cost                   float32
			dependents             int
		)
		if err := pool.QueryRow(context.Background(), `
			SELECT p.proconfig, l.lanname, pg_catalog.format_type(p.prorettype, NULL),
			       pg_catalog.oidvectortypes(p.proargtypes), p.provolatile::text, p.proparallel::text,
			       p.proisstrict, p.prosecdef, p.proleakproof, p.procost,
			       (SELECT count(*) FROM pg_catalog.pg_depend d
			         WHERE d.refclassid = 'pg_catalog.pg_proc'::regclass AND d.refobjid = p.oid)
			  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_language l ON l.oid = p.prolang
			 WHERE p.oid = $1::text::regprocedure`, tc.fn).Scan(
			&config, &lang, &returns, &args, &volatile, &parallel, &strict, &definer, &leaky, &cost, &dependents); err != nil {
			t.Fatalf("read %s: %v", tc.fn, err)
		}
		if config != nil {
			t.Errorf("%s has settings %q, want none (proconfig IS NULL)", tc.fn, config)
		}
		got := fmt.Sprintf("lang=%s returns=%s args=%s volatile=%s parallel=%s strict=%v definer=%v leakproof=%v cost=%v",
			lang, returns, args, volatile, parallel, strict, definer, leaky, cost)
		want := fmt.Sprintf("lang=plpgsql returns=%s args=text volatile=i parallel=s strict=false definer=false leakproof=false cost=100",
			tc.returns)
		if got != want {
			t.Errorf("%s attributes:\n got %s\nwant %s", tc.fn, got, want)
		}
		if dependents != 0 {
			t.Errorf("%d objects depend on %s", dependents, tc.fn)
		}
	}
}

// The plugin's pool puts public ahead of pg_catalog, so an object planted in
// public wins over pg_catalog's even at an exact match. A role granted CREATE
// on public after startup plants what the two bodies use: the operators
// *(numeric, integer), =(float8, float8) and !~(text, text), the function
// floor(numeric), and domains named float8 and timestamptz whose checks run on
// every cast to them. Each records its use. A temporal and a numeric sort
// through the plugin's search then use none of them, and sort correctly.
func TestPluginFunctions_PlantedObjectsAreNotUsedBySearch(t *testing.T) {
	dsn := freshCTDatabase(t)
	pathFirst := func(c *pgxpool.Config) { c.ConnConfig.RuntimeParams["search_path"] = "public, pg_catalog" }
	pool := newCTPool(t, dsn, 10, pathFirst)
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	f := postgres.NewStoreFactory(pool)
	f.InitTransactionManager(newTestUUIDGenerator())
	ctx := ctxWithTenant("fn-path-tenant")
	store, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	ref := spi.ModelRef{EntityName: "item", ModelVersion: "1"}
	// Temporal order z, m, a; numeric order a, z, m; id order a, m, z.
	for _, e := range []struct{ id, at, n string }{
		{"z", "2020-01-01T00:00:00Z", "2"},
		{"m", "2020-06-15T12:00:00Z", "3"},
		{"a", "2021-01-01T00:00:00Z", "1"},
	} {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: e.id, ModelRef: ref, State: "NEW"},
			Data: []byte(fmt.Sprintf(`{"at":%q,"n":%s}`, e.at, e.n)),
		}); err != nil {
			t.Fatalf("Save %s: %v", e.id, err)
		}
	}

	owner := newCTPool(t, dsn, 2, nil)
	role := "cyoda_fn_planter_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	ident := pgx.Identifier{role}.Sanitize()
	for _, stmt := range []string{
		`CREATE ROLE ` + ident + ` LOGIN PASSWORD 'probe' NOSUPERUSER`,
		`GRANT CREATE ON SCHEMA public TO ` + ident,
	} {
		if _, err := owner.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("provision the role: %v", err)
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
	u.User = url.UserPassword(role, "probe")
	planter := newCTPool(t, u.String(), 1, pathFirst)
	if _, err := planter.Exec(context.Background(), `
		CREATE TABLE public.planted_calls (op pg_catalog.text);
		GRANT INSERT ON public.planted_calls TO PUBLIC;
		CREATE FUNCTION public.planted(op pg_catalog.text) RETURNS pg_catalog.bool LANGUAGE plpgsql AS $f$
		  BEGIN INSERT INTO public.planted_calls VALUES (op); RETURN true; END $f$;
		CREATE FUNCTION public.planted_mul(pg_catalog.numeric, pg_catalog.int4) RETURNS pg_catalog.numeric LANGUAGE plpgsql AS $f$
		  BEGIN PERFORM public.planted('*(numeric, integer)'); RETURN $1 OPERATOR(pg_catalog.*) $2::pg_catalog.numeric; END $f$;
		CREATE OPERATOR public.* (LEFTARG = pg_catalog.numeric, RIGHTARG = pg_catalog.int4, FUNCTION = public.planted_mul);
		CREATE FUNCTION public.planted_eq(pg_catalog.float8, pg_catalog.float8) RETURNS pg_catalog.bool LANGUAGE plpgsql AS $f$
		  BEGIN PERFORM public.planted('=(float8, float8)'); RETURN $1 OPERATOR(pg_catalog.=) $2; END $f$;
		CREATE OPERATOR public.= (LEFTARG = pg_catalog.float8, RIGHTARG = pg_catalog.float8, FUNCTION = public.planted_eq);
		CREATE FUNCTION public.planted_nre(pg_catalog.text, pg_catalog.text) RETURNS pg_catalog.bool LANGUAGE plpgsql AS $f$
		  BEGIN PERFORM public.planted('!~(text, text)'); RETURN $1 OPERATOR(pg_catalog.!~) $2; END $f$;
		CREATE OPERATOR public.!~ (LEFTARG = pg_catalog.text, RIGHTARG = pg_catalog.text, FUNCTION = public.planted_nre);
		CREATE FUNCTION public.floor(pg_catalog.numeric) RETURNS pg_catalog.numeric LANGUAGE plpgsql AS $f$
		  BEGIN PERFORM public.planted('floor(numeric)'); RETURN pg_catalog.floor($1); END $f$;
		CREATE DOMAIN public.float8 AS pg_catalog.float8 CHECK (public.planted('domain float8'));
		CREATE DOMAIN public.timestamptz AS pg_catalog.timestamptz CHECK (public.planted('domain timestamptz'));`); err != nil {
		t.Fatalf("plant the objects: %v", err)
	}

	for _, tc := range []struct {
		name string
		spec spi.OrderSpec
		want []string
	}{
		{"temporal", spi.OrderSpec{Path: "at", Source: spi.SourceData, Kind: spi.OrderTemporal}, []string{"z", "m", "a"}},
		{"numeric", spi.OrderSpec{Path: "n", Source: spi.SourceData, Kind: spi.OrderNumeric}, []string{"a", "z", "m"}},
	} {
		results, err := store.Search(ctx,
			spi.Filter{Op: spi.FilterNotNull, Path: "at", Source: spi.SourceData},
			spi.SearchOptions{ModelName: "item", ModelVersion: "1", Limit: 10, OrderBy: []spi.OrderSpec{tc.spec}})
		if err != nil {
			t.Fatalf("%s search: %v", tc.name, err)
		}
		var got []string
		for _, r := range results {
			got = append(got, r.Meta.ID)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s order = %v, want %v", tc.name, got, tc.want)
		}
	}

	rows, err := owner.Query(context.Background(), `SELECT op FROM public.planted_calls`)
	if err != nil {
		t.Fatalf("read the planted calls: %v", err)
	}
	called, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the planted calls: %v", err)
	}
	if len(called) != 0 {
		t.Fatalf("a search used planted objects with the plugin's privileges: %v", called)
	}
}
