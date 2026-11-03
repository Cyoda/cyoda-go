package sqlite

import "testing"

// Migration 13 gives every search job written before heartbeat stamping a
// heartbeat equal to its creation time, so a RUNNING job from an older file
// is reclaimable and GetJob never reports a missing heartbeat.
func TestMigration13_BackfillsHeartbeatFromCreateTime(t *testing.T) {
	db := openMigrationDB(t, "m13.db")
	migrateTo(t, db, 12)
	if _, err := db.Exec(`INSERT INTO search_jobs
		(tenant_id, job_id, model_name, model_version, point_in_time, create_time)
		VALUES ('t', 'unstamped', 'M', '1', 5, 1234)`); err != nil {
		t.Fatalf("insert unstamped: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO search_jobs
		(tenant_id, job_id, model_name, model_version, point_in_time, create_time, heartbeat_time)
		VALUES ('t', 'stamped', 'M', '1', 5, 1000, 2000)`); err != nil {
		t.Fatalf("insert stamped: %v", err)
	}
	migrateTo(t, db, 13)
	heartbeat := func(id string) int64 {
		var hb *int64
		if err := db.QueryRow(`SELECT heartbeat_time FROM search_jobs WHERE job_id = ?`, id).Scan(&hb); err != nil {
			t.Fatalf("select %s: %v", id, err)
		}
		if hb == nil {
			t.Fatalf("job %s: heartbeat_time still NULL at version 13", id)
		}
		return *hb
	}
	if got := heartbeat("unstamped"); got != 1234 {
		t.Fatalf("unstamped heartbeat = %d, want create_time 1234", got)
	}
	if got := heartbeat("stamped"); got != 2000 {
		t.Fatalf("stamped heartbeat = %d, want unchanged 2000", got)
	}
}
