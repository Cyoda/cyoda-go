package callout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

var _ contract.ExternalProcessingService = (*Coordinator)(nil)

// --- the number of tries ---

func TestOwner_RetryPolicyNone_OneTry_OnEveryKindOfCallout(t *testing.T) {
	kinds := map[string]func(e *env) error{
		"processor": func(e *env) error {
			_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "NONE", true), "wf1", "t1", "tx-1")
			return err
		},
		"criterion": func(e *env) error {
			_, _, err := e.owner.DispatchCriteria(userCtx(tenantA), testEntity(), criterionJSON("x", "NONE"), "TRANSITION", "wf1", "t1", "", "tx-1")
			return err
		},
		"function": func(e *env) error {
			_, err := e.dispatchFunction(userCtx(tenantA), "x", "NONE")
			return err
		},
	}
	for name, dispatch := range kinds {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 3})
			first := e.attach(t, "m-1", tenantA, "x", nil) // takes the work, never answers
			second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

			err := dispatch(e)

			// Repeat-safe, a second cnode ready to answer — and still one try.
			if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
				t.Errorf("code = %s, want the one try's own DISPATCH_TIMEOUT", got)
			}
			if first.count() != 1 || second.count() != 0 {
				t.Errorf("asked m-1 %d times and m-2 %d times, want 1 and 0", first.count(), second.count())
			}
		})
	}
}

func TestOwner_RetryPolicyFixedOrUnset_OneTryPlusTheConfiguredRetries(t *testing.T) {
	for _, policy := range []string{"", "FIXED"} {
		t.Run("retryPolicy="+policy, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 2})
			var cnodes []*cnode
			for _, id := range []string{"m-1", "m-2", "m-3", "m-4"} {
				cnodes = append(cnodes, e.attach(t, id, tenantA, "x", nil))
			}

			_, err := e.dispatchFunction(userCtx(tenantA), "x", policy)

			asked := 0
			for _, n := range cnodes {
				asked += n.count()
			}
			if asked != 3 {
				t.Errorf("%d tries were made, want 1 + 2", asked)
			}
			appErr := appErrOf(t, err)
			if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 3 failures") {
				t.Errorf("error = %s", appErr.Message)
			}
		})
	}
}

// No retries configured is one try: the setting is what is added to the first
// try, and with nothing added the second cnode is never asked.
func TestOwner_NoRetriesConfigured_OneTry(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 0})
	first := e.attach(t, "m-1", tenantA, "x", nil) // takes the work, never answers
	second := e.attach(t, "m-2", tenantA, "x", nil)

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if first.count() != 1 || second.count() != 0 {
		t.Errorf("asked m-1 %d times and m-2 %d times, want 1 and 0", first.count(), second.count())
	}
	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want the one try's own DISPATCH_TIMEOUT", got)
	}
}

// --- what the client is told ---

func TestOwner_EveryTryUsed_IsCalloutFailedAndListsTheTries(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 1})
	e.attach(t, "m-1", tenantA, "x", nil)
	e.attach(t, "m-2", tenantA, "x", detaches())

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	appErr := appErrOf(t, err)
	if appErr.Status != 503 || appErr.Code != common.ErrCodeCalloutFailed || !appErr.Retryable {
		t.Fatalf("got %d %s retryable=%v, want a retryable 503 CALLOUT_FAILED", appErr.Status, appErr.Code, appErr.Retryable)
	}
	want := "CALLOUT_FAILED: the callout could not be completed, got 2 failures: " +
		"[member<m-1>: DISPATCH_TIMEOUT: function dispatch timed out after 60ms: no response], " +
		"[member<m-2>: COMPUTE_MEMBER_DISCONNECTED: compute member disconnected during function dispatch]"
	if appErr.Message != want {
		t.Errorf("message\n got %s\nwant %s", appErr.Message, want)
	}
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || len(failure.Attempts) != 2 {
		t.Errorf("failure = %+v, want both attempts on it", failure)
	}
}

func TestOwner_ExactlyOneAttemptRecorded_IsNotWrapped(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", nil)

	// Repeat-safe and three tries left, but no other cnode and no patience.
	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeDispatchTimeout || !appErr.Retryable {
		t.Errorf("got %s, want the attempt's own retryable DISPATCH_TIMEOUT", appErr.Message)
	}
	if strings.Contains(appErr.Message, "member<") {
		t.Errorf("a single attempt must not be wrapped: %s", appErr.Message)
	}
}

func TestOwner_NoAnswer_ProcessorThatIsNotIdempotent_Stops(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", nil)
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")

	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want the try's own DISPATCH_TIMEOUT", got)
	}
	if second.count() != 0 {
		t.Error("a processor that is not idempotent was given to a second cnode after a hand-off")
	}
}

func TestOwner_NoAnswer_IdempotentProcessor_NextCnodeAnswers(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	updated, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", true), "wf1", "t1", "tx-1")

	if err != nil {
		t.Fatalf("DispatchProcessor: %v", err)
	}
	if string(updated.Data) != `{"by":"m-2"}` {
		t.Errorf("entity data = %s, want m-2's answer", updated.Data)
	}
}

func TestOwner_MemberFailed_OneTry_AndTheVerdictIsKept(t *testing.T) {
	yes := true
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", fails("rates service is down", &yes))
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) || failure.Kind != contract.MemberFailed {
		t.Fatalf("err = %v, want a MemberFailed failure", err)
	}
	if failure.Message != "rates service is down" || failure.Retryable == nil || !*failure.Retryable {
		t.Errorf("failure = %+v, want the cnode's own message and verdict", failure)
	}
	if second.count() != 0 {
		t.Error("a cnode that answered \"failed\" was replaced")
	}
}

func TestOwner_SameRequestIDOnEveryTry(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	first := e.attach(t, "m-1", tenantA, "x", detaches())
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	if _, err := e.dispatchFunction(userCtx(tenantA), "x", ""); err != nil {
		t.Fatal(err)
	}
	ids1, _ := first.seen()
	ids2, _ := second.seen()
	if len(ids1) != 1 || len(ids2) != 1 || ids1[0] == "" || ids1[0] != ids2[0] {
		t.Errorf("request ids = %v and %v, want one id, the same on both tries", ids1, ids2)
	}
}

// --- precedence when nothing more can be done ---

func TestOwner_NoCnode_NoPatience_IsNoComputeMemberAtOnce(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	start := time.Now()

	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatalf("err = %v, want ErrNoMatchingMember (503 NO_COMPUTE_MEMBER_FOR_TAG at the classifier)", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v: with no patience the answer is immediate", elapsed)
	}
}

func TestOwner_AttemptsOnRecordBeatNoCnode(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", detaches())

	// Two tries, both cnodes gone, two tries left, nobody else to ask.
	_, err := e.dispatchFunction(userCtx(tenantA), "x", "")

	if errors.Is(err, contract.ErrNoMatchingMember) {
		t.Fatal("\"no compute member\" is reported only when no try was ever made")
	}
	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "got 2 failures") {
		t.Errorf("error = %s, want CALLOUT_FAILED listing the two attempts", appErr.Message)
	}
}

// A pass that made no try because the callout's deadline had already passed
// looked for no compute member, so it must not replace what a pass that did look
// reported: with no try ever made the answer is NO_COMPUTE_MEMBER_FOR_TAG.
func TestProgress_APassStoppedByTheDeadlineDoesNotReplaceNoComputeMember(t *testing.T) {
	deadlinePassed := &contract.CalloutFailure{
		Kind:    contract.NoHandOff,
		Code:    common.ErrCodeDispatchTimeout,
		Message: common.ErrCodeDispatchTimeout + ": the callout deadline passed before a try could start",
		Err:     contract.ErrCalloutDeadline,
	}
	noCnode := &contract.CalloutFailure{
		Kind:    contract.NoHandOff,
		Code:    common.ErrCodeNoComputeMemberForTag,
		Message: common.ErrCodeNoComputeMemberForTag + `: no compute member for tags "x"`,
		Err:     contract.ErrNoMatchingMember,
	}

	t.Run("the deadline does not overwrite what a pass that looked found", func(t *testing.T) {
		p := &progress{}
		p.noTry(noCnode)
		p.noTry(deadlinePassed)
		if got := p.stop(context.Background()); !errors.Is(got, contract.ErrNoMatchingMember) {
			t.Errorf("stop = %v, want the NO_COMPUTE_MEMBER_FOR_TAG the first pass found", got)
		}
	})
	t.Run("the deadline is reported when no pass ever looked for a member", func(t *testing.T) {
		p := &progress{}
		p.noTry(deadlinePassed)
		if got := p.stop(context.Background()); !errors.Is(got, contract.ErrCalloutDeadline) {
			t.Errorf("stop = %v, want the deadline: a member was there and nothing was tried", got)
		}
	})
	t.Run("one pass with no compute member after another is the latest of them", func(t *testing.T) {
		p := &progress{}
		p.noTry(deadlinePassed)
		p.noTry(noCnode)
		if got := p.stop(context.Background()); !errors.Is(got, contract.ErrNoMatchingMember) {
			t.Errorf("stop = %v, want NO_COMPUTE_MEMBER_FOR_TAG", got)
		}
	})
}

// A failure the owner returns owns its attempts: the loop goes on recording
// tries into its own slice, and nothing of that reaches the value already
// handed to the caller.
func TestOwner_AReturnedFailureDoesNotShareTheRunningAttempts(t *testing.T) {
	attempt := func(id string) contract.CalloutAttempt {
		return contract.CalloutAttempt{MemberID: id, Kind: contract.NoAnswer, Cause: "DISPATCH_TIMEOUT: no response"}
	}
	tests := map[string]*progress{
		"the failure's kind forbids another cnode": {
			triesLeft: 3,
			attempts:  []contract.CalloutAttempt{attempt("m-1")},
			lastTried: &contract.CalloutFailure{Kind: contract.Terminal, Message: "internal error"},
		},
		"every try is used": {
			triesLeft: 0,
			attempts:  []contract.CalloutAttempt{attempt("m-1"), attempt("m-2")},
			lastTried: &contract.CalloutFailure{Kind: contract.NoAnswer, Message: "no response"},
		},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			before := len(p.attempts)

			done, err := p.verdict(true)

			if !done {
				t.Fatalf("verdict said the callout may go on; this case ends it")
			}
			var failure *contract.CalloutFailure
			if !errors.As(err, &failure) || len(failure.Attempts) != before {
				t.Fatalf("failure = %+v, want the %d attempts on it", failure, before)
			}
			// The loop goes on over the slice it kept: an entry is rewritten
			// where it stands, and another try is recorded after it.
			p.attempts[0].MemberID = "overwritten"
			p.attempts = append(p.attempts, attempt("m-9"))

			if len(failure.Attempts) != before || failure.Attempts[0].MemberID != "m-1" {
				t.Errorf("the returned failure changed under the caller: %+v", failure.Attempts)
			}
		})
	}
}

// --- tenants ---

func TestOwner_TwoTenantsShareATag_OnlyTheCallersCnodesAreTriedOrNamed(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "a-1", tenantA, "shared", nil)
	e.attach(t, "a-2", tenantA, "shared", nil)
	other := e.attach(t, "b-1", tenantB, "shared", answers("b-1"))

	_, err := e.dispatchFunction(userCtx(tenantA), "shared", "")

	appErr := appErrOf(t, err)
	if appErr.Code != common.ErrCodeCalloutFailed || !strings.Contains(appErr.Message, "member<a-1>") || !strings.Contains(appErr.Message, "member<a-2>") {
		t.Errorf("error = %s, want both of tenant A's cnodes listed", appErr.Message)
	}
	if strings.Contains(appErr.Message, "b-1") {
		t.Errorf("another tenant's cnode is named: %s", appErr.Message)
	}
	if other.count() != 0 {
		t.Error("another tenant's cnode was given the work")
	}
}

// --- a scheduled fire ---

// A callout made from a scheduled fire runs under the system principal and no
// client; the rules are the same.
func TestOwner_CalloutFromAScheduledFire_FollowsTheSameRules(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 1})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	by, err := e.dispatchFunction(common.SystemUserContext(tenantA), "x", "")
	if err != nil || by != "m-2" {
		t.Fatalf("answered by %q, err %v; want m-2", by, err)
	}

	e.attach(t, "m-3", tenantA, "x", nil)
	e.attach(t, "m-4", tenantA, "x", nil)
	_, err = e.owner.DispatchProcessor(common.SystemUserContext(tenantA), testEntity(), processorDef("x", "", false), "wf1", "t1", "tx-1")
	if got := appErrOf(t, err).Code; got != common.ErrCodeDispatchTimeout {
		t.Errorf("code = %s, want a processor that is not idempotent to stop at its first NoAnswer", got)
	}
}

// --- fencing, the Coordinator's side ---

func pairOf(t *testing.T, e *env, pass string) fence.Pair {
	t.Helper()
	claims := claimsOf(t, e, pass)
	return fence.Pair{Callout: claims.Callout, Major: claims.Major, Minor: claims.Minor}
}

// The owner gives the work to a second cnode of its own: by the time the second
// cnode is handed the work, the number has risen — the first cnode's pass is
// refused — and the owner has waited for the first cnode's joined request that
// was in progress.
func TestOwner_SecondCnode_TheFirstIsShutOutAndWaitedForBeforeTheNextHandOff(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 1})
	holding := make(chan func(), 1)
	first := e.attach(t, "m-1", tenantA, "x", func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, _ string) {
		// m-1's callback is in progress on the transaction: it holds the lock.
		holding <- e.gate.Acquire("tx-1")
		e.reg.Unregister(m) // and then its stream drops
	})
	second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))

	done := make(chan struct{})
	var by string
	var err error
	go func() {
		defer close(done)
		by, err = e.dispatchFunction(userCtx(tenantA), "x", "")
	}()

	var release func()
	select {
	case release = <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("m-1 was never given the work")
	}

	// The number rises before the wait: m-1's pass is refused already.
	_, passes := first.seen()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, admitErr := e.fence.Admit(context.Background(), []fence.Pair{pairOf(t, e, passes[0])}); errors.Is(admitErr, fence.ErrSuperseded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("m-1's pass was still admitted after it was given up on")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// And the wait comes before the next hand-off: while m-1's request holds
	// the lock, m-2 is not given the work.
	time.Sleep(100 * time.Millisecond)
	if second.count() != 0 {
		t.Fatal("m-2 was given the work while m-1's joined request was still in progress")
	}

	release()
	mustFinish(t, "the callout", done)
	if err != nil || by != "m-2" {
		t.Fatalf("answered by %q, err %v; want m-2", by, err)
	}
	_, passes2 := second.seen()
	if got := pairOf(t, e, passes2[0]); got.Major != 2 || got.Minor != 0 {
		t.Errorf("m-2's pass carries (%d, %d), want (2, 0): the owner's own tries raise major", got.Major, got.Minor)
	}
}

func TestOwner_WhenTheCalloutHasEnded_ItsPassesAreRefused(t *testing.T) {
	e := newEnv(t, Config{})
	only := e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	if _, err := e.dispatchFunction(userCtx(tenantA), "x", ""); err != nil {
		t.Fatal(err)
	}
	_, passes := only.seen()
	if _, err := e.fence.Admit(context.Background(), []fence.Pair{pairOf(t, e, passes[0])}); !errors.Is(err, fence.ErrSuperseded) {
		t.Errorf("Admit = %v, want the pass of a callout that has ended to be refused", err)
	}
}

// Every callout the owner's loop runs names this pnode as its owner: a callout
// inside a transaction that names none is refused before the hand-off, because
// its cnode's callbacks could never be routed.
func TestOwner_EveryCalloutNamesThisPnodeAsItsOwner(t *testing.T) {
	kinds := map[string]func(e *env) error{
		"processor": func(e *env) error {
			_, err := e.owner.DispatchProcessor(userCtx(tenantA), testEntity(), processorDef("x", "", true), "wf1", "t1", "tx-1")
			return err
		},
		"criterion": func(e *env) error {
			_, _, err := e.owner.DispatchCriteria(userCtx(tenantA), testEntity(), criterionJSON("x", ""), "TRANSITION", "wf1", "t1", "", "tx-1")
			return err
		},
		"function": func(e *env) error {
			_, err := e.dispatchFunction(userCtx(tenantA), "x", "")
			return err
		},
	}
	for name, dispatch := range kinds {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, Config{SelfNodeID: "node-of-the-owner"})
			only := e.attach(t, "m-1", tenantA, "x", answers("m-1"))

			if err := dispatch(e); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			_, passes := only.seen()
			if len(passes) != 1 {
				t.Fatalf("passes = %v, want the one try's pass", passes)
			}
			if got := claimsOf(t, e, passes[0]).NodeID; got != "node-of-the-owner" {
				t.Errorf("pass names %q as the owner, want this pnode's own id", got)
			}
		})
	}
}

// A callout made from inside a callback is begun under the callback's own pairs:
// they travel into every pass it mints, so the cnode's callbacks are judged
// against the enclosing callout too, and the Coordinator is released when the
// enclosing cnode is replaced.
func TestOwner_CalloutFromInsideACallback_ItsPassesCarryTheOuterPairs(t *testing.T) {
	e := newEnv(t, Config{})
	only := e.attach(t, "m-1", tenantA, "x", answers("m-1"))

	_, endOuter := e.fence.Begin(context.Background(), "outer-callout", "tx-1", nil)
	defer endOuter()
	e.fence.Advance("outer-callout", 3)
	callback, err := e.fence.Admit(userCtx(tenantA), []fence.Pair{{Callout: "outer-callout", Major: 3}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	if _, err := e.dispatchFunction(callback, "x", ""); err != nil {
		t.Fatalf("the inner callout failed: %v", err)
	}

	_, passes := only.seen()
	claims := claimsOf(t, e, passes[0])
	if len(claims.Outer) != 1 || claims.Outer[0] != (token.Pair{Callout: "outer-callout", Major: 3}) {
		t.Errorf("Outer = %+v, want the callback's own pair", claims.Outer)
	}
	if claims.Callout == "outer-callout" || claims.Major != 1 || claims.Minor != 0 {
		t.Errorf("the inner pass names (%s, %d, %d), want a callout of its own at (1, 0)", claims.Callout, claims.Major, claims.Minor)
	}
}

// --- whose context ended ---

// A caller whose context ends during a try ends the callout with its own
// context error, marked as the caller's departure so a door's error funnel can
// log it quietly and without a ticket rather than guess from the cancellation.
func TestOwner_CallerGoesAwayDuringATry_CtxErrMarkedClientGone(t *testing.T) {
	tests := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{"the client went away", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(userCtx(tenantA))
			time.AfterFunc(20*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
		{"transactionTimeoutMillis fired", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(userCtx(tenantA), 20*time.Millisecond)
		}, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 3})
			e.attach(t, "m-1", tenantA, "x", nil)
			second := e.attach(t, "m-2", tenantA, "x", answers("m-2"))
			ctx, cancel := tt.ctx()
			defer cancel()

			err := e.dispatchPatientFunction(ctx, "x")

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v on the chain", err, tt.wantErr)
			}
			if !errors.Is(err, common.ErrClientGone) {
				t.Errorf("err = %v, want it marked as the caller's departure", err)
			}
			if second.count() != 0 {
				t.Error("a caller that went away ends the callout; the work does not move to the next cnode")
			}
		})
	}
}

// A callout made from inside a callback is released when the cnode that made
// the callback is replaced: CALLOUT_SUPERSEDED, not a cancelled request.
func TestOwner_ReleasedByTheFenceDuringATry_IsCalloutSuperseded(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", nil)

	_, endOuter := e.fence.Begin(context.Background(), "outer-callout", "tx-1", nil)
	defer endOuter()
	e.fence.Advance("outer-callout", 1)
	callback, err := e.fence.Admit(userCtx(tenantA), []fence.Pair{{Callout: "outer-callout", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.fence.Advance("outer-callout", 2) // the outer cnode is replaced
	}()

	err = e.dispatchPatientFunction(callback, "x")

	appErr := appErrOf(t, err)
	if appErr.Status != 410 || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Errorf("got %d %s, want 410 CALLOUT_SUPERSEDED", appErr.Status, appErr.Code)
	}
	if !errors.Is(err, fence.ErrSuperseded) {
		t.Errorf("err = %v, want fence.ErrSuperseded as its cause", err)
	}
}

// --- what the loop reports ---

func TestOwner_ReportsEveryTryWithItsOutcome(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	e.attach(t, "m-1", tenantA, "x", detaches())
	e.attach(t, "m-2", tenantA, "x", answers("m-2"))
	ctx, stats := contract.WithCalloutStats(userCtx(tenantA))

	if _, err := e.dispatchFunction(ctx, "x", ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stats.Tries, ","); got != "no_answer,ok" {
		t.Errorf("Tries = %s, want no_answer,ok", got)
	}
}
