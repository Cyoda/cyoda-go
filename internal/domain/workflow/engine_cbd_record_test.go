package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
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

// An event holding U+0000 is recorded on no backend, durably or not, and the
// transaction opened for it commits nothing.
func TestCBDFailureAfterBoundary_RecordHoldingNUL_NotRecorded(t *testing.T) {
	for _, startNewTx := range []bool{false, true} {
		t.Run(fmt.Sprintf("startNewTxOnDispatch=%t", startNewTx), func(t *testing.T) {
			h := cbdRecordHarness(t, "memory", startNewTx)
			var commits []string
			h.txMgr.commit = func(ctx context.Context, txID string) error {
				commits = append(commits, txID)
				return h.txMgr.TransactionManager.Commit(ctx, txID)
			}
			h.dispatchProcessor = func(context.Context, *spi.Entity, spi.ProcessorDefinition, string, string, string) (*spi.Entity, error) {
				return nil, memberFailedWithText("card\x00declined")
			}
			entryTxID, entryCtx := h.begin(t)

			if _, err := h.engine.Execute(entryCtx, h.entity, ""); err == nil {
				t.Fatal("expected the processor's failure to fail the operation")
			}
			if got := eventsOfType(committedTrail(t, h), spi.SMEventStateProcessResult); len(got) != 0 {
				t.Errorf("committed STATE_PROCESS_RESULT records = %+v, want none", got)
			}
			if len(commits) != 1 || commits[0] != entryTxID {
				t.Errorf("commits = %v, want TX_pre's alone (%s)", commits, entryTxID)
			}
		})
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

// A chain the fence refuses performs no store operation for the durable
// record: no transaction is begun and nothing is recorded.
func TestRecordEventInOwnTx_SupersededChain_TouchesNoStore(t *testing.T) {
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	uuids := common.NewTestUUIDGenerator()
	cnt := &segCounter{}
	engine := NewEngine(factory, uuids, &countingTxManager{inner: factory.NewTransactionManager(uuids), c: cnt})
	ctx := ctxWithTenant(testTenant)

	f := fence.New(txgate.New())
	_, end := f.Begin(ctx, "outer", "", nil)
	defer end()
	f.Advance("outer", 1)
	admitted, err := f.Admit(ctx, []fence.Pair{{Callout: "outer", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	f.Advance("outer", 2)
	if fence.Check(admitted) == nil {
		t.Fatal("the chain is not superseded; the test proves nothing")
	}

	as, err := factory.StateMachineAuditStore(ctx)
	if err != nil {
		t.Fatalf("StateMachineAuditStore: %v", err)
	}
	engine.recordEventInOwnTx(as, admitted, "e1", "tx-entry", "A", spi.SMEventStateProcessResult,
		`Processor "charge" failed`, stateProcessResultData("charge", ExecutionModeCommitBeforeDispatch, false, "card declined"))

	if cnt.begins != 0 || cnt.commits != 0 {
		t.Errorf("begins = %d, commits = %d, want none", cnt.begins, cnt.commits)
	}
	events, err := as.GetEvents(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %+v, want none", events)
	}
}
