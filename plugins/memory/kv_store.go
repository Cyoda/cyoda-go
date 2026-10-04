package memory

import (
	"bytes"
	"context"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type KeyValueStore struct {
	tenant  spi.TenantID
	factory *StoreFactory
}

func (s *KeyValueStore) Put(ctx context.Context, namespace string, key string, value []byte) error {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	s.ns(namespace)[key] = append([]byte{}, value...)
	return nil
}

func (s *KeyValueStore) Get(ctx context.Context, namespace string, key string) ([]byte, error) {
	s.factory.kvMu.RLock()
	defer s.factory.kvMu.RUnlock()
	ns, ok := s.factory.kvData[s.tenant][namespace]
	if !ok {
		return nil, fmt.Errorf("key %s not found in namespace %s: %w", key, namespace, spi.ErrNotFound)
	}
	val, ok := ns[key]
	if !ok {
		return nil, fmt.Errorf("key %s not found in namespace %s: %w", key, namespace, spi.ErrNotFound)
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

func (s *KeyValueStore) Delete(ctx context.Context, namespace string, key string) error {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	if ns, ok := s.factory.kvData[s.tenant][namespace]; ok {
		delete(ns, key)
	}
	return nil
}

func (s *KeyValueStore) List(ctx context.Context, namespace string) (map[string][]byte, error) {
	s.factory.kvMu.RLock()
	defer s.factory.kvMu.RUnlock()
	ns := s.factory.kvData[s.tenant][namespace]
	result := make(map[string][]byte, len(ns))
	for k, v := range ns {
		cp := make([]byte, len(v))
		copy(cp, v)
		result[k] = cp
	}
	return result, nil
}

// ns returns the tenant's namespace map, creating it. The caller holds kvMu.
func (s *KeyValueStore) ns(namespace string) map[string][]byte {
	if s.factory.kvData[s.tenant] == nil {
		s.factory.kvData[s.tenant] = make(map[string]map[string][]byte)
	}
	if s.factory.kvData[s.tenant][namespace] == nil {
		s.factory.kvData[s.tenant][namespace] = make(map[string][]byte)
	}
	return s.factory.kvData[s.tenant][namespace]
}

func (s *KeyValueStore) PutIfAbsent(ctx context.Context, namespace, key string, value []byte) (bool, error) {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	ns := s.ns(namespace)
	if _, ok := ns[key]; ok {
		return false, nil
	}
	ns[key] = append([]byte{}, value...)
	return true, nil
}

func (s *KeyValueStore) CompareAndPut(ctx context.Context, namespace, key string, expected, value []byte) (bool, error) {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	ns := s.ns(namespace)
	cur, ok := ns[key]
	if !ok || !bytes.Equal(cur, expected) {
		return false, nil
	}
	ns[key] = append([]byte{}, value...)
	return true, nil
}

func (s *KeyValueStore) DeleteIfEqual(ctx context.Context, namespace, key string, expected []byte) (bool, error) {
	s.factory.kvMu.Lock()
	defer s.factory.kvMu.Unlock()
	ns := s.ns(namespace)
	cur, ok := ns[key]
	if !ok || !bytes.Equal(cur, expected) {
		return false, nil
	}
	delete(ns, key)
	return true, nil
}
