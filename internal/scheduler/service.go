// Package scheduler implements the coordinator-only scan loop that fires due
// ScheduledTasks (see the cyoda-go scheduled-transition-runtime design).
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// Executor runs one due ScheduledTask against the cluster member picked by
// Distribution.Pick. target is that pick, passed through verbatim — the
// Service calls Pick exactly once per task (RoundRobin is stateful; a
// second call would advance the cursor and could return a different node
// than the one the coordinator actually decided on), so Execute must not
// re-derive it. A real implementation (Task D3) uses target to decide
// whether to fire the transition locally (target == this node's ID) or
// forward it to the peer over the cluster RPC channel.
//
// The Service invokes Execute from its own goroutine, recovered from panics
// (see tick), so the scan loop never blocks on or is brought down by a slow
// or panicking implementation. Execute itself may therefore be synchronous —
// it need not spawn its own goroutine or be otherwise async — though it may
// be if that's convenient.
type Executor interface {
	Execute(ctx context.Context, task spi.ScheduledTask, target string)
}

// Config controls the scan loop's cadence and behavior.
type Config struct {
	// Enabled gates the whole loop. When false, ticks are no-ops — this lets
	// the Service be constructed and started unconditionally while still
	// respecting a "scheduler disabled" deployment setting.
	Enabled bool
	// ScanInterval is the period between scans.
	ScanInterval time.Duration
	// RedispatchBackoff is how far into the future MarkRedispatch pushes a
	// dispatched task's RedispatchAfter, throttling redundant re-dispatch of
	// the same task on the next tick (or by another node that becomes
	// coordinator mid-flight). It is a plain best-effort throttle, not a
	// lease: it offers no exclusivity guarantee.
	RedispatchBackoff time.Duration
	// BatchSize caps how many due tasks a single scan pulls from the store.
	BatchSize int
}

// Deps wires the Service's collaborators. All fields are required.
type Deps struct {
	// Store gives access to the cross-tenant ScheduledTaskStore. Store is a
	// spi.StoreFactory rather than a single ScheduledTaskStore because the
	// scan loop calls factory.ScheduledTaskStore(context.Background()) fresh
	// on each tick, matching the store's cross-tenant/tenant-less-context
	// contract (see spi.StoreFactory.ScheduledTaskStore godoc).
	Store spi.StoreFactory
	// Registry lists live cluster members so the loop can determine
	// coordinator status and, via Distribution, pick a dispatch target.
	Registry contract.NodeRegistry
	// Coordinator decides whether this node is the one that scans on a
	// given tick.
	Coordinator CoordinatorStrategy
	// Distribution picks which member a due task should run on.
	Distribution DistributionStrategy
	// Clock abstracts wall-clock time for deterministic tests.
	Clock Clock
	// Executor actually runs each due task (fire-and-forget).
	Executor Executor
	// SelfID is this node's NodeID, as it appears in Registry.List results.
	SelfID string
}

// Service is the coordinator-only scan loop: on each tick it checks whether
// this node is the coordinator and, if so, scans for due ScheduledTasks and
// dispatches each to Executor. Non-coordinators idle every tick.
//
// Start launches exactly one background goroutine; a second Start call is a
// no-op rather than spawning a duplicate loop. Stop is safe to call any
// number of times (idempotent): it closes the stop channel to signal the
// goroutine to exit on its next select iteration, but it does not itself
// wait for that goroutine to finish — callers needing that guarantee must
// synchronize separately. Follows the reaper pattern used elsewhere in the
// codebase (e.g. the search-snapshot TTL reaper in app/app.go).
type Service struct {
	cfg  Config
	deps Deps

	stop      chan struct{}
	stopOnce  sync.Once
	startOnce sync.Once
}

// NewService constructs a Service. Start must be called to begin scanning.
func NewService(cfg Config, deps Deps) *Service {
	return &Service{
		cfg:  cfg,
		deps: deps,
		stop: make(chan struct{}),
	}
}

// Start launches the scan-loop goroutine. It returns immediately; the loop
// runs until Stop is called. A second call to Start is a no-op — only the
// first spawns a goroutine — so callers can't accidentally end up with two
// concurrent scan loops racing on the same Service.
func (s *Service) Start() {
	s.startOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(s.cfg.ScanInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					s.tick()
				case <-s.stop:
					return
				}
			}
		}()
	})
}

// Stop signals the scan-loop goroutine to exit. Safe to call multiple times.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
	})
}

// tick runs one scan-and-dispatch pass. It is a separate method (rather than
// inlined in the Start goroutine) so tests can drive it directly against a
// controlled Clock instead of depending on wall-clock ticker firings.
func (s *Service) tick() {
	if !s.cfg.Enabled {
		return
	}

	ctx := context.Background()

	members, err := s.deps.Registry.List(ctx)
	if err != nil {
		slog.Error("scheduler scan: failed to list cluster members", "pkg", "scheduler", "err", err)
		return
	}

	if !s.deps.Coordinator.IsCoordinator(members, s.deps.SelfID) {
		return
	}

	store, err := s.deps.Store.ScheduledTaskStore(ctx)
	if err != nil {
		slog.Error("scheduler scan: failed to obtain scheduled task store", "pkg", "scheduler", "err", err)
		return
	}

	now := s.deps.Clock.Now()
	tasks, err := store.ScanDue(ctx, now.UnixMilli(), s.cfg.BatchSize)
	if err != nil {
		slog.Error("scheduler scan: ScanDue failed", "pkg", "scheduler", "err", err)
		return
	}

	redispatchAfter := now.Add(s.cfg.RedispatchBackoff).UnixMilli()
	for _, task := range tasks {
		// Throttle first: even if Execute panics or the process dies right
		// after, the next tick won't immediately re-dispatch the same task.
		if err := store.MarkRedispatch(ctx, task.ID, redispatchAfter); err != nil {
			slog.Error("scheduler scan: MarkRedispatch failed", "pkg", "scheduler", "taskId", task.ID, "err", err)
			continue
		}

		target := s.deps.Distribution.Pick(members, s.deps.SelfID, task)
		slog.Debug("scheduler dispatching task", "pkg", "scheduler", "taskId", task.ID, "target", target)

		// Dispatch on its own goroutine so a slow local fire (processors run
		// synchronously) or a slow peer RPC can never stall the scan loop —
		// tick returns as soon as every due task in the batch has been
		// handed off, not once each Execute has finished. Panics are
		// recovered here so one bad task can't take down the scan-loop
		// goroutine and silently stop all future scanning.
		//
		// This intentionally has no back-pressure: a slow Executor means
		// concurrent in-flight dispatches accumulate, bounded only by
		// RedispatchBackoff (a due task isn't re-scanned until its
		// redispatch window elapses) and BatchSize per tick. Real
		// back-pressure is a future enhancement.
		go func(task spi.ScheduledTask, target string) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("scheduled task dispatch panicked", "pkg", "scheduler", "taskId", task.ID, "panic", r)
				}
			}()
			s.deps.Executor.Execute(ctx, task, target)
		}(task, target)
	}
}
