package common

import (
	"sync"
	"testing"
	"time"
)

func TestChangeSignal_FireClosesTheChannelTakenBefore(t *testing.T) {
	s := NewChangeSignal()
	ch := s.Changed()

	select {
	case <-ch:
		t.Fatal("channel closed before any Fire")
	default:
	}

	s.Fire()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("channel taken before Fire was not closed by it")
	}
}

func TestChangeSignal_ChannelIsReplacedAfterFire(t *testing.T) {
	s := NewChangeSignal()
	first := s.Changed()
	s.Fire()
	second := s.Changed()

	if first == second {
		t.Fatal("Changed returned the closed channel again; every Fire must install a new one")
	}
	select {
	case <-second:
		t.Fatal("the replacement channel is already closed")
	default:
	}
}

func TestChangeSignal_EveryWaiterWakes(t *testing.T) {
	s := NewChangeSignal()
	const waiters = 8
	var wg sync.WaitGroup
	for range waiters {
		ch := s.Changed()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch
		}()
	}
	s.Fire()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not every waiter was woken by one Fire")
	}
}

func TestChangeSignal_ConcurrentFireAndChanged(t *testing.T) {
	s := NewChangeSignal()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 1000 {
				s.Fire()
			}
		}()
		go func() {
			defer wg.Done()
			for range 1000 {
				_ = s.Changed()
			}
		}()
	}
	wg.Wait()
}
