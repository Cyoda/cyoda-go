package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// cbdRecordUser is the request's principal in these tests: a user acting
// through an on-behalf-of client, so a record attributed to anyone else — the
// system, the client alone — is told apart.
func cbdRecordUser() *spi.UserContext {
	return &spi.UserContext{
		UserID: "alice", UserName: "alice", Kind: spi.PrincipalUser,
		Tenant:   spi.Tenant{ID: testTenant, Name: string(testTenant)},
		Roles:    []string{"USER"},
		Executor: &spi.Principal{ID: "OBOCLIENT0000001", Kind: spi.PrincipalService},
	}
}

// memberFailedWithText is the callout failure of a member that answered
// success=false with text.
func memberFailedWithText(text string) *contract.CalloutFailure {
	return &contract.CalloutFailure{Kind: contract.MemberFailed, Message: text, MemberText: text}
}

// cbdRecordHarness is a segment-guard harness whose one COMMIT_BEFORE_DISPATCH
// processor has the given startNewTxOnDispatch, run under cbdRecordUser.
func cbdRecordHarness(t *testing.T, backend string, startNewTx bool) *segmentGuardHarness {
	t.Helper()
	h := newSegmentGuardHarness(t, backend)
	h.ctx = spi.WithUserContext(context.Background(), cbdRecordUser())
	h.registerCBDProcessor("charge")
	h.wf.States["A"].Transitions[0].Processors[0].Config.StartNewTxOnDispatch = &startNewTx
	return h
}

// committedTrail is the entity's audit trail as any later reader sees it: read
// outside every transaction, so only what committed is in it.
func committedTrail(t *testing.T, h *segmentGuardHarness) []spi.StateMachineEvent {
	t.Helper()
	as, err := h.factory.StateMachineAuditStore(h.ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	events, err := as.GetEvents(h.ctx, h.entity.Meta.ID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	return events
}

func eventsOfType(events []spi.StateMachineEvent, typ spi.StateMachineEventType) []spi.StateMachineEvent {
	var out []spi.StateMachineEvent
	for _, ev := range events {
		if ev.EventType == typ {
			out = append(out, ev)
		}
	}
	return out
}

// writeOtherTransaction commits a write of the anchor by a transaction of its
// own, through the unwrapped manager.
func writeOtherTransaction(h *segmentGuardHarness) error {
	otherID, otherCtx, err := h.txMgr.TransactionManager.Begin(h.ctx)
	if err != nil {
		return err
	}
	es, err := h.factory.EntityStore(otherCtx)
	if err != nil {
		return err
	}
	cur, err := es.Get(otherCtx, h.entity.Meta.ID)
	if err != nil {
		return err
	}
	cur.Data = []byte(`{"x":9}`)
	if _, err := es.Save(otherCtx, cur); err != nil {
		return err
	}
	return h.txMgr.TransactionManager.Commit(h.ctx, otherID)
}

// A COMMIT_BEFORE_DISPATCH processor that fails after its segment boundary
// committed still has its result recorded: the committed trail holds exactly
// one STATE_PROCESS_RESULT for it, failed, with the member's own text and
// nothing else, under the cascade-entry transaction id and the request's
// principal, after the PAUSE_FOR_PROCESSING the boundary committed.
func TestCBDFailureAfterBoundary_ResultRecordedDurably(t *testing.T) {
	classes := []struct {
		name       string
		arm        func(t *testing.T, h *segmentGuardHarness, entryTxID *string)
		failReason string // "" = no failReason
	}{
		{"the member failed", func(_ *testing.T, h *segmentGuardHarness, _ *string) {
			h.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
				return nil, memberFailedWithText("card declined")
			}
		}, "card declined"},
		{"the apply-result CompareAndSave conflicts", func(_ *testing.T, h *segmentGuardHarness, _ *string) {
			h.failCompareAndSave()
		}, ""},
		{"another transaction writes the anchor once TX_pre committed", func(t *testing.T, h *segmentGuardHarness, entryTxID *string) {
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				if err := h.txMgr.TransactionManager.Commit(ctx, txID); err != nil {
					return err
				}
				if txID == *entryTxID {
					if err := writeOtherTransaction(h); err != nil {
						t.Errorf("other transaction: %v", err)
					}
				}
				return nil
			}
		}, ""},
	}
	for _, backend := range []string{"memory", "sqlite"} {
		for _, startNewTx := range []bool{false, true} {
			for _, c := range classes {
				t.Run(fmt.Sprintf("%s/startNewTxOnDispatch=%t/%s", backend, startNewTx, c.name), func(t *testing.T) {
					h := cbdRecordHarness(t, backend, startNewTx)
					var entryTxID string
					c.arm(t, h, &entryTxID)
					var entryCtx context.Context
					entryTxID, entryCtx = h.begin(t)

					if _, err := h.engine.Execute(entryCtx, h.entity, ""); err == nil {
						t.Fatal("expected the processor's failure to fail the operation")
					}

					trail := committedTrail(t, h)
					results := eventsOfType(trail, spi.SMEventStateProcessResult)
					if len(results) != 1 {
						t.Fatalf("committed STATE_PROCESS_RESULT records = %d, want 1; trail %+v", len(results), trail)
					}
					got := results[0]
					want := map[string]any{"processorName": "charge", "executionMode": ExecutionModeCommitBeforeDispatch, "success": false}
					if c.failReason != "" {
						want["failReason"] = c.failReason
					}
					if !reflect.DeepEqual(got.Data, want) {
						t.Errorf("data = %v, want %v", got.Data, want)
					}
					if got.Details != `Processor "charge" failed` {
						t.Errorf("details = %q", got.Details)
					}
					if got.TransactionID != entryTxID {
						t.Errorf("transactionId = %q, want the cascade-entry %q", got.TransactionID, entryTxID)
					}
					if got.State != "A" {
						t.Errorf("state = %q, want A", got.State)
					}
					wantAttributed, wantExecutor := spi.AttributionFor(h.ctx)
					if !reflect.DeepEqual(got.Attributed, wantAttributed) || !reflect.DeepEqual(got.Executor, wantExecutor) {
						t.Errorf("attribution = %+v / %+v, want the request's %+v / %+v", got.Attributed, got.Executor, wantAttributed, wantExecutor)
					}
					paused := eventsOfType(trail, spi.SMEventProcessingPaused)
					if len(paused) != 1 {
						t.Fatalf("committed PAUSE_FOR_PROCESSING records = %d, want 1", len(paused))
					}
					if got.Timestamp.Before(paused[0].Timestamp) {
						t.Errorf("result at %v precedes the pause at %v", got.Timestamp, paused[0].Timestamp)
					}
				})
			}
		}
	}
}

// A boundary that did not commit leaves nothing committed: the operation fails
// and its records roll back with TX_pre, the processor's result among them.
func TestCBDFailureBeforeCommit_NoRecord(t *testing.T) {
	cases := []struct {
		name string
		arm  func(h *segmentGuardHarness, entryTxID *string)
	}{
		{"the flush fails", func(h *segmentGuardHarness, _ *string) {
			h.flushSaveErr = errors.New("flush refused")
		}},
		{"the commit of TX_pre fails", func(h *segmentGuardHarness, entryTxID *string) {
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				if txID == *entryTxID {
					return errors.New("commit refused")
				}
				return h.txMgr.TransactionManager.Commit(ctx, txID)
			}
		}},
	}
	for _, startNewTx := range []bool{false, true} {
		for _, c := range cases {
			t.Run(fmt.Sprintf("startNewTxOnDispatch=%t/%s", startNewTx, c.name), func(t *testing.T) {
				h := cbdRecordHarness(t, "memory", startNewTx)
				var entryTxID string
				c.arm(h, &entryTxID)
				var entryCtx context.Context
				entryTxID, entryCtx = h.begin(t)

				if _, err := h.engine.Execute(entryCtx, h.entity, ""); err == nil {
					t.Fatal("expected the boundary's failure to fail the operation")
				}
				// What the entity service does with a failed operation.
				_ = h.txMgr.TransactionManager.Rollback(h.ctx, entryTxID)

				if h.dispatched {
					t.Fatal("the processor was dispatched across a boundary that did not commit")
				}
				if trail := committedTrail(t, h); len(trail) != 0 {
					t.Errorf("committed trail = %+v, want empty", trail)
				}
			})
		}
	}
}

// The joined-transaction refusal comes before the boundary: the record stays
// in the joined transaction, for its owner to commit, and the refused chain
// commits nothing of its own.
func TestCBDJoinedRefusal_RecordStaysInJoinedTransaction(t *testing.T) {
	for _, startNewTx := range []bool{false, true} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%t", startNewTx), func(t *testing.T) {
			h := cbdRecordHarness(t, "memory", startNewTx)
			var commits []string
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				commits = append(commits, txID)
				return h.txMgr.TransactionManager.Commit(ctx, txID)
			}
			entryTxID, ownerCtx := h.begin(t)

			if _, err := h.engine.Execute(MarkJoinedTransaction(ownerCtx), h.entity, ""); err == nil {
				t.Fatal("expected the joined chain's refusal")
			}
			if len(commits) != 0 {
				t.Fatalf("the refused chain committed %v", commits)
			}
			if got := eventsOfType(committedTrail(t, h), spi.SMEventStateProcessResult); len(got) != 0 {
				t.Fatalf("records committed before the owner did: %+v", got)
			}
			if err := h.txMgr.TransactionManager.Commit(ownerCtx, entryTxID); err != nil {
				t.Fatalf("owner commit: %v", err)
			}
			results := eventsOfType(committedTrail(t, h), spi.SMEventStateProcessResult)
			if len(results) != 1 || results[0].Data["success"] != false {
				t.Fatalf("committed STATE_PROCESS_RESULT records = %+v, want the one failed record", results)
			}
		})
	}
}

// The record's own transaction fails to commit: it is rolled back, nothing is
// recorded, and the operation fails with the processor's error as before.
func TestCBDFailureAfterBoundary_OwnTransactionCommitFails(t *testing.T) {
	for _, startNewTx := range []bool{false, true} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%t", startNewTx), func(t *testing.T) {
			h := cbdRecordHarness(t, "memory", startNewTx)
			var entryTxID string
			var refused []string
			// TX_post is rolled back on this path, never committed, so the
			// only commit besides TX_pre's is the record's own transaction.
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				if txID == entryTxID {
					return h.txMgr.TransactionManager.Commit(ctx, txID)
				}
				refused = append(refused, txID)
				return errors.New("commit refused")
			}
			h.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
				return nil, memberFailedWithText("card declined")
			}
			var entryCtx context.Context
			entryTxID, entryCtx = h.begin(t)

			_, err := h.engine.Execute(entryCtx, h.entity, "")
			if err == nil || err.Error() != "processor charge failed: card declined" {
				t.Fatalf("err = %v, want the processor's own failure", err)
			}
			if len(refused) != 1 {
				t.Fatalf("refused commits = %v, want the record's own transaction alone", refused)
			}
			if !h.txMgr.sawRollbackOf(refused[0]) {
				t.Errorf("the record's transaction %s was not rolled back", refused[0])
			}
			if got := eventsOfType(committedTrail(t, h), spi.SMEventStateProcessResult); len(got) != 0 {
				t.Errorf("committed STATE_PROCESS_RESULT records = %+v, want none", got)
			}
		})
	}
}

// The record is refused in its own transaction: that transaction is rolled
// back, not committed, nothing is recorded, and the operation fails with the
// processor's error as before.
func TestCBDFailureAfterBoundary_RecordRefused_OwnTransactionRolledBack(t *testing.T) {
	for _, startNewTx := range []bool{false, true} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%t", startNewTx), func(t *testing.T) {
			h := cbdRecordHarness(t, "memory", startNewTx)
			h.failAuditEvent = spi.SMEventStateProcessResult
			var commits []string
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				commits = append(commits, txID)
				return h.txMgr.TransactionManager.Commit(ctx, txID)
			}
			h.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
				return nil, memberFailedWithText("card declined")
			}
			entryTxID, entryCtx := h.begin(t)

			_, err := h.engine.Execute(entryCtx, h.entity, "")
			if err == nil || err.Error() != "processor charge failed: card declined" {
				t.Fatalf("err = %v, want the processor's own failure", err)
			}
			if len(h.refusedAuditTxIDs) != 1 || h.refusedAuditTxIDs[0] == entryTxID {
				t.Fatalf("refused records ran in %v, want one, in a transaction of its own", h.refusedAuditTxIDs)
			}
			if len(commits) != 1 || commits[0] != entryTxID {
				t.Errorf("commits = %v, want TX_pre's alone (%s)", commits, entryTxID)
			}
			if !h.txMgr.sawRollbackOf(h.refusedAuditTxIDs[0]) {
				t.Errorf("the record's transaction %s was not rolled back", h.refusedAuditTxIDs[0])
			}
			if got := eventsOfType(committedTrail(t, h), spi.SMEventStateProcessResult); len(got) != 0 {
				t.Errorf("committed STATE_PROCESS_RESULT records = %+v, want none", got)
			}
		})
	}
}

// A scheduled run that refuses the dispatch after the boundary committed — its
// unsafe mark refused — keeps the processor's record. The refusal is not the
// member's text, so the record has no failReason.
func TestCBDDispatchRefusedAfterBoundary_RecordKeptWithoutFailReason(t *testing.T) {
	for _, site := range unsafeAtEverySite() {
		if site.proc.ExecutionMode != ExecutionModeCommitBeforeDispatch {
			continue
		}
		t.Run(site.name, func(t *testing.T) {
			ext := &scriptedExtProc{}
			env := newRunEnv(t, ext)
			id := "refused-record-" + site.name
			claimed := env.claimed(t, id, oneHopWF("CLOSED", []spi.ProcessorDefinition{site.proc}, nil))
			store := &scriptedMarkStore{ScheduledTaskStore: env.sts, markErr: fmt.Errorf("fenced: %w", spi.ErrMarkedByAnotherClaim)}

			r := newTestRun(store, claimed).fire(env.engine, env.ctx, claimed)
			if r.Outcome != OutcomeFailed {
				t.Fatalf("report = %+v, want failed", r)
			}
			if n := ext.count("p1"); n != 0 {
				t.Fatalf("p1 dispatched %d times, want 0", n)
			}
			as, err := env.factory.StateMachineAuditStore(env.ctx)
			if err != nil {
				t.Fatalf("StateMachineAuditStore: %v", err)
			}
			events, err := as.GetEvents(env.ctx, id)
			if err != nil {
				t.Fatalf("GetEvents: %v", err)
			}
			results := eventsOfType(events, spi.SMEventStateProcessResult)
			if len(results) != 1 {
				t.Fatalf("committed STATE_PROCESS_RESULT records = %d, want 1: %+v", len(results), events)
			}
			want := map[string]any{"processorName": "p1", "executionMode": ExecutionModeCommitBeforeDispatch, "success": false}
			if !reflect.DeepEqual(results[0].Data, want) {
				t.Errorf("record data = %v, want %v", results[0].Data, want)
			}
		})
	}
}
