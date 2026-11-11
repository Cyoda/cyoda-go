package txcoord

import (
	"context"
	"errors"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord/txcoordtest"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// coordFactory is a memory factory that answers TransactionCoordinator as the
// test sets.
type coordFactory struct {
	spi.StoreFactory
	co  spi.TransactionCoordinator
	err error
}

func (f *coordFactory) TransactionCoordinator(context.Context) (spi.TransactionCoordinator, error) {
	return f.co, f.err
}

// inProcessSpy is the in-process builder Select is given; it records that it
// was called.
func inProcessSpy(f spi.StoreFactory, called *bool) func() Coordination {
	return func() Coordination {
		*called = true
		tm, _ := f.TransactionManager(context.Background())
		return New(tm)
	}
}

// TestSelect_PicksDurableWhenCapable: a factory that offers a coordinator
// gets the durable coordination, and its transactions are
// location-independent.
func TestSelect_PicksDurableWhenCapable(t *testing.T) {
	f := txcoordtest.NewFactory(memory.NewStoreFactory())
	t.Cleanup(func() { _ = f.Close() })
	var called bool
	c, locationIndependent, err := Select(context.Background(), f, inProcessSpy(f, &called))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, ok := c.(*durable); !ok {
		t.Fatalf("coordination = %T; want the durable one", c)
	}
	if !locationIndependent {
		t.Error("a capable backend's transactions are location-independent")
	}
	if called {
		t.Error("the in-process coordination was built for a capable backend")
	}
}

// TestSelect_InProcessOnNilCoordinator: a factory that does not offer the
// capability, or offers it and answers (nil, nil), gets the in-process
// coordination.
func TestSelect_InProcessOnNilCoordinator(t *testing.T) {
	for name, f := range map[string]spi.StoreFactory{
		"nil coordinator": &coordFactory{StoreFactory: memory.NewStoreFactory()},
		"not implemented": memory.NewStoreFactory(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { _ = f.Close() })
			var called bool
			c, locationIndependent, err := Select(context.Background(), f, inProcessSpy(f, &called))
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if _, ok := c.(*inProcess); !ok || !called {
				t.Fatalf("coordination = %T (built in-process: %v); want the in-process one", c, called)
			}
			if locationIndependent {
				t.Error("an in-process backend's transactions are not location-independent")
			}
		})
	}
}

// TestSelect_CoordinatorErrorFailsStartUp: a backend that cannot say whether
// it coordinates transactions does not get a coordination at all.
func TestSelect_CoordinatorErrorFailsStartUp(t *testing.T) {
	sentinel := errors.New("the coordinator could not be reached")
	f := &coordFactory{StoreFactory: memory.NewStoreFactory(), co: nil, err: sentinel}
	t.Cleanup(func() { _ = f.Close() })
	var called bool
	c, _, err := Select(context.Background(), f, inProcessSpy(f, &called))
	if !errors.Is(err, sentinel) || c != nil || called {
		t.Fatalf("Select = (%v, %v), in-process built: %v; want (nil, the error), none built", c, err, called)
	}
}
