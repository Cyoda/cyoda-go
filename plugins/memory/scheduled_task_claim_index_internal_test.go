package memory

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// allCandidatesLocked is the claim candidate set read from every task row: the
// set spi.SelectClaims is defined over.
func allCandidatesLocked(f *StoreFactory, req spi.ClaimRequest) []spi.ScheduledTask {
	cutoff := f.clock.Now().Add(-req.StaleAfter)
	busy := f.txManager.busyTaskKeys()
	running := make(map[entityTenantKey]taskKey)
	for k, t := range f.scheduledTasks {
		if t.Status == spi.ScheduledTaskRunning {
			running[entityTenantKey{tenant: string(k.tenant), id: t.EntityID}] = k
		}
	}
	var cands []spi.ScheduledTask
	for k, t := range f.scheduledTasks {
		if busy[k] {
			continue
		}
		switch {
		case t.Status == spi.ScheduledTaskWaiting && t.NextAttemptTime <= req.NowMs:
		case req.AllowLostOwner && t.Status == spi.ScheduledTaskRunning:
			if beat, ok := f.schedulerOwners[t.Claim.Owner]; ok && !beat.Before(cutoff) {
				continue
			}
		default:
			continue
		}
		if r, ok := running[entityTenantKey{tenant: string(k.tenant), id: t.EntityID}]; ok && r != k {
			continue
		}
		cands = append(cands, t)
	}
	return cands
}

// The claim index gives the same claims as a read of every row, over random
// writes: arms, re-arms that move NextAttemptTime, claims by a live or a lost
// owner, give-backs, failures and deletes, random limits, and random rows
// busy under an open transaction (C6).
func TestClaimIndex_MatchesEveryRowRead(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	f := NewStoreFactory(WithClock(NewTestClockAt(time.UnixMilli(1_000_000))))
	t.Cleanup(func() { _ = f.Close() })
	sts := &scheduledTaskStore{f: f}
	live, lost := uuid.New(), uuid.New()
	f.schedulerOwners[live] = f.clock.Now()

	tenants := []spi.TenantID{"t-a", "t-b", "t-c"}
	for step := range 4000 {
		k := taskKey{tenant: tenants[rng.IntN(len(tenants))], id: fmt.Sprintf("task-%02d", rng.IntN(60))}
		row := spi.ScheduledTask{
			ID: k.id, TenantID: k.tenant, Type: spi.ScheduledTaskFireTransition,
			EntityID: fmt.Sprintf("e-%d", rng.IntN(15)), ModelName: "M", ModelVersion: 1,
			Transition: "T", SourceState: "S", ArmToken: uuid.New(),
			Status: spi.ScheduledTaskWaiting, NextAttemptTime: int64(rng.IntN(100)),
		}
		if old, ok := f.scheduledTasks[k]; ok {
			row.EntityID = old.EntityID // a task keeps its entity
		}
		op := scheduledTaskOp{key: k, after: &row}
		switch r := rng.IntN(10); {
		case r < 1:
			op.after = nil
		case r < 3:
			owner := live
			if rng.IntN(2) == 0 {
				owner = lost
			}
			if _, ok := f.taskClaimIndex.running[k.tenant][row.EntityID]; ok {
				break // one RUNNING task per entity
			}
			row.Status = spi.ScheduledTaskRunning
			row.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: owner}
		case r < 4:
			row.Status = spi.ScheduledTaskFailed
			row.FailureReason = spi.FailureRunPanicked
		}
		if old, ok := f.scheduledTasks[k]; ok && old.Status == spi.ScheduledTaskRunning &&
			row.Status == spi.ScheduledTaskRunning && op.after != nil {
			row.Claim = old.Claim
		}
		func() {
			f.entityMu.Lock()
			defer f.entityMu.Unlock()
			f.txManager.commitTaskWrites([]scheduledTaskOp{op})
		}()

		if step%10 != 0 {
			continue
		}
		req := spi.ClaimRequest{Owner: uuid.New(), NowMs: int64(rng.IntN(110)), StaleAfter: time.Minute,
			Limit: 1 + rng.IntN(12), PerTenantLimit: 1 + rng.IntN(6), AllowLostOwner: rng.IntN(2) == 0,
			TenantInProgress: map[spi.TenantID]int{tenants[rng.IntN(len(tenants))]: rng.IntN(4)}}
		// Stage a change to a few random rows in an open transaction, so they
		// are busy for this claim.
		var staged []scheduledTaskOp
		for range rng.IntN(8) {
			bk := taskKey{tenant: tenants[rng.IntN(len(tenants))], id: fmt.Sprintf("task-%02d", rng.IntN(60))}
			staged = append(staged, scheduledTaskOp{key: bk})
		}
		func() {
			f.txManager.mu.Lock()
			defer f.txManager.mu.Unlock()
			f.txManager.scheduledTaskOps["busy-tx"] = staged
		}()
		var got, want []spi.ScheduledTask
		func() {
			f.entityMu.Lock()
			defer f.entityMu.Unlock()
			got = spi.SelectClaims(sts.claimCandidatesLocked(req), req)
			want = spi.SelectClaims(allCandidatesLocked(f, req), req)
		}()
		func() {
			f.txManager.mu.Lock()
			defer f.txManager.mu.Unlock()
			delete(f.txManager.scheduledTaskOps, "busy-tx")
		}()
		if gk, wk := claimKeys(got), claimKeys(want); fmt.Sprint(gk) != fmt.Sprint(wk) {
			t.Fatalf("step %d, request %+v: index chose %v, every-row read chose %v", step, req, gk, wk)
		}
	}
}

func claimKeys(tasks []spi.ScheduledTask) []string {
	out := make([]string, len(tasks))
	for i, x := range tasks {
		out[i] = string(x.TenantID) + "/" + x.ID
	}
	return out
}

// A second RUNNING row for one entity breaks the rule ClaimDue keeps (one
// RUNNING task per entity), which the other backends enforce with a unique
// index. The index refuses it rather than silently dropping one of the two.
func TestClaimIndex_SecondRunningRowOfAnEntityPanics(t *testing.T) {
	f := NewStoreFactory(WithClock(NewTestClockAt(time.UnixMilli(1_000_000))))
	t.Cleanup(func() { _ = f.Close() })
	running := func(id string) scheduledTaskOp {
		return scheduledTaskOp{key: taskKey{tenant: "t-a", id: id}, after: &spi.ScheduledTask{
			ID: id, TenantID: "t-a", Type: spi.ScheduledTaskFireTransition, EntityID: "e-1",
			ModelName: "M", ModelVersion: 1, Transition: "T", SourceState: "S", ArmToken: uuid.New(),
			Status: spi.ScheduledTaskRunning, Claim: &spi.TaskClaim{Token: uuid.New(), Owner: uuid.New()},
		}}
	}
	commit := func(op scheduledTaskOp) {
		f.entityMu.Lock()
		defer f.entityMu.Unlock()
		f.txManager.commitTaskWrites([]scheduledTaskOp{op})
	}
	commit(running("task-1"))
	commit(running("task-1")) // the same row, claimed again: not a second row

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a second RUNNING row of entity e-1 was accepted")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "one RUNNING task per entity") {
			t.Fatalf("panic %q does not name the broken rule", msg)
		}
		if _, ok := f.scheduledTasks[taskKey{tenant: "t-a", id: "task-2"}]; ok {
			t.Fatal("the refused write was applied")
		}
	}()
	commit(running("task-2"))
}

// A Commit whose staged task ops would put a second RUNNING row on an entity
// is refused before it writes anything: no entity version, no committed-log
// entry, no task row, and entityMu released.
func TestCommit_ASecondRunningRowIsRefusedBeforeAnyWrite(t *testing.T) {
	f := NewStoreFactory(WithClock(NewTestClockAt(time.UnixMilli(1_000_000))))
	t.Cleanup(func() { _ = f.Close() })
	tm := f.NewTransactionManager(newTestUUIDGenerator())
	running := func(id string) scheduledTaskOp {
		return scheduledTaskOp{key: taskKey{tenant: "t-a", id: id}, after: &spi.ScheduledTask{
			ID: id, TenantID: "t-a", Type: spi.ScheduledTaskFireTransition, EntityID: "e-1",
			ModelName: "M", ModelVersion: 1, Transition: "T", SourceState: "S", ArmToken: uuid.New(),
			Status: spi.ScheduledTaskRunning, Claim: &spi.TaskClaim{Token: uuid.New(), Owner: uuid.New()},
		}}
	}
	func() {
		f.entityMu.Lock()
		defer f.entityMu.Unlock()
		tm.commitTaskWrites([]scheduledTaskOp{running("task-1")})
	}()

	ctx := testCtxWithTenant("t-a")
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx := spi.GetTransaction(txCtx)
	tx.WriteSet["entity-E"] = true
	tx.Buffer["entity-E"] = &spi.Entity{Meta: spi.EntityMeta{ID: "entity-E", TenantID: "t-a", ChangeType: "CREATED"}, Data: []byte(`{}`)}
	logBefore, seqBefore := func() (int, int64) {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		tm.scheduledTaskOps[txID] = []scheduledTaskOp{running("task-2")}
		return len(tm.committedLog), tm.commitSeq
	}()

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("a commit with a second RUNNING row of entity e-1 was accepted")
			}
			if msg := fmt.Sprint(r); !strings.Contains(msg, "one RUNNING task per entity") {
				t.Fatalf("panic %q does not name the broken rule", msg)
			}
		}()
		_ = tm.Commit(ctx, txID)
	}()

	if !f.entityMu.TryLock() {
		t.Fatal("entityMu is still held after the refused commit")
	}
	f.entityMu.Unlock()
	if n := len(f.entityData["t-a"]["entity-E"]); n != 0 {
		t.Fatalf("the refused commit applied %d entity version(s)", n)
	}
	if _, ok := f.scheduledTasks[taskKey{tenant: "t-a", id: "task-2"}]; ok {
		t.Fatal("the refused commit applied its task row")
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.committedLog) != logBefore || tm.commitSeq != seqBefore {
		t.Fatalf("the refused commit was logged: %d entries, seq %d; want %d, %d",
			len(tm.committedLog), tm.commitSeq, logBefore, seqBefore)
	}
}
