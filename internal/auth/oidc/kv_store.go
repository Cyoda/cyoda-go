package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// KVOidcProviderStore implements OidcProviderStore on top of spi.KeyValueStore.
// Constructed once at startup with a context that carries the system tenant —
// see app/app.go. All KV calls go through this stored context, never the
// per-request context.
type KVOidcProviderStore struct {
	kv  spi.KeyValueStore
	ctx context.Context
}

// Compile-time check that KVOidcProviderStore implements OidcProviderStore.
var _ OidcProviderStore = (*KVOidcProviderStore)(nil)

// NewKVProviderStore builds a store bound to the supplied system-tenant context.
// The ctx is stored via context.WithoutCancel so request-scoped cancellation
// cannot abort KV ops mid-operation.
func NewKVProviderStore(ctx context.Context, kv spi.KeyValueStore) (*KVOidcProviderStore, error) {
	if kv == nil {
		return nil, errors.New("oidc: NewKVProviderStore: nil KeyValueStore")
	}
	return &KVOidcProviderStore{
		kv:  kv,
		ctx: context.WithoutCancel(ctx),
	}, nil
}

func (s *KVOidcProviderStore) Register(ctx context.Context, p *OidcProvider) error {
	blob, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("oidc: marshal provider: %w", err)
	}
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	indexKey := uriIndexKey(tenant, p.WellKnownConfigURI)
	blobKey := providerBlobKey(tenant, p.ID.String())

	if err := s.kv.Put(s.ctx, Namespace, indexKey, []byte(p.ID.String())); err != nil {
		return fmt.Errorf("oidc: put index: %w", err)
	}
	if err := s.kv.Put(s.ctx, Namespace, blobKey, blob); err != nil {
		// best-effort rollback of the orphan index entry.
		_ = s.kv.Delete(s.ctx, Namespace, indexKey)
		return fmt.Errorf("oidc: put blob: %w", err)
	}
	return nil
}

func (s *KVOidcProviderStore) Get(ctx context.Context, tenantID spi.TenantID, providerID string) (*OidcProvider, error) {
	blob, err := s.kv.Get(s.ctx, Namespace, providerBlobKey(tenantID, providerID))
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("oidc: get blob: %w", err)
	}
	var p OidcProvider
	if err := json.Unmarshal(blob, &p); err != nil {
		return nil, fmt.Errorf("oidc: unmarshal blob: %w", err)
	}
	// stale-index defence: blob's OwnerLegalEntityID must match the tenant prefix.
	if spi.TenantID(p.OwnerLegalEntityID.String()) != tenantID {
		return nil, ErrProviderNotFound
	}
	return &p, nil
}

func (s *KVOidcProviderStore) GetByURI(ctx context.Context, tenantID spi.TenantID, uri string) (*OidcProvider, error) {
	idBytes, err := s.kv.Get(s.ctx, Namespace, uriIndexKey(tenantID, uri))
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("oidc: get index: %w", err)
	}
	p, err := s.Get(ctx, tenantID, string(idBytes))
	if err != nil {
		if errors.Is(err, ErrProviderNotFound) {
			// orphan index — best-effort cleanup.
			_ = s.kv.Delete(s.ctx, Namespace, uriIndexKey(tenantID, uri))
		}
		return nil, err
	}
	return p, nil
}

func (s *KVOidcProviderStore) Update(ctx context.Context, p *OidcProvider) error {
	blob, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("oidc: marshal provider: %w", err)
	}
	tenant := spi.TenantID(p.OwnerLegalEntityID.String())
	return s.kv.Put(s.ctx, Namespace, providerBlobKey(tenant, p.ID.String()), blob)
}

func (s *KVOidcProviderStore) Delete(ctx context.Context, tenantID spi.TenantID, providerID, uri string) error {
	if err := s.kv.Delete(s.ctx, Namespace, providerBlobKey(tenantID, providerID)); err != nil {
		return fmt.Errorf("oidc: delete blob: %w", err)
	}
	if err := s.kv.Delete(s.ctx, Namespace, uriIndexKey(tenantID, uri)); err != nil {
		return fmt.Errorf("oidc: delete index: %w", err)
	}
	return nil
}

func (s *KVOidcProviderStore) ListByTenant(ctx context.Context, tenantID spi.TenantID, activeOnly bool) ([]*OidcProvider, error) {
	entries, err := s.kv.List(s.ctx, Namespace)
	if err != nil {
		return nil, fmt.Errorf("oidc: list: %w", err)
	}
	var out []*OidcProvider
	prefix := string(tenantID) + ":"
	indexPrefix := string(tenantID) + ":uri:"
	for k, v := range entries {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if strings.HasPrefix(k, indexPrefix) {
			continue
		}
		var p OidcProvider
		if err := json.Unmarshal(v, &p); err != nil {
			continue
		}
		if spi.TenantID(p.OwnerLegalEntityID.String()) != tenantID {
			continue
		}
		if activeOnly && !p.Active() {
			continue
		}
		pCopy := p
		out = append(out, &pCopy)
	}
	return out, nil
}

func (s *KVOidcProviderStore) LoadAll(ctx context.Context) ([]*OidcProvider, error) {
	entries, err := s.kv.List(s.ctx, Namespace)
	if err != nil {
		return nil, fmt.Errorf("oidc: list: %w", err)
	}
	var out []*OidcProvider
	for k, v := range entries {
		if strings.HasPrefix(k, "_history:") {
			continue
		}
		if strings.Contains(k, ":uri:") {
			continue
		}
		var p OidcProvider
		if err := json.Unmarshal(v, &p); err != nil {
			continue
		}
		pCopy := p
		out = append(out, &pCopy)
	}
	return out, nil
}

func (s *KVOidcProviderStore) GetURIHistory(ctx context.Context, uriHash string) (*UriOwnershipHistory, error) {
	blob, err := s.kv.Get(s.ctx, Namespace, "_history:"+uriHash)
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("oidc: get history: %w", err)
	}
	var h UriOwnershipHistory
	if err := json.Unmarshal(blob, &h); err != nil {
		return nil, fmt.Errorf("oidc: unmarshal history: %w", err)
	}
	return &h, nil
}

func (s *KVOidcProviderStore) PutURIHistory(ctx context.Context, uriHash string, h *UriOwnershipHistory) error {
	blob, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("oidc: marshal history: %w", err)
	}
	return s.kv.Put(s.ctx, Namespace, "_history:"+uriHash, blob)
}

func (s *KVOidcProviderStore) RaceValidateIndex(ctx context.Context, tenantID spi.TenantID, uri string, expectedID string) (string, bool, error) {
	idBytes, err := s.kv.Get(s.ctx, Namespace, uriIndexKey(tenantID, uri))
	if err != nil {
		if errors.Is(err, spi.ErrNotFound) {
			return "", false, fmt.Errorf("oidc: race-validate: index disappeared (expected %s): %w", expectedID, err)
		}
		return "", false, fmt.Errorf("oidc: race-validate: %w", err)
	}
	winning := string(idBytes)
	return winning, winning == expectedID, nil
}
