package postgres

// consistency_migration_qualified_test.go — migration 000016 resolves every
// function, operator and type it uses at migration time in pg_catalog, so an
// object planted in a schema on the migration's search_path is neither called
// by the migration nor bound into what it creates.
//
// The startup check refuses to migrate when a role other than a schema's owner
// may create in a schema on the search path, so the plant cannot be made
// before a real migration run. The test therefore applies the migration's SQL
// itself, on a connection whose search_path puts the attacker's schema ahead
// of pg_catalog: then even an object whose signature equals a pg_catalog one
// would win an unqualified lookup, and only a qualified name avoids it.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// plantsFor000016 are, for each name the migration resolves at migration
// time, objects in public that an unqualified use would choose, given the
// search_path "public, pg_catalog". Each function records its call. The
// operators and domains are bound rather than called by the DDL that uses
// them, so their use shows in pg_depend and in the created objects' types.
const plantsFor000016 = `
CREATE TABLE public.planted_calls (fn pg_catalog.text);
GRANT INSERT ON public.planted_calls TO PUBLIC;
CREATE FUNCTION public.planted(fn pg_catalog.text) RETURNS void LANGUAGE sql
  AS $f$ INSERT INTO public.planted_calls VALUES (fn) $f$;

CREATE FUNCTION public.setval(pg_catalog.text, pg_catalog.int8, pg_catalog.bool) RETURNS pg_catalog.int8 LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('setval(text)'); RETURN pg_catalog.setval($1::pg_catalog.regclass, $2, $3); END $f$;
CREATE FUNCTION public.setval(pg_catalog.regclass, pg_catalog.int8, pg_catalog.bool) RETURNS pg_catalog.int8 LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('setval(regclass)'); RETURN pg_catalog.setval($1, $2, $3); END $f$;
CREATE FUNCTION public.nextval(pg_catalog.text) RETURNS pg_catalog.int8 LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('nextval(text)'); RETURN pg_catalog.nextval($1::pg_catalog.regclass); END $f$;
CREATE FUNCTION public.nextval(pg_catalog.regclass) RETURNS pg_catalog.int8 LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('nextval(regclass)'); RETURN pg_catalog.nextval($1); END $f$;
CREATE FUNCTION public.format(pg_catalog.text, pg_catalog.name, pg_catalog.text) RETURNS pg_catalog.text LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('format(text, name, text)'); RETURN pg_catalog.format($1, $2, $3); END $f$;
CREATE FUNCTION public.format(pg_catalog.text, pg_catalog.name) RETURNS pg_catalog.text LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('format(text, name)'); RETURN pg_catalog.format($1, $2); END $f$;
CREATE FUNCTION public.quote_ident(pg_catalog.name) RETURNS pg_catalog.text LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('quote_ident(name)'); RETURN pg_catalog.quote_ident($1::pg_catalog.text); END $f$;
CREATE FUNCTION public.quote_ident(pg_catalog.text) RETURNS pg_catalog.text LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('quote_ident(text)'); RETURN pg_catalog.quote_ident($1); END $f$;
CREATE FUNCTION public.current_schema() RETURNS pg_catalog.name LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('current_schema'); RETURN pg_catalog.current_schema(); END $f$;
CREATE FUNCTION public.current_setting(pg_catalog.text, pg_catalog.bool) RETURNS pg_catalog.text LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('current_setting'); RETURN pg_catalog.current_setting($1, $2); END $f$;
CREATE FUNCTION public.planted_max(pg_catalog.timestamptz, pg_catalog.timestamptz) RETURNS pg_catalog.timestamptz LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('max(timestamptz)'); RETURN GREATEST($1, $2); END $f$;
CREATE AGGREGATE public.max(pg_catalog.timestamptz) (SFUNC = public.planted_max, STYPE = pg_catalog.timestamptz);

CREATE FUNCTION public.planted_mul(pg_catalog.numeric, pg_catalog.int4) RETURNS pg_catalog.numeric LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('*(numeric, integer)'); RETURN $1 OPERATOR(pg_catalog.*) $2::pg_catalog.numeric; END $f$;
CREATE OPERATOR public.* (LEFTARG = pg_catalog.numeric, RIGHTARG = pg_catalog.int4, FUNCTION = public.planted_mul);
CREATE FUNCTION public.planted_mul_n(pg_catalog.numeric, pg_catalog.numeric) RETURNS pg_catalog.numeric LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('*(numeric, numeric)'); RETURN $1 OPERATOR(pg_catalog.*) $2; END $f$;
CREATE OPERATOR public.* (LEFTARG = pg_catalog.numeric, RIGHTARG = pg_catalog.numeric, FUNCTION = public.planted_mul_n);
CREATE FUNCTION public.planted_cat(pg_catalog.text, pg_catalog.text) RETURNS pg_catalog.text LANGUAGE plpgsql
  AS $f$ BEGIN PERFORM public.planted('||(text, text)'); RETURN $1 OPERATOR(pg_catalog.||) $2; END $f$;
CREATE OPERATOR public.|| (LEFTARG = pg_catalog.text, RIGHTARG = pg_catalog.text, FUNCTION = public.planted_cat);
CREATE FUNCTION public.planted_eq(pg_catalog.text, pg_catalog.text) RETURNS pg_catalog.bool LANGUAGE sql
  AS $f$ SELECT $1 OPERATOR(pg_catalog.=) $2 $f$;
CREATE OPERATOR public.= (LEFTARG = pg_catalog.text, RIGHTARG = pg_catalog.text, FUNCTION = public.planted_eq);
CREATE FUNCTION public.planted_gt(pg_catalog.int4, pg_catalog.int4) RETURNS pg_catalog.bool LANGUAGE sql
  AS $f$ SELECT $1 OPERATOR(pg_catalog.>) $2 $f$;
CREATE OPERATOR public.> (LEFTARG = pg_catalog.int4, RIGHTARG = pg_catalog.int4, FUNCTION = public.planted_gt);

CREATE DOMAIN public.int4 AS pg_catalog.int4;
CREATE DOMAIN public.text AS pg_catalog.text;
CREATE DOMAIN public.timestamptz AS pg_catalog.timestamptz;
`

func TestConsistencyTimeMigration_ResolvesNothingOutsidePgCatalog(t *testing.T) {
	dsn := freshDatabase(t)
	migrationDSN := dsnWithParam(t, dsn, "search_path", "public, pg_catalog")
	pool := openPool(t, migrationDSN)
	if err := MigrateToVersionForTest(pool, 15); err != nil {
		t.Fatalf("migrate to 15: %v", err)
	}
	// A seeded stamp, so the floor's seed reads a real max().
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO submit_times (tenant_id, tx_id, submit_time) VALUES ('t', 'tx', pg_catalog.now())`); err != nil {
		t.Fatalf("seed a submit time: %v", err)
	}

	attacker, attackerIdent := newRole(t, dsn, "cyoda_planter_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
	execAs(t, dsn, `GRANT CREATE ON SCHEMA public TO `+attackerIdent)
	execAs(t, dsnWithParam(t, dsnAs(t, dsn, attacker, "probe"), "search_path", "public, pg_catalog"), plantsFor000016)

	up, err := migrationFS.ReadFile("migrations/000016_consistency_time.up.sql")
	if err != nil {
		t.Fatalf("read migration 000016: %v", err)
	}
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// The simple protocol, in one implicit transaction, as golang-migrate runs it.
	if _, err := conn.Conn().PgConn().Exec(context.Background(), string(up)).ReadAll(); err != nil {
		t.Fatalf("apply migration 000016 with the plants in place: %v", err)
	}

	rows, err := pool.Query(context.Background(), `SELECT fn FROM public.planted_calls`)
	if err != nil {
		t.Fatalf("read the planted calls: %v", err)
	}
	called, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the planted calls: %v", err)
	}
	if len(called) != 0 {
		t.Errorf("the migration called planted objects: %v", called)
	}

	// Nothing the migration created is bound to a planted function or
	// operator: a column default, check constraint, policy or function that
	// used one would depend on it.
	rows, err = pool.Query(context.Background(), `
		SELECT pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid) || ' uses ' ||
		       pg_catalog.pg_describe_object(d.refclassid, d.refobjid, 0)
		  FROM pg_catalog.pg_depend d
		 WHERE (d.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
		        AND d.refobjid IN (SELECT oid FROM pg_catalog.pg_proc WHERE proowner = $1::pg_catalog.regrole))
		    OR (d.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
		        AND d.refobjid IN (SELECT oid FROM pg_catalog.pg_operator WHERE oprowner = $1::pg_catalog.regrole))
		    OR (d.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
		        AND d.refobjid IN (SELECT oid FROM pg_catalog.pg_type WHERE typowner = $1::pg_catalog.regrole))
		EXCEPT
		SELECT pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid) || ' uses ' ||
		       pg_catalog.pg_describe_object(d.refclassid, d.refobjid, 0)
		  FROM pg_catalog.pg_depend d
		 WHERE (d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass
		        AND d.objid IN (SELECT oid FROM pg_catalog.pg_proc WHERE proowner = $1::pg_catalog.regrole))
		    OR (d.classid = 'pg_catalog.pg_operator'::pg_catalog.regclass
		        AND d.objid IN (SELECT oid FROM pg_catalog.pg_operator WHERE oprowner = $1::pg_catalog.regrole))
		    OR (d.classid = 'pg_catalog.pg_type'::pg_catalog.regclass
		        AND d.objid IN (SELECT oid FROM pg_catalog.pg_type WHERE typowner = $1::pg_catalog.regrole))`,
		attacker)
	if err != nil {
		t.Fatalf("read the dependencies on planted objects: %v", err)
	}
	bound, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the dependencies on planted objects: %v", err)
	}
	if len(bound) != 0 {
		t.Errorf("objects the migration created are bound to planted objects: %v", bound)
	}

	// The column default names the sequence by OID (a regclass constant), not
	// by text looked up at each call. Which nextval it calls is shown by the
	// dependency check above.
	var def string
	if err := pool.QueryRow(context.Background(), `
		SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid)
		  FROM pg_catalog.pg_attrdef d
		  JOIN pg_catalog.pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
		 WHERE d.adrelid = 'consistency_tenant_keys'::pg_catalog.regclass AND a.attname = 'tenant_key'`).Scan(&def); err != nil {
		t.Fatalf("read the tenant_key default: %v", err)
	}
	if want := "nextval('consistency_tenant_key_seq'::regclass)"; !strings.Contains(def, want) {
		t.Errorf("tenant_key default = %s, want a call of %s", def, want)
	}

	// The floor was seeded by the real setval, from the real max().
	var floor int64
	if err := pool.QueryRow(context.Background(),
		`SELECT last_value FROM cyoda_stamp_floor`).Scan(&floor); err != nil {
		t.Fatalf("read the floor: %v", err)
	}
	if floor == 0 {
		t.Error("the floor was not seeded from the stored submit time")
	}
}
