package search

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/domain/model/schema"
)

// countingSchemaStore implements the optional SchemaNode capability and counts
// both routes, so a test can tell which one loadFieldsMap took.
type countingSchemaStore struct {
	spi.ModelStore
	node      *schema.ModelNode
	nodeErr   error
	nodeCalls int
	getCalls  int
	desc      *spi.ModelDescriptor
}

func (s *countingSchemaStore) SchemaNode(context.Context, spi.ModelRef) (*schema.ModelNode, error) {
	s.nodeCalls++
	return s.node, s.nodeErr
}

func (s *countingSchemaStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.getCalls++
	return s.desc, nil
}

func testNode(t *testing.T) *schema.ModelNode {
	t.Helper()
	root := schema.NewObjectNode()
	root.SetChild("amount", schema.NewLeafNode(schema.Integer))
	return root
}

// The whole point of caching the parsed schema is lost if loadFieldsMap keeps
// asking for the raw descriptor and re-parsing it. Pin the route.
func TestLoadFieldsMap_UsesCachedParseWhenOffered(t *testing.T) {
	store := &countingSchemaStore{node: testNode(t)}
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	for i := 0; i < 5; i++ {
		fm, err := loadFieldsMap(context.Background(), store, ref)
		if err != nil {
			t.Fatalf("loadFieldsMap: %v", err)
		}
		if _, ok := fm["$.amount"]; !ok {
			t.Fatalf("expected $.amount in fields map, got %v", fm)
		}
	}
	if store.getCalls != 0 {
		t.Errorf("loadFieldsMap fetched the raw descriptor %d times; it should use the cached parse", store.getCalls)
	}
	if store.nodeCalls != 5 {
		t.Errorf("expected 5 SchemaNode calls, got %d", store.nodeCalls)
	}
}

// A store with no schema bound yields no type constraints, not an error —
// unchanged from the descriptor-parsing behaviour this replaced.
func TestLoadFieldsMap_NilNodeIsNotAnError(t *testing.T) {
	store := &countingSchemaStore{node: nil}
	fm, err := loadFieldsMap(context.Background(), store, spi.ModelRef{EntityName: "Order", ModelVersion: "1"})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if fm != nil {
		t.Errorf("expected a nil fields map, got %v", fm)
	}
}

// loadModelNode takes the same route.
func TestLoadModelNode_UsesCachedParseWhenOffered(t *testing.T) {
	store := &countingSchemaStore{node: testNode(t)}
	ref := spi.ModelRef{EntityName: "Order", ModelVersion: "1"}

	node := loadModelNode(context.Background(), store, ref)
	if node == nil {
		t.Fatal("expected a node")
	}
	if store.getCalls != 0 {
		t.Errorf("loadModelNode fetched the raw descriptor %d times", store.getCalls)
	}
}

// A store without the capability keeps the original descriptor-parsing path,
// so nothing that supplies a plain ModelStore changes behaviour.
func TestLoadFieldsMap_FallsBackWhenCapabilityAbsent(t *testing.T) {
	root := testNode(t)
	raw, err := schema.Marshal(root)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	plain := &plainModelStore{desc: &spi.ModelDescriptor{
		Ref:    spi.ModelRef{EntityName: "Order", ModelVersion: "1"},
		State:  spi.ModelLocked,
		Schema: raw,
	}}
	fm, err := loadFieldsMap(context.Background(), plain, spi.ModelRef{EntityName: "Order", ModelVersion: "1"})
	if err != nil {
		t.Fatalf("loadFieldsMap: %v", err)
	}
	if _, ok := fm["$.amount"]; !ok {
		t.Fatalf("expected $.amount, got %v", fm)
	}
	if plain.getCalls != 1 {
		t.Errorf("expected exactly 1 descriptor fetch on the fallback path, got %d", plain.getCalls)
	}
}

type plainModelStore struct {
	spi.ModelStore
	desc     *spi.ModelDescriptor
	getCalls int
}

func (s *plainModelStore) Get(context.Context, spi.ModelRef) (*spi.ModelDescriptor, error) {
	s.getCalls++
	return s.desc, nil
}
