package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	spi "github.com/cyoda/cyoda-go-spi"
	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/platformclock"
	"github.com/cyoda/cyoda-go/internal/txcoord"
	"github.com/cyoda/cyoda-go/plugins/memory"
)

// engine_seal_test.go — every transaction the engine commits or rolls back is
// sealed first: a transaction a joined request left broken is never committed,
// and the coordination forgets the transaction afterwards.

// sealEnv is an engine on the memory plugin and one open transaction of it.
type sealEnv struct {
	e     *Engine
	tm    spi.TransactionManager
	f     *memory.StoreFactory
	ctx   context.Context // the tenant's, no transaction
	txID  string
	txCtx context.Context
}

func newSealEnv(t *testing.T) *sealEnv {
	t.Helper()
	f := memory.NewStoreFactory()
	t.Cleanup(func() { f.Close() })
	uuids := common.NewTestUUIDGenerator()
	tm := f.NewTransactionManager(uuids)
	ctx := ctxWithTenant(testTenant)
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return &sealEnv{e: NewEngine(cachedFactory(f), uuids, tm, platformclock.Real()), tm: tm, f: f, ctx: ctx, txID: txID, txCtx: txCtx}
}

// sessionEnded is a joined request's failure that met the transaction's ended
// session: its scope ends by the row that leaves the transaction broken.
func sessionEnded() *common.Outcome {
	return &common.Outcome{Status: http.StatusServiceUnavailable, Code: common.ErrCodeStorageUnavailable, Retryable: true,
		Err: fmt.Errorf("failed to save: %w", spi.ErrTxNotFound)}
}

// breakTx leaves the transaction broken as a joined request whose session
// ended does: a callback scope under a callout of the owner ends with that
// failure.
func (s *sealEnv) breakTx(t *testing.T) {
	t.Helper()
	_, end := s.e.coord.BeginCallout(s.txCtx, s.txID, "seal-callout")
	defer end()
	s.e.coord.Advance("seal-callout", 1)
	joined, err := s.tm.Join(s.ctx, s.txID)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	sc, _, err := s.e.coord.Enter(joined, s.txID, []txcoord.Pair{{Callout: "seal-callout", Major: 1}}, txcoord.Write, 0)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	sc.End(sessionEnded())
}

// assertForgotten asserts the coordination holds nothing of the transaction:
// a new seal is that of a transaction never seen, which a kept broken one
// would refuse.
func (s *sealEnv) assertForgotten(t *testing.T) {
	t.Helper()
	finish, err := s.e.coord.Seal(s.ctx, s.txID)
	finish()
	if err != nil {
		t.Fatalf("the coordination still holds the transaction: a new seal answered %v", err)
	}
}

// requireBroken asserts err is the seal's refusal of a transaction whose
// session ended.
func requireBroken(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("err = %v; want the broken transaction's cause (spi.ErrTxNotFound)", err)
	}
}

// TestEngine_FlushAndCommitSegment_SealsTxPre: TX_pre of a
// COMMIT_BEFORE_DISPATCH processor is sealed before its flush: a broken TX_pre
// is neither flushed nor committed, and the coordination forgets it.
func TestEngine_FlushAndCommitSegment_SealsTxPre(t *testing.T) {
	s := newSealEnv(t)
	s.breakTx(t)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{ID: "seal-e1", TenantID: testTenant, ModelRef: spi.ModelRef{EntityName: "SealModel", ModelVersion: "1"}, State: "S"},
		Data: []byte(`{"x":1}`),
	}

	err := s.e.flushAndCommitSegment(s.txCtx, entity, s.txID)

	requireBroken(t, err)
	s.assertForgotten(t)
	if err := s.tm.Rollback(s.ctx, s.txID); err != nil {
		t.Fatalf("TX_pre is not open for its rollback (was it committed?): %v", err)
	}
	es, err := s.f.EntityStore(s.ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Get(s.ctx, "seal-e1"); !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("Get after the refused segment = %v; want not found", err)
	}
}

// fireWithCallback runs a scheduled fire whose SYNC processor makes a callout
// with one callback that ends with o; the processor then answers success. It
// returns the fire's report and its transaction id.
func fireWithCallback(t *testing.T, entityID string, o *common.Outcome) (*runEnv, RunReport, string) {
	t.Helper()
	var env *runEnv
	var fireTxID string
	ext := &scriptedExtProc{processor: func(ctx context.Context, _ spi.ProcessorDefinition, txID string) (*spi.Entity, error) {
		fireTxID = txID
		coord := env.engine.coord
		_, end := coord.BeginCallout(ctx, txID, "fire-callout")
		defer end()
		coord.Advance("fire-callout", 1)
		joined, err := env.txMgr.Join(env.ctx, txID)
		if err != nil {
			return nil, err
		}
		sc, _, err := coord.Enter(joined, txID, []txcoord.Pair{{Callout: "fire-callout", Major: 1}}, txcoord.Write, 0)
		if err != nil {
			return nil, err
		}
		sc.End(o)
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	task := env.claimed(t, entityID, oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))
	report, _ := env.run(task)
	return env, report, fireTxID
}

// TestEngine_FireScheduled_BrokenTx_Fails: a callback of the fire's SYNC
// processor met the transaction's ended session, which leaves the
// transaction broken. The run seals before its final writes; the refusal is
// the fire's failure, nothing of the fire is committed, and the coordination
// forgets the transaction.
func TestEngine_FireScheduled_BrokenTx_Fails(t *testing.T) {
	env, report, fireTxID := fireWithCallback(t, "seal-fire-e1", sessionEnded())

	requireBroken(t, report.Err)
	if got := env.state(t, "seal-fire-e1"); got != "OPEN" {
		t.Fatalf("state = %q; want OPEN — the refused fire commits nothing", got)
	}
	finish, err := env.engine.coord.Seal(env.ctx, fireTxID)
	finish()
	if err != nil {
		t.Fatalf("the coordination still holds the fire's transaction: a new seal answered %v", err)
	}
}

// TestEngine_FireScheduled_FailedCallbackWrite_Commits: a callback of the
// fire's SYNC processor failed a write, and the processor answered success.
// The failed write kept nothing, and the fire commits.
func TestEngine_FireScheduled_FailedCallbackWrite_Commits(t *testing.T) {
	env, report, _ := fireWithCallback(t, "seal-fire-e2",
		&common.Outcome{Status: http.StatusBadRequest, Code: common.ErrCodeWorkflowFailed})

	if report.Err != nil {
		t.Fatalf("fire = %v; want it committed", report.Err)
	}
	if got := env.state(t, "seal-fire-e2"); got != "CLOSED" {
		t.Fatalf("state = %q; want CLOSED", got)
	}
}

// TestEngine_RollbackSegment_SealsAndForgets: a segment the engine rolls back
// is sealed first and forgotten after.
func TestEngine_RollbackSegment_SealsAndForgets(t *testing.T) {
	s := newSealEnv(t)
	s.breakTx(t)

	s.e.rollbackSegment(s.ctx, s.txID, "")

	s.assertForgotten(t)
	if err := s.tm.Rollback(s.ctx, s.txID); !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("second rollback = %v; want ErrTxNotFound — the segment was rolled back", err)
	}
}
