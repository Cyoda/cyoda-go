package grpc

// One failure answered over gRPC is one log line, at the level its class
// gets on the HTTP door — a client error one INFO line, a server-side failure
// one ERROR line with its ticket — and the line names the request it answers.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/search"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
	"github.com/cyoda-platform/cyoda-go/plugins/memory"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) lines(level string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(line, "level="+level) {
			out = append(out, line)
		}
	}
	return out
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// unreadableJobs fails every job read with an unclassified error.
type unreadableJobs struct {
	spi.AsyncSearchStore
}

func (unreadableJobs) GetJob(context.Context, string) (*spi.SearchJob, error) {
	return nil, errors.New("job store read failed")
}

// wantRequestIdentity requires that line names the gRPC request it answers:
// the package, the RPC, the request's event type and its CloudEvent id.
func wantRequestIdentity(t *testing.T, line, ceID string) {
	t.Helper()
	for _, want := range []string{"pkg=grpc", "rpc=entitySearch", "type=" + SnapshotGetStatusRequest, "ceId=" + ceID} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}
}

func TestGRPCFailure_LoggedOnceAtItsClassLevel(t *testing.T) {
	t.Run("client error", func(t *testing.T) {
		factory := memory.NewStoreFactory()
		defer factory.Close()
		store, _ := factory.AsyncSearchStore(context.Background())
		svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(
			factory, common.NewDefaultUUIDGenerator(), store, newTestConsistency(t, factory), platformclock.Real())}
		logs := captureLogs(t)

		resp, err := svc.EntitySearch(grpcTenantCtx(), makeCE(SnapshotGetStatusRequest, map[string]any{
			"id": "status-missing", "snapshotId": "aaaaaaaa-0000-0000-0000-00000000dead",
		}))
		if err != nil {
			t.Fatalf("EntitySearch: %v", err)
		}
		var typed events.EntitySnapshotSearchResponseJson
		validateResponse(t, resp, &typed)
		if typed.Success || typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
			t.Fatalf("answer = success %v, error %+v; want a CLIENT_ERROR", typed.Success, typed.Error)
		}
		if got := append(logs.lines("ERROR"), logs.lines("WARN")...); len(got) != 0 {
			t.Errorf("a client error logged above INFO: %q", got)
		}
		got := logs.lines("INFO")
		if len(got) != 1 || !strings.Contains(got[0], "operational error") {
			t.Fatalf("INFO lines = %q, want one operational-error line", got)
		}
		wantRequestIdentity(t, got[0], "test-req-1")
	})

	t.Run("client error answered as one before", func(t *testing.T) {
		svc, ctx := newTestEnv(t)
		logs := captureLogs(t)

		resp, err := svc.EntitySearch(ctx, makeCE(EntityGetRequest, map[string]any{
			"id": "get-missing", "entityId": "aaaaaaaa-0000-0000-0000-00000000cafe",
		}))
		if err != nil {
			t.Fatalf("EntitySearch: %v", err)
		}
		var typed events.EntityResponseJson
		validateResponse(t, resp, &typed)
		if typed.Success || typed.Error == nil || typed.Error.Code != "CLIENT_ERROR" {
			t.Fatalf("answer = success %v, error %+v; want a CLIENT_ERROR", typed.Success, typed.Error)
		}
		if got := append(logs.lines("ERROR"), logs.lines("WARN")...); len(got) != 0 {
			t.Errorf("a client error logged above INFO: %q", got)
		}
		if got := logs.lines("INFO"); len(got) != 1 || !strings.Contains(got[0], "operational error") {
			t.Errorf("INFO lines = %q, want one operational-error line", got)
		}
	})

	t.Run("server-side failure", func(t *testing.T) {
		factory := memory.NewStoreFactory()
		defer factory.Close()
		store, _ := factory.AsyncSearchStore(context.Background())
		svc := &CloudEventsServiceImpl{searchService: search.NewSearchService(
			factory, common.NewDefaultUUIDGenerator(), unreadableJobs{store}, newTestConsistency(t, factory), platformclock.Real())}
		logs := captureLogs(t)

		resp, err := svc.EntitySearch(grpcTenantCtx(), makeCE(SnapshotGetStatusRequest, map[string]any{
			"id": "status-broken", "snapshotId": "aaaaaaaa-0000-0000-0000-00000000beef",
		}))
		if err != nil {
			t.Fatalf("EntitySearch: %v", err)
		}
		var typed events.EntitySnapshotSearchResponseJson
		validateResponse(t, resp, &typed)
		if typed.Success || typed.Error == nil || typed.Error.Code != "SERVER_ERROR" {
			t.Fatalf("answer = success %v, error %+v; want a SERVER_ERROR", typed.Success, typed.Error)
		}
		got := logs.lines("ERROR")
		if len(got) != 1 || !strings.Contains(got[0], "ticket=") {
			t.Fatalf("ERROR lines = %q, want one, carrying the ticket", got)
		}
		wantRequestIdentity(t, got[0], "test-req-1")
	})
}
