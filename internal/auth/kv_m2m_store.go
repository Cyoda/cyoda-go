package auth

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// createLockStripes bounds the per-tenant create locks' memory: tenants hash
// onto a fixed set of mutexes.
const createLockStripes = 64

// undoTimeout bounds the compensation of a failed create or reset.
const undoTimeout = 30 * time.Second

// KVM2MClientStore stores M2M clients in the SYSTEM-tenant KV store: one
// namespace per tenant for the records, one global namespace mapping a
// client id to its tenant. There is no node copy: every call reads or writes
// the store, so a change is visible to every node when the call returns.
// Every call strips any transaction from its context: the postgres KV store
// joins a transaction it finds there, and a client change must never ride on
// a caller's entity transaction.
//
// A client exists when its record exists and the index entry for its id
// names its tenant. Create writes the record, then the index entry; Delete
// removes the index entry, then the record. The KV SPI has no
// compare-and-set: two admin changes to one client at the same moment, on one
// node or on two, resolve by last write; a failed reset's write-back can land
// after a later successful reset, so the secret that reset returned stops
// working and the one from before both resets works again; and the cap can be
// exceeded by one record per node. All are documented (cyoda help auth
// clients).
type KVM2MClientStore struct {
	kv           spi.KeyValueStore
	maxPerTenant int
	createLocks  [createLockStripes]sync.Mutex
}

// NewKVM2MClientStore returns a store over kv. maxPerTenant <= 0: no cap.
func NewKVM2MClientStore(kv spi.KeyValueStore, maxPerTenant int) *KVM2MClientStore {
	return &KVM2MClientStore{kv: kv, maxPerTenant: maxPerTenant}
}

// noTx removes any transaction from ctx.
func noTx(ctx context.Context) context.Context { return spi.WithTransaction(ctx, nil) }

func (s *KVM2MClientStore) createLock(t spi.TenantID) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(t))
	return &s.createLocks[h.Sum32()%createLockStripes]
}

// getIndex reads the index entry of id. found=false: absent.
func (s *KVM2MClientStore) getIndex(ctx context.Context, id string) (spi.TenantID, bool, error) {
	data, err := s.kv.Get(ctx, m2mClientIndexNamespace, id)
	if errors.Is(err, spi.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to read m2m client index: %w", err)
	}
	t, err := decodeIndexEntry(data)
	if err != nil {
		slog.Error("m2m client index entry does not decode", "pkg", "auth", "kvKey", id)
		return "", false, err
	}
	return t, true, nil
}

// getRecord reads and decodes id's record in tenant's namespace, and returns
// the stored bytes with it.
func (s *KVM2MClientStore) getRecord(ctx context.Context, t spi.TenantID, id string) (*M2MClient, []byte, bool, error) {
	data, err := s.kv.Get(ctx, m2mTenantNamespace(t), id)
	if errors.Is(err, spi.ErrNotFound) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("failed to read m2m client: %w", err)
	}
	c, err := decodeClientRecord(t, id, data)
	if err != nil {
		slog.Error("m2m client record does not decode", "pkg", "auth", "tenant", string(t), "kvKey", id)
		return nil, nil, false, err
	}
	return c, data, true, nil
}

// burnBcrypt compares against the dummy hash so a request with no usable
// client costs what a wrong secret costs.
func burnBcrypt(secret string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(secret)) }

// Authenticate returns the client whose id and secret match. Every request
// that reaches a decision makes two KV reads and one bcrypt comparison, so
// the server's own work does not depend on whether the id exists. A backend
// can take longer to read a present key than a missing one (on cassandra a
// hit is two queries and a miss one), which can let a caller who already
// holds an id confirm that it exists. Client ids are not secret (a token's
// sub) and generated ids are 80-bit random, so this does not allow
// enumeration. An id outside the grammar makes no read, so it never reaches
// the store or its error text.
// ErrInvalidClient: no such client or wrong secret. Any other error is the
// store failing.
func (s *KVM2MClientStore) Authenticate(ctx context.Context, clientID, secret string) (*M2MClient, error) {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		burnBcrypt(secret)
		return nil, ErrInvalidClient
	}
	tenant, found, err := s.getIndex(ctx, clientID)
	if err != nil {
		return nil, err
	}
	var c *M2MClient
	if found {
		c, _, _, err = s.getRecord(ctx, tenant, clientID)
	} else {
		_, err = s.kv.Get(ctx, m2mClientIndexNamespace, m2mDecoyKey)
		if errors.Is(err, spi.ErrNotFound) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("failed to read m2m client index: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	if c == nil {
		burnBcrypt(secret)
		return nil, ErrInvalidClient
	}
	if bcrypt.CompareHashAndPassword([]byte(c.HashedSecret), []byte(secret)) != nil {
		return nil, ErrInvalidClient
	}
	return c, nil
}

// Create adds a client and returns its plaintext secret, once. An id outside
// the grammar is ErrInvalidClient; an id that is taken, in any tenant, or
// whose index entry does not decode, is ErrM2MClientExists; a tenant at the
// cap is ErrM2MClientCapReached.
func (s *KVM2MClientStore) Create(ctx context.Context, tenant spi.TenantID, clientID, userID string, roles []string) (string, error) {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		return "", ErrInvalidClient
	}
	secret, err := GenerateSecret()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash secret: %w", err)
	}
	now := time.Now().UTC()
	rec, err := encodeClientRecord(&M2MClient{ClientID: clientID, HashedSecret: string(hash), TenantID: tenant, UserID: userID, Roles: append([]string(nil), roles...), CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return "", err
	}
	idx, err := encodeIndexEntry(tenant)
	if err != nil {
		return "", err
	}
	// The stripe lock is held through undoCreate too: a hung store can block
	// the creates of every tenant on this stripe for up to undoTimeout. That
	// is accepted — creates are rare admin calls, and a store that hangs
	// fails them anyway.
	mu := s.createLock(tenant)
	mu.Lock()
	defer mu.Unlock()
	if s.maxPerTenant > 0 {
		existing, err := s.kv.List(ctx, m2mTenantNamespace(tenant))
		if err != nil {
			return "", fmt.Errorf("failed to list m2m clients: %w", err)
		}
		if len(existing) >= s.maxPerTenant {
			return "", ErrM2MClientCapReached
		}
	}
	if _, err := s.kv.Get(ctx, m2mClientIndexNamespace, clientID); err == nil {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	} else if !errors.Is(err, spi.ErrNotFound) {
		return "", fmt.Errorf("failed to read m2m client index: %w", err)
	}
	// A failed write may have committed, so each failure undoes what this
	// call may have written. A failed record write leaves the index entry
	// alone: this call did not write it, and it may name another tenant.
	if err := s.kv.Put(ctx, m2mTenantNamespace(tenant), clientID, rec); err != nil {
		s.undoCreate(ctx, clientID, m2mTenantNamespace(tenant))
		return "", fmt.Errorf("failed to write m2m client: %w", err)
	}
	if err := s.kv.Put(ctx, m2mClientIndexNamespace, clientID, idx); err != nil {
		s.undoCreate(ctx, clientID, m2mClientIndexNamespace, m2mTenantNamespace(tenant))
		return "", fmt.Errorf("failed to write m2m client index: %w", err)
	}
	return secret, nil
}

// undoContext derives the context of a compensating write: one the caller
// cannot cancel, bounded by undoTimeout.
func undoContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
}

// undoCreate deletes id from each of namespaces, in order, on a context the
// caller cannot cancel. A delete that fails is logged at ERROR.
func (s *KVM2MClientStore) undoCreate(ctx context.Context, id string, namespaces ...string) {
	uctx, cancel := undoContext(ctx)
	defer cancel()
	for _, ns := range namespaces {
		if err := s.kv.Delete(uctx, ns, id); err != nil {
			slog.Error("m2m client create could not be undone", "pkg", "auth", "namespace", ns, "kvKey", id, "error", err.Error())
		}
	}
}

// List returns tenant's clients, sorted by id. Undecodable records are
// skipped and logged at ERROR with their keys.
func (s *KVM2MClientStore) List(ctx context.Context, tenant spi.TenantID) ([]*M2MClient, error) {
	entries, err := s.kv.List(noTx(ctx), m2mTenantNamespace(tenant))
	if err != nil {
		return nil, fmt.Errorf("failed to list m2m clients: %w", err)
	}
	out := make([]*M2MClient, 0, len(entries))
	var bad []string
	for k, data := range entries {
		c, err := decodeClientRecord(tenant, k, data)
		if err != nil {
			bad = append(bad, k)
			continue
		}
		out = append(out, c)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		slog.Error("m2m client records do not decode and are not listed", "pkg", "auth", "tenant", string(tenant), "kvKeys", bad)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out, nil
}

// Delete removes tenant's client id: the index entry if it names tenant,
// then the record if present — decodable or not; tenant's namespace proves
// ownership. That includes an index entry that does not decode: if tenant's
// namespace holds a record for id (decodable or not), the damaged entry is
// removed along with the record. A mere read failure on the index entry is
// not the same as an undecodable one — it carries no evidence of what the
// entry names — so it never takes that path, even with a record present:
// the read failure is returned and nothing is touched. Without a record in
// tenant's namespace, ownership cannot be proven either way, so the index
// entry — which may name another tenant — is left untouched and the read
// failure is returned. An index entry naming another tenant is never
// touched. The caller has checked clientID against the client-id grammar.
func (s *KVM2MClientStore) Delete(ctx context.Context, tenant spi.TenantID, clientID string) error {
	ctx = noTx(ctx)
	_, err := s.kv.Get(ctx, m2mTenantNamespace(tenant), clientID)
	recPresent := err == nil
	if err != nil && !errors.Is(err, spi.ErrNotFound) {
		return fmt.Errorf("failed to read m2m client: %w", err)
	}
	idxTenant, idxFound, err := s.getIndex(ctx, clientID)
	idxOurs := idxFound && idxTenant == tenant
	if err != nil {
		if !recPresent || !errors.Is(err, errM2MUndecodable) {
			return err
		}
		// The index entry does not decode, but tenant's own namespace holds a
		// record for this id: the namespace proves ownership, the same rule
		// already applied to a damaged record.
		idxOurs = true
	}
	if !recPresent && !idxOurs {
		return fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	if idxOurs {
		if err := s.kv.Delete(ctx, m2mClientIndexNamespace, clientID); err != nil {
			return fmt.Errorf("failed to delete m2m client index entry: %w", err)
		}
	}
	if recPresent {
		if err := s.kv.Delete(ctx, m2mTenantNamespace(tenant), clientID); err != nil {
			return fmt.Errorf("failed to delete m2m client: %w", err)
		}
	}
	return nil
}

// ResetSecret gives an existing client of tenant a new secret and returns it,
// once, with the client. The secret is hashed before the store is read, so
// the gap between read and write is one round trip. A failed write may have
// committed, so it restores the record it read, on a context the caller
// cannot cancel: a failed reset leaves the old secret in force, unless the
// restore itself fails (the stored secret may then be the new one, never
// returned) or lands after a later successful reset (the old secret then
// replaces that reset's). The caller has checked clientID against the
// client-id grammar.
func (s *KVM2MClientStore) ResetSecret(ctx context.Context, tenant spi.TenantID, clientID string) (string, *M2MClient, error) {
	ctx = noTx(ctx)
	secret, err := GenerateSecret()
	if err != nil {
		return "", nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", nil, fmt.Errorf("failed to hash secret: %w", err)
	}
	c, prev, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return "", nil, err
	}
	// Without a record in tenant's namespace no reset can succeed, so the
	// index entry — which may name another tenant — is not read.
	if !found {
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	idxTenant, idxFound, err := s.getIndex(ctx, clientID)
	if err != nil {
		return "", nil, err
	}
	if !idxFound || idxTenant != tenant {
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c.HashedSecret, c.UpdatedAt = string(hash), time.Now().UTC()
	rec, err := encodeClientRecord(c)
	if err != nil {
		return "", nil, err
	}
	if err := s.kv.Put(ctx, m2mTenantNamespace(tenant), clientID, rec); err != nil {
		s.undoReset(ctx, tenant, clientID, prev)
		return "", nil, fmt.Errorf("failed to write m2m client: %w", err)
	}
	return secret, c, nil
}

// undoReset writes back prev, the record a failed reset read, on a context
// the caller cannot cancel. A write that fails is logged at ERROR.
func (s *KVM2MClientStore) undoReset(ctx context.Context, t spi.TenantID, id string, prev []byte) {
	uctx, cancel := undoContext(ctx)
	defer cancel()
	if err := s.kv.Put(uctx, m2mTenantNamespace(t), id, prev); err != nil {
		slog.Error("m2m client secret reset could not be undone", "pkg", "auth", "tenant", string(t), "kvKey", id, "error", err.Error())
	}
}
