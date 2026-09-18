package memory_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

// TestMessageStore_EmptyTenantRefusedByFactory pins the invariant the blob
// layer relies on instead of re-checking it: hex("") is "", which would
// collapse a tenant directory into the blob root, so the factory must never
// hand out a store with an empty tenant. MessageStore is constructed in
// exactly one place (store_factory.go MessageStore), behind resolveTenant.
func TestMessageStore_EmptyTenantRefusedByFactory(t *testing.T) {
	f := memory.NewStoreFactory()
	defer f.Close()

	if _, err := f.MessageStore(ctxWithTenant("")); err == nil {
		t.Fatal("MessageStore accepted an empty tenant")
	}
}

// TestMessageStore_SaveRejectsEmptyID is the id half of the same invariant.
// hex("") is "", so an empty id names the tenant directory itself rather than a
// blob in it. Save has to state that as a precondition on its argument: left to
// the filesystem it still fails, but as
// "failed to rename blob file: renameat 70726f6265/tmp-…  70726f6265/: not a
// directory" — an error that describes neither the caller's mistake nor
// anything the caller can act on, and leaks the encoded layout doing it. The
// assertion is therefore on the message, not merely on non-nil.
func TestMessageStore_SaveRejectsEmptyID(t *testing.T) {
	f := memory.NewStoreFactory()
	defer f.Close()

	ctx := ctxWithTenant("tenant-empty-id")
	store, err := f.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}
	err = store.Save(ctx, "", spi.MessageHeader{}, spi.MessageMetaData{},
		bytes.NewBufferString("payload"))
	if err == nil {
		t.Fatal("Save accepted an empty message id")
	}
	if !strings.Contains(err.Error(), "message id") {
		t.Errorf("Save(%q) = %v; want an error naming the message id", "", err)
	}
}

// TestMessageStore_ConcurrentSaveSameID asserts Save is atomic across its blob
// rename and its metadata insert: whichever writer wins, the payload the
// reader gets must be the one that writer wrote, never a mix.
func TestMessageStore_ConcurrentSaveSameID(t *testing.T) {
	f := memory.NewStoreFactory()
	defer f.Close()

	ctx := ctxWithTenant("concurrent-tenant")
	store, err := f.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}

	const id = "contended"
	payloads := []string{strings.Repeat("A", 4096), strings.Repeat("B", 4096)}

	var wg sync.WaitGroup
	for _, p := range payloads {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := store.Save(ctx, id, spi.MessageHeader{Subject: p[:1]},
				spi.MessageMetaData{Values: map[string]any{"who": p[:1]}},
				strings.NewReader(p)); err != nil {
				t.Errorf("Save: %v", err)
			}
		}(p)
	}
	wg.Wait()

	header, meta, rc, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	winner := string(got[:1])
	if string(got) != strings.Repeat(winner, 4096) {
		t.Fatalf("payload is torn: starts %q but is not uniform", winner)
	}
	if header.Subject != winner {
		t.Errorf("header came from %q but the blob from %q", header.Subject, winner)
	}
	if meta.Values["who"] != winner {
		t.Errorf("metadata came from %v but the blob from %q", meta.Values["who"], winner)
	}
}
