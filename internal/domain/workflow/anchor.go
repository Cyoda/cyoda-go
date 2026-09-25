package workflow

import (
	"bytes"
	"context"
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// anchorView is the cascade-anchor entity as one transaction sees it at one
// moment.
type anchorView struct {
	found bool
	txID  string // the transaction id the entity is stamped with
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
	return anchorView{found: true, txID: cur.Meta.TransactionID, data: cur.Data}, nil
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
