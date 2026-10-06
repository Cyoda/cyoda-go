package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// rowsQuerier is the one method checkSearchPath needs; *pgxpool.Pool has it.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// unsafeSearchPathSQL lists each CREATE grant, on a schema of the session's
// effective search path, that lets a role other than the schema's owner create
// objects there. Each row is the schema, the grantee (PUBLIC, or the role's
// name), and the statement that revokes the grant.
//
// The plugin's SQL names its tables, functions and operators without a schema,
// and PostgreSQL chooses a function or operator by the best argument match
// across every schema on the path. So a role that may create in any of them can
// plant an overload that the plugin's own statements — and its migrations —
// then run with the plugin's or the migration role's privileges. This is
// PostgreSQL's "secure schema usage pattern"; it cannot be met call site by
// call site.
//
// current_schemas(true) is the path as the session resolves it: the schemas
// that exist, with the implicit ones — pg_catalog, searched first when the
// path does not name it, and the session's temporary schema if it has one —
// included. Both are checked like any other schema, which costs nothing.
// An ACL that was never changed is NULL in pg_namespace; acldefault gives the
// rights it stands for (the owner's alone). Grantee 0 is PUBLIC.
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
const unsafeSearchPathSQL = `
SELECT pg_catalog.quote_ident(n.nspname::pg_catalog.text),
       CASE WHEN a.grantee OPERATOR(pg_catalog.=) 0::pg_catalog.oid THEN 'PUBLIC'
            ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(a.grantee)::pg_catalog.text) END
  FROM pg_catalog.unnest(pg_catalog.current_schemas(true)) WITH ORDINALITY AS p(nspname, pos)
  JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) p.nspname
 CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n'::pg_catalog."char", n.nspowner))) a
 WHERE a.privilege_type OPERATOR(pg_catalog.=) 'CREATE'::pg_catalog.text
   AND CASE
         WHEN a.grantee OPERATOR(pg_catalog.=) n.nspowner THEN false
         WHEN a.grantee OPERATOR(pg_catalog.=) 0::pg_catalog.oid THEN true
         WHEN (SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid OPERATOR(pg_catalog.=) a.grantee) THEN false
         WHEN pg_catalog.pg_has_role(a.grantee, n.nspowner, 'USAGE'::pg_catalog.text) THEN false
         ELSE true
       END
 ORDER BY p.pos, 2`

// checkSearchPath refuses when any schema on q's session search_path grants
// CREATE to a role other than its owner (see unsafeSearchPathSQL). It runs on
// every start, before anything is migrated and whether or not this node
// migrates. A grant made after the check passed is not seen until the next
// start.
//
// The refusal names each schema and grantee and the REVOKE that removes the
// grant. It carries no connection detail: schema and role names only.
func checkSearchPath(ctx context.Context, q rowsQuerier) error {
	rows, err := q.Query(ctx, unsafeSearchPathSQL)
	if err != nil {
		return fmt.Errorf("postgres: check the search_path schemas: %w", err)
	}
	var found, remedies []string
	for rows.Next() {
		var schema, grantee string
		if err := rows.Scan(&schema, &grantee); err != nil {
			rows.Close()
			return fmt.Errorf("postgres: check the search_path schemas: %w", err)
		}
		found = append(found, fmt.Sprintf("schema %s grants CREATE to %s", schema, grantee))
		remedies = append(remedies, fmt.Sprintf("REVOKE CREATE ON SCHEMA %s FROM %s;", schema, grantee))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: check the search_path schemas: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("postgres: refusing to migrate or start: on this connection's search_path, %s. "+
		"A role that may create objects in a schema on the search_path can make this node's SQL "+
		"run its code with this node's privileges. Revoke each grant, then restart: %s",
		strings.Join(found, "; "), strings.Join(remedies, " "))
}
