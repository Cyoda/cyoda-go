package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
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

// ClaimDue claims due tasks for req.Owner, under the commit gate: read the
// candidates on s.db (plain reads, before any write begins), choose, then
// write the claims in commitTaskWrites' own sqlTx. It is the commit gate
// alone — held for the whole call — that makes the read-then-write atomic,
// not a single sqlTx spanning both. A WAITING task is due when its
// next_attempt_time is at or before req.NowMs (the pnode clock).
// With AllowLostOwner, a RUNNING task whose owner is stale by the store clock
// is claimable too, and the claim adds 1 to its lost_owners. A task is never
// claimed while another task of its entity is RUNNING, and a row an open
// transaction has staged a change to is not a candidate (C6).
func (s *scheduledTaskStore) ClaimDue(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	if err := spi.ValidateClaimRequest(req); err != nil {
		return nil, err
	}
	_ = s.tm.acquireCommitGate(context.Background())
	defer s.tm.releaseCommitGate()

	cands, err := s.claimCandidates(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to scan due scheduled tasks: %w", err)
	}

	// spi.SelectClaims applies the rules every backend shares: one task per
	// entity, the per-tenant limits, tenants taking turns.
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

// claimTenantsSQL lists the tenants one claim reads, with, for each, whether
// to read its WAITING tasks and whether to read its RUNNING ones:
//   - the tenants whose earliest WAITING task is due at ?1, found by a loose
//     scan of idx_scheduled_tasks_waiting that reads each tenant's first index
//     entry and nothing more (one probe per tenant with a WAITING task);
//   - when ?2 is 1 (AllowLostOwner), the tenants with a RUNNING task, read
//     through idx_scheduled_tasks_running_entity.
const claimTenantsSQL = `WITH RECURSIVE waiting_tenant(tenant_id, first_at) AS (
	SELECT tenant_id, next_attempt_time FROM (
		SELECT st.tenant_id, st.next_attempt_time FROM scheduled_tasks st
		 WHERE st.status = 'WAITING'
		 ORDER BY st.tenant_id, st.next_attempt_time LIMIT 1)
	UNION ALL
	SELECT st.tenant_id, st.next_attempt_time
	  FROM waiting_tenant w, scheduled_tasks st
	 WHERE st.rowid = (SELECT s2.rowid FROM scheduled_tasks s2
	                    WHERE s2.status = 'WAITING' AND s2.tenant_id > w.tenant_id
	                    ORDER BY s2.tenant_id, s2.next_attempt_time LIMIT 1)
)
SELECT tenant_id, 1, 0 FROM waiting_tenant WHERE first_at <= ?1
UNION ALL
SELECT DISTINCT tenant_id, 0, 1 FROM scheduled_tasks WHERE ?2 = 1 AND status = 'RUNNING'`

// claimWaitingSQL returns one tenant's first due WAITING candidates, one per
// entity: a task whose entity has no RUNNING task, that is not busy, and that
// no earlier WAITING task of its entity precedes unless that task is busy.
// Arguments: tenant, NowMs, the tenant's busy task ids (a JSON array), limit.
const claimWaitingSQL = selectTaskSQL + `
	WHERE t.tenant_id = ?1 AND t.status = 'WAITING' AND t.next_attempt_time <= ?2
	  AND NOT EXISTS (SELECT 1 FROM scheduled_tasks r
	                   WHERE r.tenant_id = t.tenant_id AND r.entity_id = t.entity_id
	                     AND r.status = 'RUNNING')
	  AND t.id NOT IN (SELECT value FROM json_each(?3))
	  AND NOT EXISTS (SELECT 1 FROM scheduled_tasks e
	                   WHERE e.tenant_id = t.tenant_id AND e.entity_id = t.entity_id
	                     AND e.status = 'WAITING'
	                     AND (e.next_attempt_time, e.id) < (t.next_attempt_time, t.id)
	                     AND e.id NOT IN (SELECT value FROM json_each(?3)))
	ORDER BY t.next_attempt_time, t.id
	LIMIT ?4`

// claimLostSQL returns one tenant's first RUNNING tasks whose owner is stale.
// scheduled_tasks has at most one RUNNING task per entity, and an entity with
// one has no WAITING candidate, so these are one per entity too. It reads the
// tenant's RUNNING tasks through their partial index, never the tenant's
// WAITING backlog. Arguments: tenant, the stale cutoff in microseconds, the
// tenant's busy task ids (a JSON array), limit.
const claimLostSQL = selectTaskSQL + ` INDEXED BY idx_scheduled_tasks_running_entity
	WHERE t.tenant_id = ?1 AND t.status = 'RUNNING'
	  AND NOT EXISTS (SELECT 1 FROM scheduler_owners o
	                   WHERE o.owner = t.claim_owner AND o.heartbeat_at >= ?2)
	  AND t.id NOT IN (SELECT value FROM json_each(?3))
	ORDER BY t.next_attempt_time, t.id
	LIMIT ?4`

// claimCandidates reads the candidates spi.SelectClaims needs, and no more.
// SelectClaims gives a tenant at most n = min(PerTenantLimit minus its runs in
// progress, Limit) turns, each to a different entity's first candidate in
// (next_attempt_time, id) order. So, per tenant, it is enough to read the
// first n WAITING tasks that are their entity's first candidate
// (claimWaitingSQL) and the first n lost-owner RUNNING tasks (claimLostSQL):
// the two name different entities, and the tenant's first n candidates lie
// within them. SelectClaims over this set chooses exactly what it would over
// every candidate, since its tenant order depends on each tenant's first
// candidate only, which the set keeps.
//
// The cost is one index probe per tenant with a WAITING task
// (claimTenantsSQL); claimWaitingSQL only for a tenant whose earliest WAITING
// task is due, and claimLostSQL only for a tenant with a RUNNING task. A
// claimWaitingSQL reads the rows its walk passes before the n-th candidate:
// the n, the busy rows, the tasks of entities with a RUNNING task, and later
// tasks of entities already met, each checked by a probe of
// idx_scheduled_tasks_waiting_entity. A claimLostSQL reads the tenant's
// RUNNING rows. None grows with a tenant's other due tasks. All of it runs
// under the commit gate.
func (s *scheduledTaskStore) claimCandidates(ctx context.Context, req spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	allowLost := 0
	if req.AllowLostOwner {
		allowLost = 1
	}
	rows, err := s.db.QueryContext(ctx, claimTenantsSQL, req.NowMs, allowLost)
	if err != nil {
		return nil, err
	}
	type reads struct{ waiting, running bool }
	var tenants []spi.TenantID
	read := make(map[spi.TenantID]*reads)
	for rows.Next() {
		var tn string
		var waiting, running bool
		if err := rows.Scan(&tn, &waiting, &running); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r, ok := read[spi.TenantID(tn)]
		if !ok {
			r = &reads{}
			read[spi.TenantID(tn)] = r
			tenants = append(tenants, spi.TenantID(tn))
		}
		r.waiting, r.running = r.waiting || waiting, r.running || running
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	busyIDs := make(map[spi.TenantID][]string)
	for k := range s.tm.busyTaskKeys() {
		busyIDs[k.tenant] = append(busyIDs[k.tenant], k.id)
	}
	staleCutoff := s.clock.Now().Add(-req.StaleAfter).UnixMicro()
	var cands []spi.ScheduledTask
	for _, tn := range tenants {
		n := min(req.PerTenantLimit-req.TenantInProgress[tn], req.Limit)
		if n <= 0 {
			continue
		}
		ids := busyIDs[tn]
		if ids == nil {
			ids = []string{}
		}
		busy, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		if read[tn].waiting {
			waiting, err := readTasks(ctx, s.db, claimWaitingSQL, string(tn), req.NowMs, string(busy), n)
			if err != nil {
				return nil, err
			}
			cands = append(cands, waiting...)
		}
		if read[tn].running {
			lost, err := readTasks(ctx, s.db, claimLostSQL, string(tn), staleCutoff, string(busy), n)
			if err != nil {
				return nil, err
			}
			cands = append(cands, lost...)
		}
	}
	return cands, nil
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
