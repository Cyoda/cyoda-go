package memory

import (
	"container/heap"
	"fmt"
	"slices"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// claimIndex is what ClaimDue reads instead of every task row: per tenant, its
// WAITING rows in (NextAttemptTime, ID) order, and its RUNNING rows by entity.
// applyTaskOps, the one writer of the task rows, keeps it; it is guarded by
// entityMu like the rows.
//
// The WAITING order is a min-heap per tenant with lazy removal: a write that
// takes a row out of WAITING, or moves its NextAttemptTime, leaves the old
// entry in the heap and only forgets its sequence number in live; a reader
// drops an entry whose sequence number is not the row's current one when it
// meets it. A heap is rebuilt from its live entries once dead ones outnumber
// them, so it stays within twice its live size.
type claimIndex struct {
	seq     uint64
	live    map[taskKey]uint64 // each WAITING row's current entry
	waiting map[spi.TenantID]*waitQueue
	running map[spi.TenantID]map[string]taskKey // tenant → entity → its RUNNING row
}

func newClaimIndex() *claimIndex {
	return &claimIndex{
		live:    make(map[taskKey]uint64),
		waiting: make(map[spi.TenantID]*waitQueue),
		running: make(map[spi.TenantID]map[string]taskKey),
	}
}

type waitEntry struct {
	at  int64 // NextAttemptTime
	id  string
	seq uint64
}

// waitQueue is one tenant's WAITING entries, earliest first; live counts the
// entries that are current.
type waitQueue struct {
	entries []waitEntry
	live    int
}

func (q *waitQueue) Len() int { return len(q.entries) }
func (q *waitQueue) Less(i, j int) bool {
	a, b := q.entries[i], q.entries[j]
	if a.at != b.at {
		return a.at < b.at
	}
	return a.id < b.id
}
func (q *waitQueue) Swap(i, j int) { q.entries[i], q.entries[j] = q.entries[j], q.entries[i] }
func (q *waitQueue) Push(x any)    { q.entries = append(q.entries, x.(waitEntry)) }
func (q *waitQueue) Pop() any {
	last := q.entries[len(q.entries)-1]
	q.entries = q.entries[:len(q.entries)-1]
	return last
}

// update records that row k went from before (absent when !hadBefore) to after
// (nil when deleted).
func (x *claimIndex) update(k taskKey, before spi.ScheduledTask, hadBefore bool, after *spi.ScheduledTask) {
	if hadBefore {
		switch before.Status {
		case spi.ScheduledTaskWaiting:
			delete(x.live, k)
			x.waiting[k.tenant].live--
		case spi.ScheduledTaskRunning:
			if byEntity := x.running[k.tenant]; byEntity[before.EntityID] == k {
				delete(byEntity, before.EntityID)
				if len(byEntity) == 0 {
					delete(x.running, k.tenant)
				}
			}
		}
	}
	if after == nil {
		x.compact(k.tenant)
		return
	}
	switch after.Status {
	case spi.ScheduledTaskWaiting:
		x.seq++
		x.live[k] = x.seq
		q := x.waiting[k.tenant]
		if q == nil {
			q = &waitQueue{}
			x.waiting[k.tenant] = q
		}
		heap.Push(q, waitEntry{at: after.NextAttemptTime, id: k.id, seq: x.seq})
		q.live++
	case spi.ScheduledTaskRunning:
		byEntity := x.running[k.tenant]
		if byEntity == nil {
			byEntity = make(map[string]taskKey)
			x.running[k.tenant] = byEntity
		}
		byEntity[after.EntityID] = k
	}
	x.compact(k.tenant)
}

// checkOneRunning panics when applying ops to rows would leave two RUNNING
// tasks on one entity. Only ClaimDue makes a task RUNNING, and it never claims
// a task beside a RUNNING sibling; the other backends enforce the rule with a
// unique index, which refuses the write. Here a write that breaks it is a
// defect, and a claim index that kept one of the two rows would silently lose
// the other, so the write is refused whole, before any of it applies:
// applyTaskOps runs it before its first write, and Commit runs it (step 3.1)
// before it flushes any entity version, so a refused commit leaves no torn
// write behind.
func (x *claimIndex) checkOneRunning(rows map[taskKey]spi.ScheduledTask, ops []scheduledTaskOp) {
	type entityKey struct {
		tenant spi.TenantID
		entity string
	}
	overlay := make(map[entityKey]*taskKey, len(ops)) // entity → its RUNNING row after the ops so far; nil: none
	current := make(map[taskKey]*spi.ScheduledTask, len(ops))
	rowOf := func(k taskKey) (spi.ScheduledTask, bool) {
		if t, ok := current[k]; ok {
			if t == nil {
				return spi.ScheduledTask{}, false
			}
			return *t, true
		}
		t, ok := rows[k]
		return t, ok
	}
	runningOf := func(e entityKey) (taskKey, bool) {
		if r, ok := overlay[e]; ok {
			if r == nil {
				return taskKey{}, false
			}
			return *r, true
		}
		r, ok := x.running[e.tenant][e.entity]
		return r, ok
	}
	for _, op := range ops {
		if before, ok := rowOf(op.key); ok && before.Status == spi.ScheduledTaskRunning {
			e := entityKey{tenant: op.key.tenant, entity: before.EntityID}
			if r, ok := runningOf(e); ok && r == op.key {
				overlay[e] = nil
			}
		}
		current[op.key] = op.after
		if op.after == nil || op.after.Status != spi.ScheduledTaskRunning {
			continue
		}
		e := entityKey{tenant: op.key.tenant, entity: op.after.EntityID}
		if r, ok := runningOf(e); ok && r != op.key {
			panic(fmt.Sprintf("scheduled tasks: one RUNNING task per entity: task %s of tenant %s would be RUNNING beside task %s",
				op.key.id, op.key.tenant, r.id))
		}
		k := op.key
		overlay[e] = &k
	}
}

// compact rebuilds tenant's heap from its live entries once dead ones
// outnumber them, and drops it when none is live.
func (x *claimIndex) compact(tenant spi.TenantID) {
	q := x.waiting[tenant]
	if q == nil {
		return
	}
	if q.live == 0 {
		delete(x.waiting, tenant)
		return
	}
	if len(q.entries) <= 2*q.live+64 {
		return
	}
	q.entries = slices.DeleteFunc(q.entries, func(e waitEntry) bool {
		return x.live[taskKey{tenant: tenant, id: e.id}] != e.seq
	})
	heap.Init(q)
}

// waitingCandidates returns tenant's first n WAITING candidates: due at nowMs,
// not busy, with no RUNNING task on their entity, one per entity, each its
// entity's earliest such task, in (NextAttemptTime, ID) order. It pops the
// entries it passes and pushes the live ones back, so it reads the n
// candidates plus the entries it passes over: dead ones (dropped for good),
// busy rows, rows of entities with a RUNNING task, and later rows of entities
// already taken. rows are the task rows.
func (x *claimIndex) waitingCandidates(rows map[taskKey]spi.ScheduledTask, tenant spi.TenantID, nowMs int64, n int, busy map[taskKey]bool) []spi.ScheduledTask {
	q := x.waiting[tenant]
	if q == nil {
		return nil
	}
	var out []spi.ScheduledTask
	var passed []waitEntry
	taken := make(map[string]bool, n)
	for len(out) < n && q.Len() > 0 {
		e := q.entries[0]
		k := taskKey{tenant: tenant, id: e.id}
		if x.live[k] != e.seq {
			heap.Pop(q) // dead: the row left WAITING or moved
			continue
		}
		if e.at > nowMs {
			break
		}
		heap.Pop(q)
		passed = append(passed, e)
		t := rows[k]
		if busy[k] || taken[t.EntityID] {
			continue
		}
		if _, ok := x.running[tenant][t.EntityID]; ok {
			continue
		}
		taken[t.EntityID] = true
		out = append(out, t)
	}
	for _, e := range passed {
		heap.Push(q, e)
	}
	x.compact(tenant)
	return out
}

// lostCandidates returns tenant's first n RUNNING tasks, in (NextAttemptTime,
// ID) order, that are not busy and whose owner lost is true for. It reads the
// tenant's RUNNING rows, which the runs in progress bound.
func (x *claimIndex) lostCandidates(rows map[taskKey]spi.ScheduledTask, tenant spi.TenantID, n int, busy map[taskKey]bool, lost func(spi.ScheduledTask) bool) []spi.ScheduledTask {
	var out []spi.ScheduledTask
	for _, k := range x.running[tenant] {
		if t := rows[k]; !busy[k] && lost(t) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b spi.ScheduledTask) int {
		if a.NextAttemptTime != b.NextAttemptTime {
			if a.NextAttemptTime < b.NextAttemptTime {
				return -1
			}
			return 1
		}
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out[:min(n, len(out))]
}
