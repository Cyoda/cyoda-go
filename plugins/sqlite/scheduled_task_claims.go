package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// The methods in this file never join a transaction: each ignores any
// transaction on ctx and commits on its own on the writer connection. That is
// how a mark survives the rollback of the run's transaction. Every one that
// reads a task row to decide a write holds the commit gate, which serialises
// it with every commit, with Begin, and with the others — MarkUnsafe with
// ClaimDue in particular (C3).
//
// A row an open transaction has staged a change to is busy (C6): ClaimDue and
// GiveBackIdle skip it; MarkUnsafe and RecordAttempt answer spi.ErrTaskBusy,
// which the caller retries.

func (s *scheduledTaskStore) Heartbeat(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduler_owners (owner, heartbeat_at) VALUES (?, ?)
		 ON CONFLICT (owner) DO UPDATE SET heartbeat_at = excluded.heartbeat_at`,
		owner.String(), s.clock.Now().UnixMicro()); err != nil {
		return fmt.Errorf("failed to record a heartbeat: %w", err)
	}
	return nil
}

func (s *scheduledTaskStore) RetireOwner(ctx context.Context, owner uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM scheduler_owners WHERE owner = ?`, owner.String()); err != nil {
		return fmt.Errorf("failed to retire an owner: %w", err)
	}
	return nil
}

// SweepOwners removes the liveness record of every owner that has not
// heartbeated for deadFor, once no RUNNING task references it.
func (s *scheduledTaskStore) SweepOwners(ctx context.Context, deadFor time.Duration) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduler_owners
		 WHERE heartbeat_at < ?
		   AND NOT EXISTS (SELECT 1 FROM scheduled_tasks t
		                   WHERE t.status = 'RUNNING' AND t.claim_owner = scheduler_owners.owner)`,
		s.clock.Now().Add(-deadFor).UnixMicro()); err != nil {
		return fmt.Errorf("failed to sweep owners: %w", err)
	}
	return nil
}

// ClaimDue claims due tasks for req.Owner, under the commit gate: scan the
// candidates on s.db (a plain read, before any write begins), choose, then
// write the claims in commitTaskWrites' own sqlTx. It is the commit gate
// alone — held for the whole call — that makes the scan-then-write
// atomic, not a single sqlTx spanning both. A WAITING task is due when its
// next_attempt_time is at or before req.NowMs (the pnode clock).
// With AllowLostOwner, a RUNNING task whose owner is stale by the store clock
// is claimable too, and the claim adds 1 to its lost_owners. A task is never
// claimed while another task of its entity is RUNNING.
func (s *scheduledTaskStore) ClaimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if err := spi.ValidateClaimRequest(req); err != nil {
		return nil, err
	}
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	allowLost := 0
	if req.AllowLostOwner {
		allowLost = 1
	}
	cands, err := readTasks(ctx, s.db, selectTaskSQL+`
		WHERE ((t.status = 'WAITING' AND t.next_attempt_time <= ?)
		    OR (? = 1 AND t.status = 'RUNNING' AND NOT EXISTS (
		          SELECT 1 FROM scheduler_owners o
		          WHERE o.owner = t.claim_owner AND o.heartbeat_at >= ?)))
		  AND NOT EXISTS (
		          SELECT 1 FROM scheduled_tasks r
		          WHERE r.tenant_id = t.tenant_id AND r.entity_id = t.entity_id
		            AND r.status = 'RUNNING' AND r.id <> t.id)`,
		req.NowMs, allowLost, s.clock.Now().Add(-req.StaleAfter).UnixMicro())
	if err != nil {
		return nil, fmt.Errorf("failed to scan due scheduled tasks: %w", err)
	}

	// spi.SelectClaims applies the rules every backend shares: one task per
	// entity, the per-tenant limits, tenants taking turns.
	busy := s.tm.busyTaskKeys()
	free := cands[:0]
	for _, c := range cands {
		if !busy[taskKey{tenant: c.TenantID, id: c.ID}] {
			free = append(free, c)
		}
	}
	chosen := spi.SelectClaims(free, req)
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
	if err := s.tm.commitTaskWrites(ctx, ops, nil); err != nil {
		return nil, err
	}
	out := make([]spi.ScheduledTask, 0, len(ops))
	for i, op := range ops {
		claimed := copyScheduledTask(*op.after)
		// UnsafeMarked as read by the candidate scan above, under the same
		// commit gate as the claim itself (C3) — not re-read after the
		// commit: a failure from that read would report an error for
		// claims that had already committed, hiding a write the caller
		// never learns about.
		claimed.UnsafeMarked = chosen[i].UnsafeMarked
		// Set on the returned copy only; no column stores it.
		claimed.ClaimedFromLostOwner = fromLostOwner[i]
		out = append(out, claimed)
	}
	return out, nil
}

// GiveBackIdle returns to WAITING, uncounted, every task RUNNING under owner
// whose claim token is not in keep. A busy row stays as it is and is not
// counted.
func (s *scheduledTaskStore) GiveBackIdle(ctx context.Context, owner uuid.UUID, keep []uuid.UUID) (int, error) {
	kept := make(map[uuid.UUID]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	running, err := readTasks(ctx, s.db, selectTaskSQL+` WHERE t.status = 'RUNNING' AND t.claim_owner = ?`, owner.String())
	if err != nil {
		return 0, fmt.Errorf("failed to read an owner's running tasks: %w", err)
	}
	busy := s.tm.busyTaskKeys()
	var ops []scheduledTaskOp
	for _, t := range running {
		if kept[t.Claim.Token] || busy[taskKey{tenant: t.TenantID, id: t.ID}] {
			continue
		}
		back := copyScheduledTask(t)
		back.Status = spi.ScheduledTaskWaiting
		back.Claim = nil
		ops = append(ops, scheduledTaskOp{key: taskKey{tenant: t.TenantID, id: t.ID}, after: &back})
	}
	if err := s.tm.commitTaskWrites(ctx, ops, nil); err != nil {
		return 0, err
	}
	return len(ops), nil
}

// MarkUnsafe writes the mark of ref's life, naming ref's claim. It is
// idempotent for the same claim.
func (s *scheduledTaskStore) MarkUnsafe(ctx context.Context, ref spi.TaskRef) error {
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	if _, err := fenced(taskView{ctx: ctx, db: s.db}, ref); err != nil {
		return err
	}
	if s.tm.taskBusy(taskKey{tenant: ref.TenantID, id: ref.ID}) {
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrTaskBusy)
	}
	var holder string
	err := s.db.QueryRowContext(ctx,
		`SELECT claim_token FROM scheduled_task_marks WHERE tenant_id = ? AND task_id = ? AND arm_token = ?`,
		string(ref.TenantID), ref.ID, ref.ArmToken.String()).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("failed to read the mark of scheduled task %s: %w", ref.ID, err)
	case holder == ref.ClaimToken.String():
		return nil
	default:
		return fmt.Errorf("scheduled task %s: %w", ref.ID, spi.ErrMarkedByAnotherClaim)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduled_task_marks (tenant_id, task_id, arm_token, claim_token) VALUES (?, ?, ?, ?)`,
		string(ref.TenantID), ref.ID, ref.ArmToken.String(), ref.ClaimToken.String()); err != nil {
		return fmt.Errorf("failed to mark scheduled task %s: %w", ref.ID, classifyRejection(err))
	}
	return nil
}

// RecordAttempt ends ref's claim: the task goes back to WAITING with the
// attempt recorded. With ClearOwnMark it also removes the mark this claim
// wrote, in the same sqlTx.
func (s *scheduledTaskStore) RecordAttempt(ctx context.Context, ref spi.TaskRef, a spi.Attempt) error {
	if err := spi.ValidateTaskErrorText(a.Error); err != nil {
		return err
	}
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	t, err := fenced(taskView{ctx: ctx, db: s.db}, ref)
	if err != nil {
		return err
	}
	if s.tm.taskBusy(taskKey{tenant: ref.TenantID, id: ref.ID}) {
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
	op := scheduledTaskOp{key: taskKey{tenant: ref.TenantID, id: ref.ID}, after: &t}
	return s.tm.commitTaskWrites(ctx, []scheduledTaskOp{op}, func(sqlTx *sql.Tx) error {
		if !a.ClearOwnMark {
			return nil
		}
		if _, err := sqlTx.ExecContext(ctx,
			`DELETE FROM scheduled_task_marks
			 WHERE tenant_id = ? AND task_id = ? AND arm_token = ? AND claim_token = ?`,
			string(ref.TenantID), ref.ID, ref.ArmToken.String(), ref.ClaimToken.String()); err != nil {
			return fmt.Errorf("failed to clear the mark of scheduled task %s: %w", ref.ID, err)
		}
		return nil
	})
}

// SweepMarks removes the marks of ended lives: those whose task is gone or
// has been re-armed.
func (s *scheduledTaskStore) SweepMarks(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduled_task_marks
		 WHERE NOT EXISTS (SELECT 1 FROM scheduled_tasks t
		                   WHERE t.tenant_id = scheduled_task_marks.tenant_id
		                     AND t.id = scheduled_task_marks.task_id
		                     AND t.arm_token = scheduled_task_marks.arm_token)`); err != nil {
		return fmt.Errorf("failed to sweep marks: %w", err)
	}
	return nil
}
