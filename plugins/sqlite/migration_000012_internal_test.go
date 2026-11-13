package sqlite

import "testing"

// Migration 12 drops model_schema_extensions.created_at, which nothing
// writes or reads.
func TestMigration_SchemaExtensionsHasNoCreatedAt(t *testing.T) {
	db := openMigrationDB(t, "m12.db")
	migrateTo(t, db, 11)
	has := func() bool {
		for _, c := range tableInfo(t, db, "model_schema_extensions") {
			if c.Name == "created_at" {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("precondition: version 11 should still have created_at")
	}
	migrateTo(t, db, 12)
	if has() {
		t.Fatal("model_schema_extensions.created_at still present at version 12")
	}
}

// The down migration restores the column with its original default and keeps
// rows.
func TestMigration12_DownRestoresCreatedAtWithDefault(t *testing.T) {
	db := openMigrationDB(t, "m12down.db")
	migrateTo(t, db, 12)
	if _, err := db.Exec(`INSERT INTO model_schema_extensions
		(tenant_id, model_name, model_version, seq, kind, payload, tx_id)
		VALUES ('t', 'M', '1', 1, 'delta', x'00', 'tx')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	migrateTo(t, db, 11)
	var created string
	if err := db.QueryRow(`SELECT created_at FROM model_schema_extensions WHERE seq = 1`).Scan(&created); err != nil {
		t.Fatalf("row lost or created_at missing after down: %v", err)
	}
	if created == "" {
		t.Fatal("created_at default not applied")
	}
}

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
