package workflow

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// joinedKeyT is the (unexported, collision-free) context key under which a call
// chain records that it joined the transaction on its context rather than
// began it.
type joinedKeyT struct{}

var joinedKey = joinedKeyT{}

// MarkJoinedTransaction records on ctx that the call chain joined the
// transaction it runs in — a compute node's callback participating in the
// transaction of the operation that called it out — rather than beginning it.
// Only the chain that began a transaction commits it; the engine reads this
// mark to refuse, before the transaction is flushed or committed, every step
// that would commit it on a joined chain's behalf.
//
// The mark says nothing about WHICH transaction: a joined chain never begins
// one of its own (the only step that would is the one refused), so every
// transaction such a chain can reach is the joined one. Keying on presence
// alone keeps the refusal independent of any id a door stamps elsewhere.
//
// The mark is set where ownership is decided (the entity handlers'
// begin-or-join step), so the engine's refusal and the handlers' commit rule
// answer the same question from the same place.
func MarkJoinedTransaction(ctx context.Context) context.Context {
	return context.WithValue(ctx, joinedKey, true)
}

// joinedTransaction reports whether ctx's call chain joined its transaction.
func joinedTransaction(ctx context.Context) bool {
	joined, _ := ctx.Value(joinedKey).(bool)
	return joined
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
