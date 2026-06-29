package schema

import (
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// testModel builds:
//
//	{
//	  "name": string        → $.name   (scalar leaf)
//	  "age":  integer       → $.age    (scalar leaf)
//	  "tags": []string      → $.tags[*] (array leaf — NOT a scalar leaf)
//	  "addr": {             → object node (not a leaf at all)
//	    "city": string      → $.addr.city (scalar leaf)
//	  }
//	}
func testModel() *ModelNode {
	root := NewObjectNode()
	root.SetChild("name", NewLeafNode(String))
	root.SetChild("age", NewLeafNode(Integer))
	root.SetChild("tags", NewArrayNode(NewLeafNode(String)))
	addr := NewObjectNode()
	addr.SetChild("city", NewLeafNode(String))
	root.SetChild("addr", addr)
	return root
}

func TestValidateUniqueKeys_OK(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name", "$.age"}},
		{ID: "uk2", Fields: []string{"$.addr.city"}},
	}
	if err := ValidateUniqueKeys(n, keys); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateUniqueKeys_EmptyKeys(t *testing.T) {
	n := testModel()
	if err := ValidateUniqueKeys(n, nil); err != nil {
		t.Fatalf("empty key slice should be valid, got: %v", err)
	}
}

func TestValidateUniqueKeys_UnknownPath(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.nonexistent"}},
	}
	err := ValidateUniqueKeys(n, keys)
	if err == nil {
		t.Fatal("expected error for unknown path, got nil")
	}
	var def *UniqueKeyDefError
	if !errors.As(err, &def) {
		t.Fatalf("expected *UniqueKeyDefError, got %T: %v", err, err)
	}
	if def.Reason == "" {
		t.Error("expected non-empty Reason")
	}
}

func TestValidateUniqueKeys_ArrayPathRejected(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.tags[*]"}},
	}
	err := ValidateUniqueKeys(n, keys)
	if err == nil {
		t.Fatal("expected error for array path, got nil")
	}
	var def *UniqueKeyDefError
	if !errors.As(err, &def) {
		t.Fatalf("expected *UniqueKeyDefError, got %T: %v", err, err)
	}
}

func TestValidateUniqueKeys_EmptyFields(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{}},
	}
	err := ValidateUniqueKeys(n, keys)
	if err == nil {
		t.Fatal("expected error for empty fields, got nil")
	}
	var def *UniqueKeyDefError
	if !errors.As(err, &def) {
		t.Fatalf("expected *UniqueKeyDefError, got %T: %v", err, err)
	}
}

func TestValidateUniqueKeys_DupID(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name"}},
		{ID: "uk1", Fields: []string{"$.age"}},
	}
	err := ValidateUniqueKeys(n, keys)
	if err == nil {
		t.Fatal("expected error for duplicate key ID, got nil")
	}
	var def *UniqueKeyDefError
	if !errors.As(err, &def) {
		t.Fatalf("expected *UniqueKeyDefError, got %T: %v", err, err)
	}
}

func TestValidateUniqueKeys_DupFieldWithinKey(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.name", "$.name"}},
	}
	err := ValidateUniqueKeys(n, keys)
	if err == nil {
		t.Fatal("expected error for duplicate field within key, got nil")
	}
	var def *UniqueKeyDefError
	if !errors.As(err, &def) {
		t.Fatalf("expected *UniqueKeyDefError, got %T: %v", err, err)
	}
}

func TestValidateUniqueKeys_EmptyID(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "", Fields: []string{"$.name"}},
	}
	err := ValidateUniqueKeys(n, keys)
	if err == nil {
		t.Fatal("expected error for empty key ID, got nil")
	}
	var def *UniqueKeyDefError
	if !errors.As(err, &def) {
		t.Fatalf("expected *UniqueKeyDefError, got %T: %v", err, err)
	}
}

func TestValidateUniqueKeys_UniqueKeyDefError_Implements_Error(t *testing.T) {
	e := &UniqueKeyDefError{Reason: "test reason"}
	// Verify Error() returns non-empty string.
	if e.Error() == "" {
		t.Error("UniqueKeyDefError.Error() should return non-empty string")
	}
}

func TestValidateUniqueKeys_NestedScalarLeafOK(t *testing.T) {
	n := testModel()
	keys := []spi.UniqueKey{
		{ID: "uk1", Fields: []string{"$.addr.city"}},
	}
	if err := ValidateUniqueKeys(n, keys); err != nil {
		t.Fatalf("nested scalar leaf should be valid, got: %v", err)
	}
}
