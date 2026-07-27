package memory_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

// failingReader returns errFail after writing n bytes.
type failingReader struct {
	n       int
	read    int
	errFail error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.read >= r.n {
		return 0, r.errFail
	}
	toWrite := r.n - r.read
	if toWrite > len(p) {
		toWrite = len(p)
	}
	for i := 0; i < toWrite; i++ {
		p[i] = 'x'
	}
	r.read += toWrite
	return toWrite, nil
}

func TestMessageStoreSaveAndGet(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("failed to get message store: %v", err)
	}

	header := spi.MessageHeader{
		Subject:         "order.created",
		ContentType:     "application/json",
		ContentLength:   14,
		ContentEncoding: "utf-8",
		MessageID:       "msg-001",
		UserID:          "user-1",
		Recipient:       "service-B",
		ReplyTo:         "reply-queue",
		CorrelationID:   "corr-123",
	}
	meta := spi.MessageMetaData{
		Values:        map[string]any{"env": "test"},
		IndexedValues: map[string]any{"orderId": 42},
	}
	payload := bytes.NewBufferString(`{"hello":"world"}`)

	if err := store.Save(ctx, "msg-001", header, meta, payload); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	gotHeader, gotMeta, reader, err := store.Get(ctx, "msg-001")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	defer reader.Close()

	if gotHeader.Subject != "order.created" {
		t.Errorf("expected subject order.created, got %s", gotHeader.Subject)
	}
	if gotHeader.ContentType != "application/json" {
		t.Errorf("expected content-type application/json, got %s", gotHeader.ContentType)
	}
	if gotHeader.CorrelationID != "corr-123" {
		t.Errorf("expected correlationID corr-123, got %s", gotHeader.CorrelationID)
	}
	if gotMeta.Values["env"] != "test" {
		t.Errorf("expected meta value env=test, got %s", gotMeta.Values["env"])
	}
	if gotMeta.IndexedValues["orderId"] != 42 {
		t.Errorf("expected indexed value orderId=42, got %v", gotMeta.IndexedValues["orderId"])
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read payload failed: %v", err)
	}
	if string(data) != `{"hello":"world"}` {
		t.Errorf("unexpected payload: %s", data)
	}
}

func TestMessageStoreDelete(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("failed to get message store: %v", err)
	}

	header := spi.MessageHeader{Subject: "test"}
	meta := spi.MessageMetaData{}
	payload := bytes.NewBufferString("data")

	if err := store.Save(ctx, "msg-del", header, meta, payload); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	if err := store.Delete(ctx, "msg-del"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	_, _, _, err = store.Get(ctx, "msg-del")
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestMessageStoreDeleteBatch(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("failed to get message store: %v", err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		header := spi.MessageHeader{Subject: id}
		meta := spi.MessageMetaData{}
		if err := store.Save(ctx, id, header, meta, bytes.NewBufferString("payload-"+id)); err != nil {
			t.Fatalf("save %s failed: %v", id, err)
		}
	}

	if err := store.DeleteBatch(ctx, []string{"m1", "m3"}); err != nil {
		t.Fatalf("batch delete failed: %v", err)
	}

	// m2 should remain
	_, _, reader, err := store.Get(ctx, "m2")
	if err != nil {
		t.Fatalf("m2 should still exist: %v", err)
	}
	reader.Close()

	// m1 and m3 should be gone
	for _, id := range []string{"m1", "m3"} {
		_, _, _, err := store.Get(ctx, id)
		if !errors.Is(err, spi.ErrNotFound) {
			t.Errorf("expected ErrNotFound for %s, got %v", id, err)
		}
	}
}

func TestMessageStoreTenantIsolation(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()

	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, _ := factory.MessageStore(ctxA)
	storeB, _ := factory.MessageStore(ctxB)

	header := spi.MessageHeader{Subject: "secret"}
	meta := spi.MessageMetaData{}
	if err := storeA.Save(ctxA, "msg-iso", header, meta, bytes.NewBufferString("tenant-A-data")); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	_, _, _, err := storeB.Get(ctxB, "msg-iso")
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("tenant-B should not see tenant-A message, got %v", err)
	}

	_, _, reader, err := storeA.Get(ctxA, "msg-iso")
	if err != nil {
		t.Fatalf("tenant-A should see own message: %v", err)
	}
	reader.Close()
}

func TestMessageStoreNotFound(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("failed to get message store: %v", err)
	}

	_, _, _, err = store.Get(ctx, "nonexistent")
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreFactoryClose(t *testing.T) {
	factory := memory.NewStoreFactory()
	ctx := ctxWithTenant("tenant-A")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("failed to get message store: %v", err)
	}

	header := spi.MessageHeader{Subject: "test"}
	meta := spi.MessageMetaData{}
	if err := store.Save(ctx, "msg-close", header, meta, bytes.NewBufferString("data")); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	if err := factory.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// After close, get should fail because the blob file is gone
	_, _, _, err = store.Get(ctx, "msg-close")
	if err == nil {
		t.Error("expected error after factory close, got nil")
	}
}

func TestMessageSave_FailureDoesNotLeaveMetadata(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("failed to get message store: %v", err)
	}

	// Use a failingReader that returns an error after a few bytes.
	fr := &failingReader{n: 5, errFail: fmt.Errorf("simulated I/O failure")}
	header := spi.MessageHeader{Subject: "fail-test"}
	meta := spi.MessageMetaData{}

	err = store.Save(ctx, "msg-fail", header, meta, fr)
	if err == nil {
		t.Fatal("expected Save to fail with failingReader, got nil")
	}

	// After a failed save, Get must return an error (no metadata left).
	_, _, _, err = store.Get(ctx, "msg-fail")
	if err == nil {
		t.Error("expected Get to return error after failed Save, got nil")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestMessageStoreRejectsPathTraversal pins the blob-path confinement.
//
// The message id is caller-supplied through the SPI and reaches the filesystem
// as a path segment. filepath.Join cleans a path but does not confine it, so a
// ".." id previously escaped the blob directory — and Delete removes whatever
// the path resolves to. Save and Get are covered too: a traversing id must not
// be able to write outside the tenant directory or read a file back from it.
func TestMessageStoreRejectsPathTraversal(t *testing.T) {
	factory := memory.NewStoreFactory()
	defer factory.Close()
	ctx := ctxWithTenant("tenant-A")
	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}

	evil := []string{
		"../escape",
		"../../escape",
		"a/../../escape",
		"sub/dir",
		"..",
		".",
		"",
	}
	for _, id := range evil {
		t.Run("save/"+id, func(t *testing.T) {
			err := store.Save(ctx, id, spi.MessageHeader{}, spi.MessageMetaData{},
				bytes.NewReader([]byte("payload")))
			if err == nil {
				t.Fatalf("Save(%q) succeeded; want rejection", id)
			}
		})
		t.Run("get/"+id, func(t *testing.T) {
			if _, _, _, err := store.Get(ctx, id); err == nil {
				t.Fatalf("Get(%q) succeeded; want rejection", id)
			}
		})
		t.Run("delete/"+id, func(t *testing.T) {
			// Delete is best-effort by contract and returns nil, but it must
			// not remove anything outside the tenant directory. The assertion
			// that matters is that it does not panic or escape; blobPath
			// returning an error makes the os.Remove unreachable.
			if err := store.Delete(ctx, id); err != nil {
				t.Fatalf("Delete(%q) = %v; want nil (best-effort)", id, err)
			}
		})
	}
}
