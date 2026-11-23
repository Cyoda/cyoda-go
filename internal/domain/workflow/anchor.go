package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	spi "github.com/cyoda/cyoda-go-spi"
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

// ErrAnchorSaveInfra marks a failure of the save the engine makes before a
// callout that carries the transaction token.
var ErrAnchorSaveInfra = errors.New("saving the entity before a callout failed")

// saveAnchorBeforeCallout makes the transaction hold the in-memory anchor
// as it is before the callout and sets entity.Meta.TransactionID to the
// transaction's id: the anchor, and the payload made from it, are the version
// the operation makes, whether or not this step wrote it. No-op unless txID != "" and a transaction is in ctx: with an id
// and no transaction a save would be a write of its own, and a dispatch with no
// id sends no token.
//
// It reads the anchor in the transaction and saves the in-memory anchor only
// when its Data bytes or Meta.State differ from the read; meta-only changes do
// not count. The save records the request's transition name (stampedForSave);
// the in-memory anchor keeps the start version's. An anchor the transaction
// does not hold is a create's that is not stored yet, and is saved: a callout
// never deletes the entity it runs for.
//
// It never acquires the txcoord gate. The caller runs it in the chain's scope,
// before beforeDispatch and before txcoord.Pause: a failure has dispatched
// nothing. Every failure is marked ErrAnchorSaveInfra.
func (e *Engine) saveAnchorBeforeCallout(ctx context.Context, entity *spi.Entity, txID string) error {
	tx := spi.GetTransaction(ctx)
	if txID == "" || tx == nil {
		return nil
	}
	es, err := e.factory.EntityStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to get entity store before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
	}
	cur, err := readAnchor(ctx, es, entity.Meta.ID)
	if err != nil {
		return fmt.Errorf("failed to read entity before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
	}
	if !cur.found || !bytes.Equal(cur.data, entity.Data) || cur.state != entity.Meta.State {
		if _, err := es.Save(ctx, stampedForSave(ctx, entity)); err != nil {
			return fmt.Errorf("failed to save entity before a callout: %w", errors.Join(ErrAnchorSaveInfra, err))
		}
	}
	entity.Meta.TransactionID = tx.ID
	return nil
}
