package callout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// No cnode, one attaches inside the patience: the callout succeeds, and the
// waiting used no try — shown by giving the callout exactly one.
func TestOwner_NoCnode_Waits_OneAttaches_Succeeds_NoTryUsed(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second})
	time.AfterFunc(50*time.Millisecond, func() { e.attach(t, "m-1", tenantA, "x", answers("m-1")) })
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))
	start := time.Now()

	by, err := e.dispatchFunction(ctx, "x", "NONE") // patience applies with retryPolicy NONE too

	if err != nil || by != "m-1" {
		t.Fatalf("answered by %q, err %v; want m-1", by, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v: the wait ends on the change, not on a timer", elapsed)
	}
	if len(stats.Tries) != 1 || stats.Waited <= 0 {
		t.Errorf("stats = %+v, want one try and a wait on record", stats)
	}
}

func TestOwner_NoCnodeWithinThePatience_IsNoComputeMember(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 80 * time.Millisecond})
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))
	start := time.Now()

	_, err := e.dispatchFunction(ctx, "x", "")

	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("gave up after %v, before the patience was spent", elapsed)
	}
	if stats.Waited != 80*time.Millisecond {
		t.Errorf("Waited = %v, want the whole patience", stats.Waited)
	}
}

// The patience is one allowance for the callout, not one per wait: changes
// that bring no matching cnode start a new pass and a new wait, and the waits
// add up to the setting.
func TestOwner_PatienceIsOneAllowanceAcrossSeveralWaits(t *testing.T) {
	const patience = 300 * time.Millisecond
	const slack = 150 * time.Millisecond
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: patience})
	// Two changes that do not help: a cnode for another tag comes at 100 ms and
	// another at 250 ms. One allowance spends 100 ms on the first wait, 150 ms on
	// the second and the last 50 ms on the third, so the callout ends at 300 ms,
	// inside the 450 ms ceiling. A per-wait allowance would start a fresh 300 ms
	// on the 250 ms change and end at 550 ms, past it.
	time.AfterFunc(100*time.Millisecond, func() { e.attach(t, "other-1", tenantA, "other", nil) })
	time.AfterFunc(250*time.Millisecond, func() { e.attach(t, "other-2", tenantA, "other", nil) })
	start := time.Now()

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	elapsed := time.Since(start)
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember", err)
	}
	if elapsed < patience || elapsed > patience+slack {
		t.Errorf("gave up after %v, want about %v in total", elapsed, patience)
	}
}

// A change and the rest of the patience can both be ready when the wait looks:
// select then picks either, and the change branch must not charge the callout
// the time it really spent, which is past the allowance. Charging it would take
// the patience below zero, put more on stats.Waited than the setting allows and
// let one more pass run on a patience that was already spent. Both branches are
// ready on every round here, so one round in the loop is enough to show it.
func TestWaitForChange_ChargesNoMoreThanThePatienceLeft(t *testing.T) {
	const patienceLeft = time.Nanosecond
	changed := make(chan struct{})
	close(changed)

	for i := 0; i < 200; i++ {
		waited, _ := waitForChange(context.Background(), changed, nil, patienceLeft)
		if waited > patienceLeft {
			t.Fatalf("round %d waited %v, want no more than the %v it had left", i, waited, patienceLeft)
		}
	}
}

// The same invariant through the loop: whatever ends a wait, the callout is
// never charged more patience than the setting. The cnode drops the moment it is
// tried, which both fails the try and fires the change signal, so the wait that
// follows finds a change already waiting for it and returns at once. Which of
// the two ready cases select picks is its own business, so the invariant is
// checked over several callouts rather than provoked once.
func TestOwner_WaitedNeverExceedsThePatience(t *testing.T) {
	const patience = time.Nanosecond
	for i := 0; i < 64; i++ {
		e := newEnv(t, Config{FixedNumRetries: 3, Patience: patience})
		e.attach(t, "m-1", tenantA, "x", detaches())
		ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

		_, _ = e.dispatchFunction(ctx, "x", "")

		if stats.Waited > patience {
			t.Fatalf("callout %d: Waited = %v, want no more than the patience of %v", i, stats.Waited, patience)
		}
	}
}

// The change signal is taken before the pass looks, so a cnode that came or
// went while the pass was looking is not lost to the wait that follows it: a
// loop that took the signal after looking would sleep out its whole patience
// beside a registry that had already changed.
func TestOwner_AChangeWhileThePassLooks_IsNotLostToTheWait(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 2 * time.Second})
	var attached atomic.Int64
	// m-1 takes the work and never answers, so every pass spends one try on it
	// and then has no other cnode to ask. Its writer runs while the pass waits
	// for the answer that never comes, so the cnode it attaches from there — for
	// a tag this callout does not want, which helps no pass — is a change that
	// falls inside the pass.
	only := e.attach(t, "m-1", tenantA, "x", func(*internalgrpc.MemberRegistry, *internalgrpc.Member, string) {
		e.attach(t, fmt.Sprintf("other-%d", attached.Add(1)), tenantA, "other", nil)
	})
	start := time.Now()

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 4 failures") {
		t.Errorf("error = %s, want every try spent on the cnode that stayed", appErr.Message)
	}
	if only.count() != 4 {
		t.Errorf("m-1 was asked %d times, want 4: each wait ends on the change its own pass made", only.count())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v: the waits end on the change, not on the patience", elapsed)
	}
}

// A pass that made tries may still wait: a cnode that dropped and comes back is
// what the patience exists for.
func TestOwner_APassThatMadeTriesMayStillWait(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 5 * time.Second})
	e.attach(t, "m-1", tenantA, "x", detaches())
	time.AfterFunc(100*time.Millisecond, func() { e.attach(t, "m-1-again", tenantA, "x", answers("m-1-again")) })

	by, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if err != nil || by != "m-1-again" {
		t.Fatalf("answered by %q, err %v; want the cnode that came back", by, err)
	}
}

// The patience ran out with an attempt on record and tries still left: the
// attempt is reported, not "no compute member".
func TestOwner_PatienceSpentWithAnAttemptOnRecord_ReportsTheAttempt(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 80 * time.Millisecond})
	e.attach(t, "m-1", tenantA, "x", detaches())

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if got := appErrOf(t, err).Code; got != common.ErrCodeComputeMemberDisconnected {
		t.Errorf("code = %s, want the one attempt's own COMPUTE_MEMBER_DISCONNECTED", got)
	}
}

// A cnode whose stream drops is evicted the instant the drop is noticed, and
// unregistered only when its stream handler returns — later, and on its own
// goroutine. Another cnode coming or going in between wakes the wait into a new
// pass, and that pass must not spend a try on the cnode it has just recorded as
// gone: the client would be told of two attempts where one was made.
func TestOwner_AWokenPass_DoesNotTryACnodeItAlreadyKnowsIsGone(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 80 * time.Millisecond})
	only := e.attach(t, "m-1", tenantA, "x", func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, _ string) {
		// The stream dropped: evicted, still listed.
		m.Evict(errors.New("stream dropped"))
		// And some other cnode attaches, for a tag this callout does not want:
		// a change that wakes the wait without bringing anywhere to go.
		e.attach(t, "other-1", tenantA, "other", nil)
	})
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	_, err := e.dispatchFunction(ctx, "x", "")

	if got := appErrOf(t, err).Code; got != common.ErrCodeComputeMemberDisconnected {
		t.Errorf("code = %s, want the one attempt's own COMPUTE_MEMBER_DISCONNECTED", got)
	}
	if only.count() != 1 {
		t.Errorf("m-1 was asked %d times, want 1", only.count())
	}
	if got := strings.Join(stats.Tries, ","); got != "no_answer" {
		t.Errorf("Tries = %s, want the one try that was made", got)
	}
}

func TestOwner_CallerGoesAwayDuringAWait_EndsAtOnce(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 30 * time.Second})
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	defer cancel()
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()

	_, err := e.dispatchFunction(ctx, "x", "")

	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled unchanged", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v: a cancelled caller ends the wait at once", elapsed)
	}
}

func TestOwner_ReleasedByTheFenceDuringAWait_IsCalloutSuperseded(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3, Patience: 30 * time.Second})
	_, endOuter := e.fence.Begin(context.Background(), "outer-callout", "tx-1", nil)
	defer endOuter()
	e.fence.Advance("outer-callout", 1)
	callback, err := e.fence.Admit(userCtx(tenantA), []fence.Pair{{Callout: "outer-callout", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	time.AfterFunc(30*time.Millisecond, func() { e.fence.Advance("outer-callout", 2) })
	start := time.Now()

	_, err = e.dispatchFunction(callback, "x", "")

	if got := appErrOf(t, err).Code; got != common.ErrCodeCalloutSuperseded {
		t.Errorf("code = %s, want CALLOUT_SUPERSEDED", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v: the fence releasing the callout ends the wait at once", elapsed)
	}
}
