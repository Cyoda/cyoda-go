package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope for this plugin's instruments.
const meterName = "github.com/cyoda-platform/cyoda-go/plugins/postgres"

// registerPoolMetrics exports pgxpool.Stat on every scrape as observable
// instruments. Pool saturation is the dominant outage mode of this design;
// empty_acquire_wait (time callers spent waiting because the pool was
// empty) is the signal to alarm on. The connections gauge reports each pool
// under pool="main", "scheduler" or "heartbeat", so a saturated scheduler
// pool is told apart from a saturated main pool; the other instruments are
// the main pool's. sched may be nil, and a scheduler pool that is not open
// is not reported. One factory per process is assumed: two factories would
// both observe backend="postgres" and the last observation per cycle would
// win. The returned func unregisters the callback and must run before the
// pools are closed.
func registerPoolMetrics(meter metric.Meter, pool *pgxpool.Pool, sched *schedulerPools) (func(), error) {
	connections, err := meter.Int64ObservableGauge("cyoda.storage.pool.connections",
		metric.WithDescription("Pool connections by state"))
	if err != nil {
		return nil, fmt.Errorf("instrument connections: %w", err)
	}
	maxConns, err := meter.Int64ObservableGauge("cyoda.storage.pool.max_connections",
		metric.WithDescription("Configured maximum pool size"))
	if err != nil {
		return nil, fmt.Errorf("instrument max_connections: %w", err)
	}
	acquires, err := meter.Int64ObservableCounter("cyoda.storage.pool.acquires",
		metric.WithDescription("Successful connection acquires"))
	if err != nil {
		return nil, fmt.Errorf("instrument acquires: %w", err)
	}
	emptyAcquires, err := meter.Int64ObservableCounter("cyoda.storage.pool.empty_acquires",
		metric.WithDescription("Acquires that found the pool empty and had to wait"))
	if err != nil {
		return nil, fmt.Errorf("instrument empty_acquires: %w", err)
	}
	canceled, err := meter.Int64ObservableCounter("cyoda.storage.pool.canceled_acquires",
		metric.WithDescription("Acquires cancelled by their context before a connection was available"))
	if err != nil {
		return nil, fmt.Errorf("instrument canceled_acquires: %w", err)
	}
	acquireDuration, err := meter.Float64ObservableCounter("cyoda.storage.pool.acquire_duration",
		metric.WithUnit("s"), metric.WithDescription("Cumulative time spent in acquire, all acquires"))
	if err != nil {
		return nil, fmt.Errorf("instrument acquire_duration: %w", err)
	}
	emptyWait, err := meter.Float64ObservableCounter("cyoda.storage.pool.empty_acquire_wait",
		metric.WithUnit("s"), metric.WithDescription("Cumulative time callers waited because the pool was empty"))
	if err != nil {
		return nil, fmt.Errorf("instrument empty_acquire_wait: %w", err)
	}

	backend := attribute.String("backend", "postgres")
	plain := metric.WithAttributes(backend)
	observeConnections := func(o metric.Observer, name string, st *pgxpool.Stat) {
		p := attribute.String("pool", name)
		o.ObserveInt64(connections, int64(st.AcquiredConns()), metric.WithAttributes(backend, p, attribute.String("state", "acquired")))
		o.ObserveInt64(connections, int64(st.IdleConns()), metric.WithAttributes(backend, p, attribute.String("state", "idle")))
		o.ObserveInt64(connections, int64(st.ConstructingConns()), metric.WithAttributes(backend, p, attribute.String("state", "constructing")))
	}

	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := pool.Stat()
		observeConnections(o, "main", st)
		if sched != nil {
			work, heartbeat := sched.snapshot()
			if work != nil {
				observeConnections(o, "scheduler", work.Stat())
			}
			if heartbeat != nil {
				observeConnections(o, "heartbeat", heartbeat.Stat())
			}
		}
		o.ObserveInt64(maxConns, int64(st.MaxConns()), plain)
		o.ObserveInt64(acquires, st.AcquireCount(), plain)
		o.ObserveInt64(emptyAcquires, st.EmptyAcquireCount(), plain)
		o.ObserveInt64(canceled, st.CanceledAcquireCount(), plain)
		o.ObserveFloat64(acquireDuration, st.AcquireDuration().Seconds(), plain)
		o.ObserveFloat64(emptyWait, st.EmptyAcquireWaitTime().Seconds(), plain)
		return nil
	}, connections, maxConns, acquires, emptyAcquires, canceled, acquireDuration, emptyWait)
	if err != nil {
		return nil, fmt.Errorf("register pool metrics callback: %w", err)
	}
	return func() { _ = reg.Unregister() }, nil
}
