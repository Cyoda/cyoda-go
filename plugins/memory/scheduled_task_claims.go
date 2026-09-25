package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The methods in this file never join a transaction: each ignores any
// transaction on ctx and commits on its own, under entityMu. That is how a
// mark survives the rollback of the run's transaction. Holding entityMu also
// serialises MarkUnsafe with ClaimDue (C3) and every one of them with every
// commit.
//
// A row an open transaction has staged a change to is busy: ClaimDue and
// GiveBackIdle skip it; MarkUnsafe and RecordAttempt answer spi.ErrTaskBusy,
// which the caller retries.

func (s *scheduledTaskStore) Heartbeat(_ context.Context, owner uuid.UUID) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	s.f.schedulerOwners[owner] = s.f.clock.Now()
	return nil
}

func (s *scheduledTaskStore) RetireOwner(_ context.Context, owner uuid.UUID) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	delete(s.f.schedulerOwners, owner)
	return nil
}

// SweepOwners removes the liveness record of every owner that has not
// heartbeated for deadFor, once no RUNNING task references it.
func (s *scheduledTaskStore) SweepOwners(_ context.Context, deadFor time.Duration) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	cutoff := s.f.clock.Now().Add(-deadFor)
	referenced := make(map[uuid.UUID]bool)
	for _, t := range s.f.scheduledTasks {
		if t.Status == spi.ScheduledTaskRunning {
			referenced[t.Claim.Owner] = true
		}
	}
	for owner, beat := range s.f.schedulerOwners {
		if beat.Before(cutoff) && !referenced[owner] {
			delete(s.f.schedulerOwners, owner)
		}
	}
	return nil
}

// ownerStaleLocked reports whether owner's liveness record is missing or
// older than cutoff. Caller holds entityMu.
func (s *scheduledTaskStore) ownerStaleLocked(owner uuid.UUID, cutoff time.Time) bool {
	beat, ok := s.f.schedulerOwners[owner]
	return !ok || beat.Before(cutoff)
}

// ClaimDue claims due tasks for req.Owner. A WAITING task is due when its
// NextAttemptTime is at or before req.NowMs (the pnode clock). With
// AllowLostOwner, a RUNNING task whose owner is stale by the store clock is
// claimable too, whatever its NextAttemptTime, and the claim adds 1 to its
// LostOwners. A task is never claimed while another task of its entity is
// RUNNING.
func (s *scheduledTaskStore) ClaimDue(_ context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if req.Limit < 1 || req.PerTenantLimit < 1 {
		return nil, fmt.Errorf("claim due scheduled tasks: limit and per-tenant limit must be >= 1, got %d and %d: %w", req.Limit, req.PerTenantLimit, spi.ErrStoreRejected)
	}
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()

	cutoff := s.f.clock.Now().Add(-req.StaleAfter)
	busy := s.f.txManager.busyTaskKeys()
	running := make(map[entityTenantKey]taskKey)
	for k, t := range s.f.scheduledTasks {
		if t.Status == spi.ScheduledTaskRunning {
			running[entityTenantKey{tenant: string(k.tenant), id: t.EntityID}] = k
		}
	}
	var cands []spi.ScheduledTask
	for k, t := range s.f.scheduledTasks {
		if busy[k] {
			continue
		}
		switch {
		case t.Status == spi.ScheduledTaskWaiting && t.NextAttemptTime <= req.NowMs:
		case req.AllowLostOwner && t.Status == spi.ScheduledTaskRunning && s.ownerStaleLocked(t.Claim.Owner, cutoff):
		default:
			continue
		}
		if r, ok := running[entityTenantKey{tenant: string(k.tenant), id: t.EntityID}]; ok && r != k {
			continue
		}
		cands = append(cands, t)
	}

	// spi.SelectClaims applies the rules every backend shares: one
	// task per entity, the per-tenant limits, tenants taking turns.
	chosen := spi.SelectClaims(cands, req)
	ops := make([]scheduledTaskOp, 0, len(chosen))
	fromLostOwner := make([]bool, 0, len(chosen))
	for _, c := range chosen {
		t := copyScheduledTask(c)
		lost := t.Status == spi.ScheduledTaskRunning
		if lost {
			t.LostOwners++
		}
		t.Status = spi.ScheduledTaskRunning
		t.Claim = &spi.TaskClaim{Token: uuid.New(), Owner: req.Owner}
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}, after: &t})
		fromLostOwner = append(fromLostOwner, lost)
	}
	s.f.txManager.commitTaskWrites(ops)

	out := make([]spi.ScheduledTask, 0, len(ops))
	for i, op := range ops {
		claimed := s.f.withMarkLocked(*op.after)
		// Set on the returned copy only; the stored row never carries it.
		claimed.ClaimedFromLostOwner = fromLostOwner[i]
		out = append(out, claimed)
	}
	return out, nil
}

// GiveBackIdle returns to WAITING, uncounted, every task RUNNING under owner
// whose claim token is not in keep. NextAttemptTime, Attempts, LostOwners and
// marks are unchanged.
func (s *scheduledTaskStore) GiveBackIdle(_ context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error) {
	kept := make(map[uuid.UUID]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	busy := s.f.txManager.busyTaskKeys()
	var ops []scheduledTaskOp
	for k, t := range s.f.scheduledTasks {
		if t.Status != spi.ScheduledTaskRunning || t.Claim.Owner != owner || kept[t.Claim.Token] || busy[k] {
			continue
		}
		back := copyScheduledTask(t)
		back.Status = spi.ScheduledTaskWaiting
		back.Claim = nil
		ops = append(ops, scheduledTaskOp{key: k, after: &back})
	}
	s.f.txManager.commitTaskWrites(ops)
	return len(ops), nil
}

// MarkUnsafe writes the mark of ref's life, naming ref's claim. It is
// idempotent for the same claim.
func (s *scheduledTaskStore) MarkUnsafe(_ context.Context, ref spi.TaskRef) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	k := taskKey{tenant: ref.TenantID, id: ref.ID}
	if _, err := fenced(taskView{f: s.f}, ref); err != nil {
		return err
	}
	if s.f.txManager.busyTaskKeys()[k] {
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrTaskBusy)
	}
	mk := markKey{task: k, arm: ref.ArmToken}
	if holder, ok := s.f.taskMarks[mk]; ok {
		if holder == ref.ClaimToken {
			return nil
		}
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrMarkedByAnotherClaim)
	}
	s.f.taskMarks[mk] = ref.ClaimToken
	return nil
}

// RecordAttempt ends ref's claim: the task goes back to WAITING with the
// attempt recorded. With ClearOwnMark it also removes the mark this claim
// wrote, in the same critical section.
func (s *scheduledTaskStore) RecordAttempt(_ context.Context, ref spi.TaskRef, a spi.Attempt) error {
	if err := spi.ValidateTaskErrorText(a.Error); err != nil {
		return err
	}
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	k := taskKey{tenant: ref.TenantID, id: ref.ID}
	t, err := fenced(taskView{f: s.f}, ref)
	if err != nil {
		return err
	}
	if s.f.txManager.busyTaskKeys()[k] {
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrTaskBusy)
	}
	if !a.NotCounted {
		t.Attempts++
	}
	at := a.AtMs
	t.Status = spi.ScheduledTaskWaiting
	t.Claim = nil
	t.LastAttemptTime = &at
	t.LastError = a.Error
	t.NextAttemptTime = a.NextAttemptTime
	s.f.txManager.commitTaskWrites([]scheduledTaskOp{{key: k, after: &t}})
	if a.ClearOwnMark {
		mk := markKey{task: k, arm: ref.ArmToken}
		if s.f.taskMarks[mk] == ref.ClaimToken {
			delete(s.f.taskMarks, mk)
		}
	}
	return nil
}

// SweepMarks removes the marks of ended lives: those whose task is gone or
// has been re-armed.
func (s *scheduledTaskStore) SweepMarks(_ context.Context) error {
	s.f.entityMu.Lock()
	defer s.f.entityMu.Unlock()
	for mk := range s.f.taskMarks {
		if t, ok := s.f.scheduledTasks[mk.task]; !ok || t.ArmToken != mk.arm {
			delete(s.f.taskMarks, mk)
		}
	}
	return nil
}
