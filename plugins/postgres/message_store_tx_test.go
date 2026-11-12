package postgres_test

import (
	"strings"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// A message never joins the caller's transaction (spi.MessageStore contract):
// it is applied when Save returns, so rolling the transaction back keeps it.
func TestMessageStore_SaveInsideTransactionSurvivesRollback(t *testing.T) {
	factory, tm := newAttrFactory(t)
	ctx := ctxWithTenant("msg-tx-tenant")

	txID, txCtx := beginGuarded(t, tm, ctx)
	store, err := factory.MessageStore(txCtx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}
	header := spi.MessageHeader{Subject: "s", ContentType: "application/json", MessageID: "msg-tx-1"}
	if err := store.Save(txCtx, "msg-tx-1", header, spi.MessageMetaData{}, strings.NewReader(`{}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := tm.Rollback(txCtx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	fresh, err := factory.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}
	if _, _, body, err := fresh.Get(ctx, "msg-tx-1"); err != nil {
		t.Fatalf("message written inside a rolled-back transaction is gone: %v", err)
	} else {
		_ = body.Close()
	}
}
