package schema

import (
	"errors"
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
	child := got.Child("custom_permissions")
	if child == nil {
		t.Fatal("extended child is nil")
	}
	if child.Kind() != KindArray {
		t.Errorf("extended child kind = %s, want %s (nullable marker must not change kind)", child.Kind(), KindArray)
	}
	hasNull := false
	for _, dt := range child.Types().Types() {
		if dt == Null {
			hasNull = true
			break
		}
	}
	if !hasNull {
		t.Errorf("ARRAY node types = %v, want to include NULL after nullable extension", child.Types().Types())
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
	child := got.Child("roles_and_permissions")
	if child.Kind() != KindObject {
		t.Errorf("extended child kind = %s, want %s", child.Kind(), KindObject)
	}
	hasNull := false
	for _, dt := range child.Types().Types() {
		if dt == Null {
			hasNull = true
			break
		}
	}
	if !hasNull {
		t.Errorf("OBJECT node types = %v, want to include NULL", child.Types().Types())
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
	child := got.Child("tags")
	if child.Kind() != KindArray {
		t.Errorf("extended child kind = %s, want %s (NULL promotes to concrete kind)", child.Kind(), KindArray)
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

			_, err := Extend(existing, incoming, spi.ChangeLevelType)
			if err == nil {
				t.Fatal("genuine kind mismatch must still be rejected after nullable-marker exception")
			}
			if !errors.Is(err, ErrPolymorphicSlot) {
				t.Errorf("unexpected error (want ErrPolymorphicSlot): %v", err)
			}
		})
	}
}

// TestExtend_NullableMarker_AtLowerChangeLevel_RejectedAsLevelViolation pins
// the intersection of two behaviors that interact non-obviously:
//
//  1. isNullOnlyLeaf carve-out: LEAF[NULL] against non-LEAF is a nullable
//     marker, not a kind mismatch (commit d3159bd).
//  2. Nullable markers add NULL to the target's TypeSet, which is a
//     TYPE-level operation and requires ChangeLevelType or higher.
//
// A caller operating at ChangeLevelArrayLength (or any level below TYPE)
// must NOT smuggle a TypeSet widening through via the nullable path. The
// carve-out returns a clear "nullable marker requires TYPE level" error
// rather than silently accepting the change OR falsely reporting a kind
// mismatch — both of which would be wrong.
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
			// Must NOT be classified as polymorphic — this is a level
			// issue, solvable by raising the level. Treating it as
			// polymorphic would mislead clients into thinking their
			// payload shape is at fault.
			if errors.Is(err, ErrPolymorphicSlot) {
				t.Errorf("level-below-TYPE nullable must NOT wrap ErrPolymorphicSlot; got: %v", err)
			}
		})
	}
}
