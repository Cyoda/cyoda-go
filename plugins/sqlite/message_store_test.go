package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/sqlite"
)

func setupMessageStore(t *testing.T) (spi.MessageStore, context.Context) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "message_store_test.db")
	f, err := sqlite.NewStoreFactoryForTest(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewStoreFactoryForTest: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx := extTestCtx("message-tenant")
	store, err := f.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}
	return store, ctx
}

// TestMessageStore_DeleteBatch_ChunksLargeIdList seeds 1,200 messages and
// deletes them all in a single DeleteBatch call. 1,200 ids at
// deleteBatchChunkSize=500 forces the loop through 3 chunks
// (500 + 500 + 200), proving the chunked implementation drops no deletes
// across the chunk boundary.
func TestMessageStore_DeleteBatch_ChunksLargeIdList(t *testing.T) {
	store, ctx := setupMessageStore(t)

	const count = 1200
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("msg-%d", i)
		ids[i] = id

		header := spi.MessageHeader{
			Subject:     "test-subject",
			ContentType: "text/plain",
			MessageID:   id,
		}
		metaData := spi.MessageMetaData{}
		if err := store.Save(ctx, id, header, metaData, bytes.NewReader([]byte("payload"))); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	if err := store.DeleteBatch(ctx, ids); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	for _, id := range ids {
		_, _, payload, err := store.Get(ctx, id)
		if payload != nil {
			_ = payload.Close()
		}
		if err == nil {
			t.Fatalf("Get(%s): expected not-found after DeleteBatch, got nil error", id)
		}
		if !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("Get(%s): expected ErrNotFound, got %v", id, err)
		}
	}
}

// TestMessageStore_DeleteBatch_Empty verifies the empty-id-list no-op path.
func TestMessageStore_DeleteBatch_Empty(t *testing.T) {
	store, ctx := setupMessageStore(t)

	if err := store.DeleteBatch(ctx, nil); err != nil {
		t.Fatalf("DeleteBatch(nil): %v", err)
	}
}

// TestMessageStore_DeleteBatch_SingleChunk verifies deletion still works
// end-to-end when the id list fits in a single chunk, and that messages
// outside the batch survive.
func TestMessageStore_DeleteBatch_SingleChunk(t *testing.T) {
	store, ctx := setupMessageStore(t)

	ids := []string{"keep-me", "delete-me-1", "delete-me-2"}
	for _, id := range ids {
		header := spi.MessageHeader{Subject: "s", ContentType: "text/plain", MessageID: id}
		if err := store.Save(ctx, id, header, spi.MessageMetaData{}, bytes.NewReader([]byte("p"))); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
	}

	if err := store.DeleteBatch(ctx, []string{"delete-me-1", "delete-me-2"}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	for _, id := range []string{"delete-me-1", "delete-me-2"} {
		_, _, payload, err := store.Get(ctx, id)
		if payload != nil {
			_ = payload.Close()
		}
		if !errors.Is(err, spi.ErrNotFound) {
			t.Fatalf("Get(%s): expected ErrNotFound, got %v", id, err)
		}
	}

	_, _, payload, err := store.Get(ctx, "keep-me")
	if err != nil {
		t.Fatalf("Get(keep-me): unexpected error %v", err)
	}
	got, _ := io.ReadAll(payload)
	_ = payload.Close()
	if string(got) != "p" {
		t.Fatalf("Get(keep-me): payload mismatch, got %q", got)
	}
}
