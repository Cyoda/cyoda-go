package postgres

import (
	"context"
	"time"

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
// idiom, at the shipped lock-timeout default — a fixture rolls back a database
// nothing else is touching, so its lock waits are uncontended. Use only in
// tests; never in production code.
func MigrateDownForTest(pool *pgxpool.Pool) error {
	return migrateDown(pool, defaultMigrateLockTimeout)
}

// ClassifyErrorForTest exposes classifyError to allow unit-testing of the
// serialization/deadlock classification logic without requiring a live database.
var ClassifyErrorForTest = classifyError

// txResidue reports which pieces of per-transaction bookkeeping are still held
// for txID: the pgx handle in the registry, the tenant, the origin and the
// txState (which carries the read and write sets). All four must be gone once a
// transaction has ended, however it ended.
func (tm *TransactionManager) txResidue(txID string) (registry, tenant, origin, state bool) {
	_, registry = tm.registry.Lookup(txID)
	_, tenant = tm.lookupTenant(txID)
	_, origin = tm.lookupOrigin(txID)
	_, state = tm.lookupTxState(txID)
	return
}

// HasTxState reports whether the given txID has an active txState entry.
func HasTxState(tm *TransactionManager, txID string) bool {
	tm.txStatesMu.RLock()
	defer tm.txStatesMu.RUnlock()
	_, ok := tm.txStates[txID]
	return ok
}

// OriginMapLenForTest returns the number of entries currently tracked in
// tm.origins. Used by leak-detection tests to verify that Commit/Rollback's
// cleanupTx removes the per-tx origin entry — see TransactionManager.origins
// godoc for why this bookkeeping map exists (postgres rebuilds
// TransactionState at Join and cannot rely on a shared pointer).
func OriginMapLenForTest(tm *TransactionManager) int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.origins)
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

// SearchCandidateIDsForTest returns the entity IDs the SQL WHERE fragment
// planQuery(filter) produces BEFORE any Go-side postFilter re-check — i.e.
// the raw pushdown candidate set exactly as searchCommitted would scan it,
// minus the residual application step. Used by the pushdown-soundness
// property test (soundness_property_test.go) to assert the SQL pre-filter
// is a SUPERSET of the kernel's true matches (never under-selects) —
// independent of, and in addition to, the "backend result == kernel result"
// equality proxy that store.Search() (which DOES apply the residual)
// provides.
func SearchCandidateIDsForTest(pool *pgxpool.Pool, ctx context.Context, tenantID spi.TenantID, entityName, modelVersion string, filter spi.Filter) ([]string, error) {
	s := &entityStore{q: pool, tenantID: tenantID}
	var plan sqlPlan
	if filter.Op != "" {
		plan = planQuery(filter)
	}
	baseQuery, baseArgs := s.searchBaseQuery(entityName, modelVersion, nil)
	if plan.where != "" {
		shifted := shiftPlaceholders(plan.where, len(baseArgs))
		baseQuery += " AND (" + shifted + ")"
		baseArgs = append(baseArgs, plan.args...)
	}
	rows, err := s.q.Query(ctx, baseQuery, baseArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		e, err := unmarshalEntityDoc(doc)
		if err != nil {
			return nil, err
		}
		ids = append(ids, e.Meta.ID)
	}
	return ids, rows.Err()
}

// NewStoreFactoryWithAcquireTimeoutForTest builds a factory whose stores carry a
// custom connection-acquire deadline, so a test can observe pool saturation in
// milliseconds instead of waiting out the shipped 10s default. Test-only; the
// production path constructs its config through parseConfig.
func NewStoreFactoryWithAcquireTimeoutForTest(pool *pgxpool.Pool, d time.Duration) *StoreFactory {
	cfg := defaultStoreConfig()
	cfg.AcquireTimeout = d
	return newStoreFactoryWithConfig(pool, cfg)
}
