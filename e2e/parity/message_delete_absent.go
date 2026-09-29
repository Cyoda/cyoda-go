package parity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// RunMessageDeleteBatchWithAbsentID verifies that a batch delete naming an
// id that does not exist alongside one that does succeeds — HTTP 200 with
// success:true — and that the id which does exist is actually deleted. This
// depends on the storage-SPI contract that deleting an absent key returns
// nil, not an error: MessageStore.DeleteBatch must not fail the whole batch
// because one of its ids was never written.
func RunMessageDeleteBatchWithAbsentID(t *testing.T, fixture BackendFixture) {
	tenant := fixture.NewTenant(t)
	c := client.NewClient(fixture.BaseURL(), tenant.Token)

	id, err := c.CreateMessage(t, "absent-delete", `{"a":1}`)
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	results, err := c.DeleteMessagesBatched(t, []string{id, uuid.NewString()}, 0)
	if err != nil {
		t.Fatalf("batch delete with an absent id: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("DeleteMessagesBatched returned no chunks")
	}
	if !results[0].Success {
		t.Errorf("DeleteMessagesBatched success = false, want true (batch: %+v)", results[0])
	}

	status, err := c.GetMessageRaw(t, id)
	if err == nil {
		t.Fatal("the present message was not deleted")
	}
	if status != 404 {
		t.Fatalf("GetMessage after delete: expected status 404, got %d (err: %v)", status, err)
	}
}
