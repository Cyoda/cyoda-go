package memory

import (
	"context"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

func testCtxWithTenant(tid spi.TenantID) context.Context {
	uc := &spi.UserContext{
		UserID: "test-user",
		Tenant: spi.Tenant{ID: tid, Name: string(tid)},
		Roles:  []string{"USER"},
	}
	return spi.WithUserContext(context.Background(), uc)
}

func TestSubmitTimeEviction(t *testing.T) {
	factory := NewStoreFactory()
	uuids := newTestUUIDGenerator()
	txMgr := factory.NewTransactionManager(uuids)
	ctx := testCtxWithTenant("tenant-A")

	// Commit 3 transactions.
	var txIDs []string
	for i := 0; i < 3; i++ {
		txID, _, err := txMgr.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin %d failed: %v", i, err)
		}
		if err := txMgr.Commit(ctx, txID); err != nil {
			t.Fatalf("Commit %d failed: %v", i, err)
		}
		txIDs = append(txIDs, txID)
	}

	// Verify all 3 have submit times.
	txMgr.mu.Lock()
	for i, txID := range txIDs {
		if _, ok := txMgr.submitTimes[txID]; !ok {
			t.Fatalf("expected submit time for tx %d (%s)", i, txID)
		}
	}

	// Artificially age the first two by setting their timestamps to 2 hours ago.
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	txMgr.submitTimes[txIDs[0]] = submitTimeEntry{submitTime: twoHoursAgo, tenantID: "tenant-A"}
	txMgr.submitTimes[txIDs[1]] = submitTimeEntry{submitTime: twoHoursAgo, tenantID: "tenant-A"}
	txMgr.mu.Unlock()

	// Commit a 4th transaction (triggers eviction).
	txID4, _, err := txMgr.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx4 failed: %v", err)
	}
	if err := txMgr.Commit(ctx, txID4); err != nil {
		t.Fatalf("Commit tx4 failed: %v", err)
	}

	// Assert the two aged entries are gone, the recent one and trigger survive.
	txMgr.mu.Lock()
	defer txMgr.mu.Unlock()

	if _, ok := txMgr.submitTimes[txIDs[0]]; ok {
		t.Errorf("expected aged tx %s to be evicted", txIDs[0])
	}
	if _, ok := txMgr.submitTimes[txIDs[1]]; ok {
		t.Errorf("expected aged tx %s to be evicted", txIDs[1])
	}
	if _, ok := txMgr.submitTimes[txIDs[2]]; !ok {
		t.Errorf("expected recent tx %s to survive eviction", txIDs[2])
	}
	if _, ok := txMgr.submitTimes[txID4]; !ok {
		t.Errorf("expected trigger tx %s to survive eviction", txID4)
	}
}
