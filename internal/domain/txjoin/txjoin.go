// Package txjoin turns an inbound transaction routing token into a joined
// transaction context, mapping verify/join failures to reusable operational
// error codes. Transport-agnostic: used by both the gRPC interceptor and the
// HTTP middleware.
package txjoin

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// Joiner is what a callback door needs to run a request as a joined request of
// the transaction its pass names.
type Joiner struct {
	signer           *token.Signer
	txMgr            spi.TransactionManager
	fence            *fence.Fence
	gate             *txgate.Registry
	maxResponseBytes int
	maxWaiters       int
	superseded       metric.Int64Counter
}

// NewJoiner builds a Joiner. maxResponseBytes is the ceiling on what one joined
// request may answer with while it holds the transaction's lock
// (CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES); maxWaiters is how many joined
// requests may queue for one transaction behind the one holding it
// (CYODA_CALLOUT_JOINED_MAX_WAITERS). meter is where the
// "cyoda.callout.superseded" counter is registered; a nil meter is the no-op
// meter, so callers that have none (tests) need not stand one up.
func NewJoiner(signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry, maxResponseBytes, maxWaiters int, meter metric.Meter) (*Joiner, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	superseded, err := meter.Int64Counter("cyoda.callout.superseded",
		metric.WithDescription("Callbacks refused or overtaken because their compute member was replaced or its callout ended"))
	if err != nil {
		return nil, err
	}
	return &Joiner{
		signer:           signer,
		txMgr:            txMgr,
		fence:            f,
		gate:             gate,
		maxResponseBytes: maxResponseBytes,
		maxWaiters:       maxWaiters,
		superseded:       superseded,
	}, nil
}

// MaxResponseBytes is the most a joined request may hold on its way out while
// it has the transaction's lock: the buffered response on the HTTP door, the
// held frames on the gRPC one. The owner's Advance and the end of the callout
// wait behind these bytes.
//
// Past it the request FAILS, with the 413 ResponseTooLargeError builds: a
// truncated answer would be a wrong answer given as an available one.
func (j *Joiner) MaxResponseBytes() int { return j.maxResponseBytes }

// ResponseTooLargeError is what a door tells a compute member whose answer
// passed the ceiling. Built once here so the status/code/message triple has one
// source of truth across the two doors. Not retryable: the same request answers
// the same bytes again, so the detail names the ceiling and the way out — page
// the read.
func (j *Joiner) ResponseTooLargeError() *common.AppError {
	return common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeJoinedResponseTooLarge,
		fmt.Sprintf("the answer of a joined request exceeds the %d bytes it may hold under the transaction's lock — page the read", j.maxResponseBytes))
}

// ErrHeldResponseTooLarge is what a door's held writer reports to a handler
// whose answer passes the ceiling. The handler's own error, if it returns one,
// is not what the caller is told: the join layer's refusal is.
var ErrHeldResponseTooLarge = errors.New("the answer of a joined request exceeds what it may hold under the transaction's lock")

// tooManyJoinedRequests is what a compute member is told when its transaction
// already has as many callbacks queued as it may have. Retryable, the capacity
// idiom of this codebase: the queue drains as the callbacks ahead of it finish.
// The figure is not in the message — which bound was met is operator
// information, and the help topic names the setting.
func tooManyJoinedRequests() *common.AppError {
	return common.Operational(http.StatusServiceUnavailable, common.ErrCodeTooManyJoinedRequests,
		"too many requests of this transaction are already waiting — retry later").AsRetryable()
}

// CheckRoom reports whether the transaction a verified pass names has room for
// another waiting request. A door that must read its request before the handler
// can run asks here first, so that a refused callback costs it no buffer;
// RunVerified asks again, at the gate itself, and that answer is the binding
// one. Between the two a request may be admitted here and refused there, or the
// other way about — the caller is told the same thing either way, and the
// refusal is retryable.
//
// It touches nothing of the transaction: the reading is of this node's queue
// for a transaction the caller's own verified pass already names, and a
// transaction with nothing queued reads the same as one that does not exist.
//
// A nil pass is not a joined request: it queues for nothing.
func (j *Joiner) CheckRoom(pass *Pass) error {
	if pass == nil {
		return nil
	}
	if j.gate.AtCapacity(pass.claims.TxRef, j.maxWaiters) {
		return tooManyJoinedRequests()
	}
	return nil
}

// Pass is a pass whose own claims have been verified. It is what a door holds
// between Verify and RunVerified; only the Joiner that verified it can read it.
type Pass struct{ claims token.Claims }

// Verify checks the pass itself: its signature, then its shape — the callout
// and the number it names — then its expiry. It needs neither the transaction's
// lock nor the request, so a door verifies before it reads anything the compute
// node sends: a pass that is refused costs no buffer.
//
// An empty pass is not a joined request: it returns (nil, nil), and
// RunVerified then runs the handler on the caller's own context.
//
// Error mapping:
//
//	token.ErrTokenExpired          → 410 TRANSACTION_EXPIRED
//	token.ErrTokenTampered/Invalid → 401 UNAUTHORIZED
//
// The pass is never logged.
func (j *Joiner) Verify(tok string) (*Pass, error) {
	if tok == "" {
		return nil, nil
	}
	claims, err := j.signer.Verify(tok)
	if err != nil {
		switch {
		case errors.Is(err, token.ErrTokenExpired):
			return nil, common.Operational(http.StatusGone, common.ErrCodeTransactionExpired, "transaction token has expired")
		default: // ErrTokenTampered or ErrTokenInvalid
			return nil, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "invalid transaction token")
		}
	}
	return &Pass{claims: *claims}, nil
}

// join resolves a verified pass into a joined transaction context: it calls
// txMgr.Join with the embedded TxRef, and then asks the fence whether the
// callout the pass names is still this compute node's. On success it returns
// the joined, admitted context. On failure it returns the original ctx
// alongside a mapped operational error, so callers always have a valid context
// regardless of outcome — a caller that dropped the error would otherwise run
// the request unfenced.
//
// Order: Join, which checks the tenant → the fence. The tenant check comes
// first so that a stolen pass tells another tenant nothing about which callouts
// exist, and a callback that arrives after the TRANSACTION has ended is
// answered TRANSACTION_NOT_FOUND as before; CALLOUT_SUPERSEDED is the answer
// while the transaction is still open.
//
// Error mapping:
//
//	spi.ErrTxTenantMismatch                      → 403 FORBIDDEN
//	spi.ErrTxNotFound/RolledBack/AlreadyCommitted → 404 TRANSACTION_NOT_FOUND
//	a pass that is no longer current              → 410 CALLOUT_SUPERSEDED
func (j *Joiner) join(ctx context.Context, pass *Pass) (context.Context, error) {
	claims := pass.claims
	joined, err := j.txMgr.Join(ctx, claims.TxRef)
	if err != nil {
		switch {
		case errors.Is(err, spi.ErrTxTenantMismatch):
			return ctx, common.Operational(http.StatusForbidden, common.ErrCodeForbidden, "transaction belongs to a different tenant")
		case errors.Is(err, spi.ErrTxNotFound),
			errors.Is(err, spi.ErrTxRolledBack),
			errors.Is(err, spi.ErrTxAlreadyCommitted):
			return ctx, common.Operational(http.StatusNotFound, common.ErrCodeTransactionNotFound, "transaction not found or no longer active")
		default:
			return ctx, common.Internal("failed to join transaction", err)
		}
	}

	pairs := make([]fence.Pair, 0, 1+len(claims.Outer))
	pairs = append(pairs, fence.Pair{Callout: claims.Callout, Major: claims.Major, Minor: claims.Minor})
	pairs = append(pairs, claims.Outer...)
	admitted, err := j.fence.Admit(joined, pairs)
	if err != nil {
		return ctx, err
	}
	return admitted, nil
}

// count records one refusal or supersession under outcome. No tenant, callout
// id or pass ever appears in the attribute: the label is the outcome class
// alone.
func (j *Joiner) count(ctx context.Context, outcome string) {
	j.superseded.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// Run verifies tok and runs handler as a joined request of the transaction it
// names. A door that has to read the request before the handler can run
// verifies first (Verify), asks CheckRoom, and calls RunVerified once it has
// the request in memory, so that a refused pass or a full queue costs it no
// buffer.
//
// An empty tok is not a joined request: handler runs on ctx as it is.
func (j *Joiner) Run(ctx context.Context, tok string, handler func(ctx context.Context)) error {
	pass, err := j.Verify(tok)
	if err != nil {
		return err
	}
	return j.RunVerified(ctx, pass, handler)
}

// RunVerified runs handler as a joined request under an already verified pass.
// The storage contract leaves it to the application to serialise its own
// concurrent operations on one transaction, so EVERY joined request — a read as
// much as a write, an entity request or not — holds the transaction's lock from
// before its first store operation until its handler returns: during a callout
// the transaction's users are its current compute node's callbacks, one at a
// time.
//
// Order: verify (done) → Join (tenant) → Admit → take the lock, which is where
// the queue's cap is applied → Check under the lock → handler → release. The
// check under the lock is the one that gives
// the right to touch the transaction: the owner's wait takes the same lock, so
// a request either passed this check before the number rose — and the owner
// waits for it — or is refused here. The engine gives the lock up for the
// length of a callout of the callback's own through the handle installed here
// (txgate.Suspend).
//
// A non-nil error means the handler did not run: a refusal, or — while the
// request was still queued for the lock, having touched nothing — ctx's own
// error, the compute node having gone away. The caller sends its response only
// after RunVerified has returned, so that a compute node that does not read its
// response holds nothing. The caller must also have the whole request in memory
// before it calls: what the lock waits on must never be the client.
//
// A nil pass is not a joined request: handler runs on ctx as it is.
func (j *Joiner) RunVerified(ctx context.Context, pass *Pass, handler func(ctx context.Context)) error {
	if pass == nil {
		handler(ctx)
		return nil
	}
	joined, err := j.join(ctx, pass)
	if err != nil {
		if errors.Is(err, fence.ErrSuperseded) {
			j.count(ctx, "refused_on_entry")
		}
		return err
	}
	txID := spi.GetTransaction(joined).ID

	// The wait for the lock is the one part of a joined request that its client
	// may still call off: nothing of the transaction has been touched yet, so a
	// compute node that goes away while its request is queued takes nothing
	// with it and the handler never runs.
	release, err := j.gate.AcquireCtx(joined, txID, j.maxWaiters)
	if err != nil {
		if errors.Is(err, txgate.ErrTooManyWaiters) {
			return tooManyJoinedRequests()
		}
		return err
	}
	// From here the request is detached from its client's cancellation: it runs
	// on the transaction of the operation it belongs to, and a compute node that
	// goes away in the middle of a statement must not take that operation's
	// connection with it. It ends by finishing or by being refused at a check.
	joined = context.WithoutCancel(joined)
	// The closure reads `release` when it runs: a Suspend/resume in between
	// stores the re-acquired lock's release through the pointer below.
	defer func() { release() }()
	joined, _ = txgate.WithHeld(joined, j.gate, txID, &release)

	if err := fence.Check(joined); err != nil {
		j.count(ctx, "refused_at_lock")
		return err
	}
	handler(joined)
	if fence.Check(joined) != nil {
		j.count(ctx, "superseded_in_progress")
	}
	return nil
}
