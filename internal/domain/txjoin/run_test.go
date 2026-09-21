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
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type runEnv struct {
	ctx     context.Context
	factory *memory.StoreFactory
	txMgr   spi.TransactionManager
	gate    *txgate.Registry
	fence   *fence.Fence
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
	gate := txgate.New()
	f := fence.New(gate)
	txID, txCtx, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, end := f.Begin(ctx, "req-1", txID, nil)
	t.Cleanup(end)
	f.Advance("req-1", 1)
	pass, _ := signer.Issue(token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	joiner, err := NewJoiner(signer, txMgr, f, gate, testResponseMax, maxWaiters, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return &runEnv{ctx: ctx, factory: factory, txMgr: txMgr, gate: gate, fence: f, signer: signer,
		joiner: joiner, txID: txID, txCtx: txCtx, end: end}, pass
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
	holder := env.gate.Acquire(env.txID) // a callback already has the transaction
	// Released explicitly below; the defer is so that a failing assertion does
	// not leave the queued callback parked and hang the package.
	defer holder()

	ran := make(chan struct{})
	queued := make(chan error, 1)
	go func() {
		queued <- env.joiner.RunVerified(env.ctx, verified, func(context.Context) { close(ran) })
	}()
	waitForCapacity(t, env.gate, env.txID, 1)

	err = env.joiner.RunVerified(env.ctx, verified, func(context.Context) {
		t.Error("the refused callback's handler ran")
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
func waitForCapacity(t *testing.T, gate *txgate.Registry, txID string, maxWaiters int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gate.AtCapacity(txID, maxWaiters) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d callers to queue for %s", maxWaiters, txID)
}

// Two parallel joined reads, and reads against a joined write, on one
// transaction. On the memory backend a Get writes tx.ReadSet under a READ lock
// (plugins/memory/entity_store.go Get): without the join layer's lock this test
// dies with the runtime's "concurrent map writes" (and is a data race under
// -race).
func TestRun_ParallelJoinedRequestsAreSerialised(t *testing.T) {
	env, pass := newRunEnv(t)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				err := env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
					es, err := env.factory.EntityStore(ctx)
					if err != nil {
						t.Errorf("EntityStore: %v", err)
						return
					}
					if w == 0 { // one writer among the readers
						_, err = es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: "w-" + string(rune('a'+i%26)), TenantID: "run-tenant",
							ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}, State: "S"}, Data: []byte(`{"n":2}`)})
					} else {
						_, err = es.Get(ctx, []string{"e-1", "e-2"}[i%2])
					}
					if err != nil {
						t.Errorf("store: %v", err)
					}
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

// The handler runs under the transaction's lock, with the suspendable handle
// the engine uses installed.
func TestRun_HandlerHoldsTheLock_AndCanSuspendIt(t *testing.T) {
	env, pass := newRunEnv(t)
	err := env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
		acquired := make(chan struct{})
		go func() { env.gate.Acquire(env.txID)(); close(acquired) }()
		select {
		case <-acquired:
			t.Error("the lock was free while a joined handler ran")
		case <-time.After(50 * time.Millisecond):
		}
		resume := txgate.Suspend(ctx) // as the engine does across a callout of the callback's own
		select {
		case <-acquired:
		case <-time.After(5 * time.Second):
			t.Error("Suspend did not release the lock the join layer took")
		}
		resume()
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Released after the handler returned, re-acquire included.
	done := make(chan struct{})
	go func() { env.gate.Acquire(env.txID)(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock is still held after Run returned")
	}
}

// A joined request queued for the lock when its cnode is replaced is refused on
// taking the lock: its handler never runs, nothing is written or read.
func TestRun_QueuedRequestIsRefusedOnTakingTheLock(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.gate.Acquire(env.txID) // another request of the same cnode, in progress

	ran := false
	var runErr error
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		runErr = env.joiner.Run(env.ctx, pass, func(context.Context) { ran = true })
	}()
	time.Sleep(50 * time.Millisecond) // admitted on entry, now queued for the lock

	advanced := make(chan struct{})
	go func() { defer close(advanced); env.fence.Advance("req-1", 2) }()
	time.Sleep(50 * time.Millisecond)
	holder()

	for name, ch := range map[string]chan struct{}{"the queued request": queued, "Advance": advanced} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}
	if ran {
		t.Fatal("the handler of a superseded request ran")
	}
	var appErr *common.AppError
	if !errors.As(runErr, &appErr) || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want CALLOUT_SUPERSEDED", runErr)
	}
}

// A joined request past its check when its callout ends runs to completion —
// all of it, however many items it writes — and the owner proceeds only
// afterwards. It is not refused: Run reports no error.
func TestRun_OwnerWaitsForARequestInProgress(t *testing.T) {
	env, pass := newRunEnv(t)
	inHandler := make(chan struct{})
	finish := make(chan struct{})
	var runErr error
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		runErr = env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
			close(inHandler)
			<-finish
			es, _ := env.factory.EntityStore(ctx)
			for _, id := range []string{"c-1", "c-2", "c-3"} { // a joined collection
				if _, err := es.Save(ctx, &spi.Entity{Meta: spi.EntityMeta{ID: id, TenantID: "run-tenant",
					ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"}, State: "S"}, Data: []byte(`{"n":3}`)}); err != nil {
					t.Errorf("Save %s: %v", id, err)
				}
			}
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
	<-ownerProceeds
	<-requestDone
	if runErr != nil {
		t.Fatalf("a request past its last check is answered normally, got %v", runErr)
	}
	if err := env.txMgr.Commit(env.txCtx, env.txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	es, _ := env.factory.EntityStore(env.ctx)
	for _, id := range []string{"c-1", "c-2", "c-3"} {
		if _, err := es.Get(env.ctx, id); err != nil {
			t.Fatalf("%s is missing: the collection did not land whole: %v", id, err)
		}
	}
}

// The release is deferred: a handler that panics gives the lock back. The
// recovery layers sit outside the join layer, so the panic passes through Run.
func TestRun_PanickingHandlerGivesTheLockBack(t *testing.T) {
	env, pass := newRunEnv(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must pass through Run to the recovery layer")
			}
		}()
		_ = env.joiner.Run(env.ctx, pass, func(context.Context) { panic("handler blew up") })
	}()
	free := make(chan struct{})
	go func() { env.gate.Acquire(env.txID)(); close(free) }()
	select {
	case <-free:
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking joined handler kept the transaction's lock: the owner would wait for ever")
	}
	// Also after a Suspend/resume inside the handler: the deferred release
	// frees the RE-acquired lock.
	func() {
		defer func() { _ = recover() }()
		_ = env.joiner.Run(env.ctx, pass, func(ctx context.Context) {
			resume := txgate.Suspend(ctx)
			resume()
			panic("after resume")
		})
	}()
	free2 := make(chan struct{})
	go func() { env.gate.Acquire(env.txID)(); close(free2) }()
	select {
	case <-free2:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock re-acquired by resume was not released on panic")
	}
}

// A joined request still queued for the transaction's lock when its client
// goes away returns at once and never runs: it has touched nothing, so there
// is nothing to protect, and the handler goroutine must not stay parked.
func TestRun_AWaiterWhoseClientGoesAwayNeverRuns(t *testing.T) {
	env, pass := newRunEnv(t)
	holder := env.gate.Acquire(env.txID) // another request of the same compute node
	defer holder()

	request, disconnect := context.WithCancel(env.ctx)
	ran := false
	returned := make(chan error, 1)
	go func() {
		returned <- env.joiner.Run(request, pass, func(context.Context) { ran = true })
	}()
	time.Sleep(50 * time.Millisecond) // admitted on entry, now queued for the lock

	select {
	case err := <-returned:
		t.Fatalf("Run returned %v while the lock was held elsewhere", err)
	case <-time.After(50 * time.Millisecond):
	}
	disconnect()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a joined request whose client went away stayed parked on the lock")
	}
	if ran {
		t.Fatal("the handler of a request whose client had gone ran")
	}
}

// A compute node that disconnects in the middle of its callback cancels the
// request's context; a joined request that holds the transaction's lock must
// not see it — its statements run on the operation's connection.
func TestRun_AJoinedRequestHoldingTheLockIsNotCancelledByItsClient(t *testing.T) {
	env, pass := newRunEnv(t)
	request, disconnect := context.WithCancel(env.ctx)
	defer disconnect()

	var joined context.Context
	err := env.joiner.Run(request, pass, func(ctx context.Context) {
		joined = ctx
		disconnect() // the compute node goes away mid-callback
		es, err := env.factory.EntityStore(ctx)
		if err != nil {
			t.Errorf("EntityStore: %v", err)
			return
		}
		if _, err := es.Get(ctx, "e-1"); err != nil {
			t.Errorf("a statement of a joined request was cut off by its client: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if joined.Err() != nil {
		t.Fatalf("the joined context was cancelled by its client: %v", joined.Err())
	}
	if spi.GetTransaction(joined) == nil || len(fence.Pairs(joined)) != 1 {
		t.Fatal("detaching cancellation must keep the transaction and the pairs")
	}
}

func TestRun_NoPass_RunsWithoutALock(t *testing.T) {
	env, _ := newRunEnv(t)
	held := env.gate.Acquire(env.txID)
	defer held()
	ran := false
	if err := env.joiner.Run(env.ctx, "", func(ctx context.Context) {
		ran = spi.GetTransaction(ctx) == nil
	}); err != nil || !ran {
		t.Fatalf("an ordinary request must run as it is: ran=%v err=%v", ran, err)
	}
}
