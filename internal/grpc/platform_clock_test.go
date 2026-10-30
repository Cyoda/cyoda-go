package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	cyodapb "github.com/cyoda-platform/cyoda-go/api/grpc/cyoda"
	events "github.com/cyoda-platform/cyoda-go/api/grpc/events"
	"github.com/cyoda-platform/cyoda-go/internal/platformclock"
)

func TestPlatformClock_GRPC_Success(t *testing.T) {
	now := time.Date(2015, 3, 1, 9, 30, 0, 250000000, time.UTC)
	svc := &CloudEventsServiceImpl{clockState: func() platformclock.State {
		return platformclock.State{Now: now, RatePPM: 2_500_000, Controllable: true}
	}}

	ce, err := svc.EntitySearch(consistencyTimeCtx(), makeCE(PlatformClockGetRequest, map[string]any{"id": "pc-1"}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if ce.Type != PlatformClockResponse {
		t.Fatalf("type = %q, want %q", ce.Type, PlatformClockResponse)
	}
	var env events.PlatformClockResponseJson
	validateResponse(t, ce, &env)
	if !env.Success || env.Error != nil {
		t.Fatalf("want success, got %+v", env)
	}
	if env.RequestID != "test-req-1" {
		t.Errorf("requestId = %q, want the request's id", env.RequestID)
	}
	if env.Now == nil || !env.Now.Equal(now) {
		t.Errorf("now = %v, want %v", env.Now, now)
	}
	if env.Rate == nil || *env.Rate != 2.5 {
		t.Errorf("rate = %v, want 2.5", env.Rate)
	}
	if env.Controllable == nil || !*env.Controllable {
		t.Errorf("controllable = %v, want true", env.Controllable)
	}
}

// A payload with a member the request does not declare is refused before the
// clock is read.
func TestPlatformClock_GRPC_InvalidPayload(t *testing.T) {
	svc := &CloudEventsServiceImpl{clockState: func() platformclock.State {
		t.Fatal("clock read for an invalid payload")
		return platformclock.State{}
	}}
	_, err := svc.EntitySearch(consistencyTimeCtx(), makeCE(PlatformClockGetRequest, map[string]any{"id": "pc-x", "extra": 1}))
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

// With clock control off the answer is real time at rate 1, not controllable —
// the same answer GET /platform/clock gives.
func TestPlatformClock_GRPC_FeatureOff(t *testing.T) {
	svc := &CloudEventsServiceImpl{clockState: func() platformclock.State { return platformclock.StateOf(nil, false) }}
	before := time.Now()
	ce, err := svc.EntitySearch(consistencyTimeCtx(), makeCE(PlatformClockGetRequest, map[string]any{"id": "pc-2"}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	var env events.PlatformClockResponseJson
	validateResponse(t, ce, &env)
	if !env.Success || env.Now == nil || env.Now.Before(before.Add(-time.Second)) {
		t.Fatalf("want success at real time, got %+v", env)
	}
	if env.Rate == nil || *env.Rate != 1 || env.Controllable == nil || *env.Controllable {
		t.Errorf("rate/controllable = %v/%v, want 1/false", env.Rate, env.Controllable)
	}
}

// An unauthenticated caller never reaches the handler: the interceptor answers
// the transport status, not an envelope.
func TestPlatformClock_GRPC_Unauthenticated(t *testing.T) {
	srv := NewServer(&mockAuthService{err: context.DeadlineExceeded}, nil, NewMemberRegistry(), nil, nil, nil, nil, nil,
		noCalloutJoiner(t, nil, nil), nil, "n", false, 0, true, nil, KeepAliveConfig{}, newFixedConsistency(),
		func() platformclock.State {
			t.Error("handler reached without authentication")
			return platformclock.State{}
		})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	conn, err := googlegrpc.NewClient(lis.Addr().String(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cyodapb.NewCloudEventsServiceClient(conn).EntitySearch(ctx, makeCE(PlatformClockGetRequest, map[string]any{"id": "pc-3"}))
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}
