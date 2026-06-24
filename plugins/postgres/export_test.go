package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ValidateInChunksForTest exposes validateInChunks for integration testing.
func ValidateInChunksForTest(
	tm *TransactionManager, ctx context.Context, tx pgx.Tx, tenantID spi.TenantID, sortedIDs []string, chunkSize int,
) (map[string]int64, error) {
	return tm.validateInChunks(ctx, tx, tenantID, sortedIDs, chunkSize)
}

// DropSchemaForTest exposes dropSchema (the unexported implementation) to
// _test.go files in this package and any external test packages that import
// "github.com/cyoda-platform/cyoda-go/plugins/postgres". The export_test.go
// idiom keeps the symbol invisible to non-test compilation: the file is
// compiled only when `go test` is building the package, so production binaries
// never see DropSchemaForTest.
//
// Use this in test helpers and conformance fixtures. Never call it from
// production code.
var DropSchemaForTest = dropSchema

// MigrateDownForTest exposes migrateDown to test files via the export_test.go
// idiom. Use only in tests; never in production code.
var MigrateDownForTest = migrateDown

// ClassifyErrorForTest exposes classifyError to allow unit-testing of the
// serialization/deadlock classification logic without requiring a live database.
var ClassifyErrorForTest = classifyError

// HasTxState reports whether the given txID has an active txState entry.
func HasTxState(tm *TransactionManager, txID string) bool {
	tm.txStatesMu.RLock()
	defer tm.txStatesMu.RUnlock()
	_, ok := tm.txStates[txID]
	return ok
}

// TxStateForTest exposes the recording/savepoint methods needed by tests.
type TxStateForTest interface {
	RecordRead(id string, version int64)
	RecordWrite(id string, preWriteVersion int64)
	PushSavepoint(id string)
	RestoreSavepoint(id string) error
	ReleaseSavepoint(id string) error
}

// LookupTxStateForTest returns the TxStateForTest for the given txID.
func LookupTxStateForTest(tm *TransactionManager, txID string) (TxStateForTest, bool) {
	return tm.lookupTxState(txID)
}

// ReadSetVersionForTest returns the captured readSet version for the given entity, or 0 if not present.
func ReadSetVersionForTest(s TxStateForTest, entityID string) int64 {
	inner, ok := s.(*txState)
	if !ok {
		return 0
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	return inner.readSet[entityID]
}

// WriteSetVersionForTest returns the captured writeSet version and whether it exists.
func WriteSetVersionForTest(s TxStateForTest, entityID string) (int64, bool) {
	inner, ok := s.(*txState)
	if !ok {
		return 0, false
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	v, present := inner.writeSet[entityID]
	return v, present
}

// NewStoreFactoryWithTMForTest creates a StoreFactory with the given pool and
// TransactionManager pre-wired. Use only in tests.
func NewStoreFactoryWithTMForTest(pool *pgxpool.Pool, tm *TransactionManager) *StoreFactory {
	f := NewStoreFactory(pool)
	f.setTransactionManager(tm)
	return f
}

// ValidateJSONPathForTest exposes the unexported path validator so tests in
// _test packages can assert the accept/reject contract directly without
// reaching through GroupedAggregate.
var ValidateJSONPathForTest = validateJSONPath

// PoolForTest returns the underlying pgx pool for diagnostic queries (e.g.
// EXPLAIN) that don't fit any typed store method.
func PoolForTest(f *StoreFactory) *pgxpool.Pool {
	return f.pool
}
