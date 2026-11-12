package workflow

import (
	"context"
	"errors"
	"net/http"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// engine_seal_test.go — every transaction the engine commits or rolls back is
// sealed first: a transaction a failed joined write made rollback-only is never
// committed, and the coordination forgets the transaction afterwards.

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
	return &sealEnv{e: NewEngine(f, uuids, tm, platformclock.Real()), tm: tm, f: f, ctx: ctx, txID: txID, txCtx: txCtx}
}

// markRoot makes the transaction rollback-only as a failed joined write does:
// a callback scope under a callout of the owner ends with a non-retryable
// failure.
func (s *sealEnv) markRoot(t *testing.T) {
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
	sc.End(&common.Outcome{Status: http.StatusBadRequest, Code: common.ErrCodeWorkflowFailed})
}

// assertForgotten asserts the coordination holds nothing of the transaction:
// a new seal is that of a transaction never seen, which a kept marked one
// would refuse.
func (s *sealEnv) assertForgotten(t *testing.T) {
	t.Helper()
	finish, err := s.e.coord.Seal(s.ctx, s.txID)
	finish()
	if err != nil {
		t.Fatalf("the coordination still holds the transaction: a new seal answered %v", err)
	}
}

func requireRollbackOnly(t *testing.T, err error) {
	t.Helper()
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeTransactionRollbackOnly {
		t.Fatalf("err = %v; want TRANSACTION_ROLLBACK_ONLY", err)
	}
}

// TestEngine_FlushAndCommitSegment_SealsTxPre: TX_pre of a
// COMMIT_BEFORE_DISPATCH processor is sealed before its flush: a rollback-only
// TX_pre is neither flushed nor committed, and the coordination forgets it.
func TestEngine_FlushAndCommitSegment_SealsTxPre(t *testing.T) {
	s := newSealEnv(t)
	s.markRoot(t)
	entity := &spi.Entity{
		Meta: spi.EntityMeta{ID: "seal-e1", TenantID: testTenant, ModelRef: spi.ModelRef{EntityName: "SealModel", ModelVersion: "1"}, State: "S"},
		Data: []byte(`{"x":1}`),
	}

	err := s.e.flushAndCommitSegment(s.txCtx, entity, s.txID)

	requireRollbackOnly(t, err)
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

// TestEngine_FireScheduled_RootMarked_Fails: a callback of the fire's SYNC
// processor failed a write. The run seals before its final writes; the
// refusal is the fire's failure, nothing of the fire is committed, and the
// coordination forgets the transaction.
func TestEngine_FireScheduled_RootMarked_Fails(t *testing.T) {
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
		sc.End(&common.Outcome{Status: http.StatusBadRequest, Code: common.ErrCodeWorkflowFailed})
		return nil, nil
	}}
	env = newRunEnv(t, ext)
	task := env.claimed(t, "seal-fire-e1", oneHopWF("CLOSED", []spi.ProcessorDefinition{safeProc("p1", ExecutionModeSync)}, nil))

	report, _ := env.run(task)

	requireRollbackOnly(t, report.Err)
	if got := env.state(t, "seal-fire-e1"); got != "OPEN" {
		t.Fatalf("state = %q; want OPEN — the refused fire commits nothing", got)
	}
	finish, err := env.engine.coord.Seal(env.ctx, fireTxID)
	finish()
	if err != nil {
		t.Fatalf("the coordination still holds the fire's transaction: a new seal answered %v", err)
	}
}

// TestEngine_RollbackSegment_SealsAndForgets: a segment the engine rolls back
// is sealed first and forgotten after.
func TestEngine_RollbackSegment_SealsAndForgets(t *testing.T) {
	s := newSealEnv(t)
	s.markRoot(t)

	s.e.rollbackSegment(s.ctx, s.txID, "")

	s.assertForgotten(t)
	if err := s.tm.Rollback(s.ctx, s.txID); !errors.Is(err, spi.ErrTxNotFound) {
		t.Fatalf("second rollback = %v; want ErrTxNotFound — the segment was rolled back", err)
	}
}
