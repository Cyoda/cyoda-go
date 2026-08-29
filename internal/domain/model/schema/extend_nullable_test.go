package schema

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestExtend_IncomingLeafNull_AgainstExistingArray_IsNullableMarker reproduces
// a production bug reported during data ingestion:
//
//	Model registered with {"custom_permissions":["a","b"]} → stored as ARRAY.
//	Subsequent create with {"custom_permissions":null} → importer.Walk returns
//	LEAF[NULL]. schema.Extend erroneously rejected this as "kind mismatch at
//	...custom_permissions: ARRAY vs LEAF" after commit 2b43009.
//
// NULL against a non-LEAF target is an existing nullable-marker pattern — the
// same pattern Diff/Apply support on broaden_type. Extend must accept it.
func TestExtend_IncomingLeafNull_AgainstExistingArray_IsNullableMarker(t *testing.T) {
	existing := NewObjectNode()
	existing.SetChild("custom_permissions", NewArrayNode(NewLeafNode(String)))

	incoming := NewObjectNode()
	incoming.SetChild("custom_permissions", NewLeafNode(Null))

	got, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err != nil {
		t.Fatalf("Extend with LEAF[NULL] against ARRAY must succeed (nullable marker); got: %v", err)
	}
	child := got.Object().Child("custom_permissions")
	if child == nil {
		t.Fatal("extended child is nil")
	}
	if child.Array() == nil {
		t.Errorf("extended child kinds = %v, want the array branch (a nullable marker adds no kind)", child.Kinds())
	}
	hasNull := false
	for _, dt := range child.DeclaredTypes() {
		if dt == Null {
			hasNull = true
			break
		}
	}
	if !hasNull {
		t.Errorf("ARRAY node types = %v, want to include NULL after nullable extension", child.DeclaredTypes())
	}
}

// TestExtend_IncomingLeafNull_AgainstExistingObject_IsNullableMarker — same
// symmetry for OBJECT targets.
func TestExtend_IncomingLeafNull_AgainstExistingObject_IsNullableMarker(t *testing.T) {
	existingChild := NewObjectNode()
	existingChild.SetChild("inner", NewLeafNode(String))
	existing := NewObjectNode()
	existing.SetChild("roles_and_permissions", existingChild)

	incoming := NewObjectNode()
	incoming.SetChild("roles_and_permissions", NewLeafNode(Null))

	got, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err != nil {
		t.Fatalf("Extend with LEAF[NULL] against OBJECT must succeed (nullable marker); got: %v", err)
	}
	child := got.Object().Child("roles_and_permissions")
	if child.Object() == nil {
		t.Errorf("extended child kinds = %v, want the object branch", child.Kinds())
	}
	hasNull := false
	for _, dt := range child.DeclaredTypes() {
		if dt == Null {
			hasNull = true
			break
		}
	}
	if !hasNull {
		t.Errorf("OBJECT node types = %v, want to include NULL", child.DeclaredTypes())
	}
}

// TestExtend_ExistingLeafNull_AgainstIncomingArray_PromotesToArray — the
// inverse: a previously-null slot now sees a concrete ARRAY. Same nullable-
// marker logic in reverse; promoting LEAF[NULL] to ARRAY should succeed and
// the resulting node carries NULL in its TypeSet.
func TestExtend_ExistingLeafNull_AgainstIncomingArray_PromotesToArray(t *testing.T) {
	existing := NewObjectNode()
	existing.SetChild("tags", NewLeafNode(Null))

	incoming := NewObjectNode()
	incoming.SetChild("tags", NewArrayNode(NewLeafNode(String)))

	got, err := Extend(existing, incoming, spi.ChangeLevelType)
	if err != nil {
		t.Fatalf("Extend with ARRAY against LEAF[NULL] must succeed (nullable promoted); got: %v", err)
	}
	child := got.Object().Child("tags")
	if child.Array() == nil {
		t.Errorf("extended child kinds = %v, want the array branch (the marker promotes to the observed kind)", child.Kinds())
	}
}

// TestExtend_GenuineKindMismatch_StillRejected — ensures the nullable-marker
// exception does NOT swallow genuine kind conflicts (ARRAY vs OBJECT, LEAF
// non-NULL vs OBJECT, etc.).
func TestExtend_GenuineKindMismatch_StillRejected(t *testing.T) {
	cases := []struct {
		name     string
		existing *ModelNode
		incoming *ModelNode
	}{
		{
			name:     "ARRAY vs OBJECT",
			existing: NewArrayNode(NewLeafNode(String)),
			incoming: NewObjectNode(),
		},
		{
			name:     "OBJECT vs ARRAY",
			existing: NewObjectNode(),
			incoming: NewArrayNode(NewLeafNode(String)),
		},
		{
			name:     "LEAF[String] vs OBJECT",
			existing: NewLeafNode(String),
			incoming: NewObjectNode(),
		},
		{
			name:     "LEAF[String] vs ARRAY",
			existing: NewLeafNode(String),
			incoming: NewArrayNode(NewLeafNode(String)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := NewObjectNode()
			existing.SetChild("f", tc.existing)
			incoming := NewObjectNode()
			incoming.SetChild("f", tc.incoming)

			// A concrete kind meeting another concrete kind is a new branch:
			// refused below STRUCTURAL, and the message names that level.
			// This is what separates it from the nullable marker above, which
			// adds no kind and stays a TYPE-level change.
			_, err := Extend(existing, incoming, spi.ChangeLevelType)
			if err == nil {
				t.Fatal("adding a kind must still be refused at TYPE, unlike the nullable marker")
			}
			if !strings.Contains(err.Error(), "STRUCTURAL") {
				t.Errorf("the rejection must name the level that resolves it: %v", err)
			}
		})
	}
}

// TestExtend_NullableMarker_AtLowerChangeLevel_RejectedAsLevelViolation pins
// the intersection of two behaviors that interact non-obviously:
//
//  1. A payload of null against a container adds no kind — the node was
//     observed as null, which is a marker, not a kind of its own.
//  2. Recording that marker is still a TYPE-level change, so a caller below
//     TYPE must not smuggle it through.
//
// The rejection names TYPE, the level that resolves it, rather than reporting
// a shape problem the caller cannot act on.
//
// Runs for the three sub-TYPE levels (ArrayLength, ArrayElements, and the
// empty level that permits nothing) to cover the full matrix of "below TYPE"
// cases.
func TestExtend_NullableMarker_AtLowerChangeLevel_RejectedAsLevelViolation(t *testing.T) {
	cases := []struct {
		name  string
		level spi.ChangeLevel
	}{
		{"ArrayLength", spi.ChangeLevelArrayLength},
		{"ArrayElements", spi.ChangeLevelArrayElements},
		{"Empty", spi.ChangeLevel("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := NewObjectNode()
			existing.SetChild("custom_permissions", NewArrayNode(NewLeafNode(String)))

			incoming := NewObjectNode()
			incoming.SetChild("custom_permissions", NewLeafNode(Null))

			_, err := Extend(existing, incoming, tc.level)
			if err == nil {
				t.Fatal("nullable marker below TYPE level must reject")
			}
			// It is a level issue, solvable by raising the level, and the
			// message must say so rather than blaming the payload's shape.
			if !strings.Contains(err.Error(), "TYPE") {
				t.Errorf("the rejection must name TYPE, the level that resolves it; got: %v", err)
			}
		})
	}
}
