package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const submitTimeTTL = 1 * time.Hour

// committedTx records a committed transaction for SI+FCW conflict detection.
//
// seq orders the log: the conflict check compares it with the sequence
// number a transaction saw at Begin (txSnapshotSeq), and pruning drops the
// entries at or below the oldest open one. Never a clock reading: a clock
// with coarse resolution, or a frozen one (as conformance tests use), can
// give two causally-ordered commits the same instant. seq is incremented
// exactly once per logged write under mu, so it is a genuine total order.
type committedTx struct {
	seq      int64
	writeSet map[string]bool
	// taskWrites holds the task rows the write changed, each with the row as
	// it was just before the write. It is apart from writeSet, whose keys
	// are entity ids, so the two checks never mix. A write that committed on
	// its own (commitTaskWrites) has only taskWrites.
	taskWrites map[taskKey]priorRow
}

// stagedAuditEvent is one audit event recorded inside an open transaction,
// with its id already assigned. Commit appends it to the trail; every abort
// path drops it.
type stagedAuditEvent struct {
	entityID string
	event    spi.StateMachineEvent
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
type submitTimeEntry struct {
	submitTime time.Time
	tenantID   spi.TenantID
}

// savepointSnapshot captures the state of a transaction's buffer maps at the
// time a savepoint is created. Used by RollbackToSavepoint to restore state.
type savepointSnapshot struct {
	buffer            map[string]*spi.Entity
	readSet           map[string]bool
	writeSet          map[string]bool
	deletes           map[string]bool
	deleteAttribution map[string]spi.WriteAttribution // paired 1:1 with deletes — see TransactionState godoc

	// scheduledTaskOpsLen is len(TransactionManager.scheduledTaskOps[txID])
	// at the moment this savepoint was taken. scheduledTaskOps is append-only
	// (see stageTaskWrite), so — unlike the maps above, which are
	// deep-copied and restored wholesale — RollbackToSavepoint restores it by
	// truncating back to this recorded length instead of snapshotting it.
	scheduledTaskOpsLen int

	// auditOpsLen is len(TransactionManager.auditOps[txID]) at the moment
	// this savepoint was taken, mirroring scheduledTaskOpsLen's
	// truncate-back-to-length approach (auditOps is append-only too — see
	// stageAuditEvent).
	auditOpsLen int

	// supersededLens is the per-entityID length of supersededSaves[txID]
	// at the moment this savepoint was taken, mirroring
	// scheduledTaskOpsLen's truncate-back-to-length approach (append-only,
	// so length is enough to restore). An entityID absent here had no
	// superseded entries yet at savepoint time; RollbackToSavepoint clears
	// it entirely rather than truncating to zero explicitly recorded.
	supersededLens map[string]int
}

// TransactionManager implements spi.TransactionManager using Snapshot Isolation
// with First-Committer-Wins (SI+FCW) — see docs/CONSISTENCY.md for the contract.
// It lives in the memory package because it needs direct access to StoreFactory's
// entityData map and mu lock for the atomic commit flush.
type TransactionManager struct {
	factory      *StoreFactory
	uuids        spi.UUIDGenerator
	mu           sync.Mutex // protects active, committedLog, committing, submitTimes, savepoints, txUniqueKeys
	active       map[string]*spi.TransactionState
	committedLog []committedTx
	committing   map[string]bool                         // tracks txIDs currently being committed
	submitTimes  map[string]submitTimeEntry              // txID -> submit time + owning tenant, survives log pruning. Evicted after submitTimeTTL.
	savepoints   map[string]map[string]savepointSnapshot // txID -> spID -> snapshot

	// txUniqueKeys holds per-entity unique keys captured at Save (buffer) time.
	// Captured when an entity is buffered so that Commit can enforce the correct
	// keys per entity even in a mixed-model batch where each Save may carry a
	// different key set in its context. Protected by mu. Cleaned up after commit
	// or rollback (no leak).
	txUniqueKeys map[string]map[string][]spi.UniqueKey // txID → entityID → keys

	// commitSeq is a monotonic counter, incremented exactly once per
	// successful Commit under mu, used as the ordering key of the FCW
	// conflict check and of log pruning instead of wall-clock submitTime
	// (see committedTx.seq's doc comment for why: submitTime can tie under
	// a coarse or frozen clock even for genuinely causally-ordered
	// commits). txSnapshotSeq records each active transaction's commitSeq
	// value AT BEGIN — "this many commits already happened before my
	// snapshot" — so Commit's FCW check becomes committed.seq >
	// txSnapshotSeq[txID]: a committedTx whose seq was assigned strictly
	// after my Begin is a real conflict candidate, with no clock-resolution
	// gap. Both fields protected by mu.
	//
	// Begin MUST read commitSeq (into txSnapshotSeq) in the SAME mu
	// critical section where it captures SnapshotTime — see Begin's
	// in-line comment for the missed-conflict window that opens up if
	// they are captured separately (or either one outside mu).
	//
	// A write that commits on its own also takes a sequence number (see
	// commitTaskWrites).
	commitSeq     int64
	txSnapshotSeq map[string]int64 // txID → commitSeq at Begin time; cleaned up after commit or rollback (no leak)

	// lastSubmitTime is the monotonic floor every stamped submit time sits
	// at or above — see nextSubmitTime. Read and written under mu only.
	lastSubmitTime time.Time

	// supersededSaves records, per (txID, entityID), each buffered
	// *spi.Entity value overwritten by a later same-entity Save/
	// CompareAndSave within the same open transaction, oldest first.
	// tx.Buffer (a shared spi.TransactionState field) only ever holds the
	// FINAL value per entity — read-your-own-writes only needs the latest —
	// so without this side channel a same-tx double-save would flush as a
	// single commit row and GetVersionByTransaction's earliest-wins
	// contract (see its SPI doc comment: "a transaction that saved the
	// same entity more than once before committing... the earliest is
	// returned") could never be satisfied for the intermediate value a
	// later Save in the same tx overwrote. Commit flushes each entityID's
	// superseded values (in order) followed by the final tx.Buffer value
	// as consecutive entityVersion rows sharing the transaction's txID.
	//
	// Behavior change this introduces: an entity Saved N times inside one
	// transaction now consumes N version numbers (one entityVersion row
	// per Save call) instead of 1 (one row for the final state only).
	// This is a backend-divergence FIX, not a new side effect: postgres
	// already behaves this way — it writes a row per Save call with no
	// buffering, since each Save is an immediate DML statement inside the
	// SQL transaction — so memory's prior single-row-per-commit collapse
	// was the odd one out. Per the project's "a backend differing on the
	// same contract is a defect" policy, aligning memory with postgres
	// here is correct, not merely a means to satisfy the new conformance
	// test.
	//
	// Protected by mu. Savepoint-scoped like tx.Buffer (length recorded at
	// Savepoint, truncated at RollbackToSavepoint — see
	// savepointSnapshot.supersededLens). Cleaned up after commit or
	// rollback (no leak).
	supersededSaves map[string]map[string][]*spi.Entity // txID -> entityID -> superseded snapshots, oldest first

	// deletedBufferModels records, per (txID, entityID), the model
	// reference of a buffered create/update Delete evicted from tx.Buffer
	// within the same open transaction. Delete removes id from tx.Buffer
	// unconditionally (Buffer and Deletes are kept mutually exclusive), so
	// when that id was never committed before this transaction, nothing
	// else records what model it was buffered under — the tombstone
	// Commit's delete flush appends is otherwise the ONLY committed
	// version for that id, and it needs the model to make the entity's
	// model-immutability check (modelRefOfLocked) answerable for a later
	// Save/CompareAndSave against this id. Not consulted (and harmless if
	// stale) when a later same-tx Save re-buffers id before commit — that
	// value is unstaged from tx.Deletes and flows through the normal
	// buffer-flush path instead. Protected by mu. Cleaned up after commit
	// or rollback (no leak); not savepoint-scoped like supersededSaves,
	// because a RollbackToSavepoint that restores id to tx.Buffer also
	// removes it from tx.Deletes, so a stale entry here is simply never
	// read again.
	deletedBufferModels map[string]map[string]spi.ModelRef // txID -> entityID -> evicted model

	// scheduledTaskOps holds the task-row ops staged while the transaction
	// is open, as post-images (see scheduledTaskOp). Applied by Commit
	// inside its entityMu section; discarded on Rollback and on every abort
	// path; truncated by RollbackToSavepoint. Protected by mu.
	scheduledTaskOps map[string][]scheduledTaskOp // txID → staged ops

	// auditOps holds the audit events recorded while the transaction is open,
	// in order. Commit appends them inside its entityMu section, before the
	// commit-instant stamp; Rollback and every abort path drop them;
	// RollbackToSavepoint truncates them. Protected by mu. PostgreSQL gets the
	// same behaviour from recording on the transaction's connection.
	auditOps map[string][]stagedAuditEvent // txID → staged events

	// lostDiscardedWrite marks a transaction that a RollbackToSavepoint made
	// discard a write which had already lost first-committer-wins: another
	// transaction committed that entity or task row after this one's
	// snapshot and before the rollback. The rollback restores the write set
	// and cuts the staged task-row ops back to the savepoint, so Commit's
	// log check can no longer see the write; the mark makes Commit refuse
	// the transaction with spi.ErrConflict, as PostgreSQL refuses one whose
	// write failed with 40001 inside a rolled-back savepoint. It survives
	// every later RollbackToSavepoint. Protected by mu.
	lostDiscardedWrite map[string]bool
}

// Verify interface compliance at compile time.
var _ spi.TransactionManager = (*TransactionManager)(nil)

// NewTransactionManager creates and registers a TransactionManager on the
// StoreFactory, carrying over the submit-time floor of whatever it replaces
// (see seedLastSubmitTime).
func (f *StoreFactory) NewTransactionManager(uuids spi.UUIDGenerator) *TransactionManager {
	floor := f.seedLastSubmitTime()
	f.uuids = uuids
	tm := &TransactionManager{
		factory:             f,
		uuids:               uuids,
		active:              make(map[string]*spi.TransactionState),
		committedLog:        nil,
		committing:          make(map[string]bool),
		submitTimes:         make(map[string]submitTimeEntry),
		savepoints:          make(map[string]map[string]savepointSnapshot),
		txUniqueKeys:        make(map[string]map[string][]spi.UniqueKey),
		txSnapshotSeq:       make(map[string]int64),
		supersededSaves:     make(map[string]map[string][]*spi.Entity),
		deletedBufferModels: make(map[string]map[string]spi.ModelRef),
		scheduledTaskOps:    make(map[string][]scheduledTaskOp),
		auditOps:            make(map[string][]stagedAuditEvent),
		lostDiscardedWrite:  make(map[string]bool),
		lastSubmitTime:      floor,
	}
	f.txManager = tm
	return tm
}

// seedLastSubmitTime returns the submit-time floor a manager being installed
// on this factory must start from: the outgoing manager's floor when there is
// one, otherwise the latest submit time already stamped on the factory's
// rows. Starting from zero would put the new manager's first snapshot below
// stamps already committed — every stamp is max(now, floor+1µs) and so can
// stand ahead of the clock — and those rows would be invisible to the first
// transaction it begins. The sqlite plugin seeds the same value from
// MAX(submit_time) on open.
//
// The outgoing manager's floor is authoritative on its own: it is at or above
// every stamp it issued.
func (f *StoreFactory) seedLastSubmitTime() time.Time {
	if prev := f.txManager; prev != nil {
		prev.mu.Lock()
		defer prev.mu.Unlock()
		return prev.lastSubmitTime
	}
	f.entityMu.RLock()
	defer f.entityMu.RUnlock()
	var latest time.Time
	for _, entities := range f.entityData {
		for _, versions := range entities {
			for _, v := range versions {
				if v.submitTime.After(latest) {
					latest = v.submitTime
				}
			}
		}
	}
	return latest
}

// recordUniqueKeys stores the unique keys for entityID under txID so that
// Commit can look them up per entity during the flush. Last-write-wins,
// matching the semantics of tx.Buffer. Protected by mu.
func (m *TransactionManager) recordUniqueKeys(txID, entityID string, keys []spi.UniqueKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.txUniqueKeys[txID] == nil {
		m.txUniqueKeys[txID] = make(map[string][]spi.UniqueKey)
	}
	m.txUniqueKeys[txID][entityID] = keys
}

// stageSuperseded appends prior — the tx.Buffer value a Save/CompareAndSave
// call is about to overwrite — to txID's superseded list for entityID, in
// overwrite order. No-op when prior is nil (the entity's first Save in this
// transaction: nothing superseded yet). See the supersededSaves field
// godoc for why this exists. Protected by mu.
func (m *TransactionManager) stageSuperseded(txID, entityID string, prior *spi.Entity) {
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

// stageDeletedBufferModel records the model reference of a buffered
// create/update that Delete is about to evict from tx.Buffer — see
// deletedBufferModels's field doc for why this is needed.
func (m *TransactionManager) stageDeletedBufferModel(txID, entityID string, ref spi.ModelRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deletedBufferModels[txID] == nil {
		m.deletedBufferModels[txID] = make(map[string]spi.ModelRef)
	}
	m.deletedBufferModels[txID][entityID] = ref
}

// stageAuditEvent appends ev to txID's staged audit events. Protected by mu.
func (m *TransactionManager) stageAuditEvent(txID string, ev stagedAuditEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditOps[txID] = append(m.auditOps[txID], ev)
}

// stagedAuditEvents returns a copy of txID's staged audit events. Protected by mu.
func (m *TransactionManager) stagedAuditEvents(txID string) []stagedAuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]stagedAuditEvent(nil), m.auditOps[txID]...)
}

// openAuditLabelsLocked reports, for each distinct non-empty TransactionID
// label among staged, whether that label currently names an active
// transaction — see appendStagedAuditEvents for why this decides whether
// the label is safe to index. The committing transaction's own id always
// answers true here: Commit calls this before its step 6 removes txID from
// m.active. Caller holds mu.
func (m *TransactionManager) openAuditLabelsLocked(staged []stagedAuditEvent) map[string]bool {
	if len(staged) == 0 {
		return nil
	}
	open := make(map[string]bool, len(staged))
	for _, st := range staged {
		label := st.event.TransactionID
		if label == "" {
			continue
		}
		if _, seen := open[label]; seen {
			continue
		}
		_, open[label] = m.active[label]
	}
	return open
}

// appendStagedAuditEvents appends txID's staged audit events to the trail,
// deciding which are safe to index under the SAME mu hold that then
// performs the append — see StoreFactory.appendStagedAuditEvents for what
// "safe to index" means and why an unsafe one is still appended, just not
// indexed.
//
// Holding mu across both steps is the fix for a time-of-check race a
// two-step "compute open, then append" design has: Rollback deletes its
// transaction from m.active AND calls discardAuditTxIndex — the index
// entry's one and only pruning trigger — under mu, but not atomically with
// any other transaction's commit. A label read as open, with mu released
// before the append/index runs, can have its transaction roll back — and
// prune its (at that point still-empty) index entry — in the gap; the
// append then indexes on the stale answer, recreating an entry nothing will
// ever prune again. Natural goroutine scheduling does not reliably hit this
// window (empirically: 2000 iterations of a racing Commit/Rollback pair
// produced zero leaks — the window is a few instructions wide), and it is
// not a Go data race -race can flag (every individual access is correctly
// mutex-protected; the bug is in the gap between two separately-locked
// critical sections, not in either one). Holding one lock across both closes
// the gap outright: whichever of a concurrent Commit or Rollback reaches mu
// first now fully determines the other's view. The pre-fix, two-step shape
// (compute open via a self-locking openAuditLabels, release mu, then append
// via StoreFactory.appendStagedAuditEvents on the stale answer) was proven
// vulnerable by driving those two steps apart by hand with a Rollback of
// the labelled transaction placed in between — reproducible only that way:
// 2000 iterations of a real racing Commit/Rollback pair (above) never hit
// the window, and it leaves no trace -race can see either.
//
// mu → smAuditMu (StoreFactory.appendStagedAuditEvents takes smAuditMu) is
// already this package's lock order: the conflict-abort branch and Commit's
// step-6 cleanup both call discardAuditTxIndex — which takes smAuditMu —
// from inside an m.mu section. This keeps that order; it does not reverse
// it.
//
// Called by Commit inside its entityMu section; entityMu → mu → smAuditMu.
func (m *TransactionManager) appendStagedAuditEvents(tenant spi.TenantID, txID string, staged []stagedAuditEvent, submitTime time.Time) {
	if len(staged) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	open := m.openAuditLabelsLocked(staged)
	m.factory.appendStagedAuditEvents(tenant, txID, staged, submitTime, open)
}

// recordUntransactedAuditEvent mints event's id and appends it to the trail
// directly — not staged, ctx carried no transaction — indexing it under its
// TransactionID label only when that label currently names an active
// transaction.
//
// smAuditTxIndex exists solely to make the commit-phase stamp
// (stampAuditEventsForTx) a lookup; nothing else reads it —
// GetEvents/GetEventsByTransaction always do a full scan of the trail
// itself, so leaving an event out of the index never changes a query
// result, only whether the commit-phase sweep can find it in O(1). A label
// does not always name a transaction that will ever call Commit: a caller
// can set TransactionID purely for correlation without ctx ever carrying
// that transaction (resolveAuditTxID, internal/domain/workflow/engine.go,
// mints a fresh one when the entity it is auditing has none of its own) —
// such a label will never back a real Begin, so nothing will ever call
// stampAuditEventsForTx or discardAuditTxIndex for it, and indexing it
// anyway leaks one entry per call for the life of the process, the same
// shape as a stale label on a staged event (see
// StoreFactory.appendStagedAuditEvents) — just reached through Record's
// untransacted branch instead of through a commit.
//
// The open-label check and the indexed append are one mu-then-smAuditMu
// critical section for the same reason appendStagedAuditEvents holds mu
// across both of its steps: released and re-acquired separately, a
// concurrent Commit/Rollback of the labelled transaction could land in the
// gap and leak an index entry nothing will ever prune.
//
// Also mints event.TimeUUID under smAuditMu, preserving the existing
// atomic mint-then-append contract non-transactional Record has always had
// (see TestSMAudit_ConcurrentRecord_IDOrderMatchesAppendOrder): the label
// check adds an outer mu hold, it does not split smAuditMu's own span.
//
// mu → smAuditMu is this package's established lock order (see
// appendStagedAuditEvents).
func (m *TransactionManager) recordUntransactedAuditEvent(tenant spi.TenantID, entityID string, event spi.StateMachineEvent) {
	label := event.TransactionID
	if label == "" {
		m.factory.smAuditMu.Lock()
		defer m.factory.smAuditMu.Unlock()
		event.TimeUUID = uuid.UUID(m.uuids.NewTimeUUID()).String()
		m.factory.appendEventLocked(tenant, entityID, event, false)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	_, open := m.active[label]

	m.factory.smAuditMu.Lock()
	defer m.factory.smAuditMu.Unlock()
	event.TimeUUID = uuid.UUID(m.uuids.NewTimeUUID()).String()
	m.factory.appendEventLocked(tenant, entityID, event, open)
}

// busyTaskKeys returns the task rows an open transaction has staged a change
// to. Such a row is not claimable, and MarkUnsafe and RecordAttempt answer
// spi.ErrTaskBusy for it, until the transaction ends. Caller holds
// factory.entityMu, so no Commit is between reading its ops and applying
// them; lock order entityMu → mu.
func (m *TransactionManager) busyTaskKeys() map[taskKey]bool {
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
// allocates nothing. Caller holds factory.entityMu; lock order entityMu → mu.
func (m *TransactionManager) taskBusy(k taskKey) bool {
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

// stageTaskWrite stages one joining write on txID. It passes plan the ops
// staged so far and the prior rows of txID's snapshot (see
// taskSnapshotLocked), and appends the ops plan returns, in one mu section:
// two joining writes on the same transaction therefore never plan from the
// same view, and neither post-image overwrites the other. plan must not keep
// or change what it is given, and must not take mu. Commit applies the
// staged ops in its entityMu section, atomically with the entity flush;
// every abort path discards them.
//
// Caller holds tx.OpMu (read), has checked that the transaction is open, and
// holds factory.entityMu (read); lock order tx.OpMu → entityMu → mu.
func (m *TransactionManager) stageTaskWrite(txID string, plan func(staged []scheduledTaskOp, prior map[taskKey]priorRow) ([]scheduledTaskOp, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ops, err := plan(m.scheduledTaskOps[txID], m.taskSnapshotLocked(txID))
	if err != nil {
		return err
	}
	if len(ops) > 0 {
		m.scheduledTaskOps[txID] = append(m.scheduledTaskOps[txID], ops...)
	}
	return nil
}

// commitTaskWrites applies task-row writes that commit on their own — a
// never-joining method, or a joining one called without a transaction — and
// records them in the committed log, so that a transaction that began before
// them and writes one of the same rows fails at commit.
//
// The apply and the log entry share one mu section. Begin reads commitSeq
// under mu, so every Begin is ordered wholly before or wholly after the
// write: a transaction that began after it can see it and never conflicts
// with it. The entry's sequence number is above every open snapshot's, so
// pruning keeps it while one of them is open.
//
// Caller holds factory.entityMu for writing; lock order entityMu → mu.
func (m *TransactionManager) commitTaskWrites(ops []scheduledTaskOp) {
	if len(ops) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	priors := priorRows(m.factory.scheduledTasks, ops)
	applyTaskOps(m.factory.scheduledTasks, ops)
	m.commitSeq++
	m.committedLog = append(m.committedLog, committedTx{seq: m.commitSeq, taskWrites: priors})
	m.pruneCommittedLogLocked()
}

// taskSnapshot returns txID's staged ops and the prior rows of its snapshot
// for tenant (see taskSnapshotLocked). Caller holds tx.OpMu (read), has
// checked that the transaction is open and of tenant, and holds
// factory.entityMu (read), so the committed rows and the log agree; lock
// order tx.OpMu → entityMu → mu.
func (m *TransactionManager) taskSnapshot(txID string, tenant spi.TenantID) ([]scheduledTaskOp, map[taskKey]priorRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	staged := append([]scheduledTaskOp(nil), m.scheduledTaskOps[txID]...)
	prior := m.taskSnapshotLocked(txID)
	for k := range prior {
		if k.tenant != tenant {
			delete(prior, k)
		}
	}
	return staged, prior
}

// taskSnapshotLocked returns, for each task row a write logged after txID's
// Begin changed, the row as txID's snapshot shows it: the prior row of the
// earliest such write. A row with no such write shows its committed state.
// Pruning keeps every entry above an open transaction's snapshot, so none of
// these is lost while txID is open. An ended transaction has no snapshot
// sequence number left, and with it no bound on which entries apply, so the
// caller must have checked that txID is open. Caller holds mu.
func (m *TransactionManager) taskSnapshotLocked(txID string) map[taskKey]priorRow {
	snapshotSeq := m.txSnapshotSeq[txID]
	var prior map[taskKey]priorRow
	for _, committed := range m.committedLog { // in sequence order
		if committed.seq <= snapshotSeq {
			continue
		}
		for k, p := range committed.taskWrites {
			if _, seen := prior[k]; seen {
				continue
			}
			if prior == nil {
				prior = make(map[taskKey]priorRow)
			}
			prior[k] = p
		}
	}
	return prior
}

// forgetLocked drops every piece of per-transaction state the manager holds
// for txID. Caller holds mu.
func (m *TransactionManager) forgetLocked(txID string) {
	delete(m.active, txID)
	delete(m.committing, txID)
	delete(m.savepoints, txID)
	delete(m.txUniqueKeys, txID)
	delete(m.txSnapshotSeq, txID)
	delete(m.supersededSaves, txID)
	delete(m.deletedBufferModels, txID)
	delete(m.scheduledTaskOps, txID)
	delete(m.auditOps, txID)
	delete(m.lostDiscardedWrite, txID)
}

// committedSinceSnapshotLocked reports whether a transaction that committed
// after txID's snapshot wrote one of the entity ids or one of the task rows.
// The task rows are compared by taskKey, tenant included, as Commit compares
// them. Caller holds mu.
func (m *TransactionManager) committedSinceSnapshotLocked(txID string, ids []string, tasks map[taskKey]bool) bool {
	if len(ids) == 0 && len(tasks) == 0 {
		return false
	}
	snapshotSeq := m.txSnapshotSeq[txID]
	for _, committed := range m.committedLog {
		if committed.seq <= snapshotSeq {
			continue
		}
		for _, id := range ids {
			if committed.writeSet[id] {
				return true
			}
		}
		for k := range committed.taskWrites {
			if tasks[k] {
				return true
			}
		}
	}
	return false
}

// pruneCommittedLogLocked drops the log entries no open transaction can
// conflict with: those at or below the oldest open snapshot's sequence
// number, or all of them when no transaction is open. Caller holds mu.
func (m *TransactionManager) pruneCommittedLogLocked() {
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

// nextSubmitTime returns the submit time to stamp on a write and records it
// as the new floor. max(now, lastSubmitTime+1µs) guarantees forward progress
// even under NTP steps, VM pause/migrate, leap-second smearing, or a frozen
// test clock, and — because Begin floors a new transaction's SnapshotTime to
// lastSubmitTime — guarantees a write never stamps at or below a snapshot
// already open. Every path that stamps a submit time uses it: Commit's flush
// and the direct writes (saveUnlocked, the non-tx Delete and DeleteAll). The
// one-microsecond step matches the sqlite plugin, so under a frozen clock
// both backends produce the same sequence of stamps.
//
// mu is a leaf lock; callers already holding factory.entityMu take it in the
// order entityMu → mu, the order Commit uses.
func (m *TransactionManager) nextSubmitTime() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.factory.clock.Now()
	if !now.After(m.lastSubmitTime) {
		now = m.lastSubmitTime.Add(time.Microsecond)
	}
	m.lastSubmitTime = now
	return now
}

// GetTransactionManager returns the registered TransactionManager, or nil.
func (f *StoreFactory) GetTransactionManager() spi.TransactionManager {
	if f.txManager == nil {
		return nil
	}
	return f.txManager
}

// Begin starts a new transaction. It resolves the tenant from the context,
// generates a unique transaction ID, captures a snapshot time, and returns
// a new context carrying the TransactionState.
func (m *TransactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", ctx, fmt.Errorf("no user context — cannot begin transaction")
	}
	if uc.Tenant.ID == "" {
		return "", ctx, fmt.Errorf("user context has no tenant — cannot begin transaction")
	}

	txID := uuid.UUID(m.uuids.NewTimeUUID()).String()

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
	// tx has not been published anywhere yet (not in m.active, not
	// returned), so mutating its SnapshotTime field below — before the
	// first read of it by any other goroutine — is safe.

	// SnapshotTime (the read-visibility boundary) and txSnapshotSeq (the
	// FCW conflict-detection baseline) MUST be captured atomically, in the
	// SAME mu critical section — not as two reads from separate sections,
	// and not with the clock read taken outside mu. If a concurrent Commit
	// X could interleave between the two captures, X could be excluded
	// from this tx's later FCW check (seq_X <= txSnapshotSeq, because X's
	// mu-protected seq assignment ran before this tx read txSnapshotSeq)
	// while X's submitTime — captured earlier, before X's own mu section —
	// is chronologically AFTER this tx's SnapshotTime — captured even
	// later still, outside any lock. That would make X's write invisible
	// to this tx's reads (SnapshotTime-gated) yet silently un-checked by
	// FCW: a missed conflict, i.e. failing open. Capturing both under one
	// Lock closes the window: mu totally orders every Begin/Commit
	// critical section, so "X's seq excluded from this tx's baseline"
	// (X's section ran first) provably implies "X's submitTime precedes
	// this tx's SnapshotTime" (submitTime was read even before X's own,
	// earlier-ordered section, and the clock is monotonic non-decreasing —
	// see clock.go: wallClock uses Go's monotonic time.Now(), TestClock's
	// virtual time only ever advances forward).
	//
	// SnapshotTime is additionally floored to lastSubmitTime, the monotonic
	// floor every stamped submit time sits at or above (see nextSubmitTime).
	// Without the floor a stamped time could stand ahead of the raw clock —
	// several writes inside one clock tick each bump it by a microsecond, and
	// a test clock can be frozen outright — and the snapshot would then sit
	// at or above a write it must not see.
	//
	// The snapshot is then RESERVED as the new floor. Reading the floor is
	// not enough: with the floor below the clock (a quiet factory leaves it
	// at zero) the snapshot is the raw clock value, and the next write stamps
	// max(now, floor+1µs) — the same instant — which the visibility rule
	// (submitTime <= SnapshotTime) counts as visible to a transaction that
	// began before it. Reserving makes the next stamp strictly later.
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		now := m.factory.clock.Now()
		if now.Before(m.lastSubmitTime) {
			now = m.lastSubmitTime
		}
		tx.SnapshotTime = now
		m.lastSubmitTime = now
		m.active[txID] = tx
		m.txSnapshotSeq[txID] = m.commitSeq
	}()

	txCtx := spi.WithTransaction(ctx, tx)
	return txID, txCtx, nil
}

// Join returns a context carrying the TransactionState for an existing active
// transaction. This allows multiple goroutines to participate in the same
// transaction. Callers must coordinate access to the transaction's Buffer,
// ReadSet, WriteSet, and Deletes maps.
//
// Locking discipline: Rollback writes tx.RolledBack
// inside m.mu only; Commit and Rollback both write tx.Closed in their
// defer under tx.OpMu.Lock only. Reading those fields requires
// tx.OpMu.RLock to be synchronised against the Closed-write — m.mu alone
// is not sufficient because Commit's defer runs outside the m.mu region.
func (m *TransactionManager) Join(ctx context.Context, txID string) (context.Context, error) {
	var tx *spi.TransactionState
	var ok bool
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx, ok = m.active[txID]
	}()

	if !ok {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}

	rolledBack, closed := func() (bool, bool) {
		tx.OpMu.RLock()
		defer tx.OpMu.RUnlock()
		return tx.RolledBack, tx.Closed
	}()
	if rolledBack {
		return nil, fmt.Errorf("Join: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if closed {
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
// flushes the write buffer and deletes to the entity store, and records the
// commit in the log.
//
// A commit that aborts — on a conflict or a unique-key violation — applies
// nothing and leaves the transaction rolled back as well as closed, so a
// later call with its ctx answers spi.ErrTxRolledBack, never
// spi.ErrTxAlreadyCommitted. Its state is discarded as Rollback discards it,
// so a Rollback after the abort answers spi.ErrTxNotFound.
func (m *TransactionManager) Commit(ctx context.Context, txID string) error {
	// 1. Look up the active transaction and mark as committing (TOCTOU guard).
	uc := spi.GetUserContext(ctx)
	var tx *spi.TransactionState
	if err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		var ok bool
		tx, ok = m.active[txID]
		if !ok {
			return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxNotFound, txID)
		}
		if uc == nil || uc.Tenant.ID != tx.TenantID {
			return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
		}
		if m.committing[txID] {
			return fmt.Errorf("Commit: %w (txID=%s)", spi.ErrTxCommitInProgress, txID)
		}
		m.committing[txID] = true
		return nil
	}(); err != nil {
		return err
	}

	// 1b. Acquire transaction operation write lock — waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	// 2–6. Acquire the factory write lock for atomic flush.
	// All abort paths (FCW conflict, claim violation) and the success path are
	// enclosed in a result-returning IIFE so that entityMu is always released
	// via defer — no bare Unlock() calls (go-mutex-discipline.md).
	tid := tx.TenantID
	if err := func() error {
		m.factory.entityMu.Lock()
		defer m.factory.entityMu.Unlock()

		// 3. Conflict detection: check committed log for overlapping write sets.
		// Also snapshot per-entity unique keys and staged scheduled-task ops
		// captured at Save/Upsert/Delete time so that step 3.5 and step 4.5
		// can read them without re-acquiring m.mu.
		var capturedKeys map[string][]spi.UniqueKey
		var capturedScheduledTaskOps []scheduledTaskOp
		var capturedSuperseded map[string][]*spi.Entity
		var capturedDeletedBufferModels map[string]spi.ModelRef
		var capturedAudit []stagedAuditEvent
		if err := func() error {
			m.mu.Lock()
			defer m.mu.Unlock()
			// FCW ordering uses commitSeq, not submitTime — see commitSeq's
			// doc comment: a wall-clock comparison can tie under a coarse or
			// frozen clock even for genuinely causally-ordered commits.
			// Entity ids and task rows are checked in two separate loops
			// over two separate sets.
			// A write a savepoint rollback discarded after it had lost is a
			// conflict too — see lostDiscardedWrite.
			snapshotSeq := m.txSnapshotSeq[txID]
			taskWrites := taskWriteSet(m.scheduledTaskOps[txID])
			conflict := m.lostDiscardedWrite[txID]
			for _, committed := range m.committedLog {
				if conflict {
					break
				}
				if committed.seq <= snapshotSeq {
					continue
				}
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
			}
			if conflict {
				tx.RolledBack = true // aborted, not committed: see Commit's doc
				m.forgetLocked(txID)
				m.factory.discardAuditTxIndex(tid, txID)
				return spi.ErrConflict
			}
			capturedKeys = m.txUniqueKeys[txID]                       // safe: tx.OpMu.Lock() prevents new recordUniqueKeys
			capturedScheduledTaskOps = m.scheduledTaskOps[txID]       // safe: tx.OpMu.Lock() prevents new stageTaskWrite
			capturedSuperseded = m.supersededSaves[txID]              // safe: tx.OpMu.Lock() prevents new stageSuperseded
			capturedDeletedBufferModels = m.deletedBufferModels[txID] // safe: tx.OpMu.Lock() prevents new stageDeletedBufferModel
			capturedAudit = m.auditOps[txID]                          // safe: tx.OpMu.Lock() prevents new stageAuditEvent
			return nil
		}(); err != nil {
			return err
		}

		// 3.5. Validate composite unique-key claims inside the entityMu critical section.
		//
		// Deterministic order: sort buffered entity IDs so that any intra-batch
		// collision is detected stably (independent of map iteration order).
		//
		// abortClaim cleans up m.mu-protected state and returns err.
		// entityMu is released by the enclosing IIFE's defer — no bare Unlock.
		abortClaim := func(err error) error {
			func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				tx.RolledBack = true // aborted, not committed: see Commit's doc
				m.forgetLocked(txID)
			}()
			m.factory.discardAuditTxIndex(tid, txID)
			return err
		}

		ids := make([]string, 0, len(tx.Buffer))
		for id := range tx.Buffer {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		// ISSUE-3: build release set from tx.Deletes so that a same-tx
		// delete+reclaim of the same key value is not falsely rejected.
		toRelease := make(map[string]bool, len(tx.Deletes))
		for id := range tx.Deletes {
			toRelease[id] = true
		}

		// ISSUE-4: compute claims once during validation; reuse during apply.
		computedClaims := make(map[string][]spi.UniqueClaim, len(ids))
		pending := make(map[claimKey]string) // claimKey → entityID within this batch
		for _, entityID := range ids {
			entity := tx.Buffer[entityID]
			keys := capturedKeys[entityID] // nil if entity was buffered without unique keys

			claims, err := spi.ComputeClaims(keys, entity.Data)
			if err != nil {
				return abortClaim(err)
			}
			computedClaims[entityID] = claims

			for _, c := range claims {
				k := claimKey{
					tenant:    string(tid),
					model:     entity.Meta.ModelRef.EntityName,
					version:   entity.Meta.ModelRef.ModelVersion,
					keyID:     c.KeyID,
					signature: c.Signature,
				}
				// Intra-batch collision: two buffered entities share a claim.
				if pendingHolder, exists := pending[k]; exists && pendingHolder != entityID {
					return abortClaim(spi.ErrUniqueViolation)
				}
				// Collision with a committed claim held by a different entity that
				// is NOT being released in the same tx (ISSUE-3 same-tx delete+reclaim).
				if holder, exists := m.factory.uniqueClaims[k]; exists && holder != entityID && !toRelease[holder] {
					return abortClaim(spi.ErrUniqueViolation)
				}
				pending[k] = entityID
			}
		}

		// 4. Flush buffer to entity store.
		//
		// Stamped under the monotonic floor (see nextSubmitTime), and still
		// captured HERE — before the mu section at step 6 that assigns this
		// commit's seq — which is what Begin's atomic-capture argument above
		// rests on: a commit whose seq section precedes a Begin has already
		// bumped lastSubmitTime, so that Begin's floored SnapshotTime is at
		// or after this submitTime and the write it excludes from its FCW
		// baseline is one it can see.
		submitTime := m.nextSubmitTime()

		// Audit events recorded inside this transaction join the trail now,
		// after the last abort path; those labelled with this transaction
		// are already stamped with its commit instant, so no reader can
		// observe one of them appended but not yet stamped. Every other
		// already-appended event labelled with this transaction then takes
		// the same instant — see
		// appendStagedAuditEvents (this one, on *TransactionManager) and
		// stampAuditEventsForTx.
		m.appendStagedAuditEvents(tid, txID, capturedAudit, submitTime)
		m.factory.stampAuditEventsForTx(tid, txID, submitTime)

		// Pre-release: free claims for all deleted entities BEFORE inserting any
		// new buffer claims. This ensures a same-tx delete+reclaim of the same
		// key value (ISSUE-3) does not clobber the freshly-inserted buffer claim.
		// Buffer and Deletes are mutually exclusive (Delete removes from Buffer).
		for entityID := range tx.Deletes {
			m.factory.releaseClaims(string(tid), entityID)
		}

		for entityID, entity := range tx.Buffer {
			if m.factory.entityData[tid] == nil {
				m.factory.entityData[tid] = make(map[string][]entityVersion)
			}

			versions := m.factory.entityData[tid][entityID]
			baseVersion := lastVersion(versions)
			hasPrior := len(versions) > 0
			// firstNonTombstone, NOT versions[0]: a create and a delete
			// committed in the same transaction flush a single tombstone
			// row whose entity is nil (see the tombstone append below, and
			// firstNonTombstone's doc comment), so an entity whose history
			// begins with one carries no creation date at versions[0] to
			// bring forward. Reading it there leaves this zero and the
			// stamp below then moves the creation date forward on every
			// later update. entity_store.go's non-transactional path uses
			// firstNonTombstone for exactly this reason and the two paths
			// must agree. Pinned by spitest's
			// Save/UpdateDoesNotRestampCreationDate.
			var creationDate time.Time
			if e, ok := firstNonTombstone(versions); ok {
				creationDate = e.Meta.CreationDate
			}

			// Flush this entity's superseded intra-tx saves (oldest first),
			// then the final tx.Buffer value, as consecutive entityVersion
			// rows sharing txID — see supersededSaves's field godoc: this
			// is what lets GetVersionByTransaction's earliest-wins contract
			// hold for a same-tx double-save on memory's buffer-coalescing
			// transaction model, where tx.Buffer itself only ever holds the
			// final value.
			toFlush := append(append([]*spi.Entity{}, capturedSuperseded[entityID]...), entity)
			var firstVersion int64
			for i, staged := range toFlush {
				nextVersion := baseVersion + 1
				baseVersion = nextVersion

				// DERIVE ChangeType from row-existence, like the non-tx save
				// path (see deriveChangeType) — never trust it verbatim from
				// the staged entity, which may carry a stale value fetched
				// before this transaction began (e.g. a scheduled-transition
				// fire re-saving an already-existing entity read with its
				// original "CREATED" Meta still attached).
				changeType := deriveChangeType(staged.Meta.ChangeType, hasPrior)
				hasPrior = true

				saved := copyEntity(staged)
				saved.Meta.Version = nextVersion
				saved.Meta.LastModifiedDate = submitTime
				// Already stamped at write time (entity_store.go), so this
				// only restates the invariant: a committed version's
				// transaction ID is the transaction that committed it.
				saved.Meta.TransactionID = txID
				saved.Meta.TenantID = tid
				saved.Meta.ChangeType = changeType

				// Preserve CreationDate from existing versions; otherwise
				// this transaction created the entity, and its commit
				// instant IS the creation date.
				//
				// A caller-supplied value is IGNORED rather than taken as a
				// fallback: the date belongs to the store (see
				// spi.EntityMeta.CreationDate), and the engine stamps one
				// from its own clock before Begin, so honouring it dated a
				// created entity at the START of the transaction — processor
				// callouts included — instead of at its commit, which is the
				// defect commit-instant stamping exists to remove. Pinned by
				// spitest's Save/CallerCreationDateIgnored.
				if !creationDate.IsZero() {
					saved.Meta.CreationDate = creationDate
				} else {
					saved.Meta.CreationDate = submitTime
				}

				m.factory.entityData[tid][entityID] = append(m.factory.entityData[tid][entityID], entityVersion{
					entity:         saved,
					version:        nextVersion,
					transactionID:  txID,
					submitTime:     submitTime,
					changeType:     changeType,
					user:           staged.Meta.ChangeUser,
					changeUserKind: staged.Meta.ChangeUserKind,
					executor:       staged.Meta.ChangeExecutor,
				})
				if i == 0 {
					firstVersion = nextVersion
				}
			}
			// Earliest-wins: index the FIRST row this commit wrote for
			// entityID, not the final one — see GetVersionByTransaction's
			// SPI doc comment.
			m.factory.recordTxIndex(tid, entityID, txID, firstVersion)

			// Apply unique-key claims: release any prior claims for this entity
			// (handles the update-moves-key case), then insert the new claim set.
			// ISSUE-4: reuse computedClaims computed in step 3.5 — no recompute.
			// ISSUE-2: pass tenantID to releaseClaims for correct tenant isolation.
			newClaims := computedClaims[entityID]
			m.factory.releaseClaims(string(tid), entityID)
			m.factory.insertClaims(entityID, string(tid),
				entity.Meta.ModelRef.EntityName, entity.Meta.ModelRef.ModelVersion, newClaims)
		}

		// 5. Apply deletes (tombstones). Claims were already released in the
		// pre-release pass above — do not call releaseClaims again here.
		//
		// Attribution: prefer tx.DeleteAttribution[entityID], captured at
		// stage time (the STAGER's context, under the same OpMu section
		// that set tx.Deletes[entityID] — see EntityStore.Delete/DeleteAll).
		// Fall back to spi.AttributionFor(ctx) — this Commit call's own
		// ctx, i.e. the committer — only when no staged entry exists (a
		// caller that mutated tx.Deletes directly, bypassing EntityStore).
		// This is what fixes the prior bug: the committer's identity was
		// always used, even for deletes staged by a different (possibly
		// joined) caller earlier in the transaction.
		for entityID := range tx.Deletes {
			attribution, staged := tx.DeleteAttribution[entityID]
			if !staged {
				a, e := spi.AttributionFor(ctx)
				attribution = spi.WriteAttribution{Attributed: a, Executor: e}
			}
			if m.factory.entityData[tid] == nil {
				m.factory.entityData[tid] = make(map[string][]entityVersion)
			}
			versions := m.factory.entityData[tid][entityID]
			nextVersion := lastVersion(versions) + 1
			// The tombstone's model reference is only needed when entityID
			// has no prior committed version at all: a same-transaction
			// create-then-delete, whose buffered create Delete evicted from
			// tx.Buffer without ever separately flushing it (see
			// deletedBufferModels's field doc) — this tombstone is then the
			// ONLY committed version, and the sole place left to recover
			// the model from. When a prior version exists, modelRefOfLocked
			// already finds the model there, so this field stays zero.
			var tombstoneModel spi.ModelRef
			if len(versions) == 0 {
				tombstoneModel = capturedDeletedBufferModels[entityID]
			}
			m.factory.entityData[tid][entityID] = append(versions, entityVersion{
				entity:         nil,
				version:        nextVersion,
				transactionID:  txID,
				submitTime:     submitTime,
				deleted:        true,
				changeType:     "DELETED",
				user:           attribution.Attributed.ID,
				changeUserKind: attribution.Attributed.Kind,
				executor:       attribution.Executor,
				modelRef:       tombstoneModel,
			})
		}

		// 5.5. Apply the staged task-row post-images. Their checks ran when
		// they were staged, and step 3 proved that no other writer changed
		// those rows since this transaction began, so nothing is evaluated
		// here and nothing can fail after the entity flush. The prior rows
		// go into this commit's log entry, for the snapshots of the
		// transactions still open.
		taskPriors := priorRows(m.factory.scheduledTasks, capturedScheduledTaskOps)
		applyTaskOps(m.factory.scheduledTasks, capturedScheduledTaskOps)

		// 6. Record in committed log, submit times, and prune.
		func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			// Assign this commit's FCW ordering sequence — see commitSeq's
			// doc comment. Incremented exactly once per successful commit,
			// under this same mu section.
			m.commitSeq++
			m.committedLog = append(m.committedLog, committedTx{
				seq:        m.commitSeq,
				writeSet:   tx.WriteSet,
				taskWrites: taskPriors,
			})
			m.submitTimes[txID] = submitTimeEntry{submitTime: submitTime, tenantID: tid}
			evictBefore := m.factory.clock.Now().Add(-submitTimeTTL)
			for id, e := range m.submitTimes {
				if e.submitTime.Before(evictBefore) {
					delete(m.submitTimes, id)
				}
			}

			// Drop this transaction's state, then prune the log.
			m.forgetLocked(txID)
			// The commit-phase stamp above is smAuditTxIndex's only reader and
			// has already run, so this transaction's entries are dead.
			m.factory.discardAuditTxIndex(tid, txID)
			m.pruneCommittedLogLocked()
		}()

		return nil
	}(); err != nil {
		return err
	}
	return nil
}

// Rollback discards an active transaction without committing any changes.
func (m *TransactionManager) Rollback(ctx context.Context, txID string) error {
	uc := spi.GetUserContext(ctx)
	var tx *spi.TransactionState
	if err := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		var ok bool
		tx, ok = m.active[txID]
		if !ok {
			return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxNotFound, txID)
		}
		if uc == nil || uc.Tenant.ID != tx.TenantID {
			return fmt.Errorf("Rollback: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
		}
		return nil
	}(); err != nil {
		return err
	}

	// Acquire transaction operation write lock — waits for in-flight operations.
	tx.OpMu.Lock()
	defer func() {
		tx.Closed = true
		tx.OpMu.Unlock()
	}()

	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx.RolledBack = true
		m.forgetLocked(txID) // staged values, models, task ops and audit events are discarded unapplied
	}()
	// A rolled-back transaction is never stamped, so its audit-event index
	// entries have no reader at all — see discardAuditTxIndex.
	m.factory.discardAuditTxIndex(tx.TenantID, txID)
	return nil
}

// GetSubmitTime returns the submit time of a committed transaction.
// Returns an error if the transaction is still active or not found.
//
// Tenant isolation: like every other tx-lifecycle method, the caller's
// tenant must match the transaction's tenant. The check runs before any
// state-dependent response so a cross-tenant caller learns neither the
// submit time nor whether the transaction is in flight or committed.
func (m *TransactionManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
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

	return time.Time{}, fmt.Errorf("GetSubmitTime: %w (txID=%s)", spi.ErrTxNotFound, txID)
}

// CommittedLogLen returns the current length of the committed log.
// Exported for testing only.
func (m *TransactionManager) CommittedLogLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.committedLog)
}

// Savepoint creates a named savepoint within the given transaction by
// deep-copying the transaction's buffer maps (including DeleteAttribution,
// paired 1:1 with Deletes) and recording the current length of the
// transaction's staged scheduledTaskOps.
//
// Locking discipline: Savepoint reads tx.Buffer / tx.ReadSet /
// tx.WriteSet / tx.Deletes — the same fields Commit's flush phase iterates
// under tx.OpMu.Lock and that other tx-path ops (Save, Get, Delete, ...)
// mutate under tx.OpMu.RLock. Savepoint must therefore hold tx.OpMu.RLock
// across those reads. The lock interleaving with m.mu follows Commit's
// pattern: drop m.mu before taking tx.OpMu, re-take m.mu briefly for the
// m.savepoints update — the same m.mu section also reads
// len(m.scheduledTaskOps[txID]), since that map is m.mu-protected, not
// tx.OpMu-protected.
//
// Tenant isolation: rejects callers whose
// UserContext tenant does not match the transaction's tenant, mirroring
// Commit/Rollback. Without this guard a caller authenticated as tenant A
// who learned a tenant B txID could record a snapshot against tenant B's
// tx-state.
func (m *TransactionManager) Savepoint(ctx context.Context, txID string) (string, error) {
	uc := spi.GetUserContext(ctx)
	var tx *spi.TransactionState
	var ok bool
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx, ok = m.active[txID]
	}()
	if !ok {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()

	// Commit and Rollback set tx.Closed/RolledBack inside their tx.OpMu.Lock
	// region; once we hold tx.OpMu.RLock those flags are stable and reading
	// them tells us whether the tx was closed during our OpMu acquisition.
	if tx.RolledBack {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return "", fmt.Errorf("Savepoint: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}

	spID := uuid.UUID(m.uuids.NewTimeUUID()).String()

	// Deep-copy the buffer maps under tx.OpMu.RLock so we are serialised
	// against Commit/Rollback (Lock). Per the Join() godoc the application
	// is responsible for serialising its own concurrent ops on a single tx,
	// so concurrent Save+Savepoint is an application contract violation,
	// not a plugin defect — RLock here intentionally allows other readers.
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
	// length, mirroring scheduledTaskOpsLen's approach — see
	// savepointSnapshot.supersededLens godoc.
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
// captured when the savepoint was created, truncates the transaction's staged
// scheduledTaskOps back to the length recorded at that savepoint, then
// removes the snapshot.
//
// Locking discipline: RollbackToSavepoint replaces tx.Buffer /
// tx.ReadSet / tx.WriteSet / tx.Deletes — exclusive against every other
// tx-path op. Holds tx.OpMu.Lock (write) for the duration of the field
// replacement. Lock interleaving with m.mu follows Commit's pattern. The
// scheduledTaskOps truncation happens in the same m.mu section as the
// snapshot lookup, since that map is m.mu-protected (see
// stageTaskWrite), not tx.OpMu-protected.
//
// Tenant isolation: rejects cross-tenant
// callers — RollbackToSavepoint is destructive on tx-state.
func (m *TransactionManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	uc := spi.GetUserContext(ctx)
	var tx *spi.TransactionState
	var ok bool
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx, ok = m.active[txID]
	}()
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

	// A write the rollback discards stops being a write, but if another
	// transaction already committed its entity or task row after this one's
	// snapshot, the transaction has lost that race and Commit must refuse
	// it — see lostDiscardedWrite. The discarded task-row writes are the
	// staged ops past the savepoint's scheduledTaskOpsLen. A commit to that
	// entity or task row after the rollback does not race any write of this
	// transaction and is not recorded. Read-set entries the rollback
	// discards are dropped, as PostgreSQL drops them.
	var discarded []string
	for id := range tx.WriteSet {
		if !snap.writeSet[id] {
			discarded = append(discarded, id)
		}
	}
	var discardedTasks map[taskKey]bool
	if opsLen := snap.scheduledTaskOpsLen; opsLen < len(m.scheduledTaskOps[txID]) {
		discardedTasks = taskWriteSet(m.scheduledTaskOps[txID][opsLen:])
	}
	if m.committedSinceSnapshotLocked(txID, discarded, discardedTasks) {
		m.lostDiscardedWrite[txID] = true
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
	// cannot happen via the normal linear-nesting flow (only a stale
	// savepoint ID from an already-superseded rollback could produce it),
	// but truncating past slice bounds would panic, and Savepoint's other
	// restored fields (whole-map replacement) have no equivalent failure
	// mode to mirror here.
	if opsLen := snap.scheduledTaskOpsLen; opsLen < len(m.scheduledTaskOps[txID]) {
		m.scheduledTaskOps[txID] = m.scheduledTaskOps[txID][:opsLen]
	}

	// Truncate staged audit events back to the length recorded at the
	// savepoint, the same append-only truncate-back-to-length approach as
	// scheduledTaskOps above.
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
// Locking discipline: ReleaseSavepoint does not read or write
// tx.Buffer / tx.ReadSet / tx.WriteSet / tx.Deletes — it only mutates
// m.savepoints. Holds m.mu only; tx.OpMu is not required because there is
// no tx-state field to coordinate against Commit/Rollback.
//
// Tenant isolation: rejects cross-tenant
// callers — m.savepoints is tenant-scoped state.
func (m *TransactionManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
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

// LostRace reports whether the transaction has already lost a write race: a
// transaction that committed after its snapshot wrote an entity in its write
// set or a task row in its staged task-row ops, or a RollbackToSavepoint
// discarded a write that had already lost (lostDiscardedWrite). It is the
// write half of Commit's check, and changes nothing.
//
// Locking discipline: reads tx.WriteSet under tx.OpMu.RLock, as Savepoint
// does, and the log and staged ops under mu, taken after tx.OpMu as Commit
// takes them.
//
// Tenant isolation: rejects callers whose UserContext tenant does not match
// the transaction's tenant, before it reads anything of the transaction.
func (m *TransactionManager) LostRace(ctx context.Context, txID string) (bool, error) {
	uc := spi.GetUserContext(ctx)
	var tx *spi.TransactionState
	var ok bool
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		tx, ok = m.active[txID]
	}()
	if !ok {
		return false, fmt.Errorf("LostRace: %w (txID=%s)", spi.ErrTxNotFound, txID)
	}
	if uc == nil || uc.Tenant.ID != tx.TenantID {
		return false, fmt.Errorf("LostRace: %w (txID=%s)", spi.ErrTxTenantMismatch, txID)
	}

	tx.OpMu.RLock()
	defer tx.OpMu.RUnlock()
	if tx.RolledBack {
		return false, fmt.Errorf("LostRace: %w (txID=%s)", spi.ErrTxRolledBack, txID)
	}
	if tx.Closed {
		return false, fmt.Errorf("LostRace: %w (txID=%s)", spi.ErrTxAlreadyCommitted, txID)
	}
	written := make([]string, 0, len(tx.WriteSet))
	for id := range tx.WriteSet {
		written = append(written, id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lostDiscardedWrite[txID] {
		return true, nil
	}
	return m.committedSinceSnapshotLocked(txID, written, taskWriteSet(m.scheduledTaskOps[txID])), nil
}
