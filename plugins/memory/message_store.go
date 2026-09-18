package memory

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// idempotentCloser wraps an io.ReadCloser so that Close() is a no-op after the
// first successful call — satisfying the SPI contract that double-close must not
// return an error.
type idempotentCloser struct {
	once sync.Once
	rc   io.ReadCloser
}

func (c *idempotentCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *idempotentCloser) Close() error {
	var err error
	c.once.Do(func() { err = c.rc.Close() })
	return err
}

type messageEntry struct {
	header   spi.MessageHeader
	metaData spi.MessageMetaData
}

// copyMessageMetaData returns a deep copy of the metadata maps.
func copyMessageMetaData(m spi.MessageMetaData) spi.MessageMetaData {
	out := spi.MessageMetaData{}
	if m.Values != nil {
		out.Values = make(map[string]any, len(m.Values))
		for k, v := range m.Values {
			out.Values[k] = v
		}
	}
	if m.IndexedValues != nil {
		out.IndexedValues = make(map[string]any, len(m.IndexedValues))
		for k, v := range m.IndexedValues {
			out.IndexedValues[k] = v
		}
	}
	return out
}

type MessageStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

// Blob I/O goes through the factory's *os.Root and the hex names in
// blob_path.go. Encoding governs what a name can be; the root governs what
// that name is allowed to resolve to. Neither subsumes the other — a symlink
// planted under the blob directory is the root's job, a case-only collision
// between two tenant directories is the encoding's — and between them a
// traversing or colliding name is unrepresentable rather than rejected, so
// there is no spelling check left to keep in step.
func (s *MessageStore) Save(_ context.Context, id string, header spi.MessageHeader, metaData spi.MessageMetaData, payload io.Reader) error {
	if id == "" {
		// The one precondition the encoding cannot express: hex("") is "",
		// which names the tenant directory itself rather than a blob in it.
		// The empty TENANT is refused earlier, by the factory.
		return fmt.Errorf("message id must not be empty")
	}

	f := s.factory
	root := f.blobRoot

	// Step 1: write the blob to a temp file OUTSIDE the lock.
	dir := tenantBlobDir(s.tenant)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create tenant blob dir: %w", err)
	}

	tmpFile, tmpName, err := createTempBlob(root, dir)
	if err != nil {
		return err
	}

	if _, err := io.Copy(tmpFile, payload); err != nil {
		tmpFile.Close()
		root.Remove(tmpName)
		return fmt.Errorf("failed to write blob payload: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		root.Remove(tmpName)
		return fmt.Errorf("failed to close temp blob file: %w", err)
	}

	// Steps 2 and 3 share one critical section. The rename and the metadata
	// insert have to land together: two concurrent saves of the same id would
	// otherwise be free to interleave and leave one writer's blob paired with
	// the other's header. The payload copy above stays outside the lock, which
	// is what the split was for.
	if err := func() error {
		f.msgMu.Lock()
		defer f.msgMu.Unlock()

		if err := root.Rename(tmpName, blobName(s.tenant, id)); err != nil {
			return fmt.Errorf("failed to rename blob file: %w", err)
		}
		tenantMap := f.msgData[s.tenant]
		if tenantMap == nil {
			tenantMap = make(map[string]*messageEntry)
			f.msgData[s.tenant] = tenantMap
		}
		tenantMap[id] = &messageEntry{
			header:   header,
			metaData: copyMessageMetaData(metaData),
		}
		return nil
	}(); err != nil {
		root.Remove(tmpName)
		return err
	}

	return nil
}

func (s *MessageStore) Get(_ context.Context, id string) (spi.MessageHeader, spi.MessageMetaData, io.ReadCloser, error) {
	f := s.factory

	// The metadata copy and the blob OPEN share one critical section, the
	// mirror of Save's rename-plus-insert. Copying the header under the read
	// lock and opening the blob after releasing it would let a Save that won
	// the write lock in between rename a new blob into place, and this reader
	// would return one writer's header with another writer's payload — the
	// exact pairing Save is at pains to make atomic.
	//
	// Holding the read lock across the openat is the same order of work Save
	// already holds the write lock across for its renameat. Nothing is held
	// for the duration of the READ: once the fd exists it refers to that
	// inode, so a later rename over the name cannot change what this reader
	// sees, and the lock is gone before a single byte is delivered.
	var header spi.MessageHeader
	var metaData spi.MessageMetaData
	var file *os.File
	if err := func() error {
		f.msgMu.RLock()
		defer f.msgMu.RUnlock()

		entry, found := f.msgData[s.tenant][id]
		if !found {
			// Answered from the map alone: an id that was never saved does
			// not reach the filesystem.
			return spi.ErrNotFound
		}
		header = entry.header
		metaData = copyMessageMetaData(entry.metaData)

		var openErr error
		file, openErr = f.blobRoot.Open(blobName(s.tenant, id))
		if openErr != nil {
			return fmt.Errorf("failed to open blob file: %w", openErr)
		}
		return nil
	}(); err != nil {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil, err
	}

	return header, metaData, &idempotentCloser{rc: file}, nil
}

func (s *MessageStore) Delete(_ context.Context, id string) error {
	f := s.factory

	// Remove metadata under lock.
	func() {
		f.msgMu.Lock()
		defer f.msgMu.Unlock()

		if tenantMap := f.msgData[s.tenant]; tenantMap != nil {
			delete(tenantMap, id)
		}
	}()

	// Best-effort: the metadata removal above is what makes the message gone.
	f.blobRoot.Remove(blobName(s.tenant, id))

	return nil
}

func (s *MessageStore) DeleteBatch(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := s.Delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
