package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEnvModelConns(t *testing.T) {
	const key = "CYODA_POSTGRES_MODEL_CONNS"
	for _, tc := range []struct {
		in      string
		want    int32
		wantErr bool
	}{
		{"", 8, false},
		{"2", 2, false},
		{"16", 16, false},
		{"1", 0, true},
		{"0", 0, true},
		{"abc", 0, true},
	} {
		got, err := envModelConns(func(k string) string {
			if k == key {
				return tc.in
			}
			return ""
		})
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s=%q: accepted, want an error", key, tc.in)
			} else if !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q: error %q does not name the variable", key, tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s=%q: %v", key, tc.in, err)
		} else if got != tc.want {
			t.Errorf("%s=%q: got %d, want %d", key, tc.in, got, tc.want)
		}
	}
}

func TestParseConfig_ModelConnsInvalidFailsStartup(t *testing.T) {
	_, err := parseConfig(func(k string) string {
		switch k {
		case "CYODA_POSTGRES_URL":
			return "postgres://test"
		case "CYODA_POSTGRES_MODEL_CONNS":
			return "1"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "CYODA_POSTGRES_MODEL_CONNS") {
		t.Fatalf("parseConfig err = %v, want one naming CYODA_POSTGRES_MODEL_CONNS", err)
	}
}

func TestModelPoolConfig_LockTimeoutZero_StatementTimeoutKept(t *testing.T) {
	base, err := pgxpool.ParseConfig("postgres://u:p@localhost:5432/db")
	if err != nil {
		t.Fatal(err)
	}
	base.MaxConns = 25
	base.MinConns = 5
	base.ConnConfig.RuntimeParams["statement_timeout"] = "7000"
	base.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "9000"
	base.ConnConfig.RuntimeParams["lock_timeout"] = "123"

	c := modelPoolConfig(base, 6)
	if c.MaxConns != 6 || c.MinConns != 0 {
		t.Errorf("MaxConns/MinConns = %d/%d, want 6/0", c.MaxConns, c.MinConns)
	}
	p := c.ConnConfig.RuntimeParams
	if p["lock_timeout"] != "0" {
		t.Errorf("lock_timeout = %q, want 0", p["lock_timeout"])
	}
	if p["statement_timeout"] != "7000" || p["idle_in_transaction_session_timeout"] != "9000" {
		t.Errorf("base ceilings not kept: %v", p)
	}
	if base.MaxConns != 25 || base.ConnConfig.RuntimeParams["lock_timeout"] != "123" {
		t.Errorf("modelPoolConfig mutated the base config")
	}
}

func TestModelPool_ExtensionSlots_LeaveConnectionsForReads(t *testing.T) {
	base, err := pgxpool.ParseConfig(testDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	m, err := newModelPool(context.Background(), base, 4, 5*time.Second)
	if err != nil {
		t.Fatalf("newModelPool: %v", err)
	}
	t.Cleanup(m.close)

	if got := cap(m.extSlots); got != 2 {
		t.Fatalf("extension slots = %d, want 2 for 4 connections", got)
	}
	ctx := context.Background()
	rel1, err := m.acquireExtension(ctx)
	if err != nil {
		t.Fatalf("first extension: %v", err)
	}
	rel2, err := m.acquireExtension(ctx)
	if err != nil {
		t.Fatalf("second extension: %v", err)
	}

	third, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := m.acquireExtension(third)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("third extension returned while both slots were held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// A read still gets one of the two connections the slots left over.
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	conn, err := m.pool.Acquire(readCtx)
	if err != nil {
		t.Fatalf("read acquire while extensions hold their slots: %v", err)
	}
	conn.Release()

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled third extension err = %v, want context.Canceled", err)
	}

	rel1()
	rel2()
	rel3, err := m.acquireExtension(ctx)
	if err != nil {
		t.Fatalf("extension after release: %v", err)
	}
	rel3()
}
