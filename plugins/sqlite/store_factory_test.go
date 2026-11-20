package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cyoda/cyoda-go/plugins/sqlite"
)

func TestNewStoreFactoryForTest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	factory, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create factory: %v", err)
	}
	defer factory.Close()

	// Verify that TransactionManager is available
	tm, err := factory.TransactionManager(context.Background())
	if err != nil {
		t.Fatalf("get transaction manager: %v", err)
	}
	if tm == nil {
		t.Fatal("transaction manager is nil")
	}
}

func TestNewStoreFactoryForTest_ExclusiveLock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	factory1, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("create first factory: %v", err)
	}
	defer factory1.Close()

	// Second factory on the same path should fail
	_, err = sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err == nil {
		t.Fatal("expected error when opening second factory on same path")
	}
}
