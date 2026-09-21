package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

func TestRunLocal_CalloutDeadline_CutsOffTheTryInProgress_AsNoAnswer(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil) // takes the work, never answers
	_, second := attach(t, reg, "m-2", testTenantID, "x", answersAs("m-2"))
	d := newTestDispatcher(t, reg)
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(80*time.Millisecond), contract.ErrCalloutDeadline)
	defer cancel()

	// repeat-safe and four tries: only the deadline can stop the second try
	res := d.RunLocal(ctx, processorCall("x", true, 30*time.Second), 4)
	if res.CtxErr != nil {
		t.Fatalf("CtxErr = %v: the callout's own deadline is not the caller going away", res.CtxErr)
	}
	if res.Failure == nil || res.Failure.Kind != contract.NoAnswer || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Fatalf("failure = %+v, want NoAnswer DISPATCH_TIMEOUT", res.Failure)
	}
	if !strings.Contains(res.Failure.Message, "callout deadline") {
		t.Errorf("message = %q, should say what cut the try off", res.Failure.Message)
	}
	if res.TriesUsed != 1 || len(res.Attempts) != 1 || second.count() != 0 {
		t.Errorf("TriesUsed = %d attempts = %d second asked = %d; no try starts after the deadline", res.TriesUsed, len(res.Attempts), second.count())
	}
}

func TestRunLocal_CalloutDeadline_DuringEnqueue_IsNoHandOff(t *testing.T) {
	d, _ := newWedgedDispatcher(t)
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(80*time.Millisecond), contract.ErrCalloutDeadline)
	defer cancel()

	res := d.RunLocal(ctx, processorCall("python", false, 30*time.Second), 4)
	if res.CtxErr != nil || res.Failure == nil || res.Failure.Kind != contract.NoHandOff || appCode(res.Err()) != common.ErrCodeDispatchTimeout {
		t.Fatalf("res = %+v, want NoHandOff DISPATCH_TIMEOUT and no CtxErr", res)
	}
}

func TestRunLocal_CalloutDeadline_AlreadyPassed_MakesNoTry(t *testing.T) {
	reg := NewMemberRegistry()
	_, a := attach(t, reg, "m-1", testTenantID, "x", answersAs("m-1"))
	d := newTestDispatcher(t, reg)
	ctx, cancel := context.WithDeadlineCause(testContext(), time.Now().Add(-time.Second), contract.ErrCalloutDeadline)
	defer cancel()

	res := d.RunLocal(ctx, processorCall("x", true, 5*time.Second), 4)
	if res.CtxErr != nil || res.TriesUsed != 0 || a.count() != 0 {
		t.Fatalf("res = %+v asked = %d; want no try and no CtxErr", res, a.count())
	}
	if res.Failure == nil || res.Failure.Kind != contract.NoHandOff || !errors.Is(res.Err(), contract.ErrCalloutDeadline) {
		t.Errorf("failure = %+v, want NoHandOff wrapping ErrCalloutDeadline", res.Failure)
	}
	if errors.Is(res.Err(), contract.ErrNoMatchingMember) {
		t.Error("a cnode was there; this must not read as \"no compute member\"")
	}
}

// The caller's own deadline is still the caller's: ctx.Err() unchanged.
func TestRunLocal_CallersOwnDeadline_IsCtxErrUnchanged(t *testing.T) {
	reg := NewMemberRegistry()
	attach(t, reg, "m-1", testTenantID, "x", nil)
	d := newTestDispatcher(t, reg)
	ctx, cancel := context.WithTimeout(testContext(), 80*time.Millisecond)
	defer cancel()

	res := d.RunLocal(ctx, processorCall("x", true, 30*time.Second), 4)
	if res.CtxErr != context.DeadlineExceeded || res.Failure != nil {
		t.Fatalf("CtxErr = %v Failure = %v, want context.DeadlineExceeded and no failure", res.CtxErr, res.Failure)
	}
}
