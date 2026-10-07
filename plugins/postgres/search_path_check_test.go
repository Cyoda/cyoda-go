package postgres

// search_path_check_test.go — the plugin refuses to migrate or start when the
// database or a schema on its connection's search_path grants CREATE to a role
// outside the trusted set (rule c). Such a role could plant a function or
// operator (or, on the database, a schema the path puts first) that the
// plugin's unqualified SQL would then call with the plugin's own privileges.
// The ownership rules (a, b, d) and the deployments that must start are in
// search_path_trust_test.go.
//
// Each test takes a database of its own, so a grant on it or on its public
// schema reaches nothing else. The default public schema of PostgreSQL 15 and
// later grants CREATE only to its owner, so a test that wants PostgreSQL 14's
// default grants it to PUBLIC itself.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// execAs runs each statement on a short-lived pool on dsn.
func execAs(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	pool := openPool(t, dsn)
	defer pool.Close()
	for _, s := range stmts {
		if _, err := pool.Exec(context.Background(), s); err != nil {
			t.Fatalf("%s on %s: %v", s, dsnDatabase(dsn), err)
		}
	}
}

// newRole creates a role for one test, with the given attributes, and drops it
// afterwards together with everything it owns or was granted in dsn's
// database. It returns the role's name and its quoted form.
func newRole(t *testing.T, dsn, prefix, attrs string) (name, ident string) {
	t.Helper()
	name = prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	ident = pgx.Identifier{name}.Sanitize()
	execAs(t, dsn, `CREATE ROLE `+ident+` `+attrs)
	t.Cleanup(func() {
		admin, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Errorf("open a pool to drop role %s: %v", name, err)
			return
		}
		defer admin.Close()
		for _, s := range []string{`DROP OWNED BY ` + ident + ` CASCADE`, `DROP ROLE ` + ident} {
			if _, err := admin.Exec(context.Background(), s); err != nil {
				t.Errorf("%s: %v", s, err)
			}
		}
	})
	return name, ident
}

// dsnAs returns dsn connecting as user with password.
func dsnAs(t *testing.T, dsn, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", errors.Unwrap(err))
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// migrationsRan reports whether any migration has touched dsn's database.
func migrationsRan(t *testing.T, dsn string) bool {
	t.Helper()
	db := openStdlibDB(t, dsn)
	defer db.Close()
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('schema_migrations')::text`).Scan(&reg); err != nil {
		t.Fatalf("look for schema_migrations: %v", err)
	}
	return reg.Valid
}

// startupPaths are the routes into the schema sequence: a node booting with
// AutoMigrate, a node booting without it against a migrated schema, the
// plugin factory, and the `cyoda migrate` subcommand.
var startupPaths = []struct {
	name    string
	migrate bool // whether the path migrates; false needs a migrated schema
	run     func(t *testing.T, dsn string) error
}{
	{"ensureSchema with AutoMigrate", true, func(t *testing.T, dsn string) error {
		return ensureSchema(context.Background(), openPool(t, dsn), true, 5*time.Minute)
	}},
	{"ensureSchema without AutoMigrate", false, func(t *testing.T, dsn string) error {
		return ensureSchema(context.Background(), openPool(t, dsn), false, 5*time.Minute)
	}},
	{"plugin factory", true, func(t *testing.T, dsn string) error {
		f, err := (&plugin{}).NewFactory(context.Background(),
			ceilingEnv(dsn, map[string]string{"CYODA_POSTGRES_MIN_CONNS": "0"}))
		if err == nil {
			_ = f.Close()
		}
		return err
	}},
	{"plugin factory without AutoMigrate", false, func(t *testing.T, dsn string) error {
		f, err := (&plugin{}).NewFactory(context.Background(),
			ceilingEnv(dsn, map[string]string{"CYODA_POSTGRES_MIN_CONNS": "0", "CYODA_POSTGRES_AUTO_MIGRATE": "false"}))
		if err == nil {
			_ = f.Close()
		}
		return err
	}},
	{"cyoda migrate", true, func(t *testing.T, dsn string) error {
		return RunMigrateWithDSN(context.Background(), dsn)
	}},
	{"Migrate fixture", true, func(t *testing.T, dsn string) error {
		pool := openPool(t, dsn)
		return Migrate(pool)
	}},
}

// assertRefused checks a refusal: it names the object ("schema public",
// "database x") and the grantee, gives the REVOKE that fixes it, and carries
// nothing from the connection string.
func assertRefused(t *testing.T, err error, dsn, object, grantee, remedy string) {
	t.Helper()
	assertRefusal(t, err, dsn, object+" grants CREATE to "+grantee, remedy)
}

// assertRefusal checks that err is a refusal containing every want, and that
// it carries nothing from the connection string.
func assertRefusal(t *testing.T, err error, dsn string, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("started although the refusal %q was due", wants)
	}
	msg := err.Error()
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not contain %q: %s", want, msg)
		}
	}
	// The test passwords ("cyoda", "probe") are also parts of role names, so
	// the check looks for the credentials as the DSN carries them.
	u, _ := url.Parse(dsn)
	for _, leak := range []string{"postgres://", u.User.String() + "@", u.Host, "password"} {
		if strings.Contains(msg, leak) {
			t.Errorf("refusal carries connection detail %q: %s", leak, msg)
		}
	}
}

// PUBLIC holding CREATE on a schema in the search path — PostgreSQL 14's
// default for public — refuses every startup path, and a migrating path
// refuses before it migrates anything.
func TestSearchPathCheck_RefusesCreateForPublic(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			execAs(t, dsn, `GRANT CREATE ON SCHEMA public TO PUBLIC`)

			err := p.run(t, dsn)
			assertRefused(t, err, dsn, "schema public", "PUBLIC", "REVOKE CREATE ON SCHEMA public FROM PUBLIC")
			if p.migrate && migrationsRan(t, dsn) {
				t.Error("the refused path migrated the database before refusing")
			}
		})
	}
}

// A named role other than the owner holding CREATE is refused the same way.
func TestSearchPathCheck_RefusesCreateForANamedRole(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			name, ident := newRole(t, dsn, "Planter_", "NOLOGIN")
			execAs(t, dsn, `GRANT CREATE ON SCHEMA public TO `+ident)

			err := p.run(t, dsn)
			assertRefused(t, err, dsn, "schema public", `"`+name+`"`, `REVOKE CREATE ON SCHEMA public FROM "`+name+`"`)
		})
	}
}

// After the REVOKE, every path starts.
func TestSearchPathCheck_StartsAfterTheRevoke(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			execAs(t, dsn,
				`GRANT CREATE ON SCHEMA public TO PUBLIC`,
				`REVOKE CREATE ON SCHEMA public FROM PUBLIC`)
			if err := p.run(t, dsn); err != nil {
				t.Fatalf("refused after the REVOKE: %v", err)
			}
		})
	}
}

// A schema that grants CREATE to PUBLIC but is not on the search path does
// not stop a start: nothing the plugin names resolves there.
func TestSearchPathCheck_IgnoresASchemaOutsideTheSearchPath(t *testing.T) {
	dsn := freshDatabase(t)
	execAs(t, dsn,
		`CREATE SCHEMA elsewhere`,
		`GRANT CREATE ON SCHEMA elsewhere TO PUBLIC`)
	if err := ensureSchema(context.Background(), openPool(t, dsn), true, 5*time.Minute); err != nil {
		t.Fatalf("refused over a schema outside the search path: %v", err)
	}
}

// A schema the search path names later than the first is checked too.
func TestSearchPathCheck_ChecksEverySchemaOnThePath(t *testing.T) {
	dsn := freshDatabase(t)
	execAs(t, dsn,
		`CREATE SCHEMA later`,
		`GRANT CREATE ON SCHEMA later TO PUBLIC`)
	pathDSN := dsnWithParam(t, dsn, "search_path", "public,later")
	err := ensureSchema(context.Background(), openPool(t, pathDSN), true, 5*time.Minute)
	assertRefused(t, err, pathDSN, "schema later", "PUBLIC", "REVOKE CREATE ON SCHEMA later FROM PUBLIC")
}

// pg_catalog is searched first even when the path does not name it, so its
// ACL is checked as well.
func TestSearchPathCheck_ChecksPgCatalog(t *testing.T) {
	dsn := freshDatabase(t)
	execAs(t, dsn, `GRANT CREATE ON SCHEMA pg_catalog TO PUBLIC`)
	err := checkSearchPathTrust(context.Background(), openPool(t, dsn))
	assertRefused(t, err, dsn, "schema pg_catalog", "PUBLIC", "REVOKE CREATE ON SCHEMA pg_catalog FROM PUBLIC")
}

// CREATE on the database lets a role create a schema. One named after the
// plugin's role is put first on the default search_path by "$user", ahead of
// every schema the check has seen, and is owned by that role, so the check
// refuses CREATE on the current database for any role but its owner, on every
// path, before anything is migrated.
func TestSearchPathCheck_RefusesDatabaseCreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named bool
	}{{"PUBLIC", false}, {"a named role", true}} {
		for _, p := range startupPaths {
			t.Run(tc.name+"/"+p.name, func(t *testing.T) {
				dsn := freshDatabase(t)
				if !p.migrate {
					migrateToHead(t, dsn)
				}
				db := dsnDatabase(dsn)
				grantee, ident := "PUBLIC", "PUBLIC"
				if tc.named {
					name, quoted := newRole(t, dsn, "Schema_Maker_", "NOLOGIN")
					grantee, ident = `"`+name+`"`, quoted
				}
				execAs(t, dsn, `GRANT CREATE ON DATABASE `+pgx.Identifier{db}.Sanitize()+` TO `+ident)

				err := p.run(t, dsn)
				assertRefused(t, err, dsn, "database "+db, grantee, "REVOKE CREATE ON DATABASE "+db+" FROM "+grantee)
				if p.migrate && migrationsRan(t, dsn) {
					t.Error("the refused path migrated the database before refusing")
				}
			})
		}
	}
}

// After the REVOKE on the database, every path starts.
func TestSearchPathCheck_StartsAfterTheDatabaseRevoke(t *testing.T) {
	for _, p := range startupPaths {
		t.Run(p.name, func(t *testing.T) {
			dsn := freshDatabase(t)
			if !p.migrate {
				migrateToHead(t, dsn)
			}
			db := pgx.Identifier{dsnDatabase(dsn)}.Sanitize()
			execAs(t, dsn,
				`GRANT CREATE ON DATABASE `+db+` TO PUBLIC`,
				`REVOKE CREATE ON DATABASE `+db+` FROM PUBLIC`)
			if err := p.run(t, dsn); err != nil {
				t.Fatalf("refused after the REVOKE: %v", err)
			}
		})
	}
}
