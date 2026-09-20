package fence

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// settle is how long a test waits before concluding that a call is blocked.
const settle = 50 * time.Millisecond

// watchdog bounds every wait in these tests: a deadlock fails the test instead
// of hanging the package.
const watchdog = 5 * time.Second

func mustFinish(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatalf("%s did not finish within %v", what, watchdog)
	}
}

func mustStillBlock(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s returned while a joined write still held the transaction's write lock", what)
	case <-time.After(settle):
	}
}

// A joined write that made its check before the number rose still holds the
// write lock; Advance and end wait for it. The earlier pass is shut out at
// once — before the wait — so no further write of the earlier cnode can start.
func TestWait_BlocksUntilAWriteInProgressHasFinished(t *testing.T) {
	tests := []struct {
		name string
		call func(f *Fence, end func())
	}{
		{"Advance", func(f *Fence, _ func()) { f.Advance("c", 2) }},
		{"end", func(_ *Fence, end func()) { end() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gate := txgate.New()
			f := New(gate)
			_, end := f.Begin(context.Background(), "c", "tx", nil)
			t.Cleanup(end)
			f.Advance("c", 1)
			writer, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
			if err != nil {
				t.Fatalf("Admit: %v", err)
			}

			// The joined write: lock, check, (write), unlock.
			release := gate.Acquire("tx")
			if err := Check(writer); err != nil {
				t.Fatalf("Check under the lock: %v", err)
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call(f, end)
			}()
			mustStillBlock(t, tc.name, done)

			// Shut out already, although the call has not returned.
			if _, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}}); !errors.Is(err, ErrSuperseded) {
				t.Fatalf("the earlier pass must be refused before the wait, got %v", err)
			}

			release()
			mustFinish(t, tc.name, done)
		})
	}
}

// A write queued for the lock when its cnode is replaced is refused once it
// takes the lock, whichever of it and the owner's wait the mutex favours.
func TestWait_AQueuedWriteIsRefusedOnTakingTheLock(t *testing.T) {
	gate := txgate.New()
	f := New(gate)
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	t.Cleanup(end)
	f.Advance("c", 1)
	queued, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	holder := gate.Acquire("tx") // another write of the same cnode, in progress

	var checkErr error
	queuedDone := make(chan struct{})
	go func() {
		defer close(queuedDone)
		release := gate.Acquire("tx")
		defer release()
		checkErr = Check(queued)
	}()
	mustStillBlock(t, "the queued write", queuedDone)

	advanced := make(chan struct{})
	go func() {
		defer close(advanced)
		f.Advance("c", 2)
	}()
	mustStillBlock(t, "Advance", advanced)

	holder()
	mustFinish(t, "the queued write", queuedDone)
	mustFinish(t, "Advance", advanced)
	if !errors.Is(checkErr, ErrSuperseded) {
		t.Fatalf("a write that takes the lock after the number rose must be refused, got %v", checkErr)
	}
}

// The wait cannot deadlock: a callout made from inside a callback, replaced
// while its own inner cnode's callback holds the lock.
//
//	owner:  callout X on tx, given to cnode A (major 1)
//	A:      callback a, joined; gives the lock up for a callout Y of its own
//	Y:      given to cnode B; B's callback b takes the lock and is writing
//	owner:  gives X to another cnode → Advance(X, 2)
func TestWait_NoDeadlockWhenTheInnerCallbackHoldsTheLock(t *testing.T) {
	gate := txgate.New()
	f := New(gate)
	_, endX := f.Begin(context.Background(), "X", "tx", nil)
	t.Cleanup(endX)
	f.Advance("X", 1)

	a, err := f.Admit(context.Background(), []Pair{{Callout: "X", Major: 1}})
	if err != nil {
		t.Fatalf("Admit a: %v", err)
	}

	// Callback a holds no lock for the length of its own callout Y.
	yCtx, endY := f.Begin(a, "Y", "tx", Pairs(a))
	f.Advance("Y", 1)

	b, err := f.Admit(context.Background(), []Pair{{Callout: "Y", Major: 1}, {Callout: "X", Major: 1}})
	if err != nil {
		t.Fatalf("Admit b: %v", err)
	}
	bRelease := gate.Acquire("tx")
	if err := Check(b); err != nil {
		t.Fatalf("Check b: %v", err)
	}

	// Callback a's chain: wait for Y to be released, end Y, re-take the lock,
	// check.
	var aErr error
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		<-yCtx.Done()
		endY()
		release := gate.Acquire("tx")
		defer release()
		aErr = Check(a)
	}()

	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		f.Advance("X", 2)
	}()
	mustStillBlock(t, "the owner's Advance", ownerDone)

	bRelease() // b's write lands
	mustFinish(t, "the owner's Advance", ownerDone)
	mustFinish(t, "callback a", aDone)
	if !errors.Is(context.Cause(yCtx), ErrSuperseded) {
		t.Fatalf("Y's context cause = %v; want ErrSuperseded", context.Cause(yCtx))
	}
	if !errors.Is(aErr, ErrSuperseded) {
		t.Fatalf("callback a must be refused on re-taking the lock, got %v", aErr)
	}
	if err := func() error {
		release := gate.Acquire("tx")
		defer release()
		return Check(b)
	}(); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("callback b's next locked write must be refused, got %v", err)
	}
}

// There is no third case. Writers loop "lock, check, write, unlock" under the
// pass of whichever major they were last admitted under; the owner keeps giving
// the work to the next cnode. A writer whose check passed under major k holds
// the lock, so Advance(k+1) cannot have returned: the owner's "advanced to"
// marker, which it sets only after Advance returns, must still be ≤ k.
//
// Run under -race while developing this package.
func TestWait_NoWriteOfAnEarlierNumberAfterAdvanceReturned(t *testing.T) {
	const (
		writers = 8
		rounds  = 200
		// How long a passing write stays under the lock, in scheduler yields.
		yieldsPerWrite = 4
	)
	gate := txgate.New()
	f := New(gate)
	_, end := f.Begin(context.Background(), "c", "tx", nil)
	t.Cleanup(end)

	type admittedAt struct {
		ctx   context.Context
		major uint32
	}
	var current atomic.Pointer[admittedAt]
	var advancedTo atomic.Uint32

	admit := func(major uint32) {
		f.Advance("c", major)
		advancedTo.Store(major)
		ctx, err := f.Admit(context.Background(), []Pair{{Callout: "c", Major: major}})
		if err != nil {
			t.Errorf("Admit under major %d: %v", major, err)
			return
		}
		current.Store(&admittedAt{ctx: ctx, major: major})
	}
	admit(1)

	stop := make(chan struct{})
	var violations atomic.Int64
	var passed, refused atomic.Int64
	// The owner's rounds are a few mutex operations each, fast enough to be over
	// before a freshly started goroutine runs at all; up holds it back until
	// every writer has taken the lock and checked once, so the rounds really run
	// against writers in the loop.
	var up sync.WaitGroup
	up.Add(writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; ; round++ {
				select {
				case <-stop:
					return
				default:
				}
				at := current.Load()
				func() {
					release := gate.Acquire("tx")
					defer release()
					if err := Check(at.ctx); err != nil {
						refused.Add(1)
						return
					}
					passed.Add(1)
					// The write. It holds the lock, so for the whole of it the
					// owner's marker must stay at the number the check passed
					// under: the yields give the owner every chance to move past
					// it, and the wait is what stops it.
					for range yieldsPerWrite {
						runtime.Gosched()
						if advancedTo.Load() > at.major {
							violations.Add(1)
							return
						}
					}
				}()
				if round == 0 {
					up.Done()
				}
			}
		}()
	}
	up.Wait()

	// The owner keeps pace with the writers instead of racing through its rounds
	// while they are still starting up: each round waits for a few more locked
	// attempts, so a round always falls inside writes in progress.
	attempts := func() int64 { return passed.Load() + refused.Load() }
	for major := uint32(2); major <= rounds; major++ {
		for from := attempts(); attempts() < from+int64(writers); {
			runtime.Gosched()
		}
		admit(major)
	}
	close(stop)
	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Fatalf("%d writes passed their check under a number the owner had already moved past", v)
	}
	if passed.Load() == 0 {
		t.Fatal("no write ever passed its check: the test exercised nothing")
	}
}

// Begin, Admit, Check, Advance and end from many goroutines at once: clean
// under -race, and every callout ends shut.
func TestFence_ConcurrentUse(t *testing.T) {
	f := newFence()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('a' + i))
			tx := "tx-" + id
			for round := range 50 {
				outerCtx, endOuter := f.Begin(context.Background(), id, tx, nil)
				f.Advance(id, 1)
				cb, err := f.Admit(outerCtx, []Pair{{Callout: id, Major: 1}})
				if err != nil {
					t.Errorf("Admit: %v", err)
					endOuter()
					return
				}
				innerID := id + "-inner"
				innerCtx, endInner := f.Begin(cb, innerID, tx, Pairs(cb))
				f.Advance(innerID, 1)
				if round%2 == 0 {
					f.Advance(id, 2)
					<-innerCtx.Done()
					if !errors.Is(Check(cb), ErrSuperseded) {
						t.Error("callback still admitted after its cnode was replaced")
					}
				}
				endInner()
				endOuter()
				if _, err := f.Admit(context.Background(), []Pair{{Callout: id, Major: 2}}); !errors.Is(err, ErrSuperseded) {
					t.Errorf("pass admitted after its callout ended: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	remaining := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.callouts)
	}()
	if remaining != 0 {
		t.Fatalf("%d callouts still registered after every end ran: nothing may be kept past a callout", remaining)
	}
}
