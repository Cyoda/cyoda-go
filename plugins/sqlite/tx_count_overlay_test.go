package sqlite_test

import (
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// In-tx counts reflect the transaction's own view for every buffer shape:
// a create, an update, a delete of a committed entity, a create then delete,
// a delete then Save, a state change, and after DeleteAll.
func TestTxCount_EveryBufferShape(t *testing.T) {
	f, tm := newAttrFactory(t)
	ctx := attrCtx("tenant-ovl", "u1", spi.PrincipalUser)
	ref := spi.ModelRef{EntityName: "m-cnt", ModelVersion: "1"}
	store, _ := f.EntityStore(ctx)
	seedN(t, store, ctx, ref, 4) // e00 open, e01 closed, e02 open, e03 closed

	txID, txCtx, _ := tm.Begin(ctx)
	defer func() { _ = tm.Rollback(txCtx, txID) }()
	save := func(id, state string) {
		t.Helper()
		if _, err := store.Save(txCtx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "tenant-ovl", ModelRef: ref, State: state}, Data: []byte(`{}`)}); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}
	del := func(id string) {
		t.Helper()
		if err := store.Delete(txCtx, id); err != nil {
			t.Fatalf("Delete %s: %v", id, err)
		}
	}
	check := func(step string, wantTotal int64, wantByState map[string]int64) {
		t.Helper()
		got, err := store.Count(txCtx, ref)
		if err != nil {
			t.Fatalf("%s: Count: %v", step, err)
		}
		if got != wantTotal {
			t.Fatalf("%s: Count = %d, want %d", step, got, wantTotal)
		}
		by, err := store.CountByState(txCtx, ref, nil)
		if err != nil {
			t.Fatalf("%s: CountByState: %v", step, err)
		}
		if len(by) != len(wantByState) {
			t.Fatalf("%s: CountByState = %v, want %v", step, by, wantByState)
		}
		for st, n := range wantByState {
			if by[st] != n {
				t.Fatalf("%s: CountByState[%s] = %d, want %d (%v)", step, st, by[st], n, by)
			}
		}
	}

	check("baseline", 4, map[string]int64{"open": 2, "closed": 2})
	save("n00", "open") // create
	check("create", 5, map[string]int64{"open": 3, "closed": 2})
	save("e01", "open") // update + state change closed→open
	check("update", 5, map[string]int64{"open": 4, "closed": 1})
	del("e02") // delete committed
	check("delete committed", 4, map[string]int64{"open": 3, "closed": 1})
	save("n01", "closed")
	del("n01") // create then delete
	check("create then delete", 4, map[string]int64{"open": 3, "closed": 1})
	del("e03")
	save("e03", "open") // delete then Save → present, open
	check("delete then save", 4, map[string]int64{"open": 4})
	if err := store.DeleteAll(txCtx, ref); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	check("after DeleteAll", 0, map[string]int64{})
}
