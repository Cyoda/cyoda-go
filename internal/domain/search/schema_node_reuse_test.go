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
