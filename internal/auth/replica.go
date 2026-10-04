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

// maxReconcileAttempts bounds the generation-guard retry of a re-read
// (Reconcile). Only continuous local change can exhaust it.
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
	name      string // log prefix, e.g. "signing-key"
	namespace string
	topic     string
	// decode turns one entry into the copy's key and record. It never fails:
	// a record that does not decode is still represented in the copy (the
	// signing-key store classifies it as broken, so its key is refused), and
	// one bad record never stops a node starting.
	decode      func(kvKey string, data []byte) (copyKey string, rec R)
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
	// ctx is this replica's one source of lifetime: Start selects on
	// r.ctx.Done() for its periodic loop, and reconcileOnce checks r.ctx.Err()
	// before every reconcile, triggered by either Start's loop or a gossip
	// ping. Canceling it therefore ends the periodic loop and turns any
	// later ping-triggered reconcile into a no-op — no store call, no
	// failure log. The broadcaster subscription set up below is never torn
	// down, so a ping can still arrive after ctx ends; that check in
	// reconcileOnce is what makes arriving safe. There is deliberately no
	// second ctx anywhere in this type: Start takes none, so the contract
	// that construction and the loop share one lifetime cannot be broken by
	// a caller passing them two different contexts.
	ctx context.Context

	mu   sync.RWMutex // the copy lock
	recs map[string]R
	gen  atomic.Uint64 // bumped by every swap and apply

	adminMu     sync.Mutex
	reconcileMu sync.Mutex
	ping        coalescingRunner
	loopStarted atomic.Bool

	// loopMu guards loopDone. Start sets it once, before its goroutine runs;
	// that goroutine closes it on exit. In the intended call pattern — start
	// the loop, later cancel r.ctx and Wait — Start happens-before Wait, so
	// Wait reads the channel Start set. Called concurrently with Start
	// instead, Wait can observe loopDone still nil and return at once,
	// without waiting for a loop that is only just starting.
	loopMu   sync.Mutex
	loopDone chan struct{}

	epoch    time.Time    // monotonic reference for staleness
	lastOK   atomic.Int64 // time.Since(epoch) at the last successful re-read
	failures atomic.Int64
}

func newKVReplica[R any](ctx context.Context, kv spi.KeyValueStore, cfg replicaConfig[R]) (*kvReplica[R], error) {
	if cfg.interval <= 0 {
		cfg.interval = defaultReconcileInterval
	}
	if cfg.metrics == nil {
		cfg.metrics = NopReconcileMetrics{}
	}
	r := &kvReplica[R]{cfg: cfg, kv: kv, ctx: ctx, epoch: time.Now()}
	entries, err := kv.List(r.ctx, cfg.namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s records: %w", cfg.name, err)
	}
	r.recs = r.build(entries)
	r.stampOK()
	if cfg.broadcaster != nil {
		cfg.broadcaster.Subscribe(cfg.topic, r.handlePing)
	}
	return r, nil
}

func (r *kvReplica[R]) build(entries map[string][]byte) map[string]R {
	recs := make(map[string]R, len(entries))
	for kvKey, data := range entries {
		k, rec := r.cfg.decode(kvKey, data)
		recs[k] = rec
	}
	return recs
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
			if r.ctx.Err() != nil {
				// The replica's OWN lifetime ended while the List call was
				// in flight — the owner tearing this replica down — not
				// just this call's ctx (which reconcileOnce derives from
				// r.ctx via a per-tick WithTimeout, and a direct caller may
				// pass anything). Mirrors reapExpiredSnapshotsTick's same
				// check, scoped to the right ctx: not a store failure, so
				// no failure count and no log line for it. Checking the
				// call's own ctx instead would also swallow a hung store
				// that merely outlasted its own deadline while r.ctx stayed
				// alive — exactly the failure an operator needs to see.
				return fmt.Errorf("failed to list %s records: %w", r.cfg.name, err)
			}
			r.logReconcileFailure(err)
			return fmt.Errorf("failed to list %s records: %w", r.cfg.name, err)
		}
		fresh := r.build(entries)
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

// logReconcileFailure records one failed re-read that was not caused by the
// reconcile's own ctx ending: bumps the consecutive-failure count and
// staleness metric, and logs at WARN, escalating to ERROR once the failure
// count passes errorEscalationThreshold.
func (r *kvReplica[R]) logReconcileFailure(err error) {
	n := r.failures.Add(1)
	r.cfg.metrics.SetReconcileConsecutiveFailures(int(n))
	r.cfg.metrics.SetReconcileStalenessSeconds(r.age().Seconds())
	msg := r.cfg.name + " reconcile failed; serving last-known state until it succeeds"
	if n > errorEscalationThreshold {
		slog.Error(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
	} else {
		slog.Warn(msg, "pkg", "auth", "consecutiveFailures", n, "error", err.Error())
	}
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
// it runs detached from any caller that could handle a panic. Triggered by
// either Start's periodic loop or a gossip ping (handlePing) — the ping path
// is wired at construction and outlives Start, so it can still fire after
// r.ctx ends (the subscription is never torn down). The check below is what
// makes that safe: once r.ctx is done there is nothing left to reconcile
// against, so this returns before making any store call at all — a ping
// after teardown is a silent no-op. Reconcile has its own, narrower check
// for the case where r.ctx ends while a reconcile it started is already
// mid-store-call.
func (r *kvReplica[R]) reconcileOnce() {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error(r.cfg.name+" reconcile panic", "pkg", "auth", "panic", rec)
		}
	}()
	if r.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.interval)
	defer cancel()
	_ = r.Reconcile(ctx)
}

// Start runs the periodic re-read until r.ctx ends. Returns false if it
// already runs. Wait blocks until the goroutine below has actually exited,
// so a caller that cancels r.ctx then Waits can rely on no further tick
// running afterward — including one against a store the caller is
// concurrently closing. Start takes no ctx of its own: r.ctx, set once at
// construction, is this replica's only lifetime source.
func (r *kvReplica[R]) Start() bool {
	if !r.loopStarted.CompareAndSwap(false, true) {
		return false
	}
	// A gap between construction and Start must not count toward staleness:
	// the loop's own clock starts now, not at construction time.
	r.stampOK()
	done := make(chan struct{})
	r.loopMu.Lock()
	r.loopDone = done
	r.loopMu.Unlock()
	go func() {
		defer close(done)
		for {
			timer := time.NewTimer(jitteredInterval(r.cfg.interval))
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				r.reconcileOnce()
			}
		}
	}()
	return true
}

// Wait blocks until the goroutine started by Start has exited (immediately
// if Start was never called), and then until any gossip-ping-triggered
// reconcile already in flight at that moment has also finished. Call it
// after cancelling r.ctx — reconcileOnce's own check then makes a ping that
// arrives after this returns a safe no-op, so Wait does not need to guard
// against one arriving later.
func (r *kvReplica[R]) Wait() {
	r.loopMu.Lock()
	done := r.loopDone
	r.loopMu.Unlock()
	if done != nil {
		<-done
	}
	r.ping.Wait()
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
//
// A re-read or single-record load can land while fn runs, after fn wrote the
// store: a peer's newer change then reaches the copy first, and apply writes
// this node's older value over it. Every other generation bump is excluded by
// the admin mutex, so a generation that moved by more than apply's own bump
// shows the overlap, and a re-read is scheduled to converge to the store.
func (r *kvReplica[R]) mutate(fn func() (apply func(recs map[string]R), wrote bool, err error)) error {
	r.adminMu.Lock()
	defer r.adminMu.Unlock()
	g0 := r.gen.Load()
	apply, wrote, err := fn()
	if apply != nil {
		overlapped := func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			apply(r.recs)
			return r.gen.Add(1) != g0+1
		}()
		if r.cfg.afterChange != nil {
			r.read(r.cfg.afterChange)
		}
		if overlapped {
			r.ping.Trigger(r.reconcileOnce)
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

// writeAll applies writes in order. If one fails it restores that write
// itself plus every write already applied before it, newest first, and
// returns the error. The failing write can itself have partially or fully
// committed before reporting failure (e.g. a timeout) — restoring it too is
// what makes this correct: a prev-restore is idempotent, and deleting a key
// that was never written or already deleted is a no-op by the storage-SPI
// contract, so restoring a write that never actually landed costs nothing.
// Restores run on a context the caller cannot
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
		return r.kv.Delete(ctx, r.cfg.namespace, key)
	}
	return r.kv.Put(ctx, r.cfg.namespace, key, value)
}
