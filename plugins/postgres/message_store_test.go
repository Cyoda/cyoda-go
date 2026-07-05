package postgres_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

func setupMessageTest(t *testing.T) *postgres.StoreFactory {
	t.Helper()
	pool := newTestPool(t)
	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool)
}

func TestMessageStore_SaveAndGet(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")

	store, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}

	header := spi.MessageHeader{
		Subject:         "test-subject",
		ContentType:     "application/json",
		ContentLength:   13,
		ContentEncoding: "utf-8",
		MessageID:       "msg-001",
		UserID:          "user-123",
		Recipient:       "recipient-456",
		ReplyTo:         "reply-queue",
		CorrelationID:   "corr-789",
	}
	meta := spi.MessageMetaData{
		Values:        map[string]any{"key1": "value1", "key2": "value2"},
		IndexedValues: map[string]any{"idx1": "idxval1"},
	}
	payload := strings.NewReader(`{"hello":"world"}`)

	if err := store.Save(ctx, "msg-001", header, meta, payload); err != nil {
		t.Fatalf("Save: %v", err)
	}

	gotHeader, gotMeta, gotBody, err := store.Get(ctx, "msg-001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer gotBody.Close()

	if gotHeader.Subject != header.Subject {
		t.Errorf("Subject: expected %q, got %q", header.Subject, gotHeader.Subject)
	}
	if gotHeader.ContentType != header.ContentType {
		t.Errorf("ContentType: expected %q, got %q", header.ContentType, gotHeader.ContentType)
	}
	if gotHeader.ContentLength != header.ContentLength {
		t.Errorf("ContentLength: expected %d, got %d", header.ContentLength, gotHeader.ContentLength)
	}
	if gotHeader.ContentEncoding != header.ContentEncoding {
		t.Errorf("ContentEncoding: expected %q, got %q", header.ContentEncoding, gotHeader.ContentEncoding)
	}
	if gotHeader.MessageID != header.MessageID {
		t.Errorf("MessageID: expected %q, got %q", header.MessageID, gotHeader.MessageID)
	}
	if gotHeader.UserID != header.UserID {
		t.Errorf("UserID: expected %q, got %q", header.UserID, gotHeader.UserID)
	}
	if gotHeader.Recipient != header.Recipient {
		t.Errorf("Recipient: expected %q, got %q", header.Recipient, gotHeader.Recipient)
	}
	if gotHeader.ReplyTo != header.ReplyTo {
		t.Errorf("ReplyTo: expected %q, got %q", header.ReplyTo, gotHeader.ReplyTo)
	}
	if gotHeader.CorrelationID != header.CorrelationID {
		t.Errorf("CorrelationID: expected %q, got %q", header.CorrelationID, gotHeader.CorrelationID)
	}

	if gotMeta.Values["key1"] != "value1" {
		t.Errorf("Values[key1]: expected 'value1', got %v", gotMeta.Values["key1"])
	}
	if gotMeta.IndexedValues["idx1"] != "idxval1" {
		t.Errorf("IndexedValues[idx1]: expected 'idxval1', got %v", gotMeta.IndexedValues["idx1"])
	}

	bodyBytes, err := io.ReadAll(gotBody)
	if err != nil {
		t.Fatalf("ReadAll body: %v", err)
	}
	if string(bodyBytes) != `{"hello":"world"}` {
		t.Errorf("payload: expected %q, got %q", `{"hello":"world"}`, string(bodyBytes))
	}
}

func TestMessageStore_SaveOverwrite(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	header1 := spi.MessageHeader{Subject: "first"}
	header2 := spi.MessageHeader{Subject: "second"}
	meta := spi.MessageMetaData{}

	store.Save(ctx, "dup-id", header1, meta, strings.NewReader("v1"))
	store.Save(ctx, "dup-id", header2, meta, strings.NewReader("v2"))

	gotHeader, _, gotBody, err := store.Get(ctx, "dup-id")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer gotBody.Close()

	if gotHeader.Subject != "second" {
		t.Errorf("expected 'second' after overwrite, got %q", gotHeader.Subject)
	}
	bodyBytes, _ := io.ReadAll(gotBody)
	if string(bodyBytes) != "v2" {
		t.Errorf("expected payload 'v2', got %q", string(bodyBytes))
	}
}

func TestMessageStore_GetNotFound(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	_, _, _, err := store.Get(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent message")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected error wrapping spi.ErrNotFound, got: %v", err)
	}
}

func TestMessageStore_Delete(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	meta := spi.MessageMetaData{}
	store.Save(ctx, "del-id", spi.MessageHeader{Subject: "to-delete"}, meta, strings.NewReader("payload"))

	if err := store.Delete(ctx, "del-id"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, _, err := store.Get(ctx, "del-id")
	if err == nil {
		t.Fatal("expected not found after delete")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got: %v", err)
	}
}

func TestMessageStore_DeleteNonexistent(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	if err := store.Delete(ctx, "does-not-exist"); err != nil {
		t.Fatalf("Delete nonexistent should not error, got: %v", err)
	}
}

func TestMessageStore_DeleteBatch(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	meta := spi.MessageMetaData{}
	store.Save(ctx, "batch-1", spi.MessageHeader{Subject: "one"}, meta, strings.NewReader("p1"))
	store.Save(ctx, "batch-2", spi.MessageHeader{Subject: "two"}, meta, strings.NewReader("p2"))
	store.Save(ctx, "batch-3", spi.MessageHeader{Subject: "three"}, meta, strings.NewReader("p3"))

	if err := store.DeleteBatch(ctx, []string{"batch-1", "batch-2"}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	// batch-3 should still be there
	_, _, body, err := store.Get(ctx, "batch-3")
	if err != nil {
		t.Fatalf("batch-3 should remain, got: %v", err)
	}
	body.Close()

	// batch-1 and batch-2 should be gone
	_, _, _, err = store.Get(ctx, "batch-1")
	if err == nil {
		t.Fatal("batch-1 should have been deleted")
	}
	_, _, _, err = store.Get(ctx, "batch-2")
	if err == nil {
		t.Fatal("batch-2 should have been deleted")
	}
}

func TestMessageStore_BinaryPayload(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	// Non-UTF-8 binary bytes
	binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD, 0x80, 0x9A, 0xAB}
	meta := spi.MessageMetaData{}

	if err := store.Save(ctx, "binary-id", spi.MessageHeader{ContentType: "application/octet-stream"}, meta, bytes.NewReader(binaryData)); err != nil {
		t.Fatalf("Save binary: %v", err)
	}

	_, _, body, err := store.Get(ctx, "binary-id")
	if err != nil {
		t.Fatalf("Get binary: %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll binary: %v", err)
	}
	if !bytes.Equal(got, binaryData) {
		t.Errorf("binary payload mismatch: expected %v, got %v", binaryData, got)
	}
}

func TestMessageStore_MetadataTypePreservation(t *testing.T) {
	factory := setupMessageTest(t)
	ctx := ctxWithTenant("msg-tenant")
	store, _ := factory.MessageStore(ctx)

	meta := spi.MessageMetaData{
		Values: map[string]any{
			"str":   "hello",
			"num":   float64(42),
			"flag":  true,
			"float": float64(3.14),
		},
		IndexedValues: map[string]any{
			"idx_str":  "indexed",
			"idx_bool": false,
		},
	}

	if err := store.Save(ctx, "type-test", spi.MessageHeader{}, meta, strings.NewReader("x")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, gotMeta, body, err := store.Get(ctx, "type-test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body.Close()

	if v, ok := gotMeta.Values["str"].(string); !ok || v != "hello" {
		t.Errorf("Values[str]: expected string 'hello', got %T %v", gotMeta.Values["str"], gotMeta.Values["str"])
	}
	if v, ok := gotMeta.Values["num"].(float64); !ok || v != 42 {
		t.Errorf("Values[num]: expected float64 42, got %T %v", gotMeta.Values["num"], gotMeta.Values["num"])
	}
	if v, ok := gotMeta.Values["flag"].(bool); !ok || v != true {
		t.Errorf("Values[flag]: expected bool true, got %T %v", gotMeta.Values["flag"], gotMeta.Values["flag"])
	}
	if v, ok := gotMeta.Values["float"].(float64); !ok || v != 3.14 {
		t.Errorf("Values[float]: expected float64 3.14, got %T %v", gotMeta.Values["float"], gotMeta.Values["float"])
	}
	if v, ok := gotMeta.IndexedValues["idx_str"].(string); !ok || v != "indexed" {
		t.Errorf("IndexedValues[idx_str]: expected string 'indexed', got %T %v", gotMeta.IndexedValues["idx_str"], gotMeta.IndexedValues["idx_str"])
	}
	if v, ok := gotMeta.IndexedValues["idx_bool"].(bool); !ok || v != false {
		t.Errorf("IndexedValues[idx_bool]: expected bool false, got %T %v", gotMeta.IndexedValues["idx_bool"], gotMeta.IndexedValues["idx_bool"])
	}
}

func TestMessageStore_TenantIsolation(t *testing.T) {
	factory := setupMessageTest(t)
	ctxA := ctxWithTenant("tenant-A")
	ctxB := ctxWithTenant("tenant-B")

	storeA, _ := factory.MessageStore(ctxA)
	storeB, _ := factory.MessageStore(ctxB)

	meta := spi.MessageMetaData{}
	storeA.Save(ctxA, "shared-id", spi.MessageHeader{Subject: "secret"}, meta, strings.NewReader("tenant-A-data"))

	_, _, _, err := storeB.Get(ctxB, "shared-id")
	if err == nil {
		t.Fatal("tenant-B should not see tenant-A's message")
	}
	if !errors.Is(err, spi.ErrNotFound) {
		t.Errorf("expected ErrNotFound for cross-tenant access, got: %v", err)
	}
}
