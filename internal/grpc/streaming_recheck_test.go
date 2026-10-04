package grpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

// assertNoMembershipChange fails when the registry saw a membership change
// after changed was taken: a member was registered.
func assertNoMembershipChange(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
		t.Error("a member was registered")
	default:
	}
}

// TestStartStreaming_ClientCheckedAtOpen: the stream's client is read once
// before the member is registered. A deleted client, one in another tenant,
// a reset secret and a store that cannot be read refuse the stream with the
// status the periodic re-check closes it with; no member is registered and
// no greet is sent.
func TestStartStreaming_ClientCheckedAtOpen(t *testing.T) {
	cases := []struct {
		name  string
		store *lookupStore
		want  codes.Code
	}{
		{"client deleted", &lookupStore{}, codes.Unauthenticated},
		{"client in another tenant", &lookupStore{client: &auth.M2MClient{ClientID: "C1", TenantID: "tenant-2", SecretGen: 1}}, codes.Unauthenticated},
		{"secret reset", &lookupStore{client: &auth.M2MClient{ClientID: "C1", TenantID: "tenant-1", SecretGen: 2}}, codes.Unauthenticated},
		{"store error", &lookupStore{err: errors.New("read failed")}, codes.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newServiceForTest()
			svc.m2mStore = tc.store
			changed := svc.registry.Changed()
			ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
			defer cancel()
			stream := newMockBidiStream(ctx)
			stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"go"}))
			done := make(chan error, 1)
			go func() { done <- svc.StartStreaming(stream) }()
			select {
			case err := <-done:
				if got := status.Code(err); got != tc.want {
					t.Fatalf("StartStreaming = %v (%v), want %v", got, err, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the stream was not refused at open")
			}
			if sent := stream.sentMessages(); len(sent) != 0 {
				t.Errorf("sent %d events, want none (first: %s)", len(sent), sent[0].Type)
			}
			assertNoMembershipChange(t, changed)
		})
	}
}

// hungStore is an M2MClientStore whose Lookup answers client for the first
// okReads calls and then blocks until its context ends.
type hungStore struct {
	auth.M2MClientStore
	client  *auth.M2MClient
	okReads int32
	reads   atomic.Int32
}

func (s *hungStore) Lookup(ctx context.Context, _ spi.TenantID, _ string) (*auth.M2MClient, error) {
	if s.reads.Add(1) <= s.okReads {
		return s.client, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStartStreaming_HungClientReadAtOpen_Unavailable: a client read at open
// that never answers is bounded by clientRecheckInterval and refuses the
// stream with Unavailable; no member is registered.
func TestStartStreaming_HungClientReadAtOpen_Unavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := newServiceForTest()
		svc.m2mStore = &hungStore{}
		changed := svc.registry.Changed()
		ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
		defer cancel()
		stream := newMockBidiStream(ctx)
		stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"go"}))
		start := time.Now()
		err := svc.StartStreaming(stream)
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("StartStreaming = %v (%v), want Unavailable", got, err)
		}
		if waited := time.Since(start); waited != clientRecheckInterval {
			t.Errorf("refused after %v, want %v", waited, clientRecheckInterval)
		}
		if sent := stream.sentMessages(); len(sent) != 0 {
			t.Errorf("sent %d events, want none", len(sent))
		}
		assertNoMembershipChange(t, changed)
	})
}

// TestStartStreaming_HungPeriodicRecheck_Unavailable: a periodic re-check
// read that never answers is bounded by clientRecheckInterval and closes the
// stream with Unavailable.
func TestStartStreaming_HungPeriodicRecheck_Unavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := newServiceWithKeepAlive(time.Hour, time.Hour) // keep-alive never ends the stream here
		svc.m2mStore = &hungStore{client: &auth.M2MClient{ClientID: "C1", TenantID: "tenant-1", SecretGen: 1}, okReads: 1}
		ctx, cancel := context.WithCancel(m2mContext("tenant-1"))
		defer cancel()
		stream := newMockBidiStream(ctx)
		stream.enqueue(makeJoinEvent(t, "tenant-1", []string{"go"}))
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- svc.StartStreaming(stream) }()
		synctest.Wait()
		if sent := stream.sentMessages(); len(sent) != 1 || sent[0].Type != CalculationMemberGreetEvent {
			t.Fatalf("sent %v, want the greet", sent)
		}
		err := <-done
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("StartStreaming = %v (%v), want Unavailable", got, err)
		}
		if waited := time.Since(start); waited != 2*clientRecheckInterval {
			t.Errorf("closed after %v, want %v (first re-check, then its bounded read)", waited, 2*clientRecheckInterval)
		}
	})
}
