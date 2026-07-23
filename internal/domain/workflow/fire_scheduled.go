package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// ScheduledOutcome reports how FireScheduledTransition resolved a single
// ScheduledTask.
type ScheduledOutcome string

const (
	// OutcomeFired means the transition fired: the criterion (if any)
	// matched, processors ran, and the entity's new state (post-cascade)
	// was persisted.
	OutcomeFired ScheduledOutcome = "fired"
	// OutcomeDeclined means the transition's criterion evaluated false —
	// terminal, one-shot, no retry (design §5.4).
	OutcomeDeclined ScheduledOutcome = "declined"
	// OutcomeExpired means the task's lateness exceeded TimeoutMs plus the
	// expiry grace band — terminal, no retry (design §5.5).
	OutcomeExpired ScheduledOutcome = "expired"
	// OutcomeDropped covers every case that is either self-healing or
	// safe to leave for the next scan: the task row was already gone, the
	// entity moved on, the task was re-armed to the future, the workflow
	// or transition no longer exists, the fire lost a concurrent-write
	// race, or a transient error occurred while resolving one of these.
	OutcomeDropped ScheduledOutcome = "dropped"
)

// firePrincipalSystemID identifies the platform system principal the fire
// path executes as and attributes legacy (zero-ArmedBy) rows to. Deliberately
// the same identity (ID and Kind) as scheduler.SystemPrincipal()/
// scheduler.SystemUserContext() — attribution must see one system principal
// regardless of which subsystem drove the write — but defined locally rather
// than imported: internal/domain/workflow must not import internal/scheduler.
const firePrincipalSystemID = "system"

// systemPrincipal is the principal FireScheduledTransition always records as
// the anchor write's executor, and falls back to as the attributed principal
// for legacy rows whose durable ScheduledTask never recorded an ArmedBy.
var systemPrincipal = spi.Principal{ID: firePrincipalSystemID, Kind: spi.PrincipalSystem}

// defaultExpiryGraceMs is the engine's default expiry grace band (design
// §5.5; the scheduler service's config knob for this, tracked separately in
// Phase D, is not wired here): sized to comfortably exceed typical
// inter-node NTP clock skew so two members can never disagree about
// fire-vs-expire for the same task.
const defaultExpiryGraceMs = int64(100)

// WithExpiryGrace overrides the engine's expiry grace band — see
// Engine.expiryGraceMs. Values <= 0 are ignored (default retained).
func WithExpiryGrace(d time.Duration) EngineOption {
	return func(e *Engine) {
		if d > 0 {
			e.expiryGraceMs = d.Milliseconds()
		}
	}
}

// FireScheduledTransition is the scheduler's ONLY door into the engine: it
// resolves exactly one ScheduledTask — firing, declining, expiring, or
// dropping it — in a single transaction that this method begins and either
// commits or rolls back itself. It is internal: no HTTP/gRPC handler calls
// it directly. A scheduler worker (Phase D) calls it after a
// peer-authenticated dispatch, having already synthesised a system
// UserContext scoped to task.TenantID.
//
// Only task.ID is trusted from the argument — every other field is re-read
// from the store inside the transaction (the "re-read guard", design §5.3),
// because the argument may be stale by the time a worker picks it up (a
// concurrent loopback can have re-armed it to a new ScheduledTime, or the
// entity may have already left task.SourceState). task.TenantID is the one
// exception with a bespoke check: it is not re-read (the row found by
// task.ID already carries the authoritative TenantID), but it IS verified
// against that authoritative value immediately after the re-read, before
// any tenant-scoped store is touched — see the tenant guard below. A
// dispatch whose ctx/task.TenantID doesn't match the row's real tenant is
// forged or corrupted and is dropped with zero effect on the row.
//
// See design doc §5.2 (two doors, one mechanism), §5.3 (re-read guard),
// §5.4 (one-shot criterion), §5.5 (grace band) for the rationale below.
func (e *Engine) FireScheduledTransition(ctx context.Context, task spi.ScheduledTask) (ScheduledOutcome, error) {
	// Seed the causal origin from the DURABLE row — never from the task
	// argument: the peer-RPC path (scheduler_rpc.go) passes an
	// RPC-deserialized task, and only task.ID is trusted from it (see the
	// doc comment above). This point-read happens before Begin so that
	// spi.ResolveOrigin, evaluated inside the plugin's Begin, sees this
	// ambient origin as the fire's root-tx Origin — every stamp site the
	// fire's cascade touches (internal/domain/entity/service.go's
	// spi.AttributionFor call sites) then inherits it automatically. The
	// in-tx re-read guard below re-checks ArmedBy against this seed and
	// aborts if it changed (fail closed; a forged/stale seed must never
	// silently attribute a fire to the wrong principal).
	preSts, err := e.factory.ScheduledTaskStore(ctx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to get scheduled task store for origin seed: %w", err)
	}
	seeded := spi.Principal{}
	if pre, found, err := preSts.Get(ctx, task.ID); err != nil {
		return OutcomeDropped, fmt.Errorf("point-read scheduled task before fire: %w", err)
	} else if found {
		seeded = pre.ArmedBy
	}
	ctx = spi.WithAmbientOrigin(ctx, seeded) // zero -> no seed -> origin falls through to the system UserContext

	txID, txCtx, err := e.txMgr.Begin(ctx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to begin scheduled-fire transaction: %w", err)
	}
	// curCtx/curTxID track the CURRENTLY OPEN transaction segment as the flow
	// advances (entry -> post-fireTransition -> post-cascade). A
	// COMMIT_BEFORE_DISPATCH processor anywhere in the fired transition or
	// its cascade commits the entry segment (TX_pre) and opens a new one
	// (TX_post); every non-commit exit after that point must roll back the
	// segment curTxID NOW names, not the (already-committed, rollback-is-a-
	// no-op) entry txID — mirrors rollbackOwned(finalCtx, finalTxID) in
	// internal/domain/entity/service.go.
	curCtx, curTxID := txCtx, txID
	committed := false
	defer func() {
		if !committed {
			// Best-effort: if curTxID's segment already committed further
			// down (e.g. this fires before a later CBD-segment commit is
			// reflected here), Rollback is a safe, ignored no-op (same
			// pattern as rollbackOpenSegmentOnFailure elsewhere in this
			// package).
			_ = e.txMgr.Rollback(curCtx, curTxID)
		}
	}()

	sts, err := e.factory.ScheduledTaskStore(txCtx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to get scheduled task store: %w", err)
	}
	es, err := e.factory.EntityStore(txCtx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to get entity store: %w", err)
	}
	auditStore, err := e.factory.StateMachineAuditStore(txCtx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to get audit store: %w", err)
	}

	// --- Re-read guard step 1: does the task row still exist? ---
	cur, found, err := sts.Get(txCtx, task.ID)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to read scheduled task: %w", err)
	}
	if !found {
		// Already resolved (fired/declined/expired) or cancelled elsewhere.
		committed = true
		return OutcomeDropped, e.txMgr.Commit(ctx, txID)
	}

	// --- Tenant guard: does the authoritative row's tenant match the
	// dispatch's asserted tenant? ---
	//
	// task.ID is the only trusted field in the argument (see the doc
	// comment above), so cur — the row just re-read by that id — is the
	// authoritative source of truth, including cur.TenantID. task.TenantID
	// is caller-supplied (ultimately a peer-RPC payload field) and MUST be
	// cross-checked against it here, before the entity is ever touched:
	// txCtx (and therefore every tenant-scoped store opened below, e.g.
	// EntityStore) is scoped to task.TenantID, not cur.TenantID. Without
	// this guard, a forged dispatch naming a real task.ID from tenant A but
	// asserting task.TenantID == tenant B would have es.Get fail closed
	// with spi.ErrNotFound (correctly finding no such entity under tenant
	// B), which the entity-load branch below would otherwise misread as
	// "hard-deleted" and self-heal by deleting tenant A's live task — a
	// cross-tenant integrity/DoS effect with no audit trail. Silently drop
	// instead: no delete, no fire, no entity access, no audit (the request
	// is forged/inconsistent, not a legitimate lifecycle event of either
	// tenant's data).
	if cur.TenantID != task.TenantID {
		slog.WarnContext(txCtx, "scheduled task dispatch tenant mismatch; dropping without touching the task row",
			slog.String("pkg", "workflow"),
			slog.String("taskId", task.ID),
			slog.String("entityId", cur.EntityID),
			slog.String("tenantId", string(cur.TenantID)),
			slog.String("assertedTenantId", string(task.TenantID)))
		return OutcomeDropped, nil
	}

	// --- Verify-or-abort: has the arming principal changed since the
	// pre-Begin point-read seeded ctx's ambient origin? ---
	//
	// A concurrent re-arm between the point-read and this in-tx re-read
	// (e.g. a racing loopback save arming the same task under a different
	// principal) means the origin already seeded onto txCtx no longer
	// matches the row this fire is about to attribute against — every write
	// in this transaction would silently inherit the STALE principal. Fail
	// closed: abort without committing anything: the scan loop's existing
	// backoff retries, and the retry re-seeds from a fresh point-read.
	if cur.ArmedBy != seeded {
		return OutcomeDropped, fmt.Errorf("scheduled task re-armed concurrently (arming principal changed); will retry")
	}

	// --- Re-read guard step 2: is the entity still in the task's source state? ---
	entity, err := es.Get(txCtx, cur.EntityID)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			// The entity is gone (hard-deleted); the task is stale.
			// Self-heal silently — no audit, nothing left to retry.
			_, _ = sts.Delete(txCtx, task.ID)
			committed = true
			return OutcomeDropped, e.txMgr.Commit(ctx, txID)
		}
		return OutcomeDropped, fmt.Errorf("failed to read entity: %w", err)
	}
	if entity.Meta.State != cur.SourceState {
		// The entity already left sourceState — transitioned out, or
		// already fired by a racing worker. Silent drop, no audit (design
		// §5.3 step 2; §8 Cancelled is reserved for the explicit-exit
		// reconcile, not this guard).
		_, _ = sts.Delete(txCtx, task.ID)
		committed = true
		return OutcomeDropped, e.txMgr.Commit(ctx, txID)
	}

	// --- Re-read guard step 3: has it been re-armed to the future? ---
	nowMs := e.now().UnixMilli()
	if cur.ScheduledTime > nowMs {
		// A loopback re-armed the row to a later time since dispatch.
		// Leave it in place for its new time — do not touch it.
		return OutcomeDropped, nil
	}

	// --- Resolve the workflow + transition (design §5.2) ---
	wfStore, err := e.factory.WorkflowStore(txCtx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to get workflow store: %w", err)
	}
	workflows, err := wfStore.Get(txCtx, entity.Meta.ModelRef)
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		return OutcomeDropped, fmt.Errorf("failed to load workflows: %w", err)
	}
	wf := e.findWorkflowForState(workflows, entity.Meta.State)
	transition := findTransitionInState(wf, entity.Meta.State, cur.Transition)
	if transition == nil {
		// The workflow was re-imported and the state or transition this
		// task references no longer exists. The task is obsolete — drop it
		// rather than retry forever.
		slog.DebugContext(txCtx, "scheduled task references a transition no longer present in the workflow; dropping",
			slog.String("pkg", "workflow"),
			slog.String("entityId", cur.EntityID),
			slog.String("sourceState", cur.SourceState),
			slog.String("transition", cur.Transition))
		_, _ = sts.Delete(txCtx, task.ID)
		committed = true
		return OutcomeDropped, e.txMgr.Commit(ctx, txID)
	}

	// --- Grace-band lateness gate (design §5.5) ---
	lateness := nowMs - cur.ScheduledTime
	if cur.TimeoutMs != nil {
		if lateness > *cur.TimeoutMs+e.expiryGraceMs {
			if removed, delErr := sts.Delete(txCtx, task.ID); delErr != nil {
				return OutcomeDropped, fmt.Errorf("failed to delete expired scheduled task: %w", delErr)
			} else if removed {
				e.recordEvent(auditStore, txCtx, entity.Meta.ID, txID, entity.Meta.State,
					spi.SMEventScheduledTransitionExpired,
					fmt.Sprintf("Scheduled transition %q expired (lateness %dms > timeout %dms + grace %dms)",
						cur.Transition, lateness, *cur.TimeoutMs, e.expiryGraceMs), nil)
			}
			committed = true
			return OutcomeExpired, e.txMgr.Commit(ctx, txID)
		}
		if lateness > *cur.TimeoutMs {
			// Grace band: neither fire nor expire. Leave the row; a later
			// scan resolves it once past the band (design §5.5, §15 F4).
			return OutcomeDropped, nil
		}
	}

	// --- Fire (design §5.2/§5.3) ---
	// expectedTxID is "the txID read in this fire transaction" (design
	// §5.3): the entity's last-committed TransactionID as of the Get above.
	// Captured before fireTransition/cascadeAutomated run — neither mutates
	// entity.Meta.TransactionID — so it stays valid as the CAS precondition
	// for the final persist below, whether or not the cascade segments.
	expectedTxID := entity.Meta.TransactionID
	fireCtx := withIfMatch(txCtx, expectedTxID)

	newCtx, newTxID, matched, fireErr := e.fireTransition(fireCtx, entity, wf, transition, auditStore, txID)
	// fireTransition may have segmented via a COMMIT_BEFORE_DISPATCH
	// processor even on a subsequent failure (e.g. a later processor in the
	// same pipeline fails after an earlier one committed TX_pre) — advance
	// curCtx/curTxID unconditionally so every exit below rolls back whatever
	// is actually still open (Finding #1).
	curCtx, curTxID = newCtx, newTxID
	if fireErr != nil {
		if errors.Is(fireErr, ErrCriterionNotMatched) {
			// One-shot decline: terminal, no retry (design §5.4). The
			// criterion-no-match audit event (design §8) was already
			// recorded by fireTransition itself above — the same event
			// type ManualTransition's criterion-fail path uses — so there
			// is nothing further to emit here; just resolve the task.
			if _, delErr := sts.Delete(newCtx, task.ID); delErr != nil {
				return OutcomeDropped, fmt.Errorf("failed to delete declined scheduled task: %w", delErr)
			}
			committed = true
			return OutcomeDeclined, e.txMgr.Commit(ctx, newTxID)
		}
		// Any other failure (criterion-evaluation error, processor
		// failure) is retried on the next scan — leave the task in place.
		return OutcomeDropped, fireErr
	}
	if !matched {
		// fireTransition's contract is matched==false <=> non-nil err (see
		// its doc comment); reaching here would be a mechanism bug rather
		// than a scheduler-policy decision. Fail safe: drop rather than
		// silently treat an unchanged entity as fired.
		return OutcomeDropped, fmt.Errorf("fireTransition reported matched=false without an error for transition %q", cur.Transition)
	}

	// Fired: complete the transition exactly like ManualTransition —
	// cascade from the new state, then reconcile (which deletes THIS task,
	// since its SourceState no longer matches entity.Meta.State, and arms
	// the settled state's own schedules), all still within txID/newTxID's
	// transaction(s), before the entity is persisted.
	finalCtx, finalTxID, err := e.cascadeAutomated(newCtx, entity, wf, auditStore, newTxID)
	// Same reasoning as above: cascadeAutomated may segment further via its
	// own CBD processors, and may fail entirely outside executeProcessors
	// (e.g. the maxStateVisits abort) — a failure executeProcessors's own
	// segment-cleanup never sees. Advance curCtx/curTxID before checking err
	// so the deferred rollback always targets the segment actually open.
	curCtx, curTxID = finalCtx, finalTxID
	if err != nil {
		return OutcomeDropped, err
	}

	// reconcileScheduledTasks/ReconcileForEntity cancels (and deletes) every
	// pending task whose SourceState != the entity's (now post-cascade)
	// CurrentState — which includes the task that just fired, since its
	// SourceState is the OLD state. That row deletion is correct (the task
	// is resolved either way), but recording a SCHEDULED_TRANSITION_CANCEL
	// for it alongside its own SCHEDULED_TRANSITION_FIRE would be wrong
	// (Finding #2): it left sourceState BECAUSE it fired, not because it was
	// left behind. task.ID is passed as the audit-suppression exclusion so
	// reconcile still cancels (and its underlying Delete still removes) any
	// genuine sibling out of the old state, just without double-counting
	// this one.
	//
	// Note: an explicit pre-reconcile Delete(task.ID) would NOT work here —
	// every ScheduledTaskStore backend (memory/sqlite/postgres) buffers
	// Upsert/Delete as staged ops applied only at commit-flush time; Get/
	// ReconcileForEntity read committed state, so a same-tx delete would be
	// invisible to the very ReconcileForEntity call right after it.
	if err := e.reconcileScheduledTasks(finalCtx, entity, wf, finalTxID, auditStore, task.ID); err != nil {
		return OutcomeDropped, fmt.Errorf("failed to reconcile scheduled tasks after fire: %w", err)
	}

	// Anchor stamp: this Save/CompareAndSave is the fire's anchor write —
	// unlike every other write in the fire's cascade (which flows through
	// internal/domain/entity/service.go's spi.AttributionFor call sites and
	// so inherits the ambient origin seeded above automatically), this call
	// goes straight to the EntityStore and bypasses those stamp sites. Stamp
	// it explicitly here: attributed to the arming principal (cur.ArmedBy,
	// re-verified unchanged by the guard above), falling back to the system
	// principal for legacy rows that never recorded one — never the literal
	// string "scheduler". Executed by is always the system principal: the
	// scheduler, not the arming principal, is what actually performs the
	// fire.
	armed := cur.ArmedBy
	if armed == (spi.Principal{}) {
		armed = systemPrincipal
	}
	entity.Meta.ChangeUser = armed.ID
	entity.Meta.ChangeUserKind = armed.Kind
	entity.Meta.ChangeExecutor = systemPrincipal

	finalEntityStore, err := e.factory.EntityStore(finalCtx)
	if err != nil {
		return OutcomeDropped, fmt.Errorf("failed to get entity store for persist: %w", err)
	}
	if finalTxID != txID {
		// The cascade segmented via a COMMIT_BEFORE_DISPATCH processor,
		// which already consumed the ifMatch slot and applied the CAS
		// against expectedTxID at its first-segment flush (design §5.3).
		// A second CompareAndSave here would spuriously conflict against
		// the now-advanced stored TransactionID — plain Save persists the
		// post-cascade state, matching the *WithIfMatch handler's own
		// segmented branch (internal/domain/entity/service.go).
		if _, err := finalEntityStore.Save(finalCtx, entity); err != nil {
			return OutcomeDropped, fmt.Errorf("failed to save fired entity: %w", err)
		}
	} else {
		if _, err := finalEntityStore.CompareAndSave(finalCtx, entity, expectedTxID); err != nil {
			// A concurrent write raced the fire's read — safe to drop; the
			// next scan re-reads and re-guards from scratch.
			return OutcomeDropped, err
		}
	}

	e.recordEvent(auditStore, finalCtx, entity.Meta.ID, txID, entity.Meta.State,
		spi.SMEventScheduledTransitionFired,
		fmt.Sprintf("Scheduled transition %q fired", cur.Transition), nil)

	committed = true
	return OutcomeFired, e.txMgr.Commit(ctx, finalTxID)
}

// findTransitionInState returns the named transition from wf's given state,
// or nil if wf, the state, or the transition itself is absent.
func findTransitionInState(wf *spi.WorkflowDefinition, state, transitionName string) *spi.TransitionDefinition {
	if wf == nil {
		return nil
	}
	stateDef, ok := wf.States[state]
	if !ok {
		return nil
	}
	for i := range stateDef.Transitions {
		if stateDef.Transitions[i].Name == transitionName {
			return &stateDef.Transitions[i]
		}
	}
	return nil
}
