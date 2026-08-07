package modelcache_test

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/modelcache"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

func schemaBytes(t *testing.T) []byte {
	t.Helper()
	root := schema.NewObjectNode()
	root.SetChild("amount", schema.NewLeafNode(schema.Integer))
	root.SetChild("status", schema.NewLeafNode(schema.String))
	b, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return b
}

func descWithSchema(t *testing.T, ref spi.ModelRef) *spi.ModelDescriptor {
	t.Helper()
	return &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: schemaBytes(t),
	}
}

// The derived parse of a model schema is the dominant cost of evaluating a
// workflow criterion — 80-99% of it, scaling with schema size. The descriptor
// bytes are already cached; the parsed form must be cached on the same entry so
// it inherits that entry's eviction, rather than being rebuilt per evaluation.
func TestSchemaNode_ParsedOncePerCacheEntry(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	inner := &stubStore{desc: descWithSchema(t, ref)}
	clk := &manualClock{now: time.Now()}
	c := modelcache.New(inner, nil, clk, time.Minute)
	ctx := context.Background()

	n1, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode: %v", err)
	}
	if n1 == nil {
		t.Fatal("expected a parsed node")
	}
	if _, ok := n1.FieldsMap()["$.amount"]; !ok {
		t.Fatalf("parsed node missing $.amount: %v", n1.FieldsMap())
	}

	n2, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode (second): %v", err)
	}
	if n1 != n2 {
		t.Error("schema was re-parsed on a cache hit: the derived parse is not held on the cache entry")
	}

	// Invalidation must drop the derived parse with the descriptor, or a stale
	// schema outlives the bytes it came from.
	if err := c.Save(ctx, descWithSchema(t, ref)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	n3, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode (after invalidation): %v", err)
	}
	if n3 == n1 {
		t.Error("invalidation did not drop the derived parse")
	}
}

// A lease expiry must drop the derived parse too — same reason.
func TestSchemaNode_ReparsedAfterLeaseExpiry(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	inner := &stubStore{desc: descWithSchema(t, ref)}
	clk := &manualClock{now: time.Now()}
	c := modelcache.New(inner, nil, clk, time.Minute)
	ctx := context.Background()

	n1, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode: %v", err)
	}
	clk.advance(2 * time.Minute)
	n2, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode after expiry: %v", err)
	}
	if n1 == n2 {
		t.Error("expired entry returned the old derived parse")
	}
}

// An unlocked descriptor is never cached (its schema can still change), so it
// must still parse correctly — just without being retained.
func TestSchemaNode_UnlockedModelParsesButIsNotCached(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	unlocked := descWithSchema(t, ref)
	unlocked.State = spi.ModelUnlocked
	inner := &stubStore{desc: unlocked}
	clk := &manualClock{now: time.Now()}
	c := modelcache.New(inner, nil, clk, time.Minute)
	ctx := context.Background()

	n1, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode: %v", err)
	}
	if n1 == nil {
		t.Fatal("expected a parsed node for an unlocked model")
	}
	n2, err := c.SchemaNode(ctx, ref)
	if err != nil {
		t.Fatalf("SchemaNode (second): %v", err)
	}
	if n1 == n2 {
		t.Error("an unlocked descriptor's parse was retained; its schema can still change")
	}
}

// A descriptor with no schema bound is "no type constraints", not an error —
// matching what fieldsFromDescriptor and loadModelNode already do.
func TestSchemaNode_NoSchemaBoundIsNotAnError(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	inner := &stubStore{desc: &spi.ModelDescriptor{Ref: ref, State: spi.ModelLocked}}
	clk := &manualClock{now: time.Now()}
	c := modelcache.New(inner, nil, clk, time.Minute)

	node, err := c.SchemaNode(context.Background(), ref)
	if err != nil {
		t.Fatalf("expected no error for an unbound schema, got %v", err)
	}
	if node != nil {
		t.Error("expected a nil node for an unbound schema")
	}
}

// An unparseable schema must surface the same error every time, from the entry,
// without re-parsing to rediscover it.
func TestSchemaNode_UnparseableSchemaErrorIsStable(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}
	inner := &stubStore{desc: &spi.ModelDescriptor{
		Ref:    ref,
		State:  spi.ModelLocked,
		Schema: []byte(`{not json`),
	}}
	clk := &manualClock{now: time.Now()}
	c := modelcache.New(inner, nil, clk, time.Minute)
	ctx := context.Background()

	_, err1 := c.SchemaNode(ctx, ref)
	if err1 == nil {
		t.Fatal("expected an error for an unparseable schema")
	}
	_, err2 := c.SchemaNode(ctx, ref)
	if err2 == nil || err1.Error() != err2.Error() {
		t.Errorf("unstable error across calls: %v vs %v", err1, err2)
	}
}

// Gate 3 requires every data path to be verified for tenant isolation, and the
// derived parse is a new one. The entry is keyed by (tenant, ref), so two
// tenants holding different schemas for the same ModelRef must never see each
// other's parse.
func TestSchemaNode_TenantIsolation(t *testing.T) {
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	rootA := schema.NewObjectNode()
	rootA.SetChild("alpha", schema.NewLeafNode(schema.String))
	bytesA, err := schema.Marshal(rootA)
	if err != nil {
		t.Fatalf("marshal A: %v", err)
	}
	rootB := schema.NewObjectNode()
	rootB.SetChild("beta", schema.NewLeafNode(schema.Integer))
	bytesB, err := schema.Marshal(rootB)
	if err != nil {
		t.Fatalf("marshal B: %v", err)
	}

	// One inner store that answers per tenant, so the only thing separating the
	// two results is the cache key.
	inner := &perTenantStore{schemas: map[string][]byte{
		"tenant-a": bytesA,
		"tenant-b": bytesB,
	}, ref: ref}
	c := modelcache.New(inner, nil, &manualClock{now: time.Now()}, time.Minute)

	ctxA := withTenantContext(context.Background(), "tenant-a")
	ctxB := withTenantContext(context.Background(), "tenant-b")

	nodeA, err := c.SchemaNode(ctxA, ref)
	if err != nil {
		t.Fatalf("SchemaNode A: %v", err)
	}
	nodeB, err := c.SchemaNode(ctxB, ref)
	if err != nil {
		t.Fatalf("SchemaNode B: %v", err)
	}

	if _, ok := nodeA.FieldsMap()["$.alpha"]; !ok {
		t.Errorf("tenant-a got the wrong schema: %v", nodeA.FieldsMap())
	}
	if _, ok := nodeB.FieldsMap()["$.beta"]; !ok {
		t.Errorf("tenant-b got the wrong schema: %v", nodeB.FieldsMap())
	}
	if _, leaked := nodeA.FieldsMap()["$.beta"]; leaked {
		t.Error("tenant-a was served tenant-b's parsed schema")
	}
	if _, leaked := nodeB.FieldsMap()["$.alpha"]; leaked {
		t.Error("tenant-b was served tenant-a's parsed schema")
	}

	// Re-read both from cache; the separation must survive a cache hit.
	againA, _ := c.SchemaNode(ctxA, ref)
	againB, _ := c.SchemaNode(ctxB, ref)
	if againA != nodeA || againB != nodeB {
		t.Error("cached parse was not reused per tenant")
	}
	if againA == againB {
		t.Error("both tenants received the same parsed node")
	}
}

type perTenantStore struct {
	spi.ModelStore
	schemas map[string][]byte
	ref     spi.ModelRef
}

func (s *perTenantStore) Get(ctx context.Context, _ spi.ModelRef) (*spi.ModelDescriptor, error) {
	return &spi.ModelDescriptor{
		Ref:    s.ref,
		State:  spi.ModelLocked,
		Schema: s.schemas[common.TenantFromContext(ctx)],
	}, nil
}
