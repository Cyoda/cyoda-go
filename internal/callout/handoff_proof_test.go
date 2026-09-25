package callout

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/dispatch"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
)

// The proof the engine reads: contract.NoHandOffProof on the callout's error
// exactly when no try had Member.Send return nil and no hand-over got past
// connecting without an authenticated no_handoff answer.

// dispatchProcessor makes a processor callout; unsafe unless idempotent.
func (e *env) dispatchProcessor(ctx context.Context, p spi.ProcessorDefinition, entity *spi.Entity) error {
	_, err := e.owner.DispatchProcessor(ctx, entity, p, "wf1", "t1", "tx-1")
	return err
}

func assertProof(t *testing.T, err error, want bool) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want the callout to fail")
	}
	if got := contract.ProvesNoHandOff(err); got != want {
		t.Errorf("ProvesNoHandOff = %v, want %v (err: %v)", got, want, err)
	}
}

func failureOf(t *testing.T, err error) *contract.CalloutFailure {
	t.Helper()
	var failure *contract.CalloutFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no *contract.CalloutFailure behind %v", err)
	}
	return failure
}

// answersUnreadably answers success with a payload the processor cannot decode:
// the work reached the cnode, and the try is Terminal.
func answersUnreadably() script {
	return func(_ *internalgrpc.MemberRegistry, m *internalgrpc.Member, requestID string) {
		m.CompleteRequest(requestID, &internalgrpc.ProcessingResponse{Success: true, Payload: json.RawMessage(`not json`)})
	}
}

// --- exits before any try ---

func TestProof_NoCnode_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})

	err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", false), testEntity())

	assertProof(t, err, true)
	if !errors.Is(err, contract.ErrNoMatchingMember) {
		t.Errorf("err = %v, want ErrNoMatchingMember still on the chain", err)
	}
	if got := failureOf(t, err).Kind; got != contract.NoHandOff {
		t.Errorf("kind = %v, want NoHandOff", got)
	}
}

func TestProof_AnswerLimitAboveTheBound_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	n := e.attach(t, "m-1", tenantA, "x", answers("m-1"))
	p := processorDef("x", "", false)
	p.Config.ResponseTimeoutMs = 120_000 // the bound newEnv sets is 60 s

	err := e.dispatchProcessor(userCtx(tenantA), p, testEntity())

	assertProof(t, err, true)
	if got := failureOf(t, err).Kind; got != contract.Terminal {
		t.Errorf("kind = %v, want Terminal", got)
	}
	if n.count() != 0 {
		t.Error("the cnode was sent the work")
	}
}

func TestProof_CriterionThatCannotBeParsed_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})

	_, _, err := e.owner.DispatchCriteria(userCtx(tenantA), testEntity(), json.RawMessage(`not json`), "TRANSITION", "wf1", "t1", "", "tx-1")

	assertProof(t, err, true)
	if got := failureOf(t, err).Kind; got != contract.Terminal {
		t.Errorf("kind = %v, want Terminal", got)
	}
}

// --- local tries ---

func TestProof_CallerGoneBeforeTheFirstSend_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	n := e.attach(t, "m-1", tenantA, "x", answers("m-1"))
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	cancel()

	err := e.dispatchProcessor(ctx, processorDef("x", "", false), testEntity())

	assertProof(t, err, true)
	assertClientGone(t, err)
	if n.count() != 0 {
		t.Error("the cnode was sent the work")
	}
}

func TestProof_CallerGoneAfterTheSend_DoesNotProve(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	ctx, cancel := context.WithCancel(userCtx(tenantA))
	defer cancel()
	// The cnode cancels the caller when it is given the work: Member.Send has
	// returned nil by then.
	e.attach(t, "m-1", tenantA, "x", func(*internalgrpc.MemberRegistry, *internalgrpc.Member, string) { cancel() })
	p := processorDef("x", "", false)
	p.Config.ResponseTimeoutMs = patientLimitMs

	err := e.dispatchProcessor(ctx, p, testEntity())

	assertProof(t, err, false)
	assertClientGone(t, err)
}

func TestProof_TerminalBeforeTheSend_Proves(t *testing.T) {
	e := newEnv(t, Config{FixedNumRetries: 3})
	n := e.attach(t, "m-1", tenantA, "x", answers("m-1"))
	p := processorDef("x", "", false)
	p.Config.AttachEntity = true
	bad := &spi.Entity{Meta: spi.EntityMeta{ID: "entity-bad", TenantID: tenantA}, Data: []byte(`not json`)}

	err := e.dispatchProcessor(userCtx(tenantA), p, bad)

	assertProof(t, err, true)
	if got := failureOf(t, err).Kind; got != contract.Terminal {
		t.Errorf("kind = %v, want Terminal", got)
	}
	if n.count() != 0 {
		t.Error("the cnode was sent the work")
	}
}

func TestProof_EveryFailureAfterTheSend_DoesNotProve(t *testing.T) {
	tests := []struct {
		name string
		s    script
		kind contract.CalloutFailureKind
	}{
		{"unreadable answer", answersUnreadably(), contract.Terminal},
		{"the cnode failed", fails("declined", nil), contract.MemberFailed},
		{"the cnode dropped", detaches(), contract.NoAnswer},
		{"no answer", nil, contract.NoAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{FixedNumRetries: 3})
			e.attach(t, "m-1", tenantA, "x", tt.s)

			err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", false), testEntity())

			assertProof(t, err, false)
			if got := failureOf(t, err).Kind; got != tt.kind {
				t.Errorf("kind = %v, want %v", got, tt.kind)
			}
		})
	}
}

// --- sticky ---

// A hand-off in the first pass is not undone by a second pass that reaches no
// cnode. A callout that is not repeat-safe stops at its first try past the
// send, so only a repeat-safe one can make a later pass at all.
func TestProof_AHandOffInAnEarlierPass_IsNeverUndone(t *testing.T) {
	router := newScriptedRouter("p-1")
	router.script("p-1", peerNotConnected()) // first pass; the second finds p-1 not connected either
	e := newClusterEnv(t, Config{FixedNumRetries: 3, Patience: 300 * time.Millisecond, HandoverAllowance: time.Second}, router)
	// Takes the work and goes away: NoAnswer, and the change wakes a second pass.
	e.attach(t, "m-1", tenantA, "x", detaches())

	err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", true), testEntity())

	if calls := router.made(); len(calls) != 2 {
		t.Fatalf("hand-overs = %+v, want p-1 asked once in each of two passes", calls)
	}
	if got := appErrOf(t, err).Code; got != common.ErrCodeComputeMemberDisconnected {
		t.Errorf("code = %s, want the one attempt's own COMPUTE_MEMBER_DISCONNECTED", got)
	}
	assertProof(t, err, false)
}

// peerNoHandOffFailure is the failure and attempt the real router reads from an
// authenticated no_handoff answer of a peer that tried a cnode (readAnswer's
// no_handoff branch with triesUsed > 0).
func peerNoHandOffFailure() (*contract.CalloutFailure, contract.CalloutAttempt) {
	appErr := common.Operational(http.StatusServiceUnavailable, common.ErrCodeComputeMemberDisconnected,
		"compute member disconnected during processor dispatch").AsRetryable()
	return &contract.CalloutFailure{Kind: contract.NoHandOff, Code: appErr.Code, Message: appErr.Message, Err: appErr},
		contract.CalloutAttempt{MemberID: "m-on-p-1", Kind: contract.NoHandOff, Cause: appErr.Message}
}

// --- hand-overs ---

func TestProof_HandOver(t *testing.T) {
	noHandOff, attempt := peerNoHandOffFailure()
	memberFailed := &contract.CalloutFailure{Kind: contract.MemberFailed, Message: "declined"}
	refusal := &contract.CalloutFailure{Kind: contract.Terminal, Message: "the hand-over could not be accepted"}
	tests := []struct {
		name       string
		answer     func(context.Context) dispatch.HandOverAnswer
		idempotent bool
		want       bool
	}{
		{"not connected", peerNotConnected(), false, true},
		{"proved impossible before connecting", provedUnhandoverable(), false, true},
		{"connected, answer lost (no_answer)", losesTheAnswer(1), false, false},
		{"connected, peer's cnode failed", peerFails(memberFailed, 1, contract.CalloutAttempt{MemberID: "m-on-p-1", Kind: contract.MemberFailed, Cause: "declined"}), false, false},
		{"connected, peer refused before any try (Terminal)", peerFails(refusal, 0), false, false},
		{"connected, no_handoff, not repeat-safe", peerFails(noHandOff, 1, attempt), false, true},
		{"connected, no_handoff, repeat-safe", peerFails(noHandOff, 1, attempt), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newScriptedRouter("p-1")
			router.script("p-1", tt.answer)
			e := newClusterEnv(t, Config{FixedNumRetries: 0, HandoverAllowance: time.Second}, router)

			err := e.dispatchProcessor(userCtx(tenantA), processorDef("x", "", tt.idempotent), testEntity())

			if calls := router.made(); len(calls) != 1 {
				t.Fatalf("hand-overs = %+v, want one", calls)
			}
			assertProof(t, err, tt.want)
		})
	}
}

// The caller goes away while a hand-over is in progress: the hand-over's
// outcome is recorded before the loop returns.
func TestProof_CallerGoneDuringAHandOver(t *testing.T) {
	tests := []struct {
		name   string
		answer func(context.Context) dispatch.HandOverAnswer
		want   bool
	}{
		{"the peer was connected to", hangs(), false},
		{"the connection was still being opened", dialNeverCompletes(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newScriptedRouter("p-1")
			router.script("p-1", tt.answer)
			e := newClusterEnv(t, Config{FixedNumRetries: 3, HandoverAllowance: 30 * time.Second}, router)
			ctx, cancel := context.WithCancel(userCtx(tenantA))
			defer cancel()
			time.AfterFunc(30*time.Millisecond, cancel)

			err := e.dispatchProcessor(ctx, processorDef("x", "", false), testEntity())

			assertClientGone(t, err)
			assertProof(t, err, tt.want)
		})
	}
}
