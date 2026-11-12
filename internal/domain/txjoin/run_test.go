package txjoin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type runEnv struct {
	ctx     context.Context
	factory *memory.StoreFactory
	txMgr   spi.TransactionManager
	coord   txcoord.Coordination
	signer  *token.Signer
	joiner  *Joiner
	txID    string
	txCtx   context.Context
	end     func()
}

// newRunEnv opens a transaction on the memory backend with one callout in
// progress on it, and seeds two committed entities to read.
func newRunEnv(t *testing.T) (*runEnv, string) {
	t.Helper()
	return newRunEnvCapped(t, testMaxWaiters)
}

// newRunEnvCapped is newRunEnv with the queue cap the test wants to reach.
func newRunEnvCapped(t *testing.T, maxWaiters int) (*runEnv, string) {
	t.Helper()
	ctx := spi.WithUserContext(context.Background(), &spi.UserContext{
		UserID: "u", Tenant: spi.Tenant{ID: "run-tenant", Name: "run"}, Roles: []string{"user"},
	})
	factory := memory.NewStoreFactory()
	t.Cleanup(func() { factory.Close() })
	txMgr, err := factory.TransactionManager(ctx)
	if err != nil {
		t.Fatalf("TransactionManager: %v", err)
	}
	es, _ := factory.EntityStore(ctx)
	ref := spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}
	for _, id := range []string{"e-1", "e-2"} {
		if _, err := es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "run-tenant", ModelRef: ref, State: "S"}, Data: []byte(`{"n":1}`)}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	signer, _ := token.NewSigner(make32(t))
	coord := txcoord.New(txMgr)
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, end := coord.BeginCallout(txCtx, txID, "req-1")
	t.Cleanup(end)
	coord.Advance("req-1", 1)
	pass, _ := signer.Issue(token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	joiner, err := NewJoiner(signer, txMgr, coord, testResponseMax, maxWaiters, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return &runEnv{ctx: ctx, factory: factory, txMgr: txMgr, coord: coord, signer: signer,
		joiner: joiner, txID: txID, txCtx: txCtx, end: end}, pass
}

// hold runs a joined read under pass on a goroutine and keeps it in its
// handler — holding the transaction's gate — until the returned func is
// called; that func waits for the request to finish.
func (env *runEnv) hold(t *testing.T, pass string) func() {
	t.Helper()
	inHandler := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := env.joiner.Run(env.ctx, pass, txcoord.Read, func(context.Context) *common.Outcome {
			close(inHandler)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("the holding request was refused: %v", err)
			close(inHandler)
		}
	}()
	waitClosed(t, "the holding request to take the gate", inHandler)
	var once sync.Once
	return func() {
		once.Do(func() { close(release) })
		waitClosed(t, "the holding request to finish", done)
	}
}

func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// writeIn saves id in the transaction ctx carries.
func (env *runEnv) writeIn(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	es, err := env.factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "run-tenant",
		ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}, State: "S"}, Data: []byte(`{"n":3}`)}); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

// committed reports which of ids exist once the transaction has committed.
func (env *runEnv) commitAndRead(t *testing.T, ids ...string) map[string]bool {
	t.Helper()
	if err := env.txMgr.Commit(env.txCtx, env.txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, _ := env.factory.EntityStore(env.ctx)
	found := make(map[string]bool, len(ids))
	for _, id := range ids {
		_, err := es.Get(env.ctx, id)
		found[id] = err == nil
	}
	return found
}

// A compute member's callbacks queue for the transaction one behind another,
// and the queue is bounded: past the cap the callback is refused, retryably,
// rather than parked holding its request for the life of the callout. The
// holder and the callback already queued are unaffected.
func TestRunVerified_PastTheWaiterCap_IsRefusedRetryably(t *testing.T) {
	env, pass := newRunEnvCapped(t, 1)
	verified, err := env.joiner.Verify(pass)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	holder := env.hold(t, pass) // a callback already has the transaction
	defer holder()

	ran := make(chan struct{})
	queued := make(chan error, 1)
	go func() {
		_, err := env.joiner.RunVerified(env.ctx, verified, txcoord.Read, func(context.Context) *common.Outcome { close(ran); return nil })
		queued <- err
	}()
	waitForCapacity(t, env.coord, env.txID, 1)

	_, err = env.joiner.RunVerified(env.ctx, verified, txcoord.Read, func(context.Context) *common.Outcome {
		t.Error("the refused callback's handler ran")
		return nil
	})
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("RunVerified = %v; want an operational refusal", err)
	}
	if appErr.Status != http.StatusServiceUnavailable || appErr.Code != common.ErrCodeTooManyJoinedRequests {
		t.Errorf("refusal = %d %s; want 503 %s", appErr.Status, appErr.Code, common.ErrCodeTooManyJoinedRequests)
	}
	if !appErr.Retryable {
		t.Error("the refusal is not retryable; the queue drains, so it is")
	}

	holder()
	select {
	case qerr := <-queued:
		if qerr != nil {
			t.Fatalf("the callback already queued was refused: %v", qerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the callback already queued never ran")
	}
	<-ran
}

// waitForCapacity waits until maxWaiters callers are queued for txID.
func waitForCapacity(t *testing.T, coord txcoord.Coordination, txID string, maxWaiters int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if coord.AtCapacity(txID, maxWaiters) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d callers to queue for %s", maxWaiters, txID)
}

// Two parallel joined reads, and reads against a joined write, on one
// transaction. On the memory backend a Get writes tx.ReadSet under a READ lock
// (plugins/memory/entity_store.go Get): without the join layer's gate this test
// dies with the runtime's "concurrent map writes" (and is a data race under
// -race).
func TestRun_ParallelJoinedRequestsAreSerialised(t *testing.T) {
	env, pass := newRunEnv(t)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			class := txcoord.Read
			if w == 0 { // one writer among the readers
				class = txcoord.Write
			}
			for i := range 200 {
				_, err := env.joiner.Run(env.ctx, pass, class, func(ctx context.Context) *common.Outcome {
					es, err := env.factory.EntityStore(ctx)
					if err != nil {
						t.Errorf("EntityStore: %v", err)
						return nil
					}
					if w == 0 {
						_, err = es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "w-" + string(rune('a'+i%26)), TenantID: "run-tenant",
							ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}, State: "S"}, Data: []byte(`{"n":2}`)})
					} else {
						_, err = es.Get(ctx, []string{"e-1", "e-2"}[i%2])
					}
					if err != nil {
						t.Errorf("store: %v", err)
					}
					return nil
				})
				if err != nil {
					t.Errorf("Run: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The handler runs holding the transaction's gate: another joined request of
// the same compute node waits until it has returned.
func TestRun_HandlerHoldsTheGate(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.hold(t, pass)
	second := make(chan struct{})
	go func() {
		defer close(second)
		if _, err := env.joiner.Run(env.ctx, pass, txcoord.Read, func(context.Context) *common.Outcome { return nil }); err != nil {
			t.Errorf("the second request was refused: %v", err)
		}
	}()
	select {
	case <-second:
		t.Fatal("a second joined request ran while the first one held the gate")
	case <-time.After(50 * time.Millisecond):
	}
	holder()
	waitClosed(t, "the second request", second)
}

// A joined request queued for the gate when its cnode is replaced is refused on
// taking the gate: its handler never runs, nothing is written or read. The
// request in progress when the number rose is discarded.
func TestRun_QueuedRequestIsRefusedOnTakingTheGate(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.hold(t, pass) // another request of the same cnode, in progress

	ran := false
	var runErr error
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		_, runErr = env.joiner.Run(env.ctx, pass, txcoord.Read, func(context.Context) *common.Outcome { ran = true; return nil })
	}()
	waitForCapacity(t, env.coord, env.txID, 1) // admitted on entry, now queued for the gate

	advanced := make(chan struct{})
	go func() { defer close(advanced); env.coord.Advance("req-1", 2) }()
	select {
	case <-advanced:
		t.Fatal("Advance returned while a request of the earlier number was in progress")
	case <-time.After(50 * time.Millisecond):
	}
	holder()

	waitClosed(t, "the queued request", queued)
	waitClosed(t, "Advance", advanced)
	if ran {
		t.Fatal("the handler of a superseded request ran")
	}
	var appErr *common.AppError
	if !errors.As(runErr, &appErr) || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want CALLOUT_SUPERSEDED", runErr)
	}
}

// A joined request still in its handler when its callout ends is discarded
// whole — however many items it wrote — and answered 410: the owner has moved
// on, and the compute node must not be told its work was kept. The callout's
// end waits for it.
func TestRun_ARequestInProgressWhenItsCalloutEndsIsDiscarded(t *testing.T) {
	env, pass := newRunEnv(t)
	inHandler := make(chan struct{})
	finish := make(chan struct{})
	var rewrite *common.AppError
	var runErr error
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		rewrite, runErr = env.joiner.Run(env.ctx, pass, txcoord.Write, func(ctx context.Context) *common.Outcome {
			close(inHandler)
			<-finish
			for _, id := range []string{"c-1", "c-2", "c-3"} { // a joined collection
				env.writeIn(t, ctx, id)
			}
			return nil
		})
	}()
	<-inHandler

	ownerProceeds := make(chan struct{})
	go func() { defer close(ownerProceeds); env.end() }() // the callout ends: answered, failed or abandoned
	select {
	case <-ownerProceeds:
		t.Fatal("the owner proceeded while a joined request was in progress")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	waitClosed(t, "the callout's end", ownerProceeds)
	waitClosed(t, "the request", requestDone)
	if runErr != nil {
		t.Fatalf("the handler ran; err = %v", runErr)
	}
	if rewrite == nil || rewrite.Status != http.StatusGone || rewrite.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("rewrite = %v; want 410 CALLOUT_SUPERSEDED", rewrite)
	}
	for id, found := range env.commitAndRead(t, "c-1", "c-2", "c-3") {
		if found {
			t.Errorf("%s was committed: a superseded request's writes must be discarded", id)
		}
	}
}

// Review Focus 3: a handler that panics in a joined write ends its scope
// discarded, marks the parent, and gives the gate back; the panic passes on
// to the recovery layer outside the join layer. The next request of the
// transaction takes the gate at once and is refused 409
// TRANSACTION_ROLLBACK_ONLY. A read that panics marks nothing: the next
// request runs.
func TestRunVerified_PanicDiscardsAndReleasesGate(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		env, pass := newRunEnv(t)
		verified, _ := env.joiner.Verify(pass)
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic must pass through RunVerified to the recovery layer")
				}
			}()
			_, _ = env.joiner.RunVerified(env.ctx, verified, txcoord.Write, func(ctx context.Context) *common.Outcome {
				env.writeIn(t, ctx, "p-1")
				panic("handler blew up")
			})
		}()
		next := make(chan error, 1)
		go func() {
			_, err := env.joiner.RunVerified(env.ctx, verified, txcoord.Read, func(context.Context) *common.Outcome {
				t.Error("a request under a marked scope ran")
				return nil
			})
			next <- err
		}()
		select {
		case err := <-next:
			var appErr *common.AppError
			if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict || appErr.Code != common.ErrCodeTransactionRollbackOnly || appErr.Retryable {
				t.Fatalf("next request = %v; want 409 TRANSACTION_ROLLBACK_ONLY, not retryable", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a panicking joined handler kept the transaction's gate")
		}
		if env.commitAndRead(t, "p-1")["p-1"] {
			t.Error("the write of a request that panicked was kept")
		}
	})
	t.Run("read", func(t *testing.T) {
		env, pass := newRunEnv(t)
		verified, _ := env.joiner.Verify(pass)
		func() {
			defer func() { _ = recover() }()
			_, _ = env.joiner.RunVerified(env.ctx, verified, txcoord.Read, func(context.Context) *common.Outcome {
				panic("read blew up")
			})
		}()
		ran := false
		done := make(chan error, 1)
		go func() {
			_, err := env.joiner.RunVerified(env.ctx, verified, txcoord.Write, func(ctx context.Context) *common.Outcome {
				ran = true
				env.writeIn(t, ctx, "n-1")
				return nil
			})
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil || !ran {
				t.Fatalf("the next request: err=%v ran=%v; want it to run", err, ran)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a panicking joined handler kept the transaction's gate")
		}
		if !env.commitAndRead(t, "n-1")["n-1"] {
			t.Error("the next request's write was not kept")
		}
	})
}

// The outcome a funnel records wins over the door's backstop, and decides how
// the scope ends: a write answered with a non-retryable failure is discarded,
// and its parent marked.
func TestRunVerified_RecordedOutcomeDecides(t *testing.T) {
	env, pass := newRunEnv(t)
	verified, _ := env.joiner.Verify(pass)
	rewrite, err := env.joiner.RunVerified(env.ctx, verified, txcoord.Write, func(ctx context.Context) *common.Outcome {
		env.writeIn(t, ctx, "f-1")
		common.RecordOutcome(ctx, common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, "the processor failed"))
		return &common.Outcome{Status: http.StatusOK} // a backstop that would read as success
	})
	if err != nil || rewrite != nil {
		t.Fatalf("RunVerified = %v, %v; want the handler's own answer", rewrite, err)
	}
	_, err = env.joiner.RunVerified(env.ctx, verified, txcoord.Read, func(context.Context) *common.Outcome { return nil })
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeTransactionRollbackOnly {
		t.Fatalf("a request after a failed write = %v; want 409 TRANSACTION_ROLLBACK_ONLY", err)
	}
	if env.commitAndRead(t, "f-1")["f-1"] {
		t.Error("the failed write was kept")
	}
}

// The backstop decides when no funnel recorded anything: a 400-or-above
// answer is a failure, not retryable.
func TestRunVerified_BackstopFailureDiscards(t *testing.T) {
	env, pass := newRunEnv(t)
	verified, _ := env.joiner.Verify(pass)
	_, err := env.joiner.RunVerified(env.ctx, verified, txcoord.Write, func(ctx context.Context) *common.Outcome {
		env.writeIn(t, ctx, "b-1")
		return &common.Outcome{Status: http.StatusRequestEntityTooLarge}
	})
	if err != nil {
		t.Fatalf("RunVerified: %v", err)
	}
	if env.commitAndRead(t, "b-1")["b-1"] {
		t.Error("a write the door answered 413 was kept")
	}
}

// A joined request still queued for the transaction's gate when its client
// goes away returns at once and never runs: it has touched nothing, so there
// is nothing to protect, and the handler goroutine must not stay parked.
func TestRun_AWaiterWhoseClientGoesAwayNeverRuns(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.hold(t, pass) // another request of the same compute node
	defer holder()

	request, disconnect := context.WithCancel(env.ctx)
	ran := false
	returned := make(chan error, 1)
	go func() {
		_, err := env.joiner.Run(request, pass, txcoord.Read, func(context.Context) *common.Outcome { ran = true; return nil })
		returned <- err
	}()
	waitForCapacity(t, env.coord, env.txID, 1) // admitted on entry, now queued for the gate

	disconnect()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; want context.Canceled", err)
		}
		// The join layer is the one place that knows this error IS the client's
		// departure rather than an internal failure that happens to carry a
		// cancellation, so it says so. A door's error funnel files a departed
		// client by this marker alone: guessing from context.Canceled on the
		// chain would quieten a genuine fault to DEBUG with no ticket.
		if !errors.Is(err, common.ErrClientGone) {
			t.Fatalf("err = %v; want it marked common.ErrClientGone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a joined request whose client went away stayed parked on the gate")
	}
	if ran {
		t.Fatal("the handler of a request whose client had gone ran")
	}
}

// A compute node that disconnects in the middle of its callback cancels the
// request's context; a joined request that holds the transaction's gate must
// not see it — its statements run on the operation's connection.
func TestRun_AJoinedRequestHoldingTheGateIsNotCancelledByItsClient(t *testing.T) {
	env, pass := newRunEnv(t)
	request, disconnect := context.WithCancel(env.ctx)
	defer disconnect()

	var joined context.Context
	_, err := env.joiner.Run(request, pass, txcoord.Read, func(ctx context.Context) *common.Outcome {
		joined = ctx
		disconnect() // the compute node goes away mid-callback
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			t.Errorf("EntityStore: %v", err)
			return nil
		}
		if _, err := es.Get(ctx, "e-1"); err != nil {
			t.Errorf("a statement of a joined request was cut off by its client: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if joined.Err() != nil {
		t.Fatalf("the joined context was cancelled by its client: %v", joined.Err())
	}
	if spi.GetTransaction(joined) == nil || len(txcoord.Pairs(joined)) != 1 {
		t.Fatal("detaching cancellation must keep the transaction and the pairs")
	}
}

func TestRun_NoPass_RunsWithoutAGate(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.hold(t, pass)
	defer holder()
	ran := false
	if _, err := env.joiner.Run(env.ctx, "", txcoord.Write, func(ctx context.Context) *common.Outcome {
		ran = spi.GetTransaction(ctx) == nil
		return nil
	}); err != nil || !ran {
		t.Fatalf("an ordinary request must run as it is: ran=%v err=%v", ran, err)
	}
}
