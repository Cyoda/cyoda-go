package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const submitTimeTTL = 1 * time.Hour

// committedTx records a committed write in the in-memory log. seq orders the
// log: the conflict check compares it with the sequence number a transaction
// saw at Begin (txSnapshotSeq), never with a clock reading. Begin reserves its
// snapshot as the submit-time floor, so under a frozen clock a commit that
// preceded Begin can carry the same instant as the snapshot; the sequence
// number still orders them.
type committedTx struct {
	seq      int64
	writeSet map[string]bool
	// taskWrites holds the task rows the write changed, each with the row as
	// it was just before the write. It is apart from writeSet, whose keys are
	// entity ids, so the two checks never mix. A write that committed on its
	// own (commitTaskWrites) has only taskWrites. It is recorded only while
	// another transaction is open; no one else could read it.
	taskWrites map[taskKey]priorRow
}

// priorRow is a task row as it was just before a logged write: the row, or
// ok false when it did not exist. For a transaction whose snapshot precedes
// the write, the prior row of the earliest such write is the row its
// snapshot shows (see taskSnapshot).
type priorRow struct {
	row spi.ScheduledTask
	ok  bool
}

// taskWriteSet returns the task rows ops write.
func taskWriteSet(ops []scheduledTaskOp) map[taskKey]bool {
	if len(ops) == 0 {
		return nil
	}
	set := make(map[taskKey]bool, len(ops))
	for _, op := range ops {
		set[op.key] = true
	}
	return set
}

// submitTimeEntry pairs a committed transaction's submit time with the
// tenant that owns it, so GetSubmitTime can enforce the same tenant gate
// as every other tx-lifecycle method after the active-tx state is gone.
// The persisted submit_times table carries the same pairing in its
// tenant_id column for lookups after the in-memory entry aged out.
type submitTimeEntry struct {
	submitTime time.Time
	tenantID   spi.TenantID
}

// stagedAuditEvent is one audit event recorded inside an open transaction:
// its id assigned and its JSON document built, so nothing about it can fail
// at flush but the insert itself.
type stagedAuditEvent struct {
	entityID string
	event    spi.StateMachineEvent
	doc      []byte
}

// savepointSnapshot holds a deep copy of transaction state at savepoint time.
type savepointSnapshot struct {
	buffer            map[string]*spi.Entity
	readSet           map[string]bool
	writeSet          map[string]bool
	deletes           map[string]bool
	deleteAttribution map[string]spi.WriteAttribution // paired 1:1 with deletes — see TransactionState godoc

	// scheduledTaskOpsLen is len(transactionManager.scheduledTaskOps[txID]) at
	// the moment this savepoint was taken. scheduledTaskOps is append-only
	// (see stageTaskWrite), so — unlike the maps above, which are
	// deep-copied and restored wholesale — RollbackToSavepoint restores it by
	// truncating back to this recorded length instead of snapshotting it.
	scheduledTaskOpsLen int

	// auditOpsLen is len(transactionManager.auditOps[txID]) at the moment
	// this savepoint was taken. auditOps is append-only (see
	// stageAuditEvent), so RollbackToSavepoint restores it the same way as
	// scheduledTaskOpsLen: by truncating back to this recorded length.
	auditOpsLen int

	// supersededLens is the per-entityID length of supersededSaves[txID] at
	// the moment this savepoint was taken, mirroring scheduledTaskOpsLen's
	// truncate-back-to-length approach (append-only, so length is enough to
	// restore). An entityID absent here had no superseded entries yet at
	// savepoint time; RollbackToSavepoint clears it entirely rather than
	// truncating to an explicitly recorded zero.
	supersededLens map[string]int
}

// transactionManager implements spi.TransactionManager with application-layer
// Snapshot Isolation + First-Committer-Wins (SI+FCW). In-memory committedLog
// tracks conflicts; SQLite is the persistence layer.
//
// Commit ordering: acquire the commit gate -> validate SI+FCW by sequence
// number -> capture submitTime -> BEGIN IMMEDIATE -> flush -> COMMIT -> append
// committedLog with the next sequence number -> prune -> release the commit
// gate.
type transactionManager struct {
	factory *StoreFactory
	uuids   spi.UUIDGenerator
	// commitGate is a one-slot semaphore serializing the entire commit path
	// for SI+FCW correctness — a mutex in every respect except that a waiter
	// can be released by its context. See acquireCommitGate.
	commitGate chan struct{}
	// Lock order: tx.OpMu → commit gate → writer connection → mu. A joining
	// task-row write and a Get in a transaction hold the gate while they read
	// (see stageTaskWrite), so they can wait behind a whole commit; a flush
	// takes mu while it holds the writer connection, so nothing may wait for
	// that connection while holding mu.
	mu sync.Mutex // protects active, committedLog, commitSeq, txSnapshotSeq, committing, submitTimes, savepoints, txUniqueKeys

	active         map[string]*spi.TransactionState
	committedLog   []committedTx
	committing     map[string]bool
	submitTimes    map[string]submitTimeEntry
	savepoints     map[string]map[string]savepointSnapshot
	lastSubmitTime int64 // monotonic submit time in microseconds; bumped and read under mu, by callers holding the commit gate

	// commitSeq counts committed writes, a transaction's and a task-row
	// write that commits on its own alike; txSnapshotSeq holds its value at
	// each open transaction's Begin. Both are advanced and written only by
	// callers holding the commit gate and mu, so every Begin is ordered
	// wholly before or wholly after every commit; Rollback removes entries
	// under mu alone.
	commitSeq     int64
	txSnapshotSeq map[string]int64 // txID → commitSeq at Begin; removed by forgetLocked

	// txUniqueKeys holds per-entity unique keys captured at Save (buffer) time.
	// Keys are recorded when an entity is buffered so that flushToSQLite can
	// apply the correct keys per entity even in a mixed-model batch where each
	// Save may carry a different set of keys in its context.
	// Protected by mu. Cleaned up after commit or rollback.
	txUniqueKeys map[string]map[string][]spi.UniqueKey // txID → entityID → keys

	// scheduledTaskOps holds the task-row ops staged while the transaction
	// is open, as post-images (see scheduledTaskOp). Written by
	// flushToSQLite in the commit's sqlTx; discarded on Rollback and on
	// every abort path; truncated by RollbackToSavepoint. Protected by mu.
	scheduledTaskOps map[string][]scheduledTaskOp // txID → staged ops

	// auditOps holds the audit events recorded while the transaction is open,
	// in order. flushToSQLite inserts them in the commit's sqlTx, before the
	// commit-instant stamp; Rollback and every abort path drop them;
	// RollbackToSavepoint truncates them. Protected by mu. PostgreSQL gets the
	// same behaviour from recording on the transaction's connection.
	auditOps map[string][]stagedAuditEvent // txID → staged events

	// supersededSaves records, per (txID, entityID), each buffered
	// *spi.Entity value overwritten by a later same-entity Save/
	// CompareAndSave within the same open transaction, oldest first.
	// tx.Buffer only ever holds the FINAL value per entity — read-your-own-
	// writes only needs the latest — so without this side channel a same-tx
	// double-save would flush as a single commit row and
	// GetVersionByTransaction's earliest-wins contract (see its SPI doc
	// comment: "a transaction that saved the same entity more than once
	// before committing... the earliest is returned") could never be
	// satisfied for the intermediate value a later Save in the same tx
	// overwrote. flushToSQLite writes each entityID's superseded values (in
	// order) followed by the final tx.Buffer value as consecutive
	// entity_versions rows sharing the transaction's txID — mirrors the
	// memory plugin's identically-named field. Protected by mu.
	// Savepoint-scoped like tx.Buffer (length recorded at Savepoint,
	// truncated at RollbackToSavepoint — see savepointSnapshot.supersededLens).
	// Cleaned up after commit or rollback (no leak).
	supersededSaves map[string]map[string][]*spi.Entity // txID -> entityID -> superseded snapshots, oldest first

	// deletedBufferedEntities records, per (txID, entityID), the buffered
	// *spi.Entity Delete evicted from tx.Buffer within the same open
	// transaction. Delete removes id from tx.Buffer unconditionally (Buffer
	// and Deletes are kept mutually exclusive), so when id was never
	// committed before this transaction, the SELECT flushToSQLite's delete
	// loop runs against `entities` finds no row at all (sql.ErrNoRows) — a
	// same-transaction create-then-delete whose create was never separately
	// flushed. Without this side channel that shape wrote NOTHING (the old
	// code `continue`d past it): no entities row, no entity_versions row —
	// so a later Save reusing the same id under a different model found no
	// committed row to check against and silently accepted the model
	// change (the exact defect spi.ErrEntityModelMismatch exists to close).
	// flushToSQLite now uses this to still write the create+tombstone as
	// one committed row when the SELECT comes back empty. Protected by mu.
	// Not savepoint-scoped like supersededSaves: a RollbackToSavepoint that
	// restores id to tx.Buffer also removes it from tx.Deletes, so a stale
	// entry here is simply never read again. Cleaned up after commit or
	// rollback (no leak).
	deletedBufferedEntities map[string]map[string]*spi.Entity // txID -> entityID -> evicted entity
}

// Verify interface compliance at compile time.
var _ spi.TransactionManager = (*transactionManager)(nil)

func newTransactionManager(factory *StoreFactory, uuids spi.UUIDGenerator) *transactionManager {
	return &transactionManager{
		factory:                 factory,
		uuids:                   uuids,
		commitGate:              make(chan struct{}, 1),
		active:                  make(map[string]*spi.TransactionState),
		committing:              make(map[string]bool),
		submitTimes:             make(map[string]submitTimeEntry),
		savepoints:              make(map[string]map[string]savepointSnapshot),
		txUniqueKeys:            make(map[string]map[string][]spi.UniqueKey),
		txSnapshotSeq:           make(map[string]int64),
		scheduledTaskOps:        make(map[string][]scheduledTaskOp),
		auditOps:                make(map[string][]stagedAuditEvent),
		supersededSaves:         make(map[string]map[string][]*spi.Entity),
		deletedBufferedEntities: make(map[string]map[string]*spi.Entity),
	}
}

// recordUniqueKeys stores the unique keys for entityID under txID so that
// flushToSQLite can look them up per entity during commit. Last-write-wins,
// matching the semantics of tx.Buffer. Protected by mu.
func (m *transactionManager) recordUniqueKeys(txID, entityID string, keys []spi.UniqueKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.txUniqueKeys[txID] == nil {
		m.txUniqueKeys[txID] = make(map[string][]spi.UniqueKey)
	}
	m.txUniqueKeys[txID][entityID] = keys
}

// uniqueKeysFor retrieves the unique keys recorded for entityID under txID.
// Returns nil if none were recorded. Protected by mu.
func (m *transactionManager) uniqueKeysFor(txID, entityID string) []spi.UniqueKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.txUniqueKeys[txID][entityID]
}

// stageAuditEvent appends ev to txID's staged audit events. Protected by mu.
func (m *transactionManager) stageAuditEvent(txID string, ev stagedAuditEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditOps[txID] = append(m.auditOps[txID], ev)
}

// stagedAuditEvents returns a copy of txID's staged audit events. Protected by mu.
func (m *transactionManager) stagedAuditEvents(txID string) []stagedAuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]stagedAuditEvent(nil), m.auditOps[txID]...)
}

// busyTaskKeys returns the task rows an open transaction has staged a change
// to. Such a row is not claimable, and MarkUnsafe and RecordAttempt answer
// spi.ErrTaskBusy for it, until the transaction ends (C6). Callers hold the
// commit gate, so no Commit is between reading its ops and writing them, and
// no joining write stages an op meanwhile (see stageTaskWrite).
func (m *transactionManager) busyTaskKeys() map[taskKey]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := make(map[taskKey]bool)
	for _, ops := range m.scheduledTaskOps {
		for _, op := range ops {
			busy[op.key] = true
		}
	}
	return busy
}

// taskBusy reports whether an open transaction has staged a change to k, as
// busyTaskKeys does for one row. It stops at the first change it finds and
// allocates nothing. Callers hold the commit gate, as for busyTaskKeys.
func (m *transactionManager) taskBusy(k taskKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ops := range m.scheduledTaskOps {
		for _, op := range ops {
			if op.key == k {
				return true
			}
		}
	}
	return false
}

// zeroStagedMarks returns a copy of ops whose post-images have UnsafeMarked
// cleared, leaving the caller's own op.after values untouched. UnsafeMarked
// is never a stored column — every read derives it fresh from
// scheduled_task_marks (taskView.get/where) — so what gets staged or
// committed internally should not carry a frozen copy of it forward
// (matches the memory backend, whose applyTaskOps does the same at apply
// time). But a caller that already read the correct value under the commit
// gate (ClaimDue, from its candidate scan) needs it as it read it, not
// zeroed out from under its own struct — hence the copy, not a mutation.
func zeroStagedMarks(ops []scheduledTaskOp) []scheduledTaskOp {
	out := make([]scheduledTaskOp, len(ops))
	for i, op := range ops {
		if op.after != nil {
			cp := *op.after
			cp.UnsafeMarked = false
			op.after = &cp
		}
		out[i] = op
	}
	return out
}

// stageTaskWrite stages one joining write on txID. It passes plan the ops
// staged so far and the prior rows of txID's snapshot (see taskSnapshot), and
// appends the ops plan returns, holding the commit gate from the read to the
// append. No write commits meanwhile, so the rows plan reads on db, the prior
// rows and the staged ops form one view: txID's snapshot, then its own ops.
// Two joining writes are never planned from the same view, so neither
// post-image overwrites the other. flushToSQLite writes the staged ops in
// the commit's sqlTx; every abort path discards them.
//
// Caller holds tx.OpMu (read).
func (m *transactionManager) stageTaskWrite(txID string, tenant spi.TenantID, plan func(staged []scheduledTaskOp, prior map[taskKey]priorRow) ([]scheduledTaskOp, error)) error {
	_ = m.acquireCommitGate(context.Background())
	defer m.releaseCommitGate()
	staged, prior := m.taskSnapshot(txID, tenant)
	ops, err := plan(staged, prior)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return nil
	}
	ops = zeroStagedMarks(ops)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduledTaskOps[txID] = append(m.scheduledTaskOps[txID], ops...)
	return nil
}

// taskSnapshot returns txID's staged ops and, for each of tenant's task rows
// a write logged after txID's Begin changed, the row as txID's snapshot shows
// it: the prior row of the earliest such write. A row with no such write
// shows its committed state. Pruning keeps every entry above an open
// transaction's snapshot, so none of these is lost while txID is open.
//
// Caller holds tx.OpMu (read) and has checked that the transaction is open
// and of tenant: an ended transaction's snapshot sequence number is gone, and
// with it the bound on which entries apply. Caller also holds the commit gate
// and reads the committed rows under it: every writer holds the gate from its
// first read to its log entry, so the committed rows and the log agree.
func (m *transactionManager) taskSnapshot(txID string, tenant spi.TenantID) ([]scheduledTaskOp, map[taskKey]priorRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	staged := append([]scheduledTaskOp(nil), m.scheduledTaskOps[txID]...)
	var prior map[taskKey]priorRow
	snapshotSeq := m.txSnapshotSeq[txID]
	for _, committed := range m.committedLog { // in sequence order
		if committed.seq <= snapshotSeq {
			continue
		}
		for k, p := range committed.taskWrites {
			if k.tenant != tenant {
				continue
			}
			if _, seen := prior[k]; seen {
				continue
			}
			if prior == nil {
				prior = make(map[taskKey]priorRow)
			}
			prior[k] = p
		}
	}
	return staged, prior
}

// otherTxOpenLocked reports whether a transaction other than txID is open.
// Caller holds mu.
func (m *transactionManager) otherTxOpenLocked(txID string) bool {
	for id := range m.active {
		if id != txID {
			return true
		}
	}
	return false
}

// taskPriors reads, before a write, the rows ops will change, for the log
// entry of that write. It returns nil when no transaction but txID is open:
// the entry is then pruned at once, and a transaction that begins later
// cannot see it. Caller holds the commit gate, which Begin takes too, so no
// transaction begins and no other write lands until the entry is logged.
func (m *transactionManager) taskPriors(ctx context.Context, txID string, ops []scheduledTaskOp) (map[taskKey]priorRow, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	if !func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.otherTxOpenLocked(txID)
	}() {
		return nil, nil
	}
	priors := make(map[taskKey]priorRow, len(ops))
	for _, op := range ops {
		if _, done := priors[op.key]; done {
			continue
		}
		rows, err := readTasks(ctx, m.factory.db, selectTaskSQL+` WHERE t.tenant_id = ? AND t.id = ?`,
			string(op.key.tenant), op.key.id)
		if err != nil {
			return nil, fmt.Errorf("failed to read scheduled task %s: %w", op.key.id, err)
		}
		var p priorRow
		if len(rows) == 1 {
			p = priorRow{row: rows[0], ok: true}
		}
		priors[op.key] = p
	}
	return priors, nil
}

// commitTaskWrites writes task-row ops that commit on their own — a
// never-joining method, or a joining one called without a transaction — in
// one sqlTx of their own, and records them in the committed log, so that a
// transaction that began before them and writes one of the same rows fails
// at commit. then, when not nil, runs in the same sqlTx after the ops.
// Caller holds the commit gate from its first read to the return, so the
// read, the write and the log entry are ordered wholly before or wholly
// after any Begin and any other commit.
func (m *transactionManager) commitTaskWrites(ctx context.Context, ops []scheduledTaskOp, then func(*sql.Tx) error) error {
	if len(ops) == 0 && then == nil {
		return nil
	}
	ops = zeroStagedMarks(ops)
	priors, err := m.taskPriors(ctx, "", ops)
	if err != nil {
		return err
	}
	sqlTx, err := m.factory.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin a scheduled task write: %w", err)
	}
	defer sqlTx.Rollback()
	for _, op := range ops {
		if err := applyTaskOp(ctx, sqlTx, op); err != nil {
			return fmt.Errorf("failed to write scheduled task %s: %w", op.key.id, err)
		}
	}
	if then != nil {
		if err := then(sqlTx); err != nil {
			return err
		}
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("failed to commit a scheduled task write: %w", err)
	}
	if len(priors) > 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.logTaskWritesLocked(priors)
	}
	return nil
}

// logTaskWritesLocked records a task-row write that committed on its own, so
// that a transaction that began before it and writes one of the same rows
// fails at commit. Caller holds the commit gate — which Begin also takes, so
// the write and its entry are ordered wholly before or wholly after any
// Begin — and mu.
func (m *transactionManager) logTaskWritesLocked(priors map[taskKey]priorRow) {
	m.commitSeq++
	m.committedLog = append(m.committedLog, committedTx{seq: m.commitSeq, taskWrites: priors})
	m.pruneCommittedLogLocked()
}

// stageSuperseded appends prior — the tx.Buffer value a Save/CompareAndSave
// call is about to overwrite — to txID's superseded list for entityID, in
// overwrite order. No-op when prior is nil (the entity's first Save in this
// transaction: nothing superseded yet). See the supersededSaves field godoc
// for why this exists. Protected by mu.
func (m *transactionManager) stageSuperseded(txID, entityID string, prior *spi.Entity) {
	if prior == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.supersededSaves[txID] == nil {
		m.supersededSaves[txID] = make(map[string][]*spi.Entity)
	}
	m.supersededSaves[txID][entityID] = append(m.supersededSaves[txID][entityID], prior)
}

// stageDeletedBufferedEntity records the buffered *spi.Entity Delete is about
// to evict from tx.Buffer — see deletedBufferedEntities's field doc for why
// this is needed. e is stored by reference; the caller must not mutate it
// afterward (Delete evicts it from tx.Buffer in the same step, so nothing
// else holds it).
func (m *transactionManager) stageDeletedBufferedEntity(txID, entityID string, e *spi.Entity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deletedBufferedEntities[txID] == nil {
		m.deletedBufferedEntities[txID] = make(map[string]*spi.Entity)
	}
	m.deletedBufferedEntities[txID][entityID] = e
}

// supersededFor retrieves the superseded values staged for entityID under
// txID, oldest first. Returns nil if none were recorded. Protected by mu.
func (m *transactionManager) supersededFor(txID, entityID string) []*spi.Entity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.supersededSaves[txID][entityID]
}

// deletedBufferedEntityFor retrieves the buffered entity Delete evicted for
// entityID under txID, if any — see deletedBufferedEntities's field doc.
// Protected by mu.
func (m *transactionManager) deletedBufferedEntityFor(txID, entityID string) (*spi.Entity, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.deletedBufferedEntities[txID][entityID]
	return e, ok
}

// insertDeletedBufferedTombstone writes the single committed row for a
// same-transaction create-then-delete: an `entities` row already deleted
// (version 1) and one `entity_versions` DELETED row (version 1) — mirroring
// the shape a real create followed by a real delete would leave, collapsed
// into one commit because the create never separately flushed (see
// deletedBufferedEntities's field doc). Matches the memory plugin's
// equivalent single-tombstone shape for the same case, so the two backends
// agree on the entity's version history rather than diverging on it.
//
// The entities row's data/meta come from the buffered entity — the content
// it would have held had the create flushed on its own — so a later
// point-in-time read of the (deleted) row sees the same content a
// non-collapsed create+delete would have left. attribution is the DELETE's
// (not the create's): this row's only recorded actor is who deleted it,
// matching the tombstone user_id column's existing convention.
func insertDeletedBufferedTombstone(ctx context.Context, sqlTx *sql.Tx, tid, txID, entityID string, submitMicro int64, buffered *spi.Entity, attribution spi.WriteAttribution) error {
	entityMeta := buffered.Meta
	entityMeta.Version = 1
	entityMeta.ChangeType = "DELETED"
	entityMeta.LastModifiedDate = microToTime(submitMicro)
	if entityMeta.CreationDate.IsZero() {
		entityMeta.CreationDate = entityMeta.LastModifiedDate
	}
	metaJSON, err := marshalEntityMeta(&entityMeta)
	if err != nil {
		return fmt.Errorf("marshal meta for deleted-buffered entity %s: %w", entityID, err)
	}
	_, err = sqlTx.ExecContext(ctx,
		`INSERT INTO entities
		 (tenant_id, entity_id, model_name, model_version, version, data, meta, deleted, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, jsonb(?), jsonb(?), 1, ?, ?)`,
		tid, entityID, buffered.Meta.ModelRef.EntityName, buffered.Meta.ModelRef.ModelVersion,
		string(buffered.Data), string(metaJSON), submitMicro, submitMicro)
	if err != nil {
		return fmt.Errorf("insert deleted-buffered entities row %s: %w", entityID, err)
	}

	tombstoneMeta, err := marshalTombstoneMeta(attribution.Attributed.Kind, attribution.Executor)
	if err != nil {
		return fmt.Errorf("marshal tombstone meta for deleted-buffered entity %s: %w", entityID, err)
	}
	_, err = sqlTx.ExecContext(ctx,
		`INSERT INTO entity_versions
		 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
		 VALUES (?, ?, ?, ?, 1, NULL, jsonb(?), 'DELETED', ?, ?, ?)`,
		tid, entityID, buffered.Meta.ModelRef.EntityName, buffered.Meta.ModelRef.ModelVersion,
		string(tombstoneMeta), txID, submitMicro, attribution.Attributed.ID)
	if err != nil {
		return fmt.Errorf("insert deleted-buffered version %s: %w", entityID, err)
	}
	return nil
}

// seedLastSubmitTime reads the maximum submit_time from entity_versions
// so that lastSubmitTime is monotonic across process restarts.
func (m *transactionManager) seedLastSubmitTime() {
	var maxTime sql.NullInt64
	err := m.factory.db.QueryRow(
		"SELECT MAX(submit_time) FROM entity_versions").Scan(&maxTime)
	if err == nil && maxTime.Valid {
		m.lastSubmitTime = maxTime.Int64
	}
}

// acquireCommitGate takes the one-slot commit gate, which serializes the whole
// commit path for SI+FCW correctness and gates Begin's snapshot floor (see
// Begin). It is a channel rather than a mutex only so that a waiter can be
// released by its context: Begin passes its caller's context and gets
// ctx.Err() back if the caller gives up before the gate frees. A path that
// must not abandon work already in flight passes context.Background(), for
// which the acquisition cannot fail. Every acquisition is paired with a
// deferred releaseCommitGate on the next line, as for a mutex.
// A context already done on entry always loses: the select below would pick
// between a free gate and the done context at random, so the check comes
// first and the answer is deterministic.
func (m *transactionManager) acquireCommitGate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.commitGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseCommitGate releases the gate taken by acquireCommitGate. Releasing a
// gate nobody holds is a discipline error — an acquisition that failed and was
// deferred anyway, or a release paired with nothing — and it panics rather than
// blocking: a mis-paired release must fail loudly at its call site, not hang
// the caller and every writer behind it.
func (m *transactionManager) releaseCommitGate() {
	select {
	case <-m.commitGate:
	default:
		panic("commit gate released without being held")
	}
}

// nextSubmitTime returns the submit time to stamp on a write, in
// microseconds, and records it as the new floor. max(now, lastSubmitTime+1)
// guarantees forward progress even under NTP steps, VM pause/migrate, leap-
// second smearing, or a frozen test clock, and — because Begin floors a new
// transaction's SnapshotTime to lastSubmitTime — guarantees a write never
// stamps at or below a snapshot already open. Every path that stamps a
// submit_time uses it: Commit's step 4 and the direct writes (saveDirectly,
// the non-tx Delete). Callers hold the commit gate from here through their own
// commit; lock order commitGate → mu.
func (m *transactionManager) nextSubmitTime() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	nowMicro := m.factory.clock.Now().UnixMicro()
	if nowMicro <= m.lastSubmitTime {
		nowMicro = m.lastSubmitTime + 1
	}
	m.lastSubmitTime = nowMicro
	return nowMicro
}

// forgetLocked drops every piece of per-transaction state the manager holds
// for txID. Caller holds mu.
func (m *transactionManager) forgetLocked(txID string) {
	delete(m.active, txID)
	delete(m.committing, txID)
	delete(m.savepoints, txID)
	delete(m.txUniqueKeys, txID)
	delete(m.txSnapshotSeq, txID)
	delete(m.scheduledTaskOps, txID)
	delete(m.auditOps, txID)
	delete(m.supersededSaves, txID)
	delete(m.deletedBufferedEntities, txID)
}

// pruneCommittedLogLocked drops the log entries no open transaction can
// conflict with: those at or below the oldest open snapshot's sequence
// number, or all of them when no transaction is open. Caller holds mu.
func (m *transactionManager) pruneCommittedLogLocked() {
	if len(m.active) == 0 {
		m.committedLog = m.committedLog[:0]
		return
	}
	oldest := int64(-1)
	for txID := range m.active {
		if s := m.txSnapshotSeq[txID]; oldest < 0 || s < oldest {
			oldest = s
		}
	}
	pruned := m.committedLog[:0]
	for _, c := range m.committedLog {
		if c.seq > oldest {
			pruned = append(pruned, c)
		}
	}
	m.committedLog = pruned
}

// Begin starts a new transaction. It resolves the tenant from the context,
// generates a unique transaction ID, captures a snapshot time, and returns
// a new context carrying the TransactionState.
func (m *transactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", ctx, fmt.Errorf("no user context — cannot begin transaction")
	}
	if uc.Tenant.ID == "" {
		return "", ctx, fmt.Errorf("user context has no tenant — cannot begin transaction")
	}

	txID := uuid.UUID(m.uuids.NewTimeUUID()).String()
	nowMicro := m.factory.clock.Now().UnixMicro()

	tx := &spi.TransactionState{
		ID:                txID,
		TenantID:          uc.Tenant.ID,
		Origin:            spi.ResolveOrigin(ctx),
		ReadSet:           make(map[string]bool),
		WriteSet:          make(map[string]bool),
		Buffer:            make(map[string]*spi.Entity),
		Deletes:           make(map[string]bool),
		DeleteAttribution: make(map[string]spi.WriteAttribution),
	}

	// Snapshot time must be at least lastSubmitTime so that the transaction
	// sees all previously committed data. Without this floor, a monotonic
	// submit-time bump could push a commit past the next Begin's raw clock
	// value, making committed entities invisible to new transactions.
	//
	// The floor is captured under the commit gate, and every write that
	// stamps a submit_time holds the gate until its rows are committed — a
	// transaction's flush (Commit bumps lastSubmitTime at step 4, before its
	// rows are visible) and a direct write (saveDirectly, the non-tx Delete)
	// alike. A Begin that read a stamped value without waiting would carry a
	// SnapshotTime at or after a write whose rows it cannot yet see on
	// readDB. Waiting here makes "submit_time <= SnapshotTime" imply "rows
	// visible" on every connection. It also makes the sequence number taken
	// below exact: no commit is between its check and its log entry, so a
	// commit either precedes this Begin wholly (its seq is at or below the
	// snapshot's) or follows it wholly. Lock order commitGate → mu, the
	// order Commit uses.
	//
	// The snapshot is then RESERVED as the new floor. Reading the floor is
	// not enough: with the floor below the clock (a quiet database leaves it
	// at zero) the snapshot is the raw clock value, and the next write stamps
	// max(now, floor+1) — the same microsecond — which the visibility rule
	// (submit_time <= SnapshotTime) counts as visible to a transaction that
	// began before it. Reserving makes the next stamp strictly later.
	//
	// The wait honours the caller's context: a client that has given up gets
	// its own context error rather than a transaction it no longer wants.
	if err := func() error {
		if err := m.acquireCommitGate(ctx); err != nil {
			return fmt.Errorf("Begin: %w", err)
		}
		defer m.releaseCommitGate()
		m.mu.Lock()
		defer m.mu.Unlock()
		if nowMicro < m.lastSubmitTime {
			nowMicro = m.lastSubmitTime
		}
		tx.SnapshotTime = time.UnixMicro(nowMicro)
		m.lastSubmitTime = nowMicro
		m.active[txID] = tx
		m.txSnapshotSeq[txID] = m.commitSeq
		return nil
	}(); err != nil {
		return "", ctx, err
	}

	return txID, spi.WithTransaction(ctx, tx), nil
}

// Join returns a context carrying the TransactionState for an existing active
// transaction. This allows multiple goroutines to participate in the same
// transaction.
func (m *transactionManager) Join(ctx context.Context, txID string) (context.Context, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, ok := m.active[txID]
	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if tx.RolledBack {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	// Verify tenant matches. Strict — rejects nil UserContext to match
	// Commit/Rollback's gate. Before the tenant-strictness fix this was
	// permissive on a nil user context, allowing any caller without one to
	// Join an arbitrary active tx.
	uc := spi.GetUserContext(ctx)
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	return spi.WithTransaction(ctx, tx), nil
}

// Commit validates the transaction against the committed log for SI+FCW conflicts,
// flushes the write buffer and deletes to SQLite, and records the commit in the
// log.
//
// The commit gate serializes the entire commit path. This is required for
// SI+FCW correctness -- without it, two commits could both validate against a
// stale committedLog and both succeed, missing a conflict.
func (m *transactionManager) Commit(ctx context.Context, txID string) error {
	// 1. Look up the active transaction and mark as committing (TOCTOU guard).
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		m.mu.Unlock()
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}
	if m.committing[txID] {
		m.mu.Unlock()
		return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxCommitInProgress, txID)
	}
	m.committing[txID] = true
	m.mu.Unlock()

	// 1b. Acquire transaction operation write lock -- waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	// 2. Acquire the commit gate -- serializes the entire commit path. Taken
	// with a background context, not the caller's: a commit that has begun
	// must finish, so a caller that gives up here must not leave the flush
	// half-done. Only Begin honours its caller's context on this gate, and
	// acquiring against a context that is never done cannot fail.
	_ = m.acquireCommitGate(context.Background())
	defer m.releaseCommitGate()

	// 3. Conflict detection. A transaction conflicts with every commit whose
	// sequence number is above the one it saw at Begin and whose write set
	// meets its read or write set, or whose task writes meet its own. Entity
	// ids and task rows are checked in two separate loops over two separate
	// sets. The staged task-row ops are captured here: tx.OpMu.Lock (step
	// 1b) blocks every stageTaskWrite, so they are stable for the rest of the
	// commit.
	var scheduledOps []scheduledTaskOp
	var auditEvents []stagedAuditEvent
	if err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		scheduledOps = append([]scheduledTaskOp(nil), m.scheduledTaskOps[txID]...)
		auditEvents = append([]stagedAuditEvent(nil), m.auditOps[txID]...)
		taskWrites := taskWriteSet(scheduledOps)
		snapshotSeq := m.txSnapshotSeq[txID]
		for _, committed := range m.committedLog {
			if committed.seq <= snapshotSeq {
				continue
			}
			conflict := false
			for entityID := range committed.writeSet {
				if tx.ReadSet[entityID] || tx.WriteSet[entityID] {
					conflict = true
					break
				}
			}
			for k := range committed.taskWrites {
				if taskWrites[k] {
					conflict = true
					break
				}
			}
			if conflict {
				m.forgetLocked(txID)
				return spi.ErrConflict
			}
		}
		return nil
	}(); err != nil {
		return err
	}

	// 4. Capture submit time under the monotonic floor every stamping path
	// shares — see nextSubmitTime.
	submitTime := time.UnixMicro(m.nextSubmitTime())

	// 5. Read the task rows' prior state for the log entry, then flush
	// buffer, deletes, and staged scheduled-task ops to SQLite. The commit
	// gate is held, so nothing changes the rows between the two.
	priors, err := m.taskPriors(ctx, txID, scheduledOps)
	if err == nil {
		err = m.flushToSQLite(ctx, tx, submitTime, scheduledOps, auditEvents)
	}
	if err != nil {
		// On flush failure, clean up the transaction.
		func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			tx.RolledBack = true
			m.forgetLocked(txID)
		}()
		// ErrUniqueViolation from claim writes must not be re-classified —
		// classifyError passes through non-sqlite errors unchanged.
		return fmt.Errorf("flush to sqlite: %w", classifyError(err))
	}

	// 6. Record in committed log, submit times, and prune.
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		m.commitSeq++
		m.committedLog = append(m.committedLog, committedTx{
			seq:        m.commitSeq,
			writeSet:   tx.WriteSet,
			taskWrites: priors,
		})
		m.submitTimes[txID] = submitTimeEntry{submitTime: submitTime, tenantID: tx.TenantID}

		// Evict old submit times beyond TTL.
		evictBefore := m.factory.clock.Now().Add(-submitTimeTTL)
		for id, e := range m.submitTimes {
			if e.submitTime.Before(evictBefore) {
				delete(m.submitTimes, id)
			}
		}

		m.forgetLocked(txID)
		m.pruneCommittedLogLocked()
	}()

	// Prune old submit_times from SQLite (best-effort).
	evictBefore := m.factory.clock.Now().Add(-submitTimeTTL)
	_, _ = m.factory.db.ExecContext(ctx,
		"DELETE FROM submit_times WHERE submit_time < ?",
		timeToMicro(evictBefore))

	return nil
}

// flushToSQLite performs the atomic write of the transaction's buffered
// entities and deletes to SQLite within a single SQLite transaction.
func (m *transactionManager) flushToSQLite(ctx context.Context, tx *spi.TransactionState, submitTime time.Time, scheduledOps []scheduledTaskOp, auditEvents []stagedAuditEvent) error {
	sqlTx, err := m.factory.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite tx: %w", err)
	}
	defer sqlTx.Rollback()

	submitMicro := timeToMicro(submitTime)
	tid := string(tx.TenantID)

	// Release unique-key claims held by entities deleted in THIS transaction
	// BEFORE flushing buffered creates/updates (which insert claims). Otherwise a
	// same-transaction "delete A holding value V, create B wanting V" sees A's
	// claim still present when B's is inserted and wrongly fails with a unique
	// violation. The whole flush is one sqlite tx, so this early release rolls
	// back atomically with everything else on any later error. releaseClaims is a
	// no-op (idempotent DELETE) for a delete target that holds no claims.
	for entityID := range tx.Deletes {
		if err := releaseClaims(ctx, sqlTx, tid, entityID); err != nil {
			return fmt.Errorf("release claims on delete %s: %w", entityID, err)
		}
	}

	// Flush buffered entities.
	for entityID, entity := range tx.Buffer {
		var existingVersion sql.NullInt64
		var existingCreatedAt sql.NullInt64
		err := sqlTx.QueryRowContext(ctx,
			"SELECT version, created_at FROM entities WHERE tenant_id = ? AND entity_id = ?",
			tid, entityID).Scan(&existingVersion, &existingCreatedAt)
		isNew := err == sql.ErrNoRows
		if err != nil && !isNew {
			return fmt.Errorf("check entity %s: %w", entityID, err)
		}

		// Version numbering starts at 1 — see saveDirectly's matching comment
		// in entity_store.go.
		baseVersion := int64(0)
		createdAtMicro := submitMicro
		if !isNew {
			baseVersion = existingVersion.Int64
			createdAtMicro = existingCreatedAt.Int64
		}

		// Flush this entity's superseded intra-tx saves (oldest first), then
		// the final tx.Buffer value, as consecutive entity_versions rows
		// sharing txID — see supersededSaves's field godoc: this is what
		// lets GetVersionByTransaction's earliest-wins contract hold for a
		// same-tx double-save, where tx.Buffer itself only ever holds the
		// final value. Only the final row updates the `entities` current-
		// state table; the entity object mutated on each iteration is the
		// staged pointer captured at Save time (each Save call copies via
		// copyEntity, so superseded entries and the final tx.Buffer entry
		// are always distinct objects — mutating in place is safe here).
		superseded := m.supersededFor(tx.ID, entityID)
		toFlush := make([]*spi.Entity, 0, len(superseded)+1)
		toFlush = append(toFlush, superseded...)
		toFlush = append(toFlush, entity)

		curIsNew := isNew
		var metaJSON []byte
		for _, staged := range toFlush {
			nextVersion := baseVersion + 1
			baseVersion = nextVersion

			// DERIVE ChangeType from row-existence, like the non-tx save
			// path (see deriveChangeType) — never trust it verbatim from
			// the staged entity, which may carry a stale value fetched
			// before this transaction began. curIsNew tracks the SAME
			// "no prior row" semantics deriveChangeType's isNew parameter
			// expects — only the first staged item (if the entity itself
			// is new) is true; every subsequent same-tx superseded save
			// finds a prior row from the iteration before it.
			changeType := deriveChangeType(staged.Meta.ChangeType, curIsNew)
			curIsNew = false

			staged.Meta.Version = nextVersion
			staged.Meta.LastModifiedDate = submitTime
			staged.Meta.TransactionID = tx.ID
			staged.Meta.ChangeType = changeType
			staged.Meta.TenantID = tx.TenantID
			if isNew {
				staged.Meta.CreationDate = submitTime
			} else {
				staged.Meta.CreationDate = microToTime(createdAtMicro)
			}

			mj, err := marshalEntityMeta(&staged.Meta)
			if err != nil {
				return fmt.Errorf("marshal meta for %s: %w", entityID, err)
			}
			metaJSON = mj

			_, err = sqlTx.ExecContext(ctx,
				`INSERT INTO entity_versions
				 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
				 VALUES (?, ?, ?, ?, ?, jsonb(?), jsonb(?), ?, ?, ?, ?)`,
				tid, entityID,
				staged.Meta.ModelRef.EntityName, staged.Meta.ModelRef.ModelVersion,
				nextVersion, string(staged.Data), string(metaJSON),
				staged.Meta.ChangeType, tx.ID, submitMicro,
				staged.Meta.ChangeUser)
			if err != nil {
				return fmt.Errorf("insert version %s: %w", entityID, err)
			}
		}

		// entity is toFlush's last element (the final tx.Buffer value) —
		// its Meta now reflects the last loop iteration's nextVersion/
		// metaJSON, i.e. the current-state row this transaction commits.
		nextVersion := entity.Meta.Version
		_, err = sqlTx.ExecContext(ctx,
			`INSERT OR REPLACE INTO entities
			 (tenant_id, entity_id, model_name, model_version, version, data, meta, deleted, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, jsonb(?), jsonb(?), 0, ?, ?)`,
			tid, entityID,
			entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion,
			nextVersion, string(entity.Data), string(metaJSON),
			createdAtMicro, submitMicro)
		if err != nil {
			return fmt.Errorf("upsert entity %s: %w", entityID, err)
		}

		// Maintain unique-key claims using keys captured at Save (buffer) time.
		// Keys must not be read from ctx here: flushToSQLite runs once at Commit
		// with a single context (the last committer's), which would be wrong for
		// entities buffered with different key contexts in a mixed-model batch.
		keys := m.uniqueKeysFor(tx.ID, entityID)
		if err := replaceClaims(ctx, sqlTx, tid, entity, keys); err != nil {
			return fmt.Errorf("replace claims %s: %w", entityID, err)
		}
	}

	// Flush deletes.
	for entityID := range tx.Deletes {
		// Attribution: prefer tx.DeleteAttribution[entityID], captured at
		// stage time (the STAGER's context, under the same OpMu section
		// that set tx.Deletes[entityID] — see entityStore.Delete/DeleteAll).
		// Fall back to spi.AttributionFor(ctx) — this Commit call's own
		// ctx, i.e. the committer — only when no staged entry exists (a
		// caller that mutated tx.Deletes directly, bypassing EntityStore).
		// This is what fixes the prior bug: the tombstone's user_id column
		// was always written as '' — no actor at all, staged or committer.
		attribution, staged := tx.DeleteAttribution[entityID]
		if !staged {
			a, e := spi.AttributionFor(ctx)
			attribution = spi.WriteAttribution{Attributed: a, Executor: e}
		}

		// Get current entity info for the delete version record.
		var curVersion int64
		var modelName, modelVersion string
		err := sqlTx.QueryRowContext(ctx,
			"SELECT version, model_name, model_version FROM entities WHERE tenant_id = ? AND entity_id = ?",
			tid, entityID).Scan(&curVersion, &modelName, &modelVersion)
		if err != nil {
			if err == sql.ErrNoRows {
				// A same-transaction create-then-delete: Delete evicted the
				// buffered create from tx.Buffer before this flush ran, so
				// there is no `entities` row to soft-delete (see
				// deletedBufferedEntities's field doc). Write the
				// create+tombstone as one committed row instead of
				// skipping — an id genuinely never saved at all (Delete
				// bypassing EntityStore) has no staged entry either, and
				// stays a no-op skip.
				buffered, ok := m.deletedBufferedEntityFor(tx.ID, entityID)
				if !ok {
					continue
				}
				if err := insertDeletedBufferedTombstone(ctx, sqlTx, tid, tx.ID, entityID, submitMicro, buffered, attribution); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("check entity for delete %s: %w", entityID, err)
		}

		nextVersion := curVersion + 1

		_, err = sqlTx.ExecContext(ctx,
			"UPDATE entities SET deleted = 1, updated_at = ?, version = ? WHERE tenant_id = ? AND entity_id = ?",
			submitMicro, nextVersion, tid, entityID)
		if err != nil {
			return fmt.Errorf("soft delete entity %s: %w", entityID, err)
		}

		tombstoneMeta, err := marshalTombstoneMeta(attribution.Attributed.Kind, attribution.Executor)
		if err != nil {
			return fmt.Errorf("marshal tombstone meta %s: %w", entityID, err)
		}

		_, err = sqlTx.ExecContext(ctx,
			`INSERT INTO entity_versions
			 (tenant_id, entity_id, model_name, model_version, version, data, meta, change_type, transaction_id, submit_time, user_id)
			 VALUES (?, ?, ?, ?, ?, NULL, jsonb(?), 'DELETED', ?, ?, ?)`,
			tid, entityID,
			modelName, modelVersion,
			nextVersion, string(tombstoneMeta), tx.ID, submitMicro,
			attribution.Attributed.ID)
		if err != nil {
			return fmt.Errorf("insert delete version %s: %w", entityID, err)
		}
		// (Claims for deleted entities are released in the pre-pass above, before
		// the buffer flush, so a same-tx delete+reclaim does not falsely conflict.)
	}

	// Record submit time, paired with the owning tenant so the persistent
	// fallback in GetSubmitTime can enforce the tenant gate after the
	// in-memory entry ages out.
	_, err = sqlTx.ExecContext(ctx,
		"INSERT OR REPLACE INTO submit_times (tx_id, tenant_id, submit_time) VALUES (?, ?, ?)",
		tx.ID, tid, submitMicro)
	if err != nil {
		return fmt.Errorf("record submit time: %w", err)
	}

	// Audit events recorded inside this transaction are inserted here, in
	// sqlTx, so they commit or roll back with it. Then every event LABELLED
	// with this transaction — those, and any recorded outside a transaction
	// under its id (EmitTransitionAborted labels by a cascade entry's id) —
	// takes the commit instant, so the audit trail and the version history
	// cannot drift apart or invert. Served by idx_sm_events_tenant_tx
	// (migration 000008).
	for _, st := range auditEvents {
		if _, err := sqlTx.ExecContext(ctx, insertAuditEventSQL,
			tid, st.entityID, st.event.TimeUUID, st.event.TransactionID,
			st.event.Timestamp.UnixMicro(), st.doc); err != nil {
			return fmt.Errorf("record staged audit event %s: %w", st.event.TimeUUID, classifyRejection(err))
		}
	}
	_, err = sqlTx.ExecContext(ctx,
		"UPDATE sm_audit_events SET timestamp = ? WHERE tenant_id = ? AND transaction_id = ?",
		submitMicro, tid, tx.ID)
	if err != nil {
		return fmt.Errorf("stamp audit events: %w", err)
	}

	// Write the staged task-row post-images. Their checks ran when they were
	// staged, and step 3 proved that no other writer changed those rows since
	// this transaction began. Still inside sqlTx, so they commit atomically
	// with the entity write, and every early return rolls them back too.
	for _, op := range scheduledOps {
		if err := applyTaskOp(ctx, sqlTx, op); err != nil {
			return fmt.Errorf("apply scheduled task op %s: %w", op.key.id, err)
		}
	}

	return sqlTx.Commit()
}

// Rollback discards an active transaction without committing any changes.
func (m *transactionManager) Rollback(ctx context.Context, txID string) error {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		m.mu.Unlock()
		return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}
	m.mu.Unlock()

	// Acquire transaction operation write lock -- waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx.RolledBack = true
		m.forgetLocked(txID) // staged ops and side-channel values are discarded unapplied — see the field docs
	}()
	return nil
}

// GetSubmitTime returns the submit time of a committed transaction.
// Checks in-memory cache first, then falls back to the submit_times table.
//
// Tenant isolation: like every other tx-lifecycle method, the caller's
// tenant must match the transaction's tenant — on the in-memory path AND
// the persistent-fallback path. The check runs before any state-dependent
// response so a cross-tenant caller learns neither the submit time nor
// whether the transaction is in flight or committed.
func (m *transactionManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()

	if tx, ok := m.active[txID]; ok {
		if uc == nil || uc.Tenant.ID != tx.TenantID {
			return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
		}
		return time.Time{}, fmt.Errorf("%w (txID=%s)", spi.ErrTxNotCommitted, txID)
	}

	if e, ok := m.submitTimes[txID]; ok {
		if uc == nil || uc.Tenant.ID != e.tenantID {
			return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
		}
		return e.submitTime, nil
	}

	// Fall back to persisted submit_times table. Only a missing row is
	// "not found" — a query failure is an infrastructure error and must
	// not masquerade as a definitive answer.
	var micro int64
	var tenantID string
	err := m.factory.db.QueryRowContext(ctx,
		"SELECT submit_time, tenant_id FROM submit_times WHERE tx_id = ?", txID).Scan(&micro, &tenantID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxNotFound, txID)
	case err != nil:
		return time.Time{}, fmt.Errorf("GetSubmitTime: query submit_times: %w", err)
	}
	if uc == nil || uc.Tenant.ID != spi.TenantID(tenantID) {
		return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}
	return time.UnixMicro(micro), nil
}

// CommittedLogLen returns the current length of the committed log.
// Exported for testing only.
func (m *transactionManager) CommittedLogLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.committedLog)
}

// Savepoint creates a named savepoint within the given transaction by
// deep-copying the transaction's buffer maps.
//
// Locking discipline (mirrors the memory plugin):
// Savepoint reads tx.Buffer / tx.ReadSet / tx.WriteSet / tx.Deletes — the
// same fields Commit's flush phase iterates under tx.OpMu.Lock and that
// other tx-path ops mutate under tx.OpMu.RLock. Savepoint must therefore
// hold tx.OpMu.RLock across those reads. Lock interleaving with m.mu
// follows Commit's pattern: drop m.mu before taking tx.OpMu, re-take m.mu
// briefly for the m.savepoints update.
//
// Tenant isolation: rejects callers whose UserContext tenant does not
// match the transaction's tenant.
//
// NOTE: txUniqueKeys is intentionally not snapshotted here. Unique-key
// DEFINITIONS are static per model (set once at model-lock time, never
// mutated within a transaction), so a RollbackToSavepoint that reverts
// the buffer cannot produce a situation where the keys for a re-saved
// entity differ from the keys captured at the earlier Save call. The only
// scenario that would require snapshotting — the same entity re-saved with
// a different Fields set across a savepoint boundary — is not a supported
// pattern. RollbackToSavepoint therefore also leaves txUniqueKeys untouched.
func (m *transactionManager) Savepoint(ctx context.Context, txID string) (string, error) {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()

	if tx.RolledBack {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	spID := uuid.UUID(m.uuids.NewTimeUUID()).String()

	// Deep-copy the buffer maps under tx.OpMu.RLock so we are serialised
	// against Commit/Rollback (Lock).
	bufCopy := make(map[string]*spi.Entity, len(tx.Buffer))
	for k, v := range tx.Buffer {
		bufCopy[k] = copyEntity(v)
	}
	readCopy := make(map[string]bool, len(tx.ReadSet))
	for k, v := range tx.ReadSet {
		readCopy[k] = v
	}
	writeCopy := make(map[string]bool, len(tx.WriteSet))
	for k, v := range tx.WriteSet {
		writeCopy[k] = v
	}
	delCopy := make(map[string]bool, len(tx.Deletes))
	for k, v := range tx.Deletes {
		delCopy[k] = v
	}
	delAttrCopy := make(map[string]spi.WriteAttribution, len(tx.DeleteAttribution))
	for k, v := range tx.DeleteAttribution {
		delAttrCopy[k] = v
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.savepoints[txID] == nil {
		m.savepoints[txID] = make(map[string]savepointSnapshot)
	}
	// supersededLens records each entityID's current supersededSaves[txID]
	// length so RollbackToSavepoint can truncate back to it — mirrors
	// scheduledTaskOpsLen's approach (append-only, so length is enough).
	supersededLens := make(map[string]int, len(m.supersededSaves[txID]))
	for eid, s := range m.supersededSaves[txID] {
		supersededLens[eid] = len(s)
	}
	m.savepoints[txID][spID] = savepointSnapshot{
		buffer:              bufCopy,
		readSet:             readCopy,
		writeSet:            writeCopy,
		deletes:             delCopy,
		deleteAttribution:   delAttrCopy,
		scheduledTaskOpsLen: len(m.scheduledTaskOps[txID]),
		auditOpsLen:         len(m.auditOps[txID]),
		supersededLens:      supersededLens,
	}
	return spID, nil
}

// RollbackToSavepoint restores the transaction's buffer maps from the snapshot
// captured when the savepoint was created, then removes the snapshot.
//
// Locking discipline: replaces tx.Buffer / tx.ReadSet /
// tx.WriteSet / tx.Deletes — exclusive against every other tx-path op.
// Holds tx.OpMu.Lock (write) for the duration of the field replacement.
//
// Tenant isolation: rejects mismatched-tenant callers — RollbackToSavepoint
// is destructive on tx-state.
func (m *transactionManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	tx, ok := m.active[txID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	tx.OpMu.Lock()
	defer tx.OpMu.Unlock()

	if tx.RolledBack {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	txSavepoints, ok := m.savepoints[txID]
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}
	snap, ok := txSavepoints[savepointID]
	if !ok {
		return fmt.Errorf("RollbackToSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}

	tx.Buffer = snap.buffer
	tx.ReadSet = snap.readSet
	tx.WriteSet = snap.writeSet
	tx.Deletes = snap.deletes
	tx.DeleteAttribution = snap.deleteAttribution

	// Truncate staged scheduled-task ops back to the length recorded at the
	// savepoint — append-only, so truncation (not replacement) is how it is
	// "restored". Clamp to the current length defensively: rolling back to a
	// savepoint ID whose recorded length exceeds what's currently staged
	// cannot happen via the normal linear-nesting flow, but truncating past
	// slice bounds would panic.
	if opsLen := snap.scheduledTaskOpsLen; opsLen < len(m.scheduledTaskOps[txID]) {
		m.scheduledTaskOps[txID] = m.scheduledTaskOps[txID][:opsLen]
	}

	// Truncate staged audit events the same way — see auditOpsLen's godoc.
	if n := snap.auditOpsLen; n < len(m.auditOps[txID]) {
		m.auditOps[txID] = m.auditOps[txID][:n]
	}

	// Truncate supersededSaves per entityID back to its recorded length —
	// same append-only truncate-back-to-length approach as
	// scheduledTaskOps above (see savepointSnapshot.supersededLens godoc).
	// An entityID with no recorded length had no superseded entries yet at
	// savepoint time, so any it accumulated since must be discarded
	// entirely, not merely truncated to zero.
	if cur, ok := m.supersededSaves[txID]; ok {
		for eid, entries := range cur {
			if l, existed := snap.supersededLens[eid]; existed {
				cur[eid] = entries[:l]
			} else {
				delete(cur, eid)
			}
		}
	}

	delete(txSavepoints, savepointID)
	return nil
}

// ReleaseSavepoint releases a savepoint. The work done since the savepoint is
// already in the parent transaction's buffer, so this just removes the snapshot.
//
// Locking discipline: does not touch any field of
// TransactionState — only mutates m.savepoints. Holds m.mu only;
// tx.OpMu is not required.
//
// Tenant isolation: rejects mismatched-tenant callers — m.savepoints is
// tenant-scoped state.
func (m *transactionManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	uc := spi.GetUserContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()

	tx, ok := m.active[txID]
	if !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	txSavepoints, ok := m.savepoints[txID]
	if !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}
	if _, ok := txSavepoints[savepointID]; !ok {
		return fmt.Errorf("ReleaseSavepoint: %w (txID=%s, savepointID=%s)", spi.ErrSavepointNotFound, txID, savepointID)
	}

	delete(txSavepoints, savepointID)
	return nil
}
