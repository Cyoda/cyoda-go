package txcoord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	spi "github.com/cyoda/cyoda-go-spi"
	spitest "github.com/cyoda/cyoda-go-spi/spitest"
	"github.com/cyoda/cyoda-go/internal/common"
	"github.com/cyoda/cyoda-go/internal/fence"
	"github.com/cyoda/cyoda-go/internal/txcoord/txcoordtest"
	"github.com/cyoda/cyoda-go/plugins/memory"
)

// cluster is two pnodes, each with its own durable coordination, over one
// capable backend (the test double): the owner's pnode and another.
type cluster struct {
	t       *testing.T
	f       spi.StoreFactory
	tm      *recTM
	owner   Coordination
	other   Coordination
	userCtx context.Context
	txID    string
	txCtx   context.Context // the owner's chain
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	f := txcoordtest.NewFactory(memory.NewStoreFactory())
	t.Cleanup(func() { _ = f.Close() })
	co, err := f.(spi.TransactionCoordinationCapable).TransactionCoordinator(context.Background())
	if err != nil || co == nil {
		t.Fatalf("TransactionCoordinator = (%v, %v)", co, err)
	}
	inner, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tm := &recTM{TransactionManager: inner}
	userCtx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u1", Kind: spi.PrincipalUser, Tenant: spi.Tenant{ID: spi.TenantID("tenant-" + uuid.NewString())},
	})
	spitest.EnsureLockedModel(t, f, userCtx, spi.ModelRef{EntityName: "m", ModelVersion: "1"})
	txID, txCtx, err := tm.Begin(userCtx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })
	return &cluster{t: t, f: f, tm: tm, owner: NewDurable(co, tm), other: NewDurable(co, tm), userCtx: userCtx, txID: txID, txCtx: txCtx}
}

// enter opens a callback scope on pnode c, on a freshly joined context.
func (cl *cluster) enter(c Coordination, class RequestClass, pairs ...fence.Pair) (Scope, context.Context, error) {
	cl.t.Helper()
	joined, err := cl.tm.Join(cl.userCtx, cl.txID)
	if err != nil {
		cl.t.Fatalf("join: %v", err)
	}
	return c.Enter(joined, cl.txID, pairs, class, 0)
}

func (cl *cluster) mustEnter(c Coordination, class RequestClass, pairs ...fence.Pair) (Scope, context.Context) {
	cl.t.Helper()
	s, ctx, err := cl.enter(c, class, pairs...)
	if err != nil {
		cl.t.Fatalf("enter %v: %v", pairs, err)
	}
	return s, ctx
}

// save writes a new entity on ctx and returns its id.
func (cl *cluster) save(ctx context.Context) string {
	cl.t.Helper()
	es, err := cl.f.EntityStore(ctx)
	if err != nil {
		cl.t.Fatal(err)
	}
	id := uuid.NewString()
	data, _ := json.Marshal(map[string]string{"by": "durable-test"})
	if _, err := es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, ModelRef: spi.ModelRef{EntityName: "m", ModelVersion: "1"}}, Data: data}); err != nil {
		cl.t.Fatal(err)
	}
	return id
}

// commit seals on the owner's pnode and commits.
func (cl *cluster) commit() {
	cl.t.Helper()
	finish, err := cl.owner.Seal(cl.txCtx, cl.txID)
	defer finish()
	if err != nil {
		cl.t.Fatalf("Seal = %v", err)
	}
	if err := cl.tm.Commit(cl.txCtx, cl.txID); err != nil {
		cl.t.Fatalf("Commit = %v", err)
	}
}

func (cl *cluster) committed(id string) bool {
	cl.t.Helper()
	es, err := cl.f.EntityStore(cl.userCtx)
	if err != nil {
		cl.t.Fatal(err)
	}
	_, err = es.Get(cl.userCtx, id)
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		cl.t.Fatal(err)
	}
	return err == nil
}

// TestDurable_CallbackServedOnAnotherPnodeCommitsWithOwner: on a capable
// backend a callback is served by whichever pnode it reaches, and its write
// commits with the owner's transaction.
func TestDurable_CallbackServedOnAnotherPnodeCommitsWithOwner(t *testing.T) {
	cl := newCluster(t)
	root := cl.save(cl.txCtx)
	cctx, end := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	cl.owner.Advance("c1", 1)

	s, sctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	w := cl.save(sctx)
	mustOwnAnswer(t, s.End(nil))
	end()
	if err := Check(cctx); err != nil {
		t.Fatalf("Check on the callout's context = %v", err)
	}

	cl.commit()
	if !cl.committed(root) || !cl.committed(w) {
		t.Fatalf("committed: root %v, callback %v; want both", cl.committed(root), cl.committed(w))
	}
}

// TestDurable_FailedWriteOnAnotherPnodeOwnerCommits: a failed joined write
// served on another pnode is discarded and leaves the transaction usable: a
// later write under the same pass is served, the callout's context reports
// nothing, and the owner commits the later write and none of the failed one.
func TestDurable_FailedWriteOnAnotherPnodeOwnerCommits(t *testing.T) {
	cl := newCluster(t)
	cctx, end := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	cl.owner.Advance("c1", 1)

	s, sctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	lost := cl.save(sctx)
	mustOwnAnswer(t, s.End(failed(http.StatusBadRequest, common.ErrCodeBadRequest)))
	s2, s2ctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	kept := cl.save(s2ctx)
	mustOwnAnswer(t, s2.End(nil))
	end()
	if err := Check(cctx); err != nil {
		t.Fatalf("Check on the callout's context = %v; want nil", err)
	}

	cl.commit()
	if !cl.committed(kept) || cl.committed(lost) {
		t.Fatalf("committed: later write %v, failed write %v; want true, false", cl.committed(kept), cl.committed(lost))
	}
}

// TestDurable_DiscardFailureFailsSeal: a scope the backend could not discard
// is answered 500, and the owner's Seal fails, so the transaction does not
// commit with the scope's writes possibly still in it.
func TestDurable_DiscardFailureFailsSeal(t *testing.T) {
	cl := newCluster(t)
	_, end := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	cl.owner.Advance("c1", 1)

	s, sctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	cl.save(sctx)
	discards := cl.f.(interface{ FailDiscards(error) })
	discards.FailDiscards(errors.New("the coordinator's store is unreachable"))
	mustAnswer(t, s.End(failed(http.StatusBadRequest, common.ErrCodeBadRequest)),
		http.StatusInternalServerError, common.ErrCodeServerError, false)
	discards.FailDiscards(nil)
	end()

	finish, err := cl.owner.Seal(cl.txCtx, cl.txID)
	finish()
	if err == nil {
		t.Fatal("Seal after a failed discard = nil; want a refusal")
	}
}

// TestDurable_ProcessorDiscardFailureFailsSeal: a processor scope the backend
// could not discard ends with ErrScopeInfra, and the owner's Seal fails.
func TestDurable_ProcessorDiscardFailureFailsSeal(t *testing.T) {
	cl := newCluster(t)
	p, pctx, err := cl.owner.OpenChild(cl.txCtx)
	if err != nil {
		t.Fatal(err)
	}
	cl.save(pctx)
	discards := cl.f.(interface{ FailDiscards(error) })
	discards.FailDiscards(errors.New("the coordinator's store is unreachable"))
	if err := p.Close(errors.New("processor failed")); !errors.Is(err, ErrScopeInfra) {
		t.Fatalf("Close = %v; want ErrScopeInfra", err)
	}
	discards.FailDiscards(nil)

	finish, err := cl.owner.Seal(cl.txCtx, cl.txID)
	finish()
	if err == nil {
		t.Fatal("Seal after a failed discard = nil; want a refusal")
	}
}

// TestDurable_AdvanceSupersedesInProgressCallback: a callback still in
// progress when its callout advances is discarded and answered 410; the
// advance does not wait for it.
func TestDurable_AdvanceSupersedesInProgressCallback(t *testing.T) {
	cl := newCluster(t)
	_, end := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	defer end()
	cl.owner.Advance("c1", 1)

	s, sctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	w := cl.save(sctx)
	mustFinish(t, "Advance", async(func() { cl.owner.Advance("c1", 2) }))
	mustSuperseded(t, sctx)
	ans := s.End(nil)
	mustAnswer(t, ans, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	if !errors.Is(ans, ErrSuperseded) {
		t.Errorf("the 410 does not carry ErrSuperseded: %v", ans)
	}
	end()
	cl.commit()
	if cl.committed(w) {
		t.Fatal("a superseded callback's write must not commit")
	}
}

// TestDurable_EnterRefusals: a pass that is not current is refused 410, and a
// sealed transaction 410.
func TestDurable_EnterRefusals(t *testing.T) {
	cl := newCluster(t)
	_, end := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	defer end()
	cl.owner.Advance("c1", 2)

	_, _, err := cl.enter(cl.other, Write, pair("c1", 1))
	mustCode(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
	if !errors.Is(err, ErrSuperseded) {
		t.Errorf("the 410 does not carry ErrSuperseded: %v", err)
	}

	finish, _ := cl.owner.Seal(cl.txCtx, cl.txID)
	finish()
	_, _, err = cl.enter(cl.other, Read, pair("c1", 2))
	mustCode(t, err, http.StatusGone, common.ErrCodeCalloutSuperseded, false)
}

// TestDurable_NestedCalloutSupersededAtAdvance: a callout begun from a
// callback scope stops at its next Advance once that scope's pass is no
// longer current.
func TestDurable_NestedCalloutSupersededAtAdvance(t *testing.T) {
	cl := newCluster(t)
	_, endC1 := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	defer endC1()
	cl.owner.Advance("c1", 1)

	s, sctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	defer s.End(nil)
	nctx, endN := cl.other.BeginCallout(sctx, cl.txID, "n1")
	defer endN()
	if got := Pairs(nctx); len(got) != 1 || got[0] != pair("c1", 1) {
		t.Fatalf("Pairs on the nested callout's context = %v; want the callback's", got)
	}
	cl.other.Advance("n1", 1)
	if nctx.Err() != nil {
		t.Fatalf("the nested callout ended while its scope is current: %v", context.Cause(nctx))
	}

	cl.owner.Advance("c1", 2)
	cl.other.Advance("n1", 2)
	if !errors.Is(context.Cause(nctx), ErrSuperseded) {
		t.Fatalf("cause = %v; want ErrSuperseded", context.Cause(nctx))
	}
}

// TestDurable_PauseIsCheck: there is no gate to let go; resume reports the
// scope's standing.
func TestDurable_PauseIsCheck(t *testing.T) {
	cl := newCluster(t)
	_, end := cl.owner.BeginCallout(cl.txCtx, cl.txID, "c1")
	defer end()
	cl.owner.Advance("c1", 1)

	s, sctx := cl.mustEnter(cl.other, Write, pair("c1", 1))
	defer s.End(nil)
	resume := Pause(sctx)
	if err := resume(); err != nil {
		t.Fatalf("resume = %v; want nil", err)
	}
	// A sibling is admitted while the first is paused: no stack rule.
	s2, _ := cl.mustEnter(cl.other, Write, pair("c1", 1))
	mustOwnAnswer(t, s2.End(nil))

	cl.owner.Advance("c1", 2)
	mustCode(t, Pause(sctx)(), http.StatusGone, common.ErrCodeCalloutSuperseded, false)
}

// TestDurable_ProcessorScope: an ASYNC_NEW_TX processor's scope keeps its
// writes when the processor succeeds, and loses them when it fails.
func TestDurable_ProcessorScope(t *testing.T) {
	cl := newCluster(t)
	p1, p1ctx, err := cl.owner.OpenChild(cl.txCtx)
	if err != nil {
		t.Fatal(err)
	}
	kept := cl.save(p1ctx)
	if err := p1.Close(nil); err != nil {
		t.Fatalf("Close(nil) = %v", err)
	}

	p2, p2ctx, err := cl.owner.OpenChild(cl.txCtx)
	if err != nil {
		t.Fatal(err)
	}
	lost := cl.save(p2ctx)
	procErr := errors.New("processor failed")
	if err := p2.Close(procErr); !errors.Is(err, procErr) {
		t.Fatalf("Close(procErr) = %v; want procErr", err)
	}

	cl.commit()
	if !cl.committed(kept) || cl.committed(lost) {
		t.Fatalf("committed: kept %v, lost %v; want true, false", cl.committed(kept), cl.committed(lost))
	}
}

// --- the mapping of the coordinator's refusals, on a scripted coordinator ---

// scripted is a coordinator whose CheckScope and CompleteScope answer as the
// test sets, and that counts the DiscardScope calls.
type scripted struct {
	spi.TransactionCoordinator // unused methods panic

	open, check, complete, discard error

	mu       sync.Mutex
	discards int
}

// OpenScope answers (nil, open) when the test set open, as a coordinator may.
func (s *scripted) OpenScope(ctx context.Context, _ string, _ []spi.CalloutPair) (context.Context, error) {
	if s.open != nil {
		return nil, s.open
	}
	return ctx, nil
}

// Seal fails on a cancelled context, as a backend's store call does.
func (s *scripted) Seal(ctx context.Context, _ string) error { return ctx.Err() }
func (s *scripted) CheckScope(context.Context) error         { return s.check }
func (s *scripted) CompleteScope(context.Context) error      { return s.complete }
func (s *scripted) DiscardScope(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
	return s.discard
}

func (s *scripted) discarded() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discards
}

// TestDurableEnd_MapsCompleteRefusals: a refused CompleteScope maps by its
// error, as durableScope.complete maps it. A refusal the backend names has
// discarded the scope already; any other error is a failed completion, which
// discards it.
func TestDurableEnd_MapsCompleteRefusals(t *testing.T) {
	storage := errors.New("the coordinator's store is unreachable")
	for _, tc := range []struct {
		name      string
		complete  error
		class     RequestClass
		lost      bool
		status    int
		code      string
		retryable bool
		discards  int // the DiscardScope calls
	}{
		{"superseded is row 1", spi.ErrCalloutSuperseded, Write, false, http.StatusGone, common.ErrCodeCalloutSuperseded, false, 0},
		{"scope conflict is a retryable conflict", spi.ErrScopeConflict, Write, false, http.StatusConflict, common.ErrCodeConflict, true, 0},
		{"any other error on a write is row 6", storage, Write, false, http.StatusInternalServerError, common.ErrCodeServerError, false, 1},
		{"any other error on a read is row 8", storage, Read, false, http.StatusInternalServerError, common.ErrCodeServerError, false, 1},
		{"any other error after a lost race is row 2", storage, Write, true, http.StatusConflict, common.ErrCodeConflict, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := memory.NewStoreFactory()
			t.Cleanup(func() { _ = f.Close() })
			inner, err := f.TransactionManager(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tm := &recTM{TransactionManager: inner}
			tm.lost.Store(tc.lost)
			co := &scripted{complete: tc.complete}
			s, _, err := NewDurable(co, tm).Enter(context.Background(), "tx-1", []fence.Pair{pair("c1", 1)}, tc.class, 0)
			if err != nil {
				t.Fatal(err)
			}

			ans := s.End(nil)
			mustAnswer(t, ans, tc.status, tc.code, tc.retryable)
			if got := co.discarded(); got != tc.discards {
				t.Errorf("DiscardScope called %d times; want %d", got, tc.discards)
			}
			if tc.complete == spi.ErrCalloutSuperseded && !errors.Is(ans, ErrSuperseded) {
				t.Errorf("the 410 does not carry ErrSuperseded: %v", ans)
			}
		})
	}
}

// TestDurableEnd_DiscardFailureIsAFailure: a scope the coordinator could not
// discard is not answered as if it were: the request gets a 500.
func TestDurableEnd_DiscardFailureIsAFailure(t *testing.T) {
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })
	inner, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	co := &scripted{discard: errors.New("the coordinator's store is unreachable")}
	s, _, err := NewDurable(co, &recTM{TransactionManager: inner}).Enter(context.Background(), "tx-1", []fence.Pair{pair("c1", 1)}, Write, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustAnswer(t, s.End(failed(http.StatusBadRequest, common.ErrCodeBadRequest)), http.StatusInternalServerError, common.ErrCodeServerError, false)
}

// newScripted returns the durable coordination over co, with a memory
// transaction manager whose LostRace answers lost.
func newScripted(t *testing.T, co *scripted, lost bool) Coordination {
	t.Helper()
	f := memory.NewStoreFactory()
	t.Cleanup(func() { _ = f.Close() })
	inner, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tm := &recTM{TransactionManager: inner}
	tm.lost.Store(lost)
	return NewDurable(co, tm)
}

// TestDurableEnter_OpenRefusals: a scope the coordinator does not open is
// refused by its error, whatever context the coordinator returned with it.
func TestDurableEnter_OpenRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		open   error
		status int
		code   string
	}{
		{"superseded", spi.ErrCalloutSuperseded, http.StatusGone, common.ErrCodeCalloutSuperseded},
		{"transaction not found", spi.ErrTxNotFound, http.StatusNotFound, common.ErrCodeTransactionNotFound},
		{"anything else", errors.New("the coordinator's store is unreachable"), http.StatusInternalServerError, common.ErrCodeServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newScripted(t, &scripted{open: tc.open}, false)
			s, _, err := c.Enter(context.Background(), "tx-1", []fence.Pair{pair("c1", 1)}, Write, 0)
			if s != nil {
				t.Fatal("a refused Enter returned a scope")
			}
			mustCode(t, err, tc.status, tc.code, false)
		})
	}
}

// TestDurableEnd_UnnamedCheckFailureIsAFailure: a CheckScope error the
// coordinator does not name is a failed completion: 500, and the scope
// discarded.
func TestDurableEnd_UnnamedCheckFailureIsAFailure(t *testing.T) {
	for _, class := range []RequestClass{Write, Read} {
		co := &scripted{check: errors.New("the coordinator's store is unreachable")}
		s, _, err := newScripted(t, co, false).Enter(context.Background(), "tx-1", []fence.Pair{pair("c1", 1)}, class, 0)
		if err != nil {
			t.Fatal(err)
		}
		mustAnswer(t, s.End(nil), http.StatusInternalServerError, common.ErrCodeServerError, false)
		if co.discarded() != 1 {
			t.Errorf("class %d: DiscardScope called %d times; want 1", class, co.discarded())
		}
	}
}

// TestDurableEnd_OutsideCompletionFailureIsLoggedNotAnswered: the answer of a
// request that writes outside the transaction is never rewritten; a refused
// completion of its scope is logged at WARN.
func TestDurableEnd_OutsideCompletionFailureIsLoggedNotAnswered(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	co := &scripted{complete: errors.New("the coordinator's store is unreachable")}
	s, _, err := newScripted(t, co, false).Enter(context.Background(), "tx-1", []fence.Pair{pair("c1", 1)}, Outside, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustOwnAnswer(t, s.End(nil))
	if line := buf.String(); !strings.Contains(line, "level=WARN") || !strings.Contains(line, "unreachable") {
		t.Fatalf("log = %q; want one WARN line with the completion's error", line)
	}
}

// TestDurableSeal_DetachedFromCancellation: the owner seals on the rollback
// path too, where its own context may already be cancelled; the backend is
// asked on a context its cancellation does not reach.
func TestDurableSeal_DetachedFromCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finish, err := newScripted(t, &scripted{}, false).Seal(ctx, "tx-1")
	finish()
	if err != nil {
		t.Fatalf("Seal on a cancelled context = %v; want nil", err)
	}
}
