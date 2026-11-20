package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	cepb "github.com/cyoda/cyoda-go/api/grpc/cloudevents"
)

type overlapStream struct {
	grpc.BidiStreamingClient[cepb.CloudEvent, cepb.CloudEvent]
	inFlight atomic.Int32
	overlaps atomic.Int32
}

func (s *overlapStream) Send(*cepb.CloudEvent) error {
	if s.inFlight.Add(1) > 1 {
		s.overlaps.Add(1)
	}
	defer s.inFlight.Add(-1)
	for i := 0; i < 1000; i++ { // widen the window
	}
	return nil
}
func (s *overlapStream) Context() context.Context     { return context.Background() }
func (s *overlapStream) Header() (metadata.MD, error) { return nil, nil }

func TestDispatcher_SendIsSerialised(t *testing.T) {
	d := &dispatcher{}
	s := &overlapStream{}
	ce, _ := newCloudEvent(ceTypeKeepAlive, map[string]any{"success": true})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = d.send(s, ce)
			}
		}()
	}
	wg.Wait()
	if n := s.overlaps.Load(); n != 0 {
		t.Fatalf("%d overlapping sends on one stream", n)
	}
}
