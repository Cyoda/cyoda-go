package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// anchorView is the cascade-anchor entity as one transaction sees it at one
// moment.
type anchorView struct {
	found bool
	txID  string // the transaction id the entity is stamped with
	state string
	data  []byte
}

// readAnchor reads entityID inside the transaction ctx carries. On every
// backend a write in the transaction is stamped with the transaction's id and
// a delete in the transaction reads as absent. A write or delete that another
// transaction commits after the snapshot is invisible here and fails at
// commit instead.
func readAnchor(ctx context.Context, es spi.EntityStore, entityID string) (anchorView, error) {
	cur, err := es.Get(ctx, entityID)
	if errors.Is(err, spi.ErrNotFound) {
		return anchorView{}, nil
	}
	if err != nil {
		return anchorView{}, err
	}
	return anchorView{found: true, txID: cur.Meta.TransactionID, state: cur.Meta.State, data: cur.Data}, nil
}

// writtenBy reports whether the transaction txID wrote the anchor.
func (v anchorView) writtenBy(txID string) bool { return v.found && v.txID == txID }

// adoptCallbackWrite keeps a write that a processor's callback made to the
// anchor during one dispatch, in the transaction txID the callback joined,
// when the processor returned no mutations: the engine's entity takes the
// written payload and keeps its own state and meta. before and after are the
// anchor as txID saw it around the dispatch. The dispatch wrote it when txID
// wrote it afterwards and had not before, or when its payload changed. A
// write that leaves the stored payload as it was cannot be told from no
// write, and the engine's payload stands.
func adoptCallbackWrite(entity *spi.Entity, before, after anchorView, txID string) {
	if !after.writtenBy(txID) {
		return
	}
	if before.writtenBy(txID) && bytes.Equal(before.data, after.data) {
		return
	}
	entity.Data = after.data
}

// ErrAnchorSaveInfra marks a failure of the save the engine makes before a
// callout that carries the transaction token.
var ErrAnchorSaveInfra = errors.New("saving the entity before a callout failed")

// cascadeRun is the state one engine entry point (Execute, ManualTransition,
// Loopback, a scheduled fire) keeps across its cascade.
type cascadeRun struct {
	// creating is set on Execute only: the anchor is new, and the
	// transaction holds no copy of it until the run stores one.
	creating bool
	// anchorStored records that the run's transaction has held the anchor:
	// the engine saved it, found it, or committed it at a segment boundary.
	// From then on an anchor the transaction does not hold was deleted in
	// it.
	anchorStored bool
}

// newAnchorPending reports a create whose anchor the run has not stored yet:
// the one case in which an anchor the transaction does not hold is saved.
func (r *cascadeRun) newAnchorPending() bool {
	return r.creating && !r.anchorStored
}

// saveAnchorBeforeCallout makes the transaction hold the in-memory anchor
// (spec §2.2) and sets entity.Meta.TransactionID to the stored anchor's id
// (spec §2.5). It returns the store's view of the anchor after the step: the
// "before" view adoptCallbackWrite compares the dispatch's outcome against
// (spec §2.4). No-op, returning the zero view, unless txID != "" and a
// transaction is in ctx: with an id and no transaction a save would be a
// write of its own, and a dispatch with no id sends no token.
//
// It reads the anchor in the transaction and saves the in-memory anchor only
// when its Data bytes or Meta.State differ from the read; meta-only changes
// do not count. After a save it reads the anchor again, as postgres and
// sqlite return re-encoded bytes, and stamps the entity with the
// transaction's id, the one the store stamps; otherwise the first read is the
// view, and the entity takes its id. An anchor the transaction does not hold
// is saved only on a create whose anchor the run has not stored yet
// (run.newAnchorPending). Otherwise a callback deleted it in this
// transaction: nothing is saved, Meta.TransactionID is left as it is and the
// view is "not found". Whenever the transaction holds the anchor after the
// step, run.anchorStored is set.
//
// It never acquires the txcoord gate. The caller runs it in the chain's
// scope, before beforeDispatch and before txcoord.Pause: a failure has
// dispatched nothing. Every failure is marked ErrAnchorSaveInfra.
func (e *Engine) saveAnchorBeforeCallout(ctx context.Context, entity *spi.Entity, txID string, run *cascadeRun) (anchorView, error) {
	tx := spi.GetTransaction(ctx)
	if txID == "" || tx == nil {
		return anchorView{}, nil
	}
	es, err := e.factory.EntityStore(ctx)
	if err != nil {
		return anchorView{}, fmt.Errorf("failed to get entity store before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
	}
	cur, err := readAnchor(ctx, es, entity.Meta.ID)
	switch {
	case err != nil:
		return anchorView{}, fmt.Errorf("failed to read entity before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
	case !cur.found:
		if !run.newAnchorPending() {
			return cur, nil
		}
	case bytes.Equal(cur.data, entity.Data) && cur.state == entity.Meta.State:
		run.anchorStored = true
		entity.Meta.TransactionID = cur.txID
		return cur, nil
	}
	if _, err := es.Save(ctx, entity); err != nil {
		return anchorView{}, fmt.Errorf("failed to save entity before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
	}
	run.anchorStored = true
	entity.Meta.TransactionID = tx.ID
	saved, err := readAnchor(ctx, es, entity.Meta.ID)
	if err != nil {
		return anchorView{}, fmt.Errorf("failed to re-read entity after saving it before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
	}
	return saved, nil
}
