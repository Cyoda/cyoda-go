// Package clusterstatus reports the state of the cluster's members.
package clusterstatus

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"
	"unicode/utf8"

	"github.com/cyoda-platform/cyoda-go/internal/lifecycle"
)

// maxAnswerText bounds the free-text fields of a peer's answer.
const maxAnswerText = 256

// maxQuotedState is how much of a refused state an error quotes. Quoting can
// escape each byte to four, so the error stays well under maxAnswerText.
const maxQuotedState = 32

// Clock summarises a node's platform clock setting.
type Clock struct {
	Controllable bool   `json:"controllable"`
	Version      uint64 `json:"version"`
	Writer       string `json:"writer"`
}

// NodeStatus is one node's status; it is also the wire encoding of a peer's answer.
type NodeStatus struct {
	State    lifecycle.State `json:"state"`
	Database bool            `json:"database"`
	Version  string          `json:"version"`
	Clock    Clock           `json:"clock"`
}

// Validate applies the bounds on a peer's answer.
func (s NodeStatus) Validate() error {
	if !s.State.Valid() {
		return fmt.Errorf("invalid lifecycle state %s", quoteBounded(string(s.State)))
	}
	if err := checkText("version", s.Version); err != nil {
		return err
	}
	if err := checkText("clock writer", s.Clock.Writer); err != nil {
		return err
	}
	if s.Clock.Version > math.MaxInt64 {
		return fmt.Errorf("clock version %d exceeds the int64 range", s.Clock.Version)
	}
	return nil
}

// quoteBounded quotes v, or its first maxQuotedState bytes and its length when
// it is longer: the value is a peer's, bounded only by the envelope.
func quoteBounded(v string) string {
	if len(v) <= maxQuotedState {
		return fmt.Sprintf("%q", v)
	}
	return fmt.Sprintf("%q... (%d bytes)", v[:maxQuotedState], len(v))
}

func checkText(field, v string) error {
	if len(v) > maxAnswerText {
		return fmt.Errorf("%s is %d bytes, over the %d-byte bound", field, len(v), maxAnswerText)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	return nil
}

// Self produces this node's own status.
type Self struct {
	lc         *lifecycle.Lifecycle
	store      interface{ Ping(context.Context) error }
	version    string
	clock      func() Clock
	askTimeout time.Duration
}

// NewSelf builds the reporter. The database probe is bounded by half of askTimeout,
// so the answer returns inside the asker's own bound.
func NewSelf(lc *lifecycle.Lifecycle, store interface{ Ping(context.Context) error }, version string, clock func() Clock, askTimeout time.Duration) *Self {
	return &Self{lc: lc, store: store, version: version, clock: clock, askTimeout: askTimeout}
}

// Status reads the node's status. A failed Ping is answered as Database false;
// its text goes to the log only, as it can carry a DSN or an address. A Ping
// that fails after the caller's context ended is not a database fault: the
// asker called the ask off and discards the answer, so it logs at DEBUG.
func (s *Self) Status(ctx context.Context) NodeStatus {
	// realtime: the database probe is bounded by a wall-clock timeout
	pctx, cancel := context.WithTimeout(ctx, s.askTimeout/2)
	defer cancel()
	db := true
	if err := s.store.Ping(pctx); err != nil {
		db = false
		if ctx.Err() != nil {
			slog.Debug("cluster status: database ping ended with the ask", "pkg", "clusterstatus", "err", err)
		} else {
			slog.Warn("cluster status: database ping failed", "pkg", "clusterstatus", "err", err)
		}
	}
	return NodeStatus{State: s.lc.State(), Database: db, Version: s.version, Clock: s.clock()}
}
