package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// rowsQuerier is the one method checkCreateGrants needs; *pgxpool.Pool has it.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// unsafeCreateGrantsSQL lists each CREATE grant that lets a role other than
// the object's owner create objects the plugin's SQL could resolve: on the
// current database, and on each schema of the session's effective search_path.
// Each row is the kind ("database" or "schema"), the object's name and the
// grantee (PUBLIC, or the role's name), quoted as identifiers.
//
// The plugin's SQL names its tables, functions and operators without a schema,
// and PostgreSQL chooses a function or operator by the best argument match
// across every schema on the path. So a role that may create in any of them can
// plant an overload that the plugin's own statements — and its migrations —
// then run with the plugin's or the migration role's privileges. This is
// PostgreSQL's "secure schema usage pattern"; it cannot be met call site by
// call site.
//
// CREATE on the database lets a role create a schema. The default path begins
// with "$user", so a schema named after the plugin's role, created and owned by
// that other role, would come first on the path once it exists — after the
// schema check ran, and passing it, since its owner may create in it. So only
// the database's owner may hold CREATE on it.
//
// current_schemas(true) is the path as the session resolves it: the schemas
// that exist, with the implicit ones — pg_catalog, searched first when the
// path does not name it, and the session's temporary schema if it has one —
// included. Both are checked like any other schema, which costs nothing.
// An ACL that was never changed is NULL in the catalog; acldefault gives the
// rights it stands for (for a schema, the owner's alone; for a database, the
// owner's plus PUBLIC's CONNECT and TEMPORARY). Grantee 0 is PUBLIC.
//
// A grant is excused when it gives its grantee nothing the grantee does not
// already hold:
//   - the grantee is the owner;
//   - the grantee is a superuser, who bypasses every privilege check;
//   - the grantee inherits the owner's privileges (pg_has_role USAGE), and so
//     holds CREATE through the owner whether or not the grant exists. Such a
//     role is trusted as far as the owner is; revoking its grant would change
//     nothing. A member that does not inherit (NOINHERIT) is checked like any
//     other role: the grant is a real privilege it would not otherwise use.
//
// The connecting role is not excused for being the connecting role. A
// runtime role with CREATE on a schema it does not own could plant objects
// that the owner's next migration run calls with the owner's privileges.
//
// Every name is qualified with pg_catalog, operators included, so the check
// cannot itself be subverted by what it looks for. The CASE fixes the order of
// evaluation: pg_has_role is only reached for a real role, never for 0.
const unsafeCreateGrantsSQL = `
WITH objects(kind, name, owner, acl, pos) AS (
  SELECT 'database'::pg_catalog.text, d.datname::pg_catalog.text, d.datdba,
         COALESCE(d.datacl, pg_catalog.acldefault('d'::pg_catalog."char", d.datdba)), 0::pg_catalog.int8
    FROM pg_catalog.pg_database d
   WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
  UNION ALL
  SELECT 'schema'::pg_catalog.text, n.nspname::pg_catalog.text, n.nspowner,
         COALESCE(n.nspacl, pg_catalog.acldefault('n'::pg_catalog."char", n.nspowner)), p.pos
    FROM pg_catalog.unnest(pg_catalog.current_schemas(true)) WITH ORDINALITY AS p(nspname, pos)
    JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) p.nspname
)
SELECT o.kind, pg_catalog.quote_ident(o.name),
       CASE WHEN a.grantee OPERATOR(pg_catalog.=) 0::pg_catalog.oid THEN 'PUBLIC'
            ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(a.grantee)::pg_catalog.text) END
  FROM objects o
 CROSS JOIN LATERAL pg_catalog.aclexplode(o.acl) a
 WHERE a.privilege_type OPERATOR(pg_catalog.=) 'CREATE'::pg_catalog.text
   AND CASE
         WHEN a.grantee OPERATOR(pg_catalog.=) o.owner THEN false
         WHEN a.grantee OPERATOR(pg_catalog.=) 0::pg_catalog.oid THEN true
         WHEN (SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid OPERATOR(pg_catalog.=) a.grantee) THEN false
         WHEN pg_catalog.pg_has_role(a.grantee, o.owner, 'USAGE'::pg_catalog.text) THEN false
         ELSE true
       END
 ORDER BY o.pos, 3`

// checkCreateGrants refuses when the current database, or any schema on q's
// session search_path, grants CREATE to a role other than its owner (see
// unsafeCreateGrantsSQL). It runs on every start, before anything is migrated
// and whether or not this node migrates. A grant made after the check passed
// is not seen until the next start.
//
// The refusal names each object and grantee and the REVOKE that removes the
// grant. It carries no connection detail: database, schema and role names
// only.
func checkCreateGrants(ctx context.Context, q rowsQuerier) error {
	rows, err := q.Query(ctx, unsafeCreateGrantsSQL)
	if err != nil {
		return fmt.Errorf("postgres: check CREATE grants: %w", err)
	}
	var found, remedies []string
	for rows.Next() {
		var kind, name, grantee string
		if err := rows.Scan(&kind, &name, &grantee); err != nil {
			rows.Close()
			return fmt.Errorf("postgres: check CREATE grants: %w", err)
		}
		found = append(found, fmt.Sprintf("%s %s grants CREATE to %s", kind, name, grantee))
		remedies = append(remedies, fmt.Sprintf("REVOKE CREATE ON %s %s FROM %s;", strings.ToUpper(kind), name, grantee))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: check CREATE grants: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("postgres: refusing to migrate or start: %s. "+
		"A role that may create objects in the database or in a schema on the connection's search_path "+
		"can make this node's SQL run its code with this node's privileges. Revoke each grant, then restart: %s",
		strings.Join(found, "; "), strings.Join(remedies, " "))
}
