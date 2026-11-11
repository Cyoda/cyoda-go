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

// givenTM is the transaction manager the caller hands Select: a decorator of
// the factory's, as app.go's traced one is.
func givenTM(t *testing.T, f spi.StoreFactory) spi.TransactionManager {
	t.Helper()
	inner, err := f.TransactionManager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &recTM{TransactionManager: inner}
}

// TestSelect_PicksDurableWhenCapable: a factory that offers a coordinator
// gets the durable coordination over the transaction manager it was given,
// and its transactions are location-independent.
func TestSelect_PicksDurableWhenCapable(t *testing.T) {
	f := txcoordtest.NewFactory(memory.NewStoreFactory())
	t.Cleanup(func() { _ = f.Close() })
	tm := givenTM(t, f)
	c, locationIndependent, err := Select(context.Background(), f, tm)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	d, ok := c.(*durable)
	if !ok {
		t.Fatalf("coordination = %T; want the durable one", c)
	}
	if d.txMgr != tm {
		t.Error("the durable coordination does not use the transaction manager it was given")
	}
	if !locationIndependent {
		t.Error("a capable backend's transactions are location-independent")
	}
}

// TestSelect_InProcessOnNilCoordinator: a factory that does not offer the
// capability, or offers it and answers (nil, nil), gets the in-process
// coordination over the transaction manager it was given.
func TestSelect_InProcessOnNilCoordinator(t *testing.T) {
	for name, f := range map[string]spi.StoreFactory{
		"nil coordinator": &coordFactory{StoreFactory: memory.NewStoreFactory()},
		"not implemented": memory.NewStoreFactory(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { _ = f.Close() })
			tm := givenTM(t, f)
			c, locationIndependent, err := Select(context.Background(), f, tm)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			p, ok := c.(*inProcess)
			if !ok {
				t.Fatalf("coordination = %T; want the in-process one", c)
			}
			if p.txMgr != tm {
				t.Error("the in-process coordination does not use the transaction manager it was given")
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
	f := &coordFactory{StoreFactory: memory.NewStoreFactory(), err: sentinel}
	t.Cleanup(func() { _ = f.Close() })
	c, _, err := Select(context.Background(), f, givenTM(t, f))
	if !errors.Is(err, sentinel) || c != nil {
		t.Fatalf("Select = (%v, %v); want (nil, the error)", c, err)
	}
}
