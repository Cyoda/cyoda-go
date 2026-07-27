package memory

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// blobPath resolves the on-disk path for a message blob and confines it to the
// store's tenant directory.
//
// The message id is caller-supplied through the SPI, and filepath.Join cleans a
// path without constraining it — a ".." segment escapes blobDir entirely. That
// matters most for Delete, which removes whatever the path resolves to, but the
// read and write paths are confined through here too so the invariant holds in
// one place rather than three. The tenant is folded into the same check because
// it also reaches the filesystem as a path segment.
func (s *MessageStore) blobPath(id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("invalid message id")
	}
	tenant := string(s.tenant)
	if tenant == "" || tenant == "." || tenant == ".." || strings.ContainsAny(tenant, `/\`) {
		return "", fmt.Errorf("invalid tenant id")
	}
	root := s.factory.blobDir
	p := filepath.Join(root, tenant, id)
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid message id")
	}
	return p, nil
}

// tenantBlobDir is blobPath's directory half, for the write path's MkdirAll.
func (s *MessageStore) tenantBlobDir() (string, error) {
	p, err := s.blobPath("placeholder")
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

func (s *MessageStore) Save(_ context.Context, id string, header spi.MessageHeader, metaData spi.MessageMetaData, payload io.Reader) error {
	f := s.factory

	// Step 1: Write blob to a temp file OUTSIDE the lock.
	tenantDir, err := s.tenantBlobDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(tenantDir, 0755); err != nil {
		return fmt.Errorf("failed to create tenant blob dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(tenantDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp blob file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := io.Copy(tmpFile, payload); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write blob payload: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp blob file: %w", err)
	}

	// Step 2: Atomic rename to final path (POSIX atomic).
	blobPath, err := s.blobPath(id)
	if err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, blobPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename blob file: %w", err)
	}

	// Step 3: Acquire lock ONLY for metadata map insertion.
	f.msgMu.Lock()
	tenantMap := f.msgData[s.tenant]
	if tenantMap == nil {
		tenantMap = make(map[string]*messageEntry)
		f.msgData[s.tenant] = tenantMap
	}
	tenantMap[id] = &messageEntry{
		header:   header,
		metaData: copyMessageMetaData(metaData),
	}
	f.msgMu.Unlock()

	return nil
}

func (s *MessageStore) Get(_ context.Context, id string) (spi.MessageHeader, spi.MessageMetaData, io.ReadCloser, error) {
	f := s.factory

	// Copy metadata under lock.
	f.msgMu.RLock()
	tenantMap := f.msgData[s.tenant]
	entry, ok := tenantMap[id]
	var header spi.MessageHeader
	var metaData spi.MessageMetaData
	if ok {
		header = entry.header
		metaData = copyMessageMetaData(entry.metaData)
	}
	f.msgMu.RUnlock()

	if !ok {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil, spi.ErrNotFound
	}

	blobPath, err := s.blobPath(id)
	if err != nil {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil, err
	}
	file, err := os.Open(blobPath)
	if err != nil {
		return spi.MessageHeader{}, spi.MessageMetaData{}, nil, fmt.Errorf("failed to open blob file: %w", err)
	}

	return header, metaData, &idempotentCloser{rc: file}, nil
}

func (s *MessageStore) Delete(_ context.Context, id string) error {
	f := s.factory

	// Remove metadata under lock.
	f.msgMu.Lock()
	tenantMap := f.msgData[s.tenant]
	if tenantMap != nil {
		delete(tenantMap, id)
	}
	f.msgMu.Unlock()

	// Remove blob file outside lock (best-effort).
	if blobPath, err := s.blobPath(id); err == nil {
		os.Remove(blobPath)
	}

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
