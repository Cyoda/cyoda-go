package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go-spi/spitest"
)

// Every write stores a meta blob, so a row whose meta is unparseable, NULL or
// empty is corrupt. A read of such a row must fail rather than return the
// entity with its meta fields blank.
func TestMetaIntegrity_ReadsFailOnBadMeta(t *testing.T) {
	cases := []struct {
		name string
		set  string // SQL expression assigned to the meta column
	}{
		{"unparseable", `CAST('{"x":' AS BLOB)`},
		{"null", `NULL`},
		{"empty", `CAST('' AS BLOB)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewStoreFactoryForTest(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
			if err != nil {
				t.Fatalf("NewStoreFactoryForTest: %v", err)
			}
			defer f.Close()
			ctx := attrInternalCtx("tenant-meta", "alice", spi.PrincipalUser)
			ref := spi.ModelRef{EntityName: "m-meta", ModelVersion: "1"}
			spitest.EnsureLockedModel(t, f, ctx, ref)

			store, err := f.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			if _, err := store.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "e-meta", ModelRef: ref, State: "open"}, Data: []byte(`{"a":1}`)}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			versions, err := store.GetVersionMetadata(ctx, "e-meta", spi.VersionMetadataOptions{})
			if err != nil || len(versions) != 1 {
				t.Fatalf("GetVersionMetadata baseline: %v (%d)", err, len(versions))
			}
			txID := versions[0].TransactionID

			for _, tbl := range []string{"entities", "entity_versions"} {
				if _, err := f.db.ExecContext(ctx, `UPDATE `+tbl+` SET meta = `+tc.set+` WHERE tenant_id = ? AND entity_id = ?`,
					"tenant-meta", "e-meta"); err != nil {
					t.Fatalf("corrupt %s: %v", tbl, err)
				}
			}

			if _, err := store.Get(ctx, "e-meta"); err == nil {
				t.Errorf("Get: want error, got nil")
			}
			if _, err := store.GetVersionMetadata(ctx, "e-meta", spi.VersionMetadataOptions{}); err == nil {
				t.Errorf("GetVersionMetadata: want error, got nil")
			}
			if _, err := store.GetVersionByTransaction(ctx, "e-meta", txID); err == nil {
				t.Errorf("GetVersionByTransaction: want error, got nil")
			}
			if _, err := store.Search(ctx, spi.Filter{}, spi.SearchOptions{ModelName: ref.EntityName, ModelVersion: ref.ModelVersion, Limit: 10}); err == nil {
				t.Errorf("Search: want error, got nil")
			}
			agg, ok := store.(spi.GroupedAggregator)
			if !ok {
				t.Fatalf("store is not a GroupedAggregator")
			}
			if _, err := agg.GroupedAggregate(ctx, ref, []spi.GroupExpr{{Kind: spi.GroupExprState}}, spi.Filter{},
				spi.GroupedAggregationsOptions{MaxBuckets: 10}); err == nil {
				t.Errorf("GroupedAggregate: want error, got nil")
			}
		})
	}
}
