// Package txjoin turns an inbound transaction routing token into a joined
// request of that transaction, run in a scope of its own, mapping verify/join
// failures to reusable operational error codes. Transport-agnostic: used by
// both the gRPC interceptor and the HTTP middleware.
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
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// Joiner is what a callback door needs to run a request as a joined request of
// the transaction its pass names.
type Joiner struct {
	signer           *token.Signer
	txMgr            spi.TransactionManager
	coord            txcoord.Coordination
	maxResponseBytes int
	maxWaiters       int
	superseded       metric.Int64Counter
}

// NewJoiner builds a Joiner over the transaction coordination its requests
// open their scopes in. maxResponseBytes is the ceiling on what one joined
// request may answer with while it holds the transaction's gate
// (CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES); maxWaiters is how many joined
// requests may queue for one transaction behind the one holding it
// (CYODA_CALLOUT_JOINED_MAX_WAITERS). meter is where the
// "cyoda.callout.superseded" counter is registered; a nil meter is the no-op
// meter, so callers that have none (tests) need not stand one up.
func NewJoiner(signer *token.Signer, txMgr spi.TransactionManager, coord txcoord.Coordination, maxResponseBytes, maxWaiters int, meter metric.Meter) (*Joiner, error) {
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
		coord:            coord,
		maxResponseBytes: maxResponseBytes,
		maxWaiters:       maxWaiters,
		superseded:       superseded,
	}, nil
}

// MaxResponseBytes is the most a joined request may hold on its way out while
// it has the transaction's gate: the buffered response on the HTTP door, the
// held frames on the gRPC one. The owner's Advance and the end of the callout
// wait behind these bytes.
//
// Past it the request FAILS, with the 413 ResponseTooLargeError builds: a
// truncated answer would be a wrong answer given as an available one. The door
// decides it inside the handler, so it is the request's outcome: a write
// answered with it is discarded and marks its parent.
func (j *Joiner) MaxResponseBytes() int { return j.maxResponseBytes }

// ResponseTooLargeError is what a door tells a compute member whose answer
// passed the ceiling. Built once here so the status/code/message triple has one
// source of truth across the two doors. Not retryable: the same request answers
// the same bytes again, so the detail names the ceiling and the way out — page
// the read.
func (j *Joiner) ResponseTooLargeError() *common.AppError {
	return common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeJoinedResponseTooLarge,
		fmt.Sprintf("the answer of a joined request exceeds the %d bytes it may hold while it holds the transaction — page the read", j.maxResponseBytes))
}

// ErrHeldResponseTooLarge is what a door's held writer reports to a handler
// whose answer passes the ceiling. The handler's own error, if it returns one,
// is not what the caller is told: the join layer's refusal is.
var ErrHeldResponseTooLarge = errors.New("the answer of a joined request exceeds what it may hold while it holds the transaction")

// CheckRoom reports whether the transaction a verified pass names has room for
// another waiting request. A door that must read its request before the handler
// can run asks here first, so that a refused callback costs it no buffer;
// RunVerified asks again, at the gate itself, and that answer is the binding
// one. Between the two a request may be admitted here and refused there, or the
// other way about — the caller is told the same thing either way, and the
// refusal is retryable.
//
// It reaches nothing of the transaction — not even to resolve it. The reading
// is of this node's queue for a transaction the caller's own verified pass
// already names, and a transaction with nothing queued reads the same as one
// that does not exist, so the reading says nothing about which transactions
// there are. (The gate's own refusal, later in RunVerified, has joined and been
// admitted by then; it still mutates nothing.)
//
// A nil pass is not a joined request: it queues for nothing.
func (j *Joiner) CheckRoom(pass *Pass) error {
	if pass == nil {
		return nil
	}
	if j.coord.AtCapacity(pass.claims.TxRef, j.maxWaiters) {
		return common.TooManyJoinedRequests()
	}
	return nil
}

// Pass is a pass whose own claims have been verified. It is what a door holds
// between Verify and RunVerified; only the Joiner that verified it can read it.
type Pass struct{ claims token.Claims }

// Verify checks the pass itself: its signature, then its shape — the callout
// and the number it names — then its expiry. It needs neither the transaction's
// gate nor the request, so a door verifies before it reads anything the compute
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
// txMgr.Join with the embedded TxRef and checks an on-behalf-of request's
// user. It returns the joined context and the pairs the pass names, own pair
// first. On failure it returns the original ctx alongside a mapped operational
// error, so callers always have a valid context regardless of outcome.
//
// Order: Join, which checks the tenant → the on-behalf-of check → (Enter, in
// RunVerified) the fence. The tenant check comes first so that a stolen pass
// tells another tenant nothing about which callouts exist, and a callback that
// arrives after the TRANSACTION has ended is answered TRANSACTION_NOT_FOUND as
// before; CALLOUT_SUPERSEDED is the answer while the transaction is still open.
//
// That 403 and 404 are different answers tells a caller nothing it did not
// already hold. The transaction is the one claims.TxRef names — read from the
// pass this Joiner has just verified, never from the caller — so there is
// nothing to vary and nothing to enumerate. Reaching the 403 at all means
// presenting a JWT for one tenant together with a pass that verifies against
// this cluster's key and names another tenant's transaction, which cannot be
// minted and can only be held if a live pass has already leaked; whoever holds
// it knows that transaction exists, because the pass was minted for it. The
// three 404 causes are deliberately one answer: a transaction that never
// existed does not read differently from one that has finished.
//
// An on-behalf-of request — a principal with an Executor — joins only a
// transaction whose origin is its own user. The pass binds no caller, and an
// on-behalf-of write attributes to its own user rather than to the
// transaction's origin, so without this check a compute node presenting the
// pass with another user's on-behalf-of token would be recorded as that user
// inside the first user's transaction. It runs after Join, which supplies the
// origin, and before the fence, so a refused request absorbs nothing. A
// principal without an Executor joins unchecked (a service client, or a
// user-kind token such as the operator's).
//
// Error mapping:
//
//	spi.ErrTxTenantMismatch                      → 403 FORBIDDEN
//	spi.ErrTxNotFound/RolledBack/AlreadyCommitted → 404 TRANSACTION_NOT_FOUND
//	an on-behalf-of request, another origin       → 403 FORBIDDEN
func (j *Joiner) join(ctx context.Context, pass *Pass) (context.Context, []txcoord.Pair, error) {
	claims := pass.claims
	joined, err := j.txMgr.Join(ctx, claims.TxRef)
	if err != nil {
		switch {
		case errors.Is(err, spi.ErrTxTenantMismatch):
			return ctx, nil, common.Operational(http.StatusForbidden, common.ErrCodeForbidden, "transaction belongs to a different tenant")
		case errors.Is(err, spi.ErrTxNotFound),
			errors.Is(err, spi.ErrTxRolledBack),
			errors.Is(err, spi.ErrTxAlreadyCommitted):
			return ctx, nil, common.Operational(http.StatusNotFound, common.ErrCodeTransactionNotFound, "transaction not found or no longer active")
		default:
			return ctx, nil, common.Internal("failed to join transaction", err)
		}
	}

	if uc := spi.GetUserContext(ctx); uc != nil && uc.Executor != nil {
		own := spi.Principal{ID: uc.UserID, Kind: uc.Kind}
		if tx := spi.GetTransaction(joined); tx == nil || tx.Origin != own {
			return ctx, nil, common.Operational(http.StatusForbidden, common.ErrCodeForbidden,
				"an on-behalf-of request may join only its own user's transaction")
		}
	}

	pairs := make([]txcoord.Pair, 0, 1+len(claims.Outer))
	pairs = append(pairs, txcoord.Pair{Callout: claims.Callout, Major: claims.Major, Minor: claims.Minor})
	pairs = append(pairs, claims.Outer...)
	return joined, pairs, nil
}

// enter joins the transaction a verified pass names and opens the request's
// scope in it (Coordination.Enter: admit, take the gate, check, savepoint).
// It returns the scope and the context the handler runs on: detached from the
// request's cancellation, carrying the transaction and the scope.
func (j *Joiner) enter(ctx context.Context, pass *Pass, class txcoord.RequestClass) (txcoord.Scope, context.Context, error) {
	joined, pairs, err := j.join(ctx, pass)
	if err != nil {
		return nil, ctx, err
	}
	scope, sctx, err := j.coord.Enter(joined, spi.GetTransaction(joined).ID, pairs, class, j.maxWaiters)
	if err != nil {
		if errors.Is(err, txcoord.ErrSuperseded) {
			j.count(ctx, "refused_on_entry")
		}
		return nil, ctx, err
	}
	return scope, sctx, nil
}

// count records one refusal or supersession under outcome. No tenant, callout
// id or pass ever appears in the attribute: the label is the outcome class
// alone.
func (j *Joiner) count(ctx context.Context, outcome string) {
	j.superseded.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// Run verifies tok and runs handler as a joined request of the transaction it
// names (RunVerified). A door that has to read the request before the handler
// can run verifies first (Verify), asks CheckRoom, and calls RunVerified once
// it has the request in memory, so that a refused pass or a full queue costs
// it no buffer.
//
// An empty tok is not a joined request: handler runs on ctx as it is.
func (j *Joiner) Run(ctx context.Context, tok string, class txcoord.RequestClass, handler func(ctx context.Context) *common.Outcome) (*common.AppError, error) {
	pass, err := j.Verify(tok)
	if err != nil {
		return nil, err
	}
	return j.RunVerified(ctx, pass, class, handler)
}

// RunVerified runs handler as a joined request under an already verified pass,
// in a scope of its own: the request's writes become part of the transaction
// all together, or none of them do. class is what the request does to the
// transaction, decided by the door before the handler runs.
//
// Order: verify (done) → Join (tenant) → an on-behalf-of request's own-user
// check → Enter (admit the pass, take the transaction's gate, where the
// queue's cap and the stack rule are applied, check the pass and the scopes
// above, take a savepoint) → handler → End → release of the gate. The storage
// contract leaves it to the application to serialise its own concurrent
// operations on one transaction, so EVERY joined request — a read as much as a
// write, an entity request or not — holds the gate from before its first store
// operation until its scope has ended. The engine gives the gate up for the
// length of a callout of the request's own (txcoord.Pause).
//
// The handler's outcome decides how the scope ends. What the shared error
// funnels record on the context (common.RecordOutcome) wins; the outcome the
// handler returns is the door's backstop for an answer that passed no funnel,
// and nil when it has none. A handler that panics ends its scope as a failed
// request — discarded, and a write marks its parent — and the panic goes on
// to the recovery layer outside the door.
//
// rewrite non-nil: the scope's end overrides the handler's answer (the pass
// was superseded, the transaction lost a race or its session ended, the scope
// was marked, or the completion failed). The door sends rewrite and drops what
// the handler produced.
//
// err non-nil means the handler did not run: a refusal, or — while the request
// was still queued for the gate, having touched nothing — ctx's own error, the
// compute node having gone away. The caller sends its response only after
// RunVerified has returned, so that a compute node that does not read its
// response holds nothing. The caller must also have the whole request in
// memory before it calls: what the gate waits on must never be the client.
//
// A nil pass is not a joined request: handler runs on ctx as it is.
func (j *Joiner) RunVerified(ctx context.Context, pass *Pass, class txcoord.RequestClass, handler func(ctx context.Context) *common.Outcome) (rewrite *common.AppError, err error) {
	if pass == nil {
		handler(ctx)
		return nil, nil
	}
	scope, sctx, err := j.enter(ctx, pass, class)
	if err != nil {
		return nil, err
	}
	sctx, recorded := common.WithOutcome(sctx)
	// A panic ends the scope as a failed request while the gate is still held,
	// then goes on: recovery is the outermost layer.
	returned := false
	defer func() {
		if !returned {
			scope.End(&common.Outcome{Panicked: true})
		}
	}()
	backstop := handler(sctx)
	returned = true
	if recorded.Status == 0 && backstop != nil {
		recorded.Status = backstop.Status
		recorded.Code = backstop.Code
		recorded.Retryable = backstop.Retryable
		recorded.Err = backstop.Err
	}
	rewrite = scope.End(recorded)
	if rewrite != nil && errors.Is(rewrite, txcoord.ErrSuperseded) {
		j.count(ctx, "superseded_in_progress")
	}
	return rewrite, nil
}
