package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// tenantLockStripes bounds the memory of the per-tenant locks of the M2M
// client and trusted-key stores: tenants hash onto a fixed set of mutexes.
const tenantLockStripes = 64

// tenantStripe is the index of t's lock among tenantLockStripes.
func tenantStripe(t spi.TenantID) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(t))
	return h.Sum32() % tenantLockStripes
}

// undoTimeout bounds the compensation of a failed create or reset.
const undoTimeout = 30 * time.Second

// KVM2MClientStore stores M2M clients in the SYSTEM-tenant KV store, one
// namespace per tenant, keyed by client id: a client is found by (tenant,
// client id). There is no node copy: every call reads or writes the store, so
// a change is in force on every node when the call returns.
// Every call strips any transaction from its context: the postgres KV store
// joins a transaction it finds there, and a client change must never ride on
// a caller's entity transaction.
//
// Every write is conditional on the state this call read or wrote
// (spi.KeyValueStore), so concurrent changes on any nodes resolve without a
// lost update: of two creates of one id exactly one succeeds; a reset that
// loses to another change is ErrM2MClientChanged; a delete always wins; and
// every undo of a failed write touches only this call's own bytes (a record
// carries a fresh bcrypt salt, so no two writes are equal). The cap is
// checked on each node before the write, so concurrent creates on several
// nodes can exceed it by one client per node.
//
// Authenticate keeps a per-node cache of verified secrets. Every bcrypt
// operation runs in one of a bounded number of slots.
type KVM2MClientStore struct {
	kv           spi.KeyValueStore
	maxPerTenant int
	createLocks  [tenantLockStripes]sync.Mutex
	verified     *verifiedSecretCache
	slots        *secretSlots
}

// NewKVM2MClientStore returns a store over kv. maxPerTenant <= 0: no cap.
// limit bounds the bcrypt comparisons Authenticate runs at once.
func NewKVM2MClientStore(kv spi.KeyValueStore, maxPerTenant int, limit SecretCheckLimit) *KVM2MClientStore {
	return &KVM2MClientStore{
		kv:           kv,
		maxPerTenant: maxPerTenant,
		verified:     newVerifiedSecretCache(maxVerifiedSecrets),
		slots:        newSecretSlots(limit),
	}
}

// noTx removes any transaction from ctx.
func noTx(ctx context.Context) context.Context { return spi.WithTransaction(ctx, nil) }

func (s *KVM2MClientStore) createLock(t spi.TenantID) *sync.Mutex {
	return &s.createLocks[tenantStripe(t)]
}

// getRecord reads and decodes (t, id), and returns the stored bytes with it.
// found=false: absent.
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

// burnBcrypt compares against the dummy hash, in a slot, so a request with
// no usable client costs what a wrong secret costs.
func (s *KVM2MClientStore) burnBcrypt(ctx context.Context, secret string) error {
	return s.slots.run(ctx, func() { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(secret)) })
}

// Authenticate returns tenant's client whose id and secret match. An id
// outside the client-id grammar is ErrInvalidClient at once, with no read and
// no bcrypt: the grammar is public, so the refusal reveals nothing. Any other
// request reads (tenant, id) once and makes one bcrypt comparison — against
// the record's hash, or against a dummy hash when there is no record — in one
// of the node's slots, unless the node's verified-secret cache matches the
// record just read.
//
// ErrInvalidClient: no such client in tenant, or a wrong secret.
// ErrSecretCheckBusy: no slot freed up within the wait. Any other error is
// the store failing.
func (s *KVM2MClientStore) Authenticate(ctx context.Context, tenant spi.TenantID, clientID, secret string) (*M2MClient, error) {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		return nil, ErrInvalidClient
	}
	key := clientKey{tenant, clientID}
	c, _, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return nil, err
	}
	if !found {
		s.verified.drop(key)
		if err := s.burnBcrypt(ctx, secret); err != nil {
			return nil, err
		}
		return nil, ErrInvalidClient
	}
	sum := sha256.Sum256([]byte(secret))
	if s.verified.hit(key, c.HashedSecret, sum) {
		return c, nil
	}
	var mismatch error
	if err := s.slots.run(ctx, func() {
		mismatch = bcrypt.CompareHashAndPassword([]byte(c.HashedSecret), []byte(secret))
	}); err != nil {
		return nil, err
	}
	// A wrong secret leaves the cached entry alone: it can never hit (the
	// sums differ), and anyone holding the public client id could otherwise
	// evict the client's warm entry.
	if mismatch != nil {
		return nil, ErrInvalidClient
	}
	s.verified.put(key, c.HashedSecret, sum)
	return c, nil
}

// newSecret generates a secret and hashes it in one of the node's
// secret-check slots. No free slot within the wait: ErrSecretCheckBusy.
func (s *KVM2MClientStore) newSecret(ctx context.Context) (string, []byte, error) {
	secret, err := GenerateSecret()
	if err != nil {
		return "", nil, err
	}
	var hash []byte
	var hashErr error
	if err := s.slots.run(ctx, func() {
		hash, hashErr = bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	}); err != nil {
		return "", nil, err
	}
	if hashErr != nil {
		return "", nil, fmt.Errorf("failed to hash secret: %w", hashErr)
	}
	return secret, hash, nil
}

// Lookup returns tenant's client clientID as the store holds it now, without
// checking a secret. ErrM2MClientNotFound: outside the grammar, or no record.
// Any other error is the store failing, wrapped so a storage-unavailable one
// keeps its marker.
func (s *KVM2MClientStore) Lookup(ctx context.Context, tenant spi.TenantID, clientID string) (*M2MClient, error) {
	ctx = noTx(ctx)
	if !ValidClientID(clientID) {
		return nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c, _, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	return c, nil
}

// newGeneration returns a new client's first secret generation: random in
// [1, 2^52], so a client re-created under a deleted client's id never shares
// its generation, and a token of the deleted client never opens a stream.
func newGeneration() (uint64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<52))
	if err != nil {
		return 0, fmt.Errorf("failed to draw a secret generation: %w", err)
	}
	return n.Uint64() + 1, nil
}

// Create adds tenant's client clientID and returns its plaintext secret, once.
// The id is taken (ErrM2MClientExists) when a record exists for it in tenant,
// decodable or not, or when another create of it, on any node, wrote first.
// A tenant at the cap is ErrM2MClientCapReached; a taken id is reported
// before the cap. The client's user id is its client id. The secret is
// hashed in a secret-check slot first; with none free the result is
// ErrSecretCheckBusy and nothing is written. A write whose outcome is
// unknown is undone by deleting exactly the bytes this call wrote, so
// another call's client is never removed.
func (s *KVM2MClientStore) Create(ctx context.Context, tenant spi.TenantID, clientID string, roles []string, onBehalfOf bool) (string, error) {
	ctx = noTx(ctx)
	secret, hash, err := s.newSecret(ctx)
	if err != nil {
		return "", err
	}
	gen, err := newGeneration()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	rec, err := encodeClientRecord(&M2MClient{ClientID: clientID, HashedSecret: string(hash), TenantID: tenant, UserID: clientID, Roles: append([]string(nil), roles...), OnBehalfOf: onBehalfOf, SecretGen: gen, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return "", err
	}
	ns := m2mTenantNamespace(tenant)
	// The stripe lock is held through the undo too: a hung store can block
	// the creates of every tenant on this stripe for up to undoTimeout.
	mu := s.createLock(tenant)
	mu.Lock()
	defer mu.Unlock()
	if _, err := s.kv.Get(ctx, ns, clientID); err == nil {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	} else if !errors.Is(err, spi.ErrNotFound) {
		return "", fmt.Errorf("failed to read m2m client: %w", err)
	}
	if s.maxPerTenant > 0 {
		existing, err := s.kv.List(ctx, ns)
		if err != nil {
			return "", fmt.Errorf("failed to list m2m clients: %w", err)
		}
		if len(existing) >= s.maxPerTenant {
			return "", ErrM2MClientCapReached
		}
	}
	applied, err := s.kv.PutIfAbsent(ctx, ns, clientID, rec)
	if err != nil {
		s.undo(ctx, tenant, clientID, "create", func(uctx context.Context) (bool, error) {
			return s.kv.DeleteIfEqual(uctx, ns, clientID, rec)
		})
		return "", fmt.Errorf("failed to write m2m client: %w", err)
	}
	if !applied {
		return "", fmt.Errorf("%w: %s", ErrM2MClientExists, clientID)
	}
	return secret, nil
}

// undoContext derives the context of a compensating write: one the caller
// cannot cancel, bounded by undoTimeout.
func undoContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
}

// undo runs a compensating conditional write on a context the caller cannot
// cancel, bounded by undoTimeout. A failure is logged at ERROR; a write that
// is not applied needs no log — this call's write did not land, or was
// already replaced.
func (s *KVM2MClientStore) undo(ctx context.Context, t spi.TenantID, id, op string, write func(context.Context) (bool, error)) {
	uctx, cancel := undoContext(ctx)
	defer cancel()
	if _, err := write(uctx); err != nil {
		slog.Error("m2m client "+op+" could not be undone", "pkg", "auth", "tenant", string(t), "kvKey", id, "error", err.Error())
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

// Delete removes tenant's client clientID, decodable or not.
// ErrM2MClientNotFound when tenant has no such record. A delete always wins:
// a concurrent reset's conditional write then fails. The caller has checked
// clientID against the client-id grammar.
func (s *KVM2MClientStore) Delete(ctx context.Context, tenant spi.TenantID, clientID string) error {
	ctx = noTx(ctx)
	ns := m2mTenantNamespace(tenant)
	if _, err := s.kv.Get(ctx, ns, clientID); errors.Is(err, spi.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	} else if err != nil {
		return fmt.Errorf("failed to read m2m client: %w", err)
	}
	if err := s.kv.Delete(ctx, ns, clientID); err != nil {
		return fmt.Errorf("failed to delete m2m client: %w", err)
	}
	return nil
}

// ResetSecret gives tenant's client clientID a new secret and returns it,
// once, with the client. The secret is hashed in a secret-check slot before
// the store is read (none free: ErrSecretCheckBusy, nothing read or
// written). The write is conditional on the record read:
// ErrM2MClientNotFound if the client was deleted meanwhile, and
// ErrM2MClientChanged if another change replaced it. A write whose outcome
// is unknown is undone by restoring the read record, conditional on this
// call's own bytes, so it never revives a deleted client or overwrites a
// later change. The caller has checked clientID against the grammar.
func (s *KVM2MClientStore) ResetSecret(ctx context.Context, tenant spi.TenantID, clientID string) (string, *M2MClient, error) {
	ctx = noTx(ctx)
	secret, hash, err := s.newSecret(ctx)
	if err != nil {
		return "", nil, err
	}
	c, prev, found, err := s.getRecord(ctx, tenant, clientID)
	if err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
	}
	c.HashedSecret, c.UpdatedAt = string(hash), time.Now().UTC()
	c.SecretGen++
	next, err := encodeClientRecord(c)
	if err != nil {
		return "", nil, err
	}
	ns := m2mTenantNamespace(tenant)
	applied, err := s.kv.CompareAndPut(ctx, ns, clientID, prev, next)
	if err != nil {
		s.undo(ctx, tenant, clientID, "secret reset", func(uctx context.Context) (bool, error) {
			return s.kv.CompareAndPut(uctx, ns, clientID, next, prev)
		})
		return "", nil, fmt.Errorf("failed to write m2m client: %w", err)
	}
	if !applied {
		if _, err := s.kv.Get(ctx, ns, clientID); errors.Is(err, spi.ErrNotFound) {
			return "", nil, fmt.Errorf("%w: %s", ErrM2MClientNotFound, clientID)
		} else if err != nil {
			return "", nil, fmt.Errorf("failed to read m2m client: %w", err)
		}
		return "", nil, fmt.Errorf("%w: %s", ErrM2MClientChanged, clientID)
	}
	return secret, c, nil
}
