package platformclock

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// Peer is another node of the cluster. An empty Addr means the node has no
// usable address.
type Peer struct {
	NodeID string
	Addr   string
}

// Push sends a setting to one peer and returns the setting the peer holds
// afterwards. It must return when ctx ends: the round's bound
// (CYODA_PROXY_TIMEOUT) relies on it.
type Push func(ctx context.Context, peer Peer, s Setting) (Setting, error)

// Confirm pushes a setting to every node and returns the ids of the nodes
// holding it, the handling node included, ascending.
type Confirm func(ctx context.Context, s Setting) ([]string, error)

// NotConfirmedError reports the nodes that did not confirm a setting. Nodes
// is ascending. The text names nodes only: addresses and push errors go to the
// log.
type NotConfirmedError struct {
	Nodes []string
}

func (e *NotConfirmedError) Error() string {
	return "the platform clock setting was not confirmed by node(s) " + strings.Join(e.Nodes, ", ")
}

// SingleNode is the confirmation of a deployment without peers: the handling
// node alone holds the setting.
func SingleNode(self string) Confirm {
	return func(context.Context, Setting) ([]string, error) {
		return []string{self}, nil
	}
}

// Cluster returns the confirmation round of a cluster: it pushes the setting
// to every peer other than self in parallel and requires each peer to echo it
// exactly, all within bound. peers is read once per round.
func Cluster(self string, peers func() []Peer, push Push, bound time.Duration) Confirm {
	if self == "" || peers == nil || push == nil || bound <= 0 {
		panic("platformclock: Cluster needs a node id, a peer list, a push and a positive bound")
	}
	return func(ctx context.Context, s Setting) ([]string, error) {
		// realtime: the round's bound is a wait for peers, not business time
		round, cancel := context.WithTimeout(ctx, bound)
		defer cancel()

		var (
			mu        sync.Mutex
			confirmed = []string{self}
			failed    []string
			reasons   = map[string][]any{}
			peerOf    = map[string]Peer{}
			wg        sync.WaitGroup
		)
		record := func(p Peer, ok bool, reason string, extra ...any) {
			mu.Lock()
			defer mu.Unlock()
			if ok {
				confirmed = append(confirmed, p.NodeID)
				return
			}
			failed = append(failed, p.NodeID)
			peerOf[p.NodeID] = p
			reasons[p.NodeID] = append([]any{reason}, extra...)
		}
		for _, p := range peers() {
			if p.NodeID == self {
				continue
			}
			if p.Addr == "" {
				record(p, false, "no usable address")
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				echo, err := push(round, p, s)
				switch {
				case err != nil:
					record(p, false, err.Error())
				case echo != s:
					record(p, false, "echoed a different setting", "version", echo.Version, "writer", echo.Writer)
				default:
					record(p, true, "")
				}
			}()
		}
		wg.Wait()

		if ctx.Err() != nil {
			return nil, common.ClientGone(fmt.Errorf("platform clock confirmation stopped: %w", ctx.Err()))
		}
		if len(failed) > 0 {
			sort.Strings(failed)
			for _, id := range failed {
				r := reasons[id]
				warnNotConfirmed(peerOf[id], r[0].(string), r[1:]...)
			}
			return nil, &NotConfirmedError{Nodes: failed}
		}
		sort.Strings(confirmed)
		return confirmed, nil
	}
}

func warnNotConfirmed(p Peer, reason string, extra ...any) {
	attrs := append([]any{"pkg", "platformclock", "nodeId", p.NodeID, "addr", p.Addr, "reason", reason}, extra...)
	slog.Warn("platform clock setting not confirmed by a node", attrs...)
}
