package postgres_test

// consistency_tenant_keys_test.go — the per-tenant marker key
// (consistency_tenant_keys, migration 000016) on every path that stamps a
// commit, its allocation race between two resolvers, the one cache a factory
// and its transaction manager share, and the schema's "never 0" invariant.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// Every non-transactional write stamps its own commit, and its in-flight
// marker must sit under the tenant's stored key: a marker under any other key
// is one no consistency-time call for the tenant waits for. Each write is
// frozen inside cyoda_stamp — the marker is taken before the floor mutex,
// which is held elsewhere — and its backend's markers are read from pg_locks.
// cyoda_stamp's 2 s lock_timeout bounds the freeze, so the mutex is released
// as soon as the locks are read.
func TestStamp_NonTransactionalWritesMarkUnderTheTenantKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(ctx context.Context, es spi.EntityStore, id, txID string) error
	}{
		{"save", func(ctx context.Context, es spi.EntityStore, _, _ string) error {
			_, err := es.Save(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
			})
			return err
		}},
		{"save all", func(ctx context.Context, es spi.EntityStore, _, _ string) error {
			_, err := es.SaveAll(ctx, slices.Values([]*spi.Entity{{
				Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
			}}))
			return err
		}},
		{"delete", func(ctx context.Context, es spi.EntityStore, id, _ string) error {
			return es.Delete(ctx, id)
		}},
		{"delete all", func(ctx context.Context, es spi.EntityStore, _, _ string) error {
			return es.DeleteAll(ctx, ctModel)
		}},
		{"compare-and-save", func(ctx context.Context, es spi.EntityStore, id, txID string) error {
			_, err := es.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":2}`),
			}, txID)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ctx := newCTFactory(t)
			pool := postgres.PoolForTest(f)
			prewarm(t, pool, 4)
			id, txID := seedEntity(t, f, ctx)
			key := storedTenantKey(t, pool, ctTenant)
			es, err := f.EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}

			release := takeFloorMutex(t)
			done := make(chan error, 1)
			go func() { done <- tc.write(ctx, es, id, txID) }()

			pid := floorMutexWaiter(t, pool)
			var underKey, otherKey int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FILTER (WHERE classid = $2::oid),
				        count(*) FILTER (WHERE classid <> $2::oid)
				   FROM pg_locks
				  WHERE pid = $1 AND locktype = 'advisory' AND objsubid = 2 AND objid <> 0
				    AND mode = 'ExclusiveLock' AND granted`, pid, key).Scan(&underKey, &otherKey); err != nil {
				release()
				t.Fatalf("read the write's markers: %v", err)
			}
			release()
			if err := <-done; err != nil {
				t.Fatalf("write: %v", err)
			}
			if underKey != 1 || otherKey != 0 {
				t.Fatalf("the write holds %d markers under the tenant's key %d and %d under other keys, want 1 and 0",
					underKey, key, otherKey)
			}
		})
	}
}

// floorMutexWaiter returns the pid of the backend queued for the floor mutex
// (0, 0).
func floorMutexWaiter(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var pid int
		err := pool.QueryRow(context.Background(),
			`SELECT pid FROM pg_locks
			  WHERE locktype = 'advisory' AND classid = 0 AND objid = 0 AND objsubid = 2 AND NOT granted
			    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  LIMIT 1`).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend ever queued for the floor mutex")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Two resolvers allocate the same new tenant's key at once: the first holds
// its INSERT uncommitted, so the second (a Begin on a factory that has never
// seen the tenant) blocks on it, then finds the first's row. Both end up with
// the same key, and the table holds one row. The key the manager cached is
// shown to be the holder's by a consistency-time call that waits for a marker
// taken under it.
func TestTenantKeys_ConcurrentAllocationAgrees(t *testing.T) {
	f, _ := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	const tenant spi.TenantID = "ct-keys-race"
	ctx := ctxWithTenant(tenant)
	tm := ctTM(t, f, ctx)
	prewarm(t, pool, 4)

	holder, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	var holderKey int32
	if err := holder.QueryRow(context.Background(),
		`INSERT INTO consistency_tenant_keys (tenant_id) VALUES ($1) RETURNING tenant_key`,
		string(tenant)).Scan(&holderKey); err != nil {
		t.Fatalf("holder insert: %v", err)
	}

	type begun struct {
		txID  string
		txCtx context.Context
		err   error
	}
	got := make(chan begun, 1)
	go func() {
		txID, txCtx, err := tm.Begin(ctx)
		got <- begun{txID, txCtx, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'
			    AND query LIKE '%INSERT INTO consistency_tenant_keys%'`).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second resolution never blocked on the first's insert")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case r := <-got:
		t.Fatalf("Begin returned while the first allocation was uncommitted: %v", r.err)
	default:
	}

	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	r := <-got
	if r.err != nil {
		t.Fatalf("Begin after a concurrent allocation: %v", r.err)
	}
	if err := tm.Rollback(r.txCtx, r.txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM consistency_tenant_keys WHERE tenant_id = $1`, string(tenant)).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d key rows for one tenant", rows)
	}

	held := holdStamp(t, pool, tenant) // takes its marker under the stored (holder's) key
	if k := storedTenantKey(t, pool, tenant); k != holderKey {
		t.Fatalf("stored key %d is not the holder's %d", k, holderKey)
	}
	ct := consistencyTimeAsync(ctx, tm)
	waitForMarkerWaiter(t, pool)
	held.commit()
	if res := <-ct; res.err != nil {
		t.Fatalf("ConsistencyTime: %v", res.err)
	}
}

// A factory and its transaction manager share one key cache: a key the
// manager resolved serves the factory's non-transactional writes, so once it
// is cached the table is not read again by either.
func TestTenantKeys_OneCachePerFactory(t *testing.T) {
	f, ctx := newCTFactoryOwnDB(t)
	pool := postgres.PoolForTest(f)
	id, _ := seedEntity(t, f, ctx) // Begin resolves and caches the key
	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE consistency_tenant_keys RENAME TO consistency_tenant_keys_gone`); err != nil {
		t.Fatalf("hide the key table: %v", err)
	}

	es, err := f.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if _, err := es.Save(ctx, &spi.Entity{
		Meta: spi.EntityMeta{ID: uuid.NewString(), ModelRef: ctModel}, Data: []byte(`{"n":1}`),
	}); err != nil {
		t.Fatalf("a non-transactional Save looked the key up again: %v", err)
	}
	if err := es.Delete(ctx, id); err != nil {
		t.Fatalf("a non-transactional Delete looked the key up again: %v", err)
	}
	if _, err := ctTM(t, f, ctx).ConsistencyTime(ctx); err != nil {
		t.Fatalf("ConsistencyTime looked the key up again: %v", err)
	}
}

// The schema states "never 0": a key of 0 would make a marker's first half
// the floor mutex's, and is refused even when inserted explicitly.
func TestTenantKeys_SchemaRefusesNonPositiveKeys(t *testing.T) {
	f, _ := newCTFactory(t)
	pool := postgres.PoolForTest(f)
	for _, key := range []int32{0, -1} {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO consistency_tenant_keys (tenant_id, tenant_key) VALUES ($1, $2)`,
			"ct-keys-bad", key)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("key %d: want a check violation (23514), got %v", key, err)
		}
	}
}
