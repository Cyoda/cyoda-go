package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	internalgrpc "github.com/cyoda-platform/cyoda-go/internal/grpc"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// scriptedDispatch records the context each dispatch ran under.
type scriptedDispatch struct {
	during func(ctx context.Context) error
}

func (s *scriptedDispatch) DispatchProcessor(ctx context.Context, e *spi.Entity, _ spi.ProcessorDefinition, _, _, _ string) (*spi.Entity, error) {
	return e, s.during(ctx)
}
func (s *scriptedDispatch) DispatchCriteria(ctx context.Context, _ *spi.Entity, _ json.RawMessage, _, _, _, _, _ string) (bool, string, error) {
	return true, "", s.during(ctx)
}
func (s *scriptedDispatch) DispatchFunction(ctx context.Context, _ *spi.Entity, _ spi.ScheduleFunction, _, _, _ string) (contract.FunctionResult, error) {
	return contract.FunctionResult{}, s.during(ctx)
}

func newOnceFencedForTest(t *testing.T, during func(ctx context.Context) error) (*onceFenced, *fence.Fence, *token.Signer) {
	t.Helper()
	signer, err := token.NewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	f := fence.New(txgate.New())
	return newOnceFenced(&scriptedDispatch{during: during}, f, signer, "node-A", time.Minute, common.NewTestUUIDGenerator()), f, signer
}

// The pass the dispatch carries is admitted while the dispatch runs and
// refused once it has returned — for all three kinds of callout.
func TestOnceFenced_PassIsCurrentForTheLengthOfTheDispatch(t *testing.T) {
	kinds := map[string]func(d *onceFenced, ctx context.Context) error{
		"processor": func(d *onceFenced, ctx context.Context) error {
			_, err := d.DispatchProcessor(ctx, &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", "tx-1")
			return err
		},
		"criteria": func(d *onceFenced, ctx context.Context) error {
			_, _, err := d.DispatchCriteria(ctx, &spi.Entity{}, nil, "TRANSITION", "wf", "tr", "", "tx-1")
			return err
		},
		"function": func(d *onceFenced, ctx context.Context) error {
			_, err := d.DispatchFunction(ctx, &spi.Entity{}, spi.ScheduleFunction{}, "wf", "tr", "tx-1")
			return err
		},
	}
	for name, dispatch := range kinds {
		t.Run(name, func(t *testing.T) {
			var f *fence.Fence
			var signer *token.Signer
			var pairs []fence.Pair
			d, f, signer := newOnceFencedForTest(t, func(ctx context.Context) error {
				claims, err := signer.Verify(internalgrpc.TxTokenFromContext(ctx))
				if err != nil {
					t.Errorf("the dispatch must carry a valid pass: %v", err)
					return nil
				}
				if claims.NodeID != "node-A" || claims.TxRef != "tx-1" || claims.Major != 1 || claims.Minor != 0 {
					t.Errorf("claims = %+v", claims)
				}
				pairs = []fence.Pair{{Callout: claims.Callout, Major: claims.Major}}
				if _, err := f.Admit(context.Background(), pairs); err != nil {
					t.Errorf("a callback during the dispatch must be admitted: %v", err)
				}
				return nil
			})
			if err := dispatch(d, context.Background()); err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if _, err := f.Admit(context.Background(), pairs); !errors.Is(err, fence.ErrSuperseded) {
				t.Fatalf("a callback after the dispatch returned must be refused, got %v", err)
			}
		})
	}
}

// A callout made from inside a callback names its enclosing pairs in its pass
// and is released with them; the decorator then reports CALLOUT_SUPERSEDED.
func TestOnceFenced_ReleasedByTheFence_ReportsSuperseded(t *testing.T) {
	var f *fence.Fence
	var signer *token.Signer
	d, f, signer := newOnceFencedForTest(t, func(ctx context.Context) error {
		claims, err := signer.Verify(internalgrpc.TxTokenFromContext(ctx))
		if err != nil {
			t.Errorf("Verify: %v", err)
			return nil
		}
		if len(claims.Outer) != 1 || claims.Outer[0] != (fence.Pair{Callout: "outer", Major: 1}) {
			t.Errorf("Outer = %v; want the enclosing pair", claims.Outer)
		}
		f.Advance("outer", 2) // the owner gives the enclosing work to another cnode
		<-ctx.Done()
		return ctx.Err()
	})
	_, endOuter := f.Begin(context.Background(), "outer", "tx-1", nil)
	defer endOuter()
	f.Advance("outer", 1)
	callback, err := f.Admit(context.Background(), []fence.Pair{{Callout: "outer", Major: 1}})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	_, err = d.DispatchProcessor(callback, &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", "tx-1")
	var appErr *common.AppError
	if !errors.As(err, &appErr) || appErr.Code != common.ErrCodeCalloutSuperseded {
		t.Fatalf("err = %v; want CALLOUT_SUPERSEDED", err)
	}
}

// A caller that went away is reported as today, not as a supersede.
func TestOnceFenced_CallerCancellation_IsNotASupersede(t *testing.T) {
	d, _, _ := newOnceFencedForTest(t, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.DispatchProcessor(ctx, &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", "tx-1")
	if !errors.Is(err, context.Canceled) || errors.Is(err, fence.ErrSuperseded) {
		t.Fatalf("err = %v; want context.Canceled unchanged", err)
	}
}

// A callout with no transaction carries no pass, and is still begun and ended.
func TestOnceFenced_NoTransaction_NoPass(t *testing.T) {
	d, _, _ := newOnceFencedForTest(t, func(ctx context.Context) error {
		if tok := internalgrpc.TxTokenFromContext(ctx); tok != "" {
			t.Error("a callout with no transaction must carry no pass")
		}
		return nil
	})
	if _, err := d.DispatchProcessor(context.Background(), &spi.Entity{}, spi.ProcessorDefinition{}, "wf", "tr", ""); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}
