package sqlite

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sqliteColumn is one PRAGMA table_info row.
type sqliteColumn struct {
	CID     int
	Name    string
	Type    string
	NotNull int
	Default sql.NullString
	PK      int
}

func tableInfo(t *testing.T, db *sql.DB, table string) []sqliteColumn {
	t.Helper()
	rows, err := db.Query(`SELECT cid, name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var cols []sqliteColumn
	for rows.Next() {
		var c sqliteColumn
		if err := rows.Scan(&c.CID, &c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if len(cols) == 0 {
		t.Fatalf("table %s has no columns; does it exist?", table)
	}
	return cols
}

// schemaObjects returns every sqlite_master row for the table itself and the
// indexes and triggers on it, keyed by name, so a rebuild can be checked to
// have kept all of them.
func schemaObjects(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT type || ':' || name, coalesce(sql, '') FROM sqlite_master WHERE tbl_name = ? ORDER BY 1`, table)
	if err != nil {
		t.Fatalf("sqlite_master(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sqlite_master rows: %v", err)
	}
	return out
}

func openMigrationDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func insertJobWithPointInTime(db *sql.DB, id string, pit any) error {
	_, err := db.Exec(`INSERT INTO search_jobs
		(tenant_id, job_id, status, model_name, model_version, point_in_time, create_time)
		VALUES ('tenant-A', ?, 'RUNNING', 'M', '1', ?, 1000)`, id, pit)
	return err
}

// Migration 10 rebuilds search_jobs with point_in_time NOT NULL and changes
// nothing else: every column keeps its position, type, default and key
// status, the table stays STRICT and WITHOUT ROWID, no index or trigger is
// lost, and existing rows are carried over.
func TestMigration10_OnlyPointInTimeBecomesNotNull(t *testing.T) {
	db := openMigrationDB(t, "m10.db")
	migrateTo(t, db, 9)
	if err := insertJobWithPointInTime(db, "kept", 4242); err != nil {
		t.Fatalf("insert version-9 row: %v", err)
	}
	before := tableInfo(t, db, "search_jobs")
	beforeObjects := schemaObjects(t, db, "search_jobs")

	migrateTo(t, db, 10)

	want := make([]sqliteColumn, len(before))
	copy(want, before)
	changed := false
	for i := range want {
		if want[i].Name == "point_in_time" {
			want[i].NotNull = 1
			changed = true
		}
	}
	if !changed {
		t.Fatal("version 9 has no point_in_time column")
	}
	if got := tableInfo(t, db, "search_jobs"); !reflect.DeepEqual(got, want) {
		t.Errorf("search_jobs columns after 000010:\n got %+v\nwant %+v", got, want)
	}

	afterObjects := schemaObjects(t, db, "search_jobs")
	for k := range beforeObjects {
		if _, ok := afterObjects[k]; !ok {
			t.Errorf("%s on search_jobs was lost by the rebuild", k)
		}
	}
	for k, v := range afterObjects {
		if k == "table:search_jobs" {
			for _, clause := range []string{"STRICT", "WITHOUT ROWID"} {
				if !strings.Contains(v, clause) {
					t.Errorf("rebuilt search_jobs is not %s: %s", clause, v)
				}
			}
			continue
		}
		if beforeObjects[k] != v {
			t.Errorf("%s changed across the rebuild:\n got %s\nwant %s", k, v, beforeObjects[k])
		}
	}

	var pit int64
	if err := db.QueryRow(`SELECT point_in_time FROM search_jobs WHERE tenant_id = 'tenant-A' AND job_id = 'kept'`).Scan(&pit); err != nil || pit != 4242 {
		t.Errorf("row after rebuild: point_in_time = %d, err = %v; want 4242 carried over", pit, err)
	}

	err := insertJobWithPointInTime(db, "j-null", nil)
	if err == nil || !strings.Contains(err.Error(), "NOT NULL constraint failed: search_jobs.point_in_time") {
		t.Fatalf("insert with NULL point_in_time: err = %v, want the NOT NULL constraint refusal", err)
	}
}

func TestMigration10_Down(t *testing.T) {
	db := openMigrationDB(t, "m10down.db")
	migrateTo(t, db, 9)
	version9 := tableInfo(t, db, "search_jobs")
	version9Objects := schemaObjects(t, db, "search_jobs")

	migrateTo(t, db, 10)
	if err := insertJobWithPointInTime(db, "kept", 4242); err != nil {
		t.Fatalf("insert version-10 row: %v", err)
	}
	migrateTo(t, db, 9)

	if got := tableInfo(t, db, "search_jobs"); !reflect.DeepEqual(got, version9) {
		t.Errorf("search_jobs columns after down:\n got %+v\nwant %+v", got, version9)
	}
	afterObjects := schemaObjects(t, db, "search_jobs")
	for k, v := range version9Objects {
		if k == "table:search_jobs" {
			continue // the rebuilt DDL is spelled differently; the columns are compared above
		}
		if afterObjects[k] != v {
			t.Errorf("%s after down:\n got %s\nwant %s", k, afterObjects[k], v)
		}
	}
	var pit int64
	if err := db.QueryRow(`SELECT point_in_time FROM search_jobs WHERE tenant_id = 'tenant-A' AND job_id = 'kept'`).Scan(&pit); err != nil || pit != 4242 {
		t.Errorf("row after down: point_in_time = %d, err = %v; want 4242 carried over", pit, err)
	}
	if err := insertJobWithPointInTime(db, "j-null", nil); err != nil {
		t.Fatalf("insert with NULL point_in_time at version 9: %v", err)
	}
}
