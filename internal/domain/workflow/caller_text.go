package workflow

import (
	"net/http"
	"strings"

	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// refuseNULCallerText refuses a caller's transition name or If-Match that holds
// U+0000, before the run records anything. Both reach the run's audit events,
// and the audit trail cannot hold U+0000: PostgreSQL refuses it in jsonb. Every
// other text an event carries is checked where it enters — a workflow at
// import, a compute answer at dispatch, a collection body at its handler — so
// this is the last entry. Every run that takes caller text calls it first
// (Execute, manualTransition, loopback), so it covers every door, a joined
// write included.
func refuseNULCallerText(transitionName string, ifMatch IfMatch) error {
	if strings.ContainsRune(transitionName, 0) {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "the transition name holds U+0000")
	}
	if strings.ContainsRune(ifMatch.Expected, 0) {
		return common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "the If-Match value holds U+0000")
	}
	return nil
}
