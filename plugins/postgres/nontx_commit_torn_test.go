package postgres_test

// nontx_commit_torn_test.go — a non-transactional write's own COMMIT that
// loses its socket has an outcome in doubt, so it is not reported retryable.
//
// The COMMIT is made to take time server-side with a deferred constraint
// trigger that sleeps, and the socket is torn under it by a TCP proxy between
// the pool and the server. The backend goes on to commit after the client has
// seen the socket die: the outcome really is in doubt, which is why a retry
// would apply the write twice.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// tearProxy forwards TCP connections to a server and can close every one of
// them on demand.
type tearProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newTearProxy(t *testing.T, target string) *tearProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &tearProxy{ln: ln}
	t.Cleanup(func() {
		_ = ln.Close()
		p.tear()
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.conns = append(p.conns, client, server)
			}()
			go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
			go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
		}
	}()
	return p
}

func (p *tearProxy) tear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func TestNonTxCommit_TornSocketIsNotRetryable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(ctx context.Context, es spi.EntityStore, id, txID string) error
	}{
		{"save", func(ctx context.Context, es spi.EntityStore, id, _ string) error {
			_, err := es.Save(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":2}`),
			})
			return err
		}},
		{"delete", func(ctx context.Context, es spi.EntityStore, id, _ string) error {
			return es.Delete(ctx, id)
		}},
		{"compare-and-save", func(ctx context.Context, es spi.EntityStore, id, txID string) error {
			_, err := es.CompareAndSave(ctx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":2}`),
			}, txID)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			direct := newCTPool(t, testDBURL(t), 4, nil)
			resetSchema(t, direct)
			seed := postgres.NewStoreFactory(direct)
			seed.InitTransactionManager(newTestUUIDGenerator())
			ctx := ctxWithTenant(ctTenant)

			// The entity the write targets, committed by a transaction so
			// compare-and-save has a transaction id to name.
			tm := ctTM(t, seed, ctx)
			txID, txCtx, err := tm.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			seedStore, err := seed.EntityStore(txCtx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}
			id := uuid.NewString()
			if _, err := seedStore.Save(txCtx, &spi.Entity{
				Meta: spi.EntityMeta{ID: id, ModelRef: ctModel}, Data: []byte(`{"n":1}`),
			}); err != nil {
				t.Fatalf("seed Save: %v", err)
			}
			if err := tm.Commit(txCtx, txID); err != nil {
				t.Fatalf("seed Commit: %v", err)
			}

			if _, err := direct.Exec(context.Background(), `
				CREATE FUNCTION ct_slow_commit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN PERFORM pg_sleep(10); RETURN NULL; END $$;
				CREATE CONSTRAINT TRIGGER ct_slow_commit AFTER INSERT OR UPDATE ON entities
				  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ct_slow_commit();`); err != nil {
				t.Fatalf("install the slow-commit trigger: %v", err)
			}
			// The torn COMMIT's backend goes on sleeping in the trigger after
			// the client is gone; end it so the schema drop in cleanup does
			// not wait for it. Registered after the pool's and the schema's
			// cleanups, so it runs before them.
			t.Cleanup(func() {
				_, _ = direct.Exec(context.Background(),
					`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
					  WHERE datname = current_database() AND pid <> pg_backend_pid()
					    AND wait_event = 'PgSleep'`)
			})

			u, err := url.Parse(testDBURL(t))
			if err != nil {
				t.Fatalf("parse CYODA_TEST_DB_URL: %v", errors.Unwrap(err))
			}
			proxy := newTearProxy(t, u.Host)
			u.Host = proxy.ln.Addr().String()
			proxied := newCTPool(t, u.String(), 2, nil)
			es, err := postgres.NewStoreFactory(proxied).EntityStore(ctx)
			if err != nil {
				t.Fatalf("EntityStore: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- tc.write(ctx, es, id, txID) }()

			deadline := time.Now().Add(10 * time.Second)
			for {
				var n int
				if err := direct.QueryRow(context.Background(),
					`SELECT count(*) FROM pg_stat_activity
					  WHERE datname = current_database() AND wait_event = 'PgSleep'
					    AND query ILIKE 'commit%'`).Scan(&n); err != nil {
					t.Fatalf("poll pg_stat_activity: %v", err)
				}
				if n > 0 {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("the write returned before its COMMIT was seen running: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the write's COMMIT was never seen running")
				}
				time.Sleep(10 * time.Millisecond)
			}

			proxy.tear()
			err = <-done
			if err == nil {
				t.Fatal("precondition: the COMMIT whose socket was torn reported success")
			}
			if storageUnavailable(err) {
				t.Fatalf("an in-doubt COMMIT was advertised as retryable; a retry would apply it twice: %v", err)
			}
		})
	}
}
