package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

const defaultReconcileInterval = 60 * time.Second

// maxReconcileAttempts bounds the generation-guard retry of a re-read and of a
// single-record load. Only continuous local change can exhaust it.
const maxReconcileAttempts = 5

// stalenessMultiplier × interval is the fail-closed bound: a copy with no
// successful re-read for that long is stale.
const stalenessMultiplier = 10

// errorEscalationThreshold is the consecutive-failure count from which re-read
// failures log at ERROR instead of WARN.
const errorEscalationThreshold = 3

// errReconcileContention marks a re-read that gave up because local changes
// kept landing mid-rebuild. It is not a store failure.
var errReconcileContention = errors.New("re-read gave up after repeated mid-rebuild mutations")

// kvWrite is one step of a multi-record write. value nil deletes the key. prev
// is the key's bytes before the write, nil when it was absent; a failed
// multi-record write restores it.
type kvWrite struct {
	key   string
	value []byte
	prev  []byte
}

type replicaConfig[R any] struct {
	name      string // log prefix, e.g. "trusted-key"
	namespace string
	topic     string
	// decode turns one entry into the copy's key and record. ok=false skips
	// the entry (counted at load); err skips it with an ERROR on a re-read and
	// fails the initial load.
	decode      func(kvKey string, data []byte) (copyKey string, rec R, ok bool, err error)
	interval    time.Duration
	broadcaster spi.ClusterBroadcaster
	metrics     ReconcileMetrics
	// afterChange runs after every swap or apply, with the resulting copy
	// passed directly under a read lock (r.read(r.cfg.afterChange)) — never
	// by reaching back into the replica for it, so a caller that assigns its
	// own reference to the replica only after construction (e.g. KVKeyStore
	// assigning s.rep after newKVReplica returns) is never raced: a gossip
	// message delivered during Subscribe, before that assignment, still only
	// ever hands the callback a recs map, nothing that could be nil. It must
	// not call mutate or Reconcile on this replica: mutate holds adminMu and
	// Reconcile holds reconcileMu across the whole call including this
	// callback, so either would deadlock against itself. Like read's fn, it
	// must not retain recs beyond the call.
	afterChange func(recs map[string]R)
}

// kvReplica keeps a node copy of one KV namespace. A change message from a
// peer and the periodic re-read rebuild the copy; a local admin change updates
// it directly. Hot paths read the copy only. Admin changes run one at a time
// under the admin mutex and never hold the copy lock across a store call.
type kvReplica[R any] struct {
	cfg replicaConfig[R]
	kv  spi.KeyValueStore
	ctx context.Context // process lifetime: no cancellation, no deadline

	mu   sync.RWMutex // the copy lock
	recs map[string]R
	gen  atomic.Uint64 // bumped by every swap and apply

	adminMu     sync.Mutex
	reconcileMu sync.Mutex
	ping        coalescingRunner
	loopStarted atomic.Bool

	epoch    time.Time    // monotonic reference for staleness
	lastOK   atomic.Int64 // time.Since(epoch) at the last successful re-read
	failures atomic.Int64

	skippedAtLoad int
}

func newKVReplica[R any](ctx context.Context, kv spi.KeyValueStore, cfg replicaConfig[R]) (*kvReplica[R], error) {
	if cfg.interval <= 0 {
		cfg.interval = defaultReconcileInterval
	}
	if cfg.metrics == nil {
		cfg.metrics = NopReconcileMetrics{}
	}
	r := &kvReplica[R]{cfg: cfg, kv: kv, ctx: context.WithoutCancel(ctx), epoch: time.Now()}
	entries, err := kv.List(r.ctx, cfg.namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s records: %w", cfg.name, err)
	}
	recs, skipped, err := r.build(entries, true)
	if err != nil {
		return nil, err
	}
	r.recs, r.skippedAtLoad = recs, skipped
	r.stampOK()
	if cfg.broadcaster != nil {
		cfg.broadcaster.Subscribe(cfg.topic, r.handlePing)
	}
	return r, nil
}

func (r *kvReplica[R]) build(entries map[string][]byte, strict bool) (map[string]R, int, error) {
	recs := make(map[string]R, len(entries))
	skipped := 0
	for kvKey, data := range entries {
		k, rec, ok, err := r.cfg.decode(kvKey, data)
		if err != nil {
			if strict {
				return nil, 0, fmt.Errorf("failed to decode %s record %q: %w", r.cfg.name, kvKey, err)
			}
			slog.Error(r.cfg.name+" reconcile: skipping undeserializable record",
				"pkg", "auth", "kvKey", kvKey, "error", err.Error())
			continue
		}
		if !ok {
			skipped++
			continue
		}
		recs[k] = rec
	}
	return recs, skipped, nil
}

// read runs fn with the copy under the read lock. fn must not keep the map.
func (r *kvReplica[R]) read(fn func(recs map[string]R)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn(r.recs)
}

func (r *kvReplica[R]) stampOK() { r.lastOK.Store(int64(time.Since(r.epoch))) }

func (r *kvReplica[R]) age() time.Duration {
	return time.Since(r.epoch) - time.Duration(r.lastOK.Load())
}

// Stale reports whether the copy has had no successful re-read for the
// fail-closed bound. Always false until the loop has started.
func (r *kvReplica[R]) Stale() bool {
	return r.loopStarted.Load() && r.age() > stalenessMultiplier*r.cfg.interval
}

// Reconcile rebuilds the copy from the store. A rebuild that overlapped a
// local change is discarded and retried; a failed List keeps the old copy.
func (r *kvReplica[R]) Reconcile(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		gen := r.gen.Load()
		entries, err := r.kv.List(ctx, r.cfg.namespace)
		if err != nil {
			n := r.failures.Add(1)
			r.cfg.metrics.SetReconcileConsecutiveFailures(int(n))
			r.cfg.metrics.SetReconcileStalenessSeconds(r.age().Seconds())
			msg := r.cfg.name + " reconcile failed; serving last-known state until it succeeds"
			if n > errorEscalationThreshold {
				slog.Error(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
			} else {
				slog.Warn(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
			}
			return fmt.Errorf("failed to list %s records: %w", r.cfg.name, err)
		}
		fresh, _, _ := r.build(entries, false)
		swapped := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.gen.Load() != gen {
				return false
			}
			r.recs = fresh
			r.gen.Add(1)
			return true
		}()
		if swapped {
			r.stampOK()
			r.failures.Store(0)
			r.cfg.metrics.SetReconcileConsecutiveFailures(0)
			r.cfg.metrics.SetReconcileStalenessSeconds(0)
			if r.cfg.afterChange != nil {
				r.read(r.cfg.afterChange)
			}
			return nil
		}
	}
	stale := r.age().Seconds()
	r.cfg.metrics.SetReconcileStalenessSeconds(stale)
	msg := r.cfg.name + " reconcile: giving up after repeated mid-rebuild mutations"
	fields := []any{"pkg", "auth", "attempts", maxReconcileAttempts, "stalenessSeconds", int64(stale + 0.5)}
	if r.age() > (stalenessMultiplier/2)*r.cfg.interval {
		slog.Error(msg, fields...)
	} else {
		slog.Warn(msg, fields...)
	}
	return errReconcileContention
}

// handlePing runs on the broadcaster's receive goroutine: it must not block
// and must not panic. The payload is never read or logged.
func (r *kvReplica[R]) handlePing(_ []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error(r.cfg.name+" ping handler panic", "pkg", "auth", "panic", rec)
		}
	}()
	r.ping.Trigger(r.reconcileOnce)
}

// reconcileOnce is one bounded re-read on its own deadline, recover-wrapped:
// it runs detached from any caller that could handle a panic.
func (r *kvReplica[R]) reconcileOnce() {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error(r.cfg.name+" reconcile panic", "pkg", "auth", "panic", rec)
		}
	}()
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.interval)
	defer cancel()
	_ = r.Reconcile(ctx)
}

// Start runs the periodic re-read until ctx ends. Returns false if it already runs.
func (r *kvReplica[R]) Start(ctx context.Context) bool {
	if !r.loopStarted.CompareAndSwap(false, true) {
		return false
	}
	// A gap between construction and Start must not count toward staleness:
	// the loop's own clock starts now, not at construction time.
	r.stampOK()
	go func() {
		for {
			timer := time.NewTimer(jitteredInterval(r.cfg.interval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				r.reconcileOnce()
			}
		}
	}()
	return true
}

// jitteredInterval returns d × [0.9, 1.1).
func jitteredInterval(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

func (r *kvReplica[R]) broadcast() {
	if r.cfg.broadcaster != nil {
		r.cfg.broadcaster.Broadcast(r.cfg.topic, nil)
	}
}

// mutate runs one admin change on this node. fn runs under the admin mutex
// with no copy lock held: it reads and writes the store with the caller's
// context and returns the change to the copy (nil for none) and whether it
// wrote the store. The copy lock is held only while apply runs. A change
// message goes out whenever the store was written, even if fn then failed,
// so peers re-read what is actually stored.
func (r *kvReplica[R]) mutate(fn func() (apply func(recs map[string]R), wrote bool, err error)) error {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	apply, wrote, err := fn()
	if apply != nil {
		func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			apply(r.recs)
			r.gen.Add(1)
		}()
		if r.cfg.afterChange != nil {
			r.read(r.cfg.afterChange)
		}
	} else if wrote {
		// The store was written but fn had no direct patch for the copy
		// (a compensation, or another ambiguous partial write). Gossip never
		// delivers this node's own broadcast back to itself, so without a
		// local re-read this node would keep serving the old copy until the
		// periodic loop next runs. Trigger is non-blocking: it runs on its
		// own goroutine and never holds adminMu.
		r.ping.Trigger(r.reconcileOnce)
	}
	if wrote {
		r.broadcast()
	}
	return err
}

// loadOne reads one record through to the store and puts it in the copy,
// unless a change committed while it read — then it reads again, so an older
// read never overwrites a newer copy. found=false: absent or skipped.
func (r *kvReplica[R]) loadOne(ctx context.Context, kvKey string) (R, bool, error) {
	var zero R
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		gen := r.gen.Load()
		data, err := r.kv.Get(ctx, r.cfg.namespace, kvKey)
		if errors.Is(err, spi.ErrNotFound) {
			return zero, false, nil
		}
		if err != nil {
			return zero, false, fmt.Errorf("failed to read %s record: %w", r.cfg.name, err)
		}
		k, rec, ok, err := r.cfg.decode(kvKey, data)
		if err != nil {
			return zero, false, fmt.Errorf("failed to decode %s record: %w", r.cfg.name, err)
		}
		if !ok {
			return zero, false, nil
		}
		stored := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.gen.Load() != gen {
				return false
			}
			r.recs[k] = rec
			r.gen.Add(1)
			return true
		}()
		if stored {
			return rec, true, nil
		}
	}
	return zero, false, errReconcileContention
}

// writeAll applies writes in order. If one fails it restores that write
// itself plus every write already applied before it, newest first, and
// returns the error. The failing write can itself have partially or fully
// committed before reporting failure (e.g. a timeout) — restoring it too is
// what makes this correct: a prev-restore is idempotent and a delete of an
// absent key already tolerates ErrNotFound, so restoring a write that never
// actually landed costs nothing. Restores run on a context the caller cannot
// cancel; a restore that fails is logged at ERROR with every key left
// changed.
func (r *kvReplica[R]) writeAll(ctx context.Context, writes []kvWrite) error {
	for i, w := range writes {
		if err := r.put(ctx, w.key, w.value); err != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.interval)
			var stuck []string
			for j := i; j >= 0; j-- {
				if rerr := r.put(rctx, writes[j].key, writes[j].prev); rerr != nil {
					stuck = append(stuck, writes[j].key)
				}
			}
			cancel()
			if len(stuck) > 0 {
				slog.Error(r.cfg.name+" multi-record write failed and could not be fully undone",
					"pkg", "auth", "keysLeftChanged", stuck)
			}
			return fmt.Errorf("failed to write %s record %q: %w", r.cfg.name, w.key, err)
		}
	}
	return nil
}

func (r *kvReplica[R]) put(ctx context.Context, key string, value []byte) error {
	if value == nil {
		if err := r.kv.Delete(ctx, r.cfg.namespace, key); err != nil && !errors.Is(err, spi.ErrNotFound) {
			return err
		}
		return nil
	}
	return r.kv.Put(ctx, r.cfg.namespace, key, value)
}
