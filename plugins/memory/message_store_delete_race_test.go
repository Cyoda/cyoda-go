package memory

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TestMessageStore_DeleteIsAtomicWithConcurrentSave pins the pairing Save and
// Get establish: metadata and blob are only ever observed together.
//
// Delete used to remove the map entry under the write lock but unlink the blob
// after releasing it. A Save of the same id that won the lock in that window
// inserted fresh metadata and renamed a fresh blob into place, and Delete's
// unlink then took that blob out from under it — leaving metadata with no blob,
// which Get reports as "failed to open blob file" rather than spi.ErrNotFound,
// breaking the SPI contract TestMessageStoreNotFound pins.
//
// The window between Delete's Unlock and its Remove is nanoseconds wide, so a
// blind stress loop never lands in it: Save spends microseconds writing its
// temp blob before it even reaches the lock. This test parks both operations on
// the lock first — the test holds msgMu, both goroutines queue behind it, and
// releasing it puts them in a genuine race for the critical section. That is
// what package-internal access to msgMu buys, and why this test is not in
// package memory_test; nothing is injected into production code.
//
// The assertion is on the outcome, not on an interleave: after each round Get
// must either succeed or return ErrNotFound. Any other error is the torn state.
// Unreachable over HTTP, where message ids are server-generated, but the SPI
// admits any id.
func TestMessageStore_DeleteIsAtomicWithConcurrentSave(t *testing.T) {
	f := NewStoreFactory()
	defer f.Close()

	ctx := blobTenantCtx("tenant-delete-race")
	store, err := f.MessageStore(ctx)
	if err != nil {
		t.Fatalf("MessageStore: %v", err)
	}

	const id = "msg-race"
	for round := 0; round < 500; round++ {
		if err := store.Save(ctx, id, spi.MessageHeader{Subject: "seed"},
			spi.MessageMetaData{}, bytes.NewBufferString("seed")); err != nil {
			t.Fatalf("round %d: seed save: %v", round, err)
		}

		// Hold the lock so both operations queue on it rather than arriving
		// microseconds apart.
		//
		// This Lock/Unlock pair is deliberately not defer-paired, and must
		// stay that way: the Unlock below is the starting gun that releases
		// the two racers, so deferring it to the end of the round would
		// deadlock the test against the goroutines it is waiting on. It is
		// safe because nothing between the two lines can t.Fatalf or panic —
		// the statements in between only start goroutines and sleep.
		f.msgMu.Lock()

		var wg sync.WaitGroup
		wg.Add(2)
		var saveErr error
		go func() {
			defer wg.Done()
			saveErr = store.Save(ctx, id, spi.MessageHeader{Subject: "racer"},
				spi.MessageMetaData{}, bytes.NewBufferString("racer"))
		}()
		go func() {
			defer wg.Done()
			_ = store.Delete(ctx, id)
		}()

		// Let both goroutines get as far as the lock. Save's temp-blob write
		// happens outside it, so this also puts its rename next in line.
		time.Sleep(time.Millisecond)
		f.msgMu.Unlock()
		wg.Wait()

		if saveErr != nil {
			t.Fatalf("round %d: concurrent Save failed: %v", round, saveErr)
		}

		_, _, rc, getErr := store.Get(ctx, id)
		switch {
		case getErr == nil:
			io.Copy(io.Discard, rc)
			rc.Close()
		case errors.Is(getErr, spi.ErrNotFound):
		default:
			t.Fatalf("round %d: metadata present with no blob — Get returned %v", round, getErr)
		}

		if err := store.Delete(ctx, id); err != nil {
			t.Fatalf("round %d: cleanup delete: %v", round, err)
		}
	}
}
