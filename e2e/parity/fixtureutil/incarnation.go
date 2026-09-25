package fixtureutil

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
)

// incarnationLine matches the INFO line a pnode's scheduler writes when it
// starts (slog text handler, internal/logging/logging.go).
var incarnationLine = regexp.MustCompile(`msg="scheduler started"[^\n]*\bincarnation=([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// IncarnationFromLog returns the scheduler incarnation a pnode's captured
// log announces. A scenario uses it to map a task's claim_owner to the pnode
// that holds the claim. Exactly one start line is expected per process.
func IncarnationFromLog(log string) (uuid.UUID, error) {
	m := incarnationLine.FindAllStringSubmatch(log, -1)
	switch len(m) {
	case 0:
		return uuid.Nil, errors.New(`no "scheduler started" line with an incarnation`)
	case 1:
		id, err := uuid.Parse(m[0][1])
		if err != nil {
			return uuid.Nil, fmt.Errorf("failed to parse the scheduler incarnation: %w", err)
		}
		return id, nil
	default:
		return uuid.Nil, fmt.Errorf(`%d "scheduler started" lines; want one per process`, len(m))
	}
}
