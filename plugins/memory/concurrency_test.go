package memory_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

func ctxForConcurrency(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func TestConcurrentCrossStoreAccess(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxForConcurrency("tenant-A")

	const goroutinesPerStore = 20
	const opsPerGoroutine = 50

	var wg sync.WaitGroup

	// Entity writers: 20 goroutines x 50 ops.
	for g := 0; g < goroutinesPerStore; g++ {
		wg.Add(1)
		go func(gIdx int) {
			defer wg.Done()
			store, err := factory.EntityStore(ctx)
			if err != nil {
				t.Errorf("EntityStore failed: %v", err)
				return
			}
			for i := 0; i < opsPerGoroutine; i++ {
				id := fmt.Sprintf("entity-g%d-i%d", gIdx, i)
				entity := &spi.Entity{
					Meta: spi.EntityMeta{
						ID:         id,
						TenantID:   "tenant-A",
						ModelRef:   spi.ModelRef{EntityName: "ConcTest", ModelVersion: "1"},
						ChangeType: "CREATED",
						ChangeUser: "test-user",
					},
					Data: []byte(fmt.Sprintf(`{"g":%d,"i":%d}`, gIdx, i)),
				}
				if _, err := store.Save(ctx, entity); err != nil {
					t.Errorf("entity Save failed: %v", err)
				}
			}
		}(g)
	}

	// Model writers: 20 goroutines x 50 ops.
	for g := 0; g < goroutinesPerStore; g++ {
		wg.Add(1)
		go func(gIdx int) {
			defer wg.Done()
			store, err := factory.ModelStore(ctx)
			if err != nil {
				t.Errorf("ModelStore failed: %v", err)
				return
			}
			for i := 0; i < opsPerGoroutine; i++ {
				desc := &spi.ModelDescriptor{
					Ref: spi.ModelRef{
						EntityName:   fmt.Sprintf("Model-g%d-i%d", gIdx, i),
						ModelVersion: "1",
					},
					Schema: []byte(`{}`),
				}
				if err := store.Save(ctx, desc); err != nil {
					t.Errorf("model Save failed: %v", err)
				}
			}
		}(g)
	}

	// Message writers: 20 goroutines x 50 ops.
	for g := 0; g < goroutinesPerStore; g++ {
		wg.Add(1)
		go func(gIdx int) {
			defer wg.Done()
			store, err := factory.MessageStore(ctx)
			if err != nil {
				t.Errorf("MessageStore failed: %v", err)
				return
			}
			for i := 0; i < opsPerGoroutine; i++ {
				id := fmt.Sprintf("msg-g%d-i%d", gIdx, i)
				header := spi.MessageHeader{Subject: "test"}
				meta := spi.MessageMetaData{}
				payload := bytes.NewReader([]byte("hello"))
				if err := store.Save(ctx, id, header, meta, payload); err != nil {
					t.Errorf("message Save failed: %v", err)
				}
			}
		}(g)
	}

	// KV writers: 20 goroutines x 50 ops.
	for g := 0; g < goroutinesPerStore; g++ {
		wg.Add(1)
		go func(gIdx int) {
			defer wg.Done()
			store, err := factory.KeyValueStore(ctx)
			if err != nil {
				t.Errorf("KeyValueStore failed: %v", err)
				return
			}
			for i := 0; i < opsPerGoroutine; i++ {
				key := fmt.Sprintf("key-g%d-i%d", gIdx, i)
				if err := store.Put(ctx, "ns", key, []byte("value")); err != nil {
					t.Errorf("kv Put failed: %v", err)
				}
			}
		}(g)
	}

	wg.Wait()

	// Verify entity count matches expected: 20 * 50 = 1000.
	entityStore, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore failed: %v", err)
	}
	entities := drainAll(t, ctx, entityStore, spi.ModelRef{EntityName: "ConcTest", ModelVersion: "1"}, nil)
	expected := goroutinesPerStore * opsPerGoroutine
	if len(entities) != expected {
		t.Errorf("expected %d entities, got %d", expected, len(entities))
	}
}

func TestConcurrentMessageSaves(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxForConcurrency("tenant-B")

	const goroutines = 10
	const opsPerGoroutine = 20

	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gIdx int) {
			defer wg.Done()
			store, err := factory.MessageStore(ctx)
			if err != nil {
				t.Errorf("MessageStore failed: %v", err)
				return
			}
			for i := 0; i < opsPerGoroutine; i++ {
				id := fmt.Sprintf("cmsg-g%d-i%d", gIdx, i)
				header := spi.MessageHeader{Subject: "concurrent-test"}
				meta := spi.MessageMetaData{}
				payload := bytes.NewReader([]byte("hello"))
				if err := store.Save(ctx, id, header, meta, payload); err != nil {
					t.Errorf("message Save failed: %v", err)
				}
			}
		}(g)
	}

	wg.Wait()

	// Verify all messages are retrievable.
	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore failed: %v", err)
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < opsPerGoroutine; i++ {
			id := fmt.Sprintf("cmsg-g%d-i%d", g, i)
			_, _, rc, err := store.Get(ctx, id)
			if err != nil {
				t.Errorf("Get(%s) failed: %v", id, err)
				continue
			}
			rc.Close()
		}
	}
}
