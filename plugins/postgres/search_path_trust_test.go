package postgres

// search_path_trust_test.go — the ownership rules of the start check, and the
// deployments that must still start.
//
// The trusted set: superusers; the connecting role and every role whose
// privileges it inherits; the owner of the plugin's tables (schema_migrations);
// pg_database_owner when the database's owner is trusted. The check refuses
// when (a) the database's owner, (b) the owner of a schema on the search path,
// (c) a CREATE grantee on either (search_path_check_test.go) or (d) the owner
// of an object in such a schema is outside that set.
//
// Each test takes a database of its own. Roles are cluster-wide, so each test
// creates its own and drops it with everything it owns there.

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ownDatabase makes role the owner of dsn's database, and hands it back to the
// connecting superuser before the role is dropped.
func ownDatabase(t *testing.T, dsn, roleIdent string) {
	t.Helper()
	db := pgx.Identifier{dsnDatabase(dsn)}.Sanitize()
	execAs(t, dsn, `ALTER DATABASE `+db+` OWNER TO `+roleIdent)
	t.Cleanup(func() { execAs(t, dsn, `ALTER DATABASE `+db+` OWNER TO `+superuserIdent(t, dsn)) })
}

// superuserName is the role the test DSN connects as: the server's superuser.
func superuserName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return u.User.Username()
}

func superuserIdent(t *testing.T, dsn string) string {
	return pgx.Identifier{superuserName(t, dsn)}.Sanitize()
}

// runtimeGrants are what a non-owner runtime role is given in a two-role
// deployment: use of the schema, the plugin's tables and sequences, nothing
// to create with. They include the grants POSTGRES.md "Roles" lists for the
// consistency time objects.
func runtimeGrants(roleIdent string) []string {
	return []string{
		`GRANT USAGE ON SCHEMA public TO ` + roleIdent,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + roleIdent,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO ` + roleIdent,
	}
}

// startRuntime starts the plugin as a runtime node that does not migrate.
func startRuntime(t *testing.T, dsn string) error {
	t.Helper()
	f, err := (&plugin{}).NewFactory(context.Background(),
		ceilingEnv(dsn, map[string]string{"CYODA_POSTGRES_MIN_CONNS": "0", "CYODA_POSTGRES_AUTO_MIGRATE": "false"}))
	if err == nil {
		_ = f.Close()
	}
	return err
}

// (a) A database owned by a role outside the trusted set is refused on every
// path, before anything is migrated: its owner may create a schema the path
// puts first, and owns public on PostgreSQL 15 and later. This is the layout
// where the runtime role owns the database and a superuser migrates.
func TestTrustedSet_RefusesAnUntrustedDatabaseOwner(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			app, appIdent := newRole(t, dsn, "cyoda_app_", "NOLOGIN")
			ownDatabase(t, dsn, appIdent)

			err := p.run(t, dsn)
			db := dsnDatabase(dsn)
			assertRefusal(t, err, dsn,
				"database "+db+" is owned by "+app,
				"ALTER DATABASE "+db+" OWNER TO "+superuserName(t, dsn)+";")
			if p.migrate && migrationsRan(t, dsn) {
				t.Error("the refused path migrated the database before refusing")
			}
		})
	}
}

// (b) A schema on the search path owned by a role outside the trusted set is
// refused, on every path.
func TestTrustedSet_RefusesAnUntrustedSchemaOwner(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			mallory, malloryIdent := newRole(t, dsn, "cyoda_mallory_", "NOLOGIN")
			execAs(t, dsn,
				`CREATE SCHEMA app AUTHORIZATION `+malloryIdent,
				`GRANT USAGE ON SCHEMA app TO PUBLIC`)

			pathDSN := dsnWithParam(t, dsn, "search_path", "public, app")
			err := p.run(t, pathDSN)
			// The connecting superuser owns the database, so a schema is handed
			// to pg_database_owner.
			assertRefusal(t, err, pathDSN,
				"schema app is owned by "+mallory,
				"ALTER SCHEMA app OWNER TO pg_database_owner;")
		})
	}
}

// (b) The "$user" schema is checked although the connecting role has no USAGE
// on it, which keeps it off the active path: its owner could grant USAGE at
// any time after start, and it would come first.
func TestTrustedSet_ChecksTheUserSchemaWithoutUsage(t *testing.T) {
	dsn := freshDatabase(t)
	rt, _ := newRole(t, dsn, "cyoda_rt_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
	mallory, malloryIdent := newRole(t, dsn, "cyoda_mallory_", "NOLOGIN")
	execAs(t, dsn, `CREATE SCHEMA `+pgx.Identifier{rt}.Sanitize()+` AUTHORIZATION `+malloryIdent)

	rtPool := openPool(t, dsnAs(t, dsn, rt, "probe"))
	var active []string
	if err := rtPool.QueryRow(context.Background(), `SELECT current_schemas(false)::text[]`).Scan(&active); err != nil {
		t.Fatalf("read the active path: %v", err)
	}
	for _, s := range active {
		if s == rt {
			t.Fatalf("the schema %s is on the active path %v; the test needs it off", rt, active)
		}
	}
	err := checkSearchPathTrust(context.Background(), rtPool)
	assertRefusal(t, err, dsn, "schema "+rt+" is owned by "+mallory)
}

// (c) Over the trusted set: a CREATE grant to a superuser or to a role the
// connecting role inherits is allowed; a grant to a role that inherits the
// connecting role, which is not in the set, is refused.
func TestTrustedSet_CreateGrantees(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attrs   string
		link    func(granteeIdent, rtIdent string) string
		refused bool
	}{
		{"superuser", "NOLOGIN SUPERUSER", nil, false},
		{"a role the connecting role inherits", "NOLOGIN", func(g, rt string) string { return `GRANT ` + g + ` TO ` + rt }, false},
		{"a role that inherits the connecting role", "NOLOGIN", func(g, rt string) string { return `GRANT ` + rt + ` TO ` + g }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			rt, rtIdent := newRole(t, dsn, "cyoda_rt_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
			grantee, granteeIdent := newRole(t, dsn, "cyoda_grantee_", tc.attrs)
			stmts := []string{
				`CREATE SCHEMA app AUTHORIZATION ` + rtIdent,
				`GRANT CREATE ON SCHEMA app TO ` + granteeIdent,
			}
			if tc.link != nil {
				stmts = append(stmts, tc.link(granteeIdent, rtIdent))
			}
			execAs(t, dsn, stmts...)

			rtDSN := dsnWithParam(t, dsnAs(t, dsn, rt, "probe"), "search_path", "app")
			err := checkSearchPathTrust(context.Background(), openPool(t, rtDSN))
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused a grantee in the trusted set: %v", err)
				}
				return
			}
			assertRefused(t, err, rtDSN, "schema app", grantee, "REVOKE CREATE ON SCHEMA app FROM "+grantee+";")
		})
	}
}

// (d) Objects another role created in public while it held CREATE survive the
// REVOKE. A planted function and operator owned by that role refuse every
// path after the REVOKE, before anything is migrated.
func TestTrustedSet_RefusesObjectsPlantedBeforeTheRevoke(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			mallory, malloryIdent := newRole(t, dsn, "cyoda_mallory_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
			execAs(t, dsn, `GRANT CREATE ON SCHEMA public TO `+malloryIdent)
			execAs(t, dsnAs(t, dsn, mallory, "probe"),
				`CREATE FUNCTION public.planted_fn(pg_catalog.numeric) RETURNS pg_catalog.numeric LANGUAGE sql AS 'SELECT $1'`,
				`CREATE FUNCTION public.planted_mul(pg_catalog.numeric, pg_catalog.int4) RETURNS pg_catalog.numeric LANGUAGE sql
				   AS 'SELECT $1 OPERATOR(pg_catalog.*) $2::pg_catalog.numeric'`,
				`CREATE OPERATOR public.* (LEFTARG = pg_catalog.numeric, RIGHTARG = pg_catalog.int4, FUNCTION = public.planted_mul)`)
			execAs(t, dsn, `REVOKE CREATE ON SCHEMA public FROM `+malloryIdent)

			err := p.run(t, dsn)
			assertRefusal(t, err, dsn,
				"planted_fn(numeric) in schema public is owned by "+mallory,
				"*(numeric,integer) in schema public is owned by "+mallory,
				"Drop each object listed")
			if p.migrate && migrationsRan(t, dsn) {
				t.Error("the refused path migrated the database before refusing")
			}
		})
	}
}

// The two-role deployment on PostgreSQL 15 and later: the owner role owns the
// database, so public (owned by pg_database_owner) is its; it migrates, and the
// runtime role, holding only the runtime grants, starts. A CREATE grant on
// public to the runtime role then refuses the owner's next migration: the
// runtime role could plant what the owner's migration runs.
func TestTrustedSet_TwoRoleDeploymentStarts(t *testing.T) {
	dsn := freshDatabase(t)
	owner, ownerIdent := newRole(t, dsn, "cyoda_owner_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
	rt, rtIdent := newRole(t, dsn, "cyoda_rt_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
	ownDatabase(t, dsn, ownerIdent)
	ownerDSN := dsnAs(t, dsn, owner, "probe")
	rtDSN := dsnAs(t, dsn, rt, "probe")

	if err := RunMigrateWithDSN(context.Background(), ownerDSN); err != nil {
		t.Fatalf("the owner role's migration: %v", err)
	}
	execAs(t, ownerDSN, runtimeGrants(rtIdent)...)
	if err := startRuntime(t, rtDSN); err != nil {
		t.Fatalf("the runtime role's start: %v", err)
	}
	if err := RunMigrateWithDSN(context.Background(), ownerDSN); err != nil {
		t.Fatalf("the owner role's second migration: %v", err)
	}

	execAs(t, dsn, `GRANT CREATE ON SCHEMA public TO `+rtIdent)
	err := RunMigrateWithDSN(context.Background(), ownerDSN)
	assertRefused(t, err, ownerDSN, "schema public", rt, "REVOKE CREATE ON SCHEMA public FROM "+rt+";")
}

// The two-role deployment on PostgreSQL 14's layout: public owned by the
// bootstrap superuser, with CREATE for PUBLIC. The refusal names the procedure:
// first give public to the migrating role (to pg_database_owner when that role
// owns the database), then revoke PUBLIC's CREATE. The REVOKE alone leaves a
// migrating role that is no superuser unable to create in public. After the
// procedure the owner migrates and the runtime role starts.
func TestTrustedSet_PG14ProcedureLetsTheTwoRoleDeploymentStart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ownsDB bool
	}{{"the migrating role owns the database", true}, {"a superuser owns the database", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			owner, ownerIdent := newRole(t, dsn, "cyoda_owner_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
			rt, rtIdent := newRole(t, dsn, "cyoda_rt_", "LOGIN PASSWORD 'probe' NOSUPERUSER")
			if tc.ownsDB {
				ownDatabase(t, dsn, ownerIdent)
			}
			execAs(t, dsn,
				`ALTER SCHEMA public OWNER TO `+superuserIdent(t, dsn),
				`GRANT CREATE ON SCHEMA public TO PUBLIC`)
			ownerDSN := dsnAs(t, dsn, owner, "probe")
			target := owner
			if tc.ownsDB {
				target = "pg_database_owner"
			}

			err := RunMigrateWithDSN(context.Background(), ownerDSN)
			t.Logf("refusal: %v", err)
			assertRefusal(t, err, ownerDSN,
				"schema public grants CREATE to PUBLIC",
				"ALTER SCHEMA public OWNER TO "+target+"; REVOKE CREATE ON SCHEMA public FROM PUBLIC;")

			// The REVOKE alone.
			execAs(t, dsn, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
			err = RunMigrateWithDSN(context.Background(), ownerDSN)
			if err == nil || !strings.Contains(err.Error(), "permission denied for schema public") {
				t.Fatalf("after the REVOKE alone, the owner role's migration: %v; want permission denied for schema public", err)
			}

			execAs(t, dsn, `ALTER SCHEMA public OWNER TO `+target)
			if err := RunMigrateWithDSN(context.Background(), ownerDSN); err != nil {
				t.Fatalf("the owner role's migration after the procedure: %v", err)
			}
			execAs(t, ownerDSN, runtimeGrants(rtIdent)...)
			if err := startRuntime(t, dsnAs(t, dsn, rt, "probe")); err != nil {
				t.Fatalf("the runtime role's start after the procedure: %v", err)
			}
		})
	}
}

// BenchmarkCheckSearchPathTrust measures the start check on a database migrated
// to head, with every object the plugin creates in it: the cost every start
// pays once.
func BenchmarkCheckSearchPathTrust(b *testing.B) {
	ctx := context.Background()
	base := os.Getenv("CYODA_TEST_DB_URL")
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		b.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	name := "cyoda_bench_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		b.Fatalf("create database: %v", err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)") }()
	u, err := url.Parse(base)
	if err != nil {
		b.Fatalf("parse CYODA_TEST_DB_URL: %v", errors.Unwrap(err))
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		b.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	if err := runMigrations(ctx, pool, defaultMigrateLockTimeout); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	for b.Loop() {
		if err := checkSearchPathTrust(ctx, pool); err != nil {
			b.Fatalf("check: %v", err)
		}
	}
}
