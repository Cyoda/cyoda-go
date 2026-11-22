package sqlite

import "testing"

// Migration 14 drops model_schema_extensions.tx_id; the down migration
// restores it and keeps rows.
func TestMigration14_DropsSchemaExtensionsTxID(t *testing.T) {
	db := openMigrationDB(t, "m14.db")
	migrateTo(t, db, 13)
	has := func() bool {
		for _, c := range tableInfo(t, db, "model_schema_extensions") {
			if c.Name == "tx_id" {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("precondition: version 13 should still have tx_id")
	}
	migrateTo(t, db, 14)
	if has() {
		t.Fatal("model_schema_extensions.tx_id still present at version 14")
	}
	if _, err := db.Exec(`INSERT INTO model_schema_extensions
		(tenant_id, model_name, model_version, seq, kind, payload)
		VALUES ('t', 'M', '1', 1, 'delta', x'00')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	migrateTo(t, db, 13)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_schema_extensions WHERE seq = 1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row lost across down: n=%d err=%v", n, err)
	}
	if !has() {
		t.Fatal("tx_id not restored by down")
	}
}
