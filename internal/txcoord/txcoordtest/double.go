// Package txcoordtest is a test double for the optional SPI capability
// spi.TransactionCoordinationCapable: a store factory over the memory plugin
// whose transactions are coordinated by a spi.TransactionCoordinator that
// follows the SPI's contract text. No in-tree plugin offers the capability;
// the double is how cyoda-go's side of it is tested. Production code never
// imports this package.
//
// Every scope keeps its writes to itself until it completes: a write made on
// a scope's context is held in the scope's own buffer, and reads on that
// context see the buffers of the scope and its ancestors over the
// transaction. CompleteScope merges the buffer into the parent's, or, for a
// scope whose parent is the root, writes it into the memory transaction.
// DiscardScope drops the buffer. Scopes of one transaction may therefore be
// open at the same time, and two of them may write the same entity; the one
// that completes second, having written it from the same version, is refused
// with spi.ErrScopeConflict.
//
// Inside a scope the double serves the entity store's point operations (Save,
// CompareAndSave, SaveAll, Get, Exists, Delete). The other entity-store
// operations are served by the memory plugin only while the scope and its
// ancestors hold no write of their own; otherwise they are refused rather than
// answered without those writes. Outside a scope the double is the memory
// plugin unchanged.
package txcoordtest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// NewFactory wraps inner, a memory plugin factory, with a transaction
// coordinator.
func NewFactory(inner spi.StoreFactory) spi.StoreFactory {
	return &factory{StoreFactory: inner, co: &coordinator{inner: inner, txs: make(map[string]*txState)}}
}

type factory struct {
	spi.StoreFactory
	co *coordinator
}

// TransactionCoordinator implements spi.TransactionCoordinationCapable.
func (f *factory) TransactionCoordinator(context.Context) (spi.TransactionCoordinator, error) {
	return f.co, nil
}

// EntityStore returns the memory plugin's store for a context on no scope,
// and the scope-aware store otherwise.
func (f *factory) EntityStore(ctx context.Context) (spi.EntityStore, error) {
	es, err := f.StoreFactory.EntityStore(ctx)
	if err != nil {
		return nil, err
	}
	if f.co.scopeOn(ctx) == nil {
		return es, nil
	}
	return &scopedStore{inner: es, co: f.co}, nil
}

// errUnsupported refuses an entity-store operation the double cannot answer
// with a scope's own writes in it.
var errUnsupported = errors.New("the transaction-coordination test double does not serve this operation inside a scope that holds writes")

type coordinator struct {
	inner spi.StoreFactory

	mu   sync.Mutex
	txs  map[string]*txState
	next uint64 // the last write token issued
}

type txState struct {
	id       string
	root     *scope
	callouts map[string]*callout
	sealed   bool
	// completed records, for every completed scope still in the tree, the
	// entities it wrote and the versions it wrote them from.
	completed []record
}

type callout struct {
	scope    *scope
	advanced bool
	ended    bool
	major    uint32
	minor    uint32 // the highest minor seen at major
}

type scope struct {
	tx     *txState
	parent *scope // nil for the root
	pairs  []spi.CalloutPair
	marked bool
	ended  bool
	writes map[string]*write // by entity id: the scope's own and its completed children's
}

type write struct {
	entity *spi.Entity // nil: deleted
	from   string      // the version the write was made from
	token  string      // the version the write makes
	by     *scope
}

type record struct {
	id, from string
	by       *scope
}

type scopeKey struct{}

// scopeOn returns the scope ctx carries when it belongs to ctx's transaction,
// nil otherwise (ctx is on the root, or on no transaction).
func (c *coordinator) scopeOn(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	tx := spi.GetTransaction(ctx)
	if s == nil || tx == nil || s.tx.id != tx.ID {
		return nil
	}
	return s
}

// txLocked returns txID's state, creating it. mu must be held.
func (c *coordinator) txLocked(txID string) *txState {
	tx := c.txs[txID]
	if tx == nil {
		tx = &txState{id: txID, callouts: make(map[string]*callout)}
		tx.root = &scope{tx: tx}
		c.txs[txID] = tx
	}
	return tx
}

// at returns the scope on ctx: its own scope, or the root of its
// transaction. mu must be held.
func (c *coordinator) atLocked(ctx context.Context) (*scope, error) {
	if s := c.scopeOn(ctx); s != nil {
		return s, nil
	}
	tx := spi.GetTransaction(ctx)
	if tx == nil {
		return nil, errors.New("the context carries no transaction")
	}
	return c.txLocked(tx.ID).root, nil
}

// fromLocked returns the scope ctx carries when it belongs to txID, its root
// otherwise. mu must be held.
func (c *coordinator) fromLocked(ctx context.Context, txID string) *scope {
	if s, _ := ctx.Value(scopeKey{}).(*scope); s != nil && s.tx.id == txID {
		return s
	}
	return c.txLocked(txID).root
}

// currentLocked reports whether every pair is current. mu must be held.
func currentLocked(tx *txState, pairs []spi.CalloutPair) bool {
	for _, p := range pairs {
		co := tx.callouts[p.Callout]
		if co == nil || co.ended || !co.advanced || co.major != p.Major || p.Minor < co.minor {
			return false
		}
	}
	return true
}

// supersededLocked reports whether a pair of s, or of an ancestor, is not
// current, or the transaction is sealed. mu must be held.
func (s *scope) supersededLocked() bool {
	if s.tx.sealed && s.parent != nil {
		return true
	}
	for n := s; n != nil; n = n.parent {
		if !currentLocked(n.tx, n.pairs) {
			return true
		}
	}
	return false
}

// markedLocked reports whether s or an ancestor is marked. mu must be held.
func (s *scope) markedLocked() bool {
	for n := s; n != nil; n = n.parent {
		if n.marked {
			return true
		}
	}
	return false
}

// under reports whether s is a or a descendant of a.
func (s *scope) under(a *scope) bool {
	for n := s; n != nil; n = n.parent {
		if n == a {
			return true
		}
	}
	return false
}

// BeginCallout implements spi.TransactionCoordinator.
func (c *coordinator) BeginCallout(ctx context.Context, txID, calloutID string, outer []spi.CalloutPair) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := c.txLocked(txID)
	if !currentLocked(tx, outer) {
		return spi.ErrCalloutSuperseded
	}
	tx.callouts[calloutID] = &callout{scope: c.fromLocked(ctx, txID)}
	return nil
}

// AdvanceCallout implements spi.TransactionCoordinator.
func (c *coordinator) AdvanceCallout(_ context.Context, txID, calloutID string, major uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.txLocked(txID).callouts[calloutID]
	if co == nil || co.ended || (co.advanced && major <= co.major) {
		return nil
	}
	co.advanced, co.major, co.minor = true, major, 0
	return nil
}

// EndCallout implements spi.TransactionCoordinator.
func (c *coordinator) EndCallout(_ context.Context, txID, calloutID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if co := c.txLocked(txID).callouts[calloutID]; co != nil {
		co.ended = true
	}
	return nil
}

// OpenScope implements spi.TransactionCoordinator.
func (c *coordinator) OpenScope(ctx context.Context, txID string, pairs []spi.CalloutPair) (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := c.txLocked(txID)
	if len(pairs) == 0 || tx.sealed || !currentLocked(tx, pairs) {
		return ctx, spi.ErrCalloutSuperseded
	}
	parent := tx.callouts[pairs[0].Callout].scope
	if parent.markedLocked() {
		return ctx, spi.ErrTxRollbackOnly
	}
	for _, p := range pairs {
		if co := tx.callouts[p.Callout]; p.Minor > co.minor {
			co.minor = p.Minor
		}
	}
	s := &scope{tx: tx, parent: parent, pairs: append([]spi.CalloutPair(nil), pairs...), writes: make(map[string]*write)}
	return context.WithValue(ctx, scopeKey{}, s), nil
}

// OpenChildScope implements spi.TransactionCoordinator.
func (c *coordinator) OpenChildScope(ctx context.Context) (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parent, err := c.atLocked(ctx)
	if err != nil {
		return ctx, fmt.Errorf("failed to open a child scope: %w", err)
	}
	s := &scope{tx: parent.tx, parent: parent, writes: make(map[string]*write)}
	return context.WithValue(ctx, scopeKey{}, s), nil
}

// CheckScope implements spi.TransactionCoordinator.
func (c *coordinator) CheckScope(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := c.atLocked(ctx)
	if err != nil {
		return fmt.Errorf("failed to check a scope: %w", err)
	}
	return s.refusalLocked()
}

// refusalLocked is CompleteScope's refusal of s, before the conflict check.
// mu must be held.
func (s *scope) refusalLocked() error {
	switch {
	case s.supersededLocked():
		return spi.ErrCalloutSuperseded
	case s.markedLocked():
		return spi.ErrTxRollbackOnly
	}
	return nil
}

// CompleteScope implements spi.TransactionCoordinator.
func (c *coordinator) CompleteScope(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.scopeOn(ctx)
	if s == nil {
		return errors.New("failed to complete a scope: the context carries none")
	}
	if s.ended {
		return errors.New("failed to complete a scope: it has ended")
	}
	if err := s.refusalLocked(); err != nil {
		s.discardLocked()
		return err
	}
	for id, w := range s.writes {
		for _, r := range s.tx.completed {
			if r.id == id && r.from == w.from && !r.by.under(s) {
				s.discardLocked()
				return spi.ErrScopeConflict
			}
		}
	}
	if s.parent.parent == nil {
		// The parent is the root: the writes go into the transaction.
		if err := c.applyLocked(ctx, s); err != nil {
			s.discardLocked()
			return fmt.Errorf("failed to complete a scope: %w", err)
		}
	} else {
		for id, w := range s.writes {
			s.parent.writes[id] = w
		}
	}
	for id, w := range s.writes {
		if w.by == s {
			s.tx.completed = append(s.tx.completed, record{id: id, from: w.from, by: s})
		}
	}
	s.ended = true
	return nil
}

// applyLocked writes s's buffer into the memory transaction ctx carries.
// mu must be held.
func (c *coordinator) applyLocked(ctx context.Context, s *scope) error {
	es, err := c.inner.EntityStore(ctx)
	if err != nil {
		return err
	}
	for id, w := range s.writes {
		if w.entity == nil {
			if err := es.Delete(ctx, id); err != nil && !errors.Is(err, spi.ErrNotFound) {
				return err
			}
			continue
		}
		if _, err := es.Save(ctx, w.entity); err != nil {
			return err
		}
	}
	return nil
}

// discardLocked drops s's writes, and forgets the completions of the scopes
// under it. mu must be held.
func (s *scope) discardLocked() {
	s.ended = true
	s.writes = make(map[string]*write)
	kept := s.tx.completed[:0]
	for _, r := range s.tx.completed {
		if !r.by.under(s) {
			kept = append(kept, r)
		}
	}
	s.tx.completed = kept
}

// DiscardScope implements spi.TransactionCoordinator. On a scope that has
// already ended (a refused completion discards it) it removes nothing more,
// and with rollbackOnly it still marks the parent.
func (c *coordinator) DiscardScope(ctx context.Context, rollbackOnly bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.scopeOn(ctx)
	if s == nil {
		return errors.New("failed to discard a scope: the context carries none")
	}
	s.discardLocked()
	if rollbackOnly {
		s.parent.marked = true
	}
	return nil
}

// Seal implements spi.TransactionCoordinator.
func (c *coordinator) Seal(_ context.Context, txID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := c.txLocked(txID)
	tx.sealed = true
	if tx.root.marked {
		return spi.ErrTxRollbackOnly
	}
	return nil
}

// --- the entity store inside a scope ---

type scopedStore struct {
	inner spi.EntityStore
	co    *coordinator
}

// pendingLocked returns the write to id the scope chain from s holds, nil when
// none does. mu must be held.
func pendingLocked(s *scope, id string) *write {
	for n := s; n != nil && n.parent != nil; n = n.parent {
		if w := n.writes[id]; w != nil {
			return w
		}
	}
	return nil
}

// cleanLocked reports whether no scope on the chain from s holds a write. mu
// must be held.
func cleanLocked(s *scope) bool {
	for n := s; n != nil && n.parent != nil; n = n.parent {
		if len(n.writes) > 0 {
			return false
		}
	}
	return true
}

// visibleLocked returns the entity id as s sees it (nil when absent) and the
// token of its version. mu must be held.
func (st *scopedStore) visibleLocked(ctx context.Context, s *scope, id string) (*spi.Entity, string, error) {
	if w := pendingLocked(s, id); w != nil {
		return w.entity, w.token, nil
	}
	e, err := st.inner.Get(ctx, id)
	switch {
	case errors.Is(err, spi.ErrNotFound):
		return nil, "absent", nil
	case err != nil:
		return nil, "", err
	}
	return e, "stored:" + strconv.FormatInt(e.Meta.Version, 10) + ":" + e.Meta.TransactionID + ":" + e.Meta.LastModifiedDate.String(), nil
}

// putLocked records a write of id (entity nil: a delete) on s. mu must be held.
func (st *scopedStore) putLocked(ctx context.Context, s *scope, id string, entity *spi.Entity) (int64, error) {
	if s.ended {
		return 0, errors.New("the scope has ended")
	}
	seen, from, err := st.visibleLocked(ctx, s, id)
	if err != nil {
		return 0, err
	}
	if own := s.writes[id]; own != nil && own.by == s {
		from = own.from // the scope's own earlier write: still from the version it first wrote over
	}
	var version int64 = 1
	if seen != nil {
		version = seen.Meta.Version + 1
	}
	var stored *spi.Entity
	if entity != nil {
		cp := *entity
		cp.Data = append([]byte(nil), entity.Data...)
		cp.Meta.Version = version
		cp.Meta.TransactionID = s.tx.id
		stored = &cp
	}
	st.co.next++
	s.writes[id] = &write{entity: stored, from: from, token: "pending:" + strconv.FormatUint(st.co.next, 10), by: s}
	return version, nil
}

func (st *scopedStore) Save(ctx context.Context, entity *spi.Entity) (int64, error) {
	st.co.mu.Lock()
	defer st.co.mu.Unlock()
	s := st.co.scopeOn(ctx)
	if s == nil {
		return st.inner.Save(ctx, entity)
	}
	return st.putLocked(ctx, s, entity.Meta.ID, entity)
}

func (st *scopedStore) CompareAndSave(ctx context.Context, entity *spi.Entity, expectedTxID string) (int64, error) {
	st.co.mu.Lock()
	defer st.co.mu.Unlock()
	s := st.co.scopeOn(ctx)
	if s == nil {
		return st.inner.CompareAndSave(ctx, entity, expectedTxID)
	}
	seen, _, err := st.visibleLocked(ctx, s, entity.Meta.ID)
	if err != nil {
		return 0, err
	}
	if seen == nil {
		return 0, spi.ErrNotFound
	}
	if seen.Meta.TransactionID != expectedTxID {
		return 0, spi.ErrConflict
	}
	return st.putLocked(ctx, s, entity.Meta.ID, entity)
}

func (st *scopedStore) SaveAll(ctx context.Context, entities iter.Seq[*spi.Entity]) ([]int64, error) {
	if st.co.scopeOn(ctx) == nil {
		return st.inner.SaveAll(ctx, entities)
	}
	var versions []int64
	for e := range entities {
		v, err := st.Save(ctx, e)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, nil
}

func (st *scopedStore) Get(ctx context.Context, entityID string) (*spi.Entity, error) {
	st.co.mu.Lock()
	defer st.co.mu.Unlock()
	s := st.co.scopeOn(ctx)
	if s == nil {
		return st.inner.Get(ctx, entityID)
	}
	e, _, err := st.visibleLocked(ctx, s, entityID)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, spi.ErrNotFound
	}
	cp := *e
	cp.Data = append([]byte(nil), e.Data...)
	return &cp, nil
}

func (st *scopedStore) Exists(ctx context.Context, entityID string) (bool, error) {
	_, err := st.Get(ctx, entityID)
	switch {
	case errors.Is(err, spi.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

func (st *scopedStore) Delete(ctx context.Context, entityID string) error {
	st.co.mu.Lock()
	defer st.co.mu.Unlock()
	s := st.co.scopeOn(ctx)
	if s == nil {
		return st.inner.Delete(ctx, entityID)
	}
	seen, _, err := st.visibleLocked(ctx, s, entityID)
	if err != nil {
		return err
	}
	if seen == nil {
		return spi.ErrNotFound
	}
	_, err = st.putLocked(ctx, s, entityID, nil)
	return err
}

// served reports whether the memory plugin may answer an operation on ctx:
// outside a scope, or inside one whose chain holds no write.
func (st *scopedStore) served(ctx context.Context) bool {
	st.co.mu.Lock()
	defer st.co.mu.Unlock()
	s := st.co.scopeOn(ctx)
	return s == nil || cleanLocked(s)
}

func (st *scopedStore) GetAsAt(ctx context.Context, entityID string, asAt time.Time) (*spi.Entity, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.GetAsAt(ctx, entityID, asAt)
}

func (st *scopedStore) DeleteAll(ctx context.Context, modelRef spi.ModelRef) error {
	if st.co.scopeOn(ctx) != nil {
		return errUnsupported // its deletes would bypass the scope's buffer
	}
	return st.inner.DeleteAll(ctx, modelRef)
}

func (st *scopedStore) Count(ctx context.Context, modelRef spi.ModelRef, asAt *time.Time) (int64, error) {
	if !st.served(ctx) {
		return 0, errUnsupported
	}
	return st.inner.Count(ctx, modelRef, asAt)
}

func (st *scopedStore) CountByState(ctx context.Context, modelRef spi.ModelRef, states []string, asAt *time.Time) (map[string]int64, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.CountByState(ctx, modelRef, states, asAt)
}

func (st *scopedStore) GetPage(ctx context.Context, modelRef spi.ModelRef, limit, offset int, asAt *time.Time) ([]*spi.Entity, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.GetPage(ctx, modelRef, limit, offset, asAt)
}

func (st *scopedStore) GetVersionByTransaction(ctx context.Context, entityID, txID string) (*spi.EntityVersion, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.GetVersionByTransaction(ctx, entityID, txID)
}

func (st *scopedStore) GetVersionMetadata(ctx context.Context, entityID string, opts spi.VersionMetadataOptions) ([]spi.EntityVersionMeta, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.GetVersionMetadata(ctx, entityID, opts)
}

func (st *scopedStore) Search(ctx context.Context, filter spi.Filter, opts spi.SearchOptions) ([]*spi.Entity, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.Search(ctx, filter, opts)
}

func (st *scopedStore) Iterate(ctx context.Context, model spi.ModelRef, filter spi.Filter, opts spi.IterateOptions) (spi.Iterator, error) {
	if !st.served(ctx) {
		return nil, errUnsupported
	}
	return st.inner.Iterate(ctx, model, filter, opts)
}
