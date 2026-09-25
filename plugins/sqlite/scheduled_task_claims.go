package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// Not implemented yet: each of these answers errors.ErrUnsupported.

func (s *scheduledTaskStore) ClaimDue(context.Context, spi.ClaimRequest) ([]spi.ScheduledTask, error) {
	return nil, errors.ErrUnsupported
}

func (s *scheduledTaskStore) Heartbeat(context.Context, uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) RetireOwner(context.Context, uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) SweepOwners(context.Context, time.Duration) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) GiveBackIdle(context.Context, uuid.UUID, []uuid.UUID) (int, error) {
	return 0, errors.ErrUnsupported
}

func (s *scheduledTaskStore) MarkUnsafe(context.Context, spi.TaskRef) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) RecordAttempt(context.Context, spi.TaskRef, spi.Attempt) error {
	return errors.ErrUnsupported
}

func (s *scheduledTaskStore) SweepMarks(context.Context) error { return errors.ErrUnsupported }
