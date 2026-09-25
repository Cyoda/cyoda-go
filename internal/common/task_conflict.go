package common

import (
	"context"
	"errors"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// TaskConflictRetries is how many more times an owned operation that removes
// scheduled tasks runs after a first-committer-wins refusal (spi.ErrConflict).
// The scheduler writes task rows when it claims, stamps, records, fails and
// gives back runs, so an entity delete or a workflow import can lose a race
// with it. Each run starts a new transaction, so running it again is safe.
const TaskConflictRetries = 3

// RetryOnTaskConflict runs op. While op fails with spi.ErrConflict it runs op
// again, at most TaskConflictRetries more times, and returns op's last error.
//
// owned=false means op joined a transaction that another request owns. Such
// an op runs once: its conflict belongs to that owner, whose commit reports
// it. A done ctx also stops the retries.
func RetryOnTaskConflict(ctx context.Context, owned bool, op func() error) error {
	err := op()
	if !owned {
		return err
	}
	for retry := 0; retry < TaskConflictRetries; retry++ {
		if err == nil || !errors.Is(err, spi.ErrConflict) || ctx.Err() != nil {
			return err
		}
		err = op()
	}
	return err
}
