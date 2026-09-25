package workflow

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// joinedTxKeyT is the (unexported, collision-free) context key under which a
// call chain records the transaction it joined rather than began.
type joinedTxKeyT struct{}

var joinedTxKey = joinedTxKeyT{}

// WithJoinedTransaction records on ctx that the call chain joined txID — a
// compute node's callback participating in the transaction of the operation
// that called it out — rather than beginning it. Only the chain that began a
// transaction commits it; the engine reads this mark to refuse, before
// anything is written, every step that would commit txID on a joined chain's
// behalf.
//
// The mark is set where ownership is decided (the entity handlers' begin-or-join
// step), so the engine's refusal and the handlers' commit rule answer the same
// question from the same place.
func WithJoinedTransaction(ctx context.Context, txID string) context.Context {
	return context.WithValue(ctx, joinedTxKey, txID)
}

// joinedTransaction reports whether ctx's call chain joined txID.
func joinedTransaction(ctx context.Context, txID string) bool {
	joined, _ := ctx.Value(joinedTxKey).(string)
	return joined != "" && joined == txID
}

// refuseCommitInJoinedTransaction is the refusal a COMMIT_BEFORE_DISPATCH
// processor meets on a chain that joined the transaction it would commit.
// A 4xx the callback's own configuration explains: it names the workflow and
// the processor, and nothing of the transaction.
func refuseCommitInJoinedTransaction(workflow string, proc string) *common.AppError {
	return common.Operational(http.StatusConflict, common.ErrCodeCommitInJoinedTransaction,
		fmt.Sprintf("workflow %q processor %q is COMMIT_BEFORE_DISPATCH, which commits the transaction it runs in; "+
			"a callback runs in the transaction it joined, and only the operation that began it commits it", workflow, proc))
}
