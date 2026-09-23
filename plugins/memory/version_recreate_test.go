package memory_test

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestSave_AfterCommittedDelete_TakesNextVersion pins the rule that a memory
// version number is never reused: a save that recreates an id after a
// committed delete must take the tombstone's version + 1, not the last
// live row's version + 1 (which would collide with the tombstone).
func TestSave_AfterCommittedDelete_TakesNextVersion(t *testing.T) {
	f, tm := newTxManager(t)
	ctx := tenantCtx(spi.TenantID("tenant-recreate"))
	ref := spi.ModelRef{EntityName: "m-recreate", ModelVersion: "1"}
	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(fn func(txCtx context.Context) error) {
		t.Helper()
		txID, txCtx, err := tm.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(txCtx); err != nil {
			t.Fatal(err)
		}
		if err := tm.Commit(txCtx, txID); err != nil {
			t.Fatal(err)
		}
	}
	commit(func(c context.Context) error {
		_, err := es.Save(c, &spi.Entity{Meta: spi.EntityMeta{ID: "e-r", ModelRef: ref}, Data: []byte(`{"n":1}`)})
		return err
	})
	commit(func(c context.Context) error {
		return es.Delete(c, "e-r")
	})
	commit(func(c context.Context) error {
		_, err := es.Save(c, &spi.Entity{Meta: spi.EntityMeta{ID: "e-r", ModelRef: ref}, Data: []byte(`{"n":2}`)})
		return err
	})
	metas, err := es.GetVersionMetadata(ctx, "e-r", spi.VersionMetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{}
	for _, m := range metas {
		got = append(got, m.Version)
	}
	if len(got) != 3 || !(got[0] > got[1] && got[1] > got[2]) {
		t.Fatalf("versions newest first = %v, want three strictly decreasing", got)
	}
}
