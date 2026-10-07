package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// rowsQuerier is the one method checkSearchPathTrust needs; *pgxpool.Pool has it.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// untrustedSearchPathSQL finds everything on the connection's name-resolution
// path that a role outside the trusted set controls. One row per finding: the
// rule, the finding, its remedy, and the role a remedy hands ownership to.
//
// Why: the plugin's SQL names its tables, functions, operators and types
// without a schema, and PostgreSQL resolves such a name across every schema on
// the search_path — a function or operator by the best argument match, so
// pg_catalog coming first decides only ties. Whoever can put an object on that
// path can make the plugin's statements, and its migrations, run code with the
// plugin's or the migration role's privileges. This is PostgreSQL's "secure
// schema usage pattern"; it cannot be met call site by call site.
//
// The trusted set T:
//   - superusers;
//   - the connecting role, and every role whose privileges it inherits
//     (pg_has_role USAGE). USAGE rather than MEMBER: it is the narrower set,
//     and exactly the roles whose objects the connecting role can already
//     alter or drop with its own privileges. A superuser connecting role is
//     a member of every role, so for it this rule adds only itself — trusting
//     every role it nominally inherits would trust every role there is;
//   - the owner of the plugin's tables: the owner of golang-migrate's
//     schema_migrations in current_schema(), when it exists. On a fresh
//     database there is none, and the connecting role, about to own them,
//     is already in T. This lets the runtime role of a two-role deployment
//     trust the role that migrated;
//   - pg_database_owner, which stands for the database's owner, only when
//     that owner is in T.
//
// PUBLIC is never in T.
//
// The path: current_schemas(false) — the schemas that exist and that the
// connecting role may use, in search order, "$user" expanded. These are
// exactly the schemas this role's name resolution reaches. A schema the
// setting names but current_schemas(false) leaves out either does not exist or
// is not usable by this role, so it takes no part in this role's name
// resolution; every role that runs the plugin, the migrating role included,
// runs its own check. pg_catalog is added: it is searched first when the path
// does not name it. The session's temporary schema holds only the session's
// own objects and is never searched for functions or operators.
//
// The rules:
//
//	a  the database's owner is not in T: it may create a schema the path puts
//	   first ("$user"), and owns public on PostgreSQL 15 and later;
//	b  a schema on the path is owned by a role not in T;
//	c  the database or a schema on the path grants CREATE to a role not in T,
//	   PUBLIC included (the owner's own entry is rule a or b);
//	d  an object in a schema on the path is owned by a role not in T. Revoking
//	   a grant does not remove what was created while it was held.
//
// Rule d reads every catalog whose objects live in a schema and have an
// owner: pg_class (tables, views, sequences, materialized views, foreign and
// partitioned tables, composite types; not indexes, which belong to their
// table), pg_proc (functions, procedures, aggregates), pg_operator, pg_type
// (types and domains; not the array and row types PostgreSQL creates for
// another object), pg_collation, pg_conversion (a default conversion is
// found by path), pg_opclass and pg_opfamily (creating them needs a
// superuser, but a default operator class decides how a type sorts),
// pg_ts_config and pg_ts_dict, and pg_statistic_ext. pg_ts_parser and
// pg_ts_template have no owner and need a superuser. Casts are global and
// named by type, so an untrusted type covers them.
//
// Every name is qualified with pg_catalog, operators included, so the check
// cannot itself be subverted by what it looks for.
const untrustedSearchPathSQL = `
WITH me AS (
  SELECT r.oid, r.rolsuper FROM pg_catalog.pg_roles r
   WHERE r.rolname OPERATOR(pg_catalog.=) CURRENT_USER
), db AS (
  SELECT d.datname, d.datdba, d.datacl FROM pg_catalog.pg_database d
   WHERE d.datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
), mig AS (
  SELECT c.relowner AS oid FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
   WHERE n.nspname OPERATOR(pg_catalog.=) pg_catalog.current_schema()
     AND c.relname OPERATOR(pg_catalog.=) 'schema_migrations'::pg_catalog.name
     AND c.relkind OPERATOR(pg_catalog.=) ANY ('{r,p}'::pg_catalog."char"[])
), t0 AS (
  SELECT r.oid FROM pg_catalog.pg_roles r CROSS JOIN me
   WHERE r.rolsuper
      OR r.oid OPERATOR(pg_catalog.=) me.oid
      OR (NOT me.rolsuper AND pg_catalog.pg_has_role(me.oid, r.oid, 'USAGE'::pg_catalog.text))
      OR r.oid OPERATOR(pg_catalog.=) ANY (SELECT mig.oid FROM mig)
), trusted AS (
  SELECT t0.oid FROM t0
  UNION
  SELECT 'pg_database_owner'::pg_catalog.regrole::pg_catalog.oid FROM db
   WHERE db.datdba OPERATOR(pg_catalog.=) ANY (SELECT t0.oid FROM t0)
), m AS (
  SELECT COALESCE((SELECT mig.oid FROM mig LIMIT 1), (SELECT me.oid FROM me)) AS oid
), target AS (
  SELECT CASE WHEN m.oid OPERATOR(pg_catalog.=) db.datdba THEN 'pg_database_owner'::pg_catalog.text
              ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(m.oid)::pg_catalog.text) END AS name
    FROM m CROSS JOIN db
), named AS (
  SELECT s.name::pg_catalog.text AS name, s.pos
    FROM pg_catalog.unnest(pg_catalog.current_schemas(false)) WITH ORDINALITY AS s(name, pos)
  UNION ALL
  SELECT 'pg_catalog'::pg_catalog.text, 0::pg_catalog.int8
), onpath AS (
  SELECT n.oid, pg_catalog.min(named.pos) AS pos
    FROM named JOIN pg_catalog.pg_namespace n ON n.nspname OPERATOR(pg_catalog.=) named.name
   GROUP BY n.oid
), schemas AS (
  SELECT n.oid, n.nspname, n.nspowner, n.nspacl, onpath.pos
    FROM onpath JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) onpath.oid
), objs(classid, objid, nsp, owner) AS (
  SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid, c.oid, c.relnamespace, c.relowner
    FROM pg_catalog.pg_class c
   WHERE NOT (c.relkind OPERATOR(pg_catalog.=) ANY ('{i,I}'::pg_catalog."char"[]))
  UNION ALL SELECT 'pg_catalog.pg_proc'::pg_catalog.regclass::pg_catalog.oid, p.oid, p.pronamespace, p.proowner FROM pg_catalog.pg_proc p
  UNION ALL SELECT 'pg_catalog.pg_operator'::pg_catalog.regclass::pg_catalog.oid, o.oid, o.oprnamespace, o.oprowner FROM pg_catalog.pg_operator o
  UNION ALL SELECT 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid, t.oid, t.typnamespace, t.typowner
    FROM pg_catalog.pg_type t
   WHERE t.typrelid OPERATOR(pg_catalog.=) 0::pg_catalog.oid
     AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type e WHERE e.typarray OPERATOR(pg_catalog.=) t.oid)
  UNION ALL SELECT 'pg_catalog.pg_collation'::pg_catalog.regclass::pg_catalog.oid, c.oid, c.collnamespace, c.collowner FROM pg_catalog.pg_collation c
  UNION ALL SELECT 'pg_catalog.pg_conversion'::pg_catalog.regclass::pg_catalog.oid, c.oid, c.connamespace, c.conowner FROM pg_catalog.pg_conversion c
  UNION ALL SELECT 'pg_catalog.pg_opclass'::pg_catalog.regclass::pg_catalog.oid, c.oid, c.opcnamespace, c.opcowner FROM pg_catalog.pg_opclass c
  UNION ALL SELECT 'pg_catalog.pg_opfamily'::pg_catalog.regclass::pg_catalog.oid, f.oid, f.opfnamespace, f.opfowner FROM pg_catalog.pg_opfamily f
  UNION ALL SELECT 'pg_catalog.pg_ts_config'::pg_catalog.regclass::pg_catalog.oid, c.oid, c.cfgnamespace, c.cfgowner FROM pg_catalog.pg_ts_config c
  UNION ALL SELECT 'pg_catalog.pg_ts_dict'::pg_catalog.regclass::pg_catalog.oid, d.oid, d.dictnamespace, d.dictowner FROM pg_catalog.pg_ts_dict d
  UNION ALL SELECT 'pg_catalog.pg_statistic_ext'::pg_catalog.regclass::pg_catalog.oid, s.oid, s.stxnamespace, s.stxowner FROM pg_catalog.pg_statistic_ext s
), acls AS (
  SELECT 'database'::pg_catalog.text AS kind, db.datname::pg_catalog.text AS name, db.datdba AS owner,
         COALESCE(db.datacl, pg_catalog.acldefault('d'::pg_catalog."char", db.datdba)) AS acl,
         0::pg_catalog.int8 AS pos, NULL::pg_catalog.oid AS nsp
    FROM db
  UNION ALL
  SELECT 'schema'::pg_catalog.text, s.nspname::pg_catalog.text, s.nspowner,
         COALESCE(s.nspacl, pg_catalog.acldefault('n'::pg_catalog."char", s.nspowner)), s.pos, s.oid
    FROM schemas s
), findings(rule, pos, sortkey, finding, remedy) AS (
  SELECT 'a'::pg_catalog.text, 0::pg_catalog.int8, ''::pg_catalog.text,
         pg_catalog.format('database %I is owned by %I', db.datname, pg_catalog.pg_get_userbyid(db.datdba)),
         pg_catalog.format('ALTER DATABASE %I OWNER TO %I;', db.datname, pg_catalog.pg_get_userbyid(m.oid))
    FROM db CROSS JOIN m
   WHERE NOT (db.datdba OPERATOR(pg_catalog.=) ANY (SELECT trusted.oid FROM trusted))
  UNION ALL
  SELECT 'b', s.pos, s.nspname::pg_catalog.text,
         pg_catalog.format('schema %I is owned by %I', s.nspname, pg_catalog.pg_get_userbyid(s.nspowner)),
         pg_catalog.format('ALTER SCHEMA %I OWNER TO %s;', s.nspname, target.name)
    FROM schemas s CROSS JOIN target
   WHERE NOT (s.nspowner OPERATOR(pg_catalog.=) ANY (SELECT trusted.oid FROM trusted))
  UNION ALL
  SELECT 'c', o.pos, o.name,
         pg_catalog.format('%s %I grants CREATE to %s', o.kind, o.name, g.grantee),
         CASE WHEN o.kind OPERATOR(pg_catalog.=) 'schema'::pg_catalog.text AND NOT (
                     (SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid OPERATOR(pg_catalog.=) m.oid)
                  OR pg_catalog.pg_has_role(m.oid, o.owner, 'USAGE'::pg_catalog.text)
                  OR EXISTS (SELECT 1 FROM pg_catalog.aclexplode(o.acl) k
                              WHERE k.privilege_type OPERATOR(pg_catalog.=) 'CREATE'::pg_catalog.text
                                AND NOT (k.grantee OPERATOR(pg_catalog.=) 0::pg_catalog.oid)
                                AND k.grantee OPERATOR(pg_catalog.=) ANY (SELECT trusted.oid FROM trusted)
                                AND pg_catalog.pg_has_role(m.oid, k.grantee, 'USAGE'::pg_catalog.text)))
              THEN pg_catalog.format('ALTER SCHEMA %I OWNER TO %s; ', o.name, target.name)
              ELSE ''::pg_catalog.text
         END OPERATOR(pg_catalog.||)
         pg_catalog.format('REVOKE CREATE ON %s %I FROM %s;', pg_catalog.upper(o.kind), o.name, g.grantee)
    FROM acls o
   CROSS JOIN LATERAL pg_catalog.aclexplode(o.acl) a
   CROSS JOIN LATERAL (SELECT CASE WHEN a.grantee OPERATOR(pg_catalog.=) 0::pg_catalog.oid THEN 'PUBLIC'::pg_catalog.text
                                   ELSE pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(a.grantee)::pg_catalog.text) END AS grantee) g
   CROSS JOIN m CROSS JOIN target
   WHERE a.privilege_type OPERATOR(pg_catalog.=) 'CREATE'::pg_catalog.text
     AND NOT (a.grantee OPERATOR(pg_catalog.=) o.owner)
     AND NOT (a.grantee OPERATOR(pg_catalog.=) ANY (SELECT trusted.oid FROM trusted))
  UNION ALL
  SELECT 'd', s.pos, pg_catalog.pg_describe_object(x.classid, x.objid, 0),
         pg_catalog.format('%s in schema %I is owned by %I', pg_catalog.pg_describe_object(x.classid, x.objid, 0),
                           s.nspname, pg_catalog.pg_get_userbyid(x.owner)),
         ''::pg_catalog.text
    FROM objs x JOIN schemas s ON s.oid OPERATOR(pg_catalog.=) x.nsp
   WHERE NOT (x.owner OPERATOR(pg_catalog.=) ANY (SELECT trusted.oid FROM trusted))
)
SELECT f.rule, f.finding, f.remedy, pg_catalog.quote_ident(pg_catalog.pg_get_userbyid(m.oid)::pg_catalog.text)
  FROM findings f CROSS JOIN m
 ORDER BY f.rule, f.pos, f.sortkey, f.finding`

// maxListedObjects bounds how many rule-d objects a refusal names.
const maxListedObjects = 20

// checkSearchPathTrust refuses when anything on the connection's name-resolution
// path is controlled by a role the plugin does not trust (see
// untrustedSearchPathSQL). It runs on every start, before anything is migrated
// and whether or not this node migrates. A grant made, or an object created,
// after the check passed is not seen until the next start.
//
// The refusal names each finding and the statements that fix it. It carries
// no connection detail: database, schema, object and role names only.
func checkSearchPathTrust(ctx context.Context, q rowsQuerier) error {
	rows, err := q.Query(ctx, untrustedSearchPathSQL)
	if err != nil {
		return fmt.Errorf("postgres: check the search path's owners and grants: %w", err)
	}
	var (
		found, remedies []string
		seen            = map[string]bool{}
		objects         int
		trustedOwner    string
	)
	for rows.Next() {
		var rule, finding, remedy, m string
		if err := rows.Scan(&rule, &finding, &remedy, &m); err != nil {
			rows.Close()
			return fmt.Errorf("postgres: check the search path's owners and grants: %w", err)
		}
		trustedOwner = m
		if rule == "d" {
			objects++
			if objects > maxListedObjects {
				continue
			}
		}
		found = append(found, finding)
		for _, r := range strings.SplitAfter(remedy, "; ") {
			if r = strings.TrimSpace(r); r != "" && !seen[r] {
				seen[r] = true
				remedies = append(remedies, r)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("postgres: check the search path's owners and grants: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	if objects > maxListedObjects {
		found = append(found, fmt.Sprintf("and %d more objects", objects-maxListedObjects))
	}
	msg := fmt.Sprintf("postgres: refusing to migrate or start: %s. "+
		"Every owner of this database, of a schema on the connection's search_path or of an object in one, "+
		"and every role that may create in them, must be trusted: a superuser, the connecting role or a role "+
		"it inherits, or the owner of the plugin's tables (%s). Fix each, then restart:",
		strings.Join(found, "; "), trustedOwner)
	if len(remedies) > 0 {
		msg += " " + strings.Join(remedies, " ")
	}
	if objects > 0 {
		msg += " Drop each object listed, or change its owner to a trusted role once you have reviewed it."
	}
	return fmt.Errorf("%s", msg)
}
