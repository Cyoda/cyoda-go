package observability

import (
	"context"
	"errors"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// TracingTransactionManager wraps a TransactionManager with OTel spans and metrics.
type TracingTransactionManager struct {
	inner       spi.TransactionManager
	tracer      trace.Tracer
	txDuration  metric.Float64Histogram
	txActive    metric.Int64UpDownCounter
	txConflicts metric.Int64Counter
	opBegin     metric.MeasurementOption
	opCommit    metric.MeasurementOption
	opRollback  metric.MeasurementOption
}

// NewTracingTransactionManager returns a TracingTransactionManager that decorates inner
// with OTel tracing spans and metrics.
func NewTracingTransactionManager(inner spi.TransactionManager, meter metric.Meter) *TracingTransactionManager {
	tracer := Tracer()

	txDuration, err := meter.Float64Histogram("cyoda.tx.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Transaction operation duration"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	instrErr("cyoda.tx.duration", err)
	txActive, err := meter.Int64UpDownCounter("cyoda.tx.active",
		metric.WithDescription("Number of active transactions"))
	instrErr("cyoda.tx.active", err)
	txConflicts, err := meter.Int64Counter("cyoda.tx.conflicts",
		metric.WithDescription("Transaction serialization conflicts"))
	instrErr("cyoda.tx.conflicts", err)

	return &TracingTransactionManager{
		inner:       inner,
		tracer:      tracer,
		txDuration:  txDuration,
		txActive:    txActive,
		txConflicts: txConflicts,
		opBegin:     metric.WithAttributes(AttrTxOp.String("begin")),
		opCommit:    metric.WithAttributes(AttrTxOp.String("commit")),
		opRollback:  metric.WithAttributes(AttrTxOp.String("rollback")),
	}
}

func (t *TracingTransactionManager) Begin(ctx context.Context) (string, context.Context, error) {
	ctx, span := t.tracer.Start(ctx, "tx.begin")
	start := time.Now()
	txID, txCtx, err := t.inner.Begin(ctx)
	t.txDuration.Record(ctx, time.Since(start).Seconds(), t.opBegin)
	if err != nil {
		span.RecordError(err)
		span.End()
		return "", nil, err
	}
	span.SetAttributes(AttrTxID.String(txID))
	t.txActive.Add(ctx, 1)
	span.End()
	return txID, txCtx, nil
}

func (t *TracingTransactionManager) Commit(ctx context.Context, txID string) error {
	ctx, span := t.tracer.Start(ctx, "tx.commit", trace.WithAttributes(AttrTxID.String(txID)))
	defer span.End()
	start := time.Now()
	err := t.inner.Commit(ctx, txID)
	t.txDuration.Record(ctx, time.Since(start).Seconds(), t.opCommit)
	if err != nil {
		span.RecordError(err)
		if isConflict(err) {
			span.SetAttributes(attribute.Bool("tx.conflict", true))
			t.txConflicts.Add(ctx, 1)
		}
		return err
	}
	t.txActive.Add(ctx, -1)
	return nil
}

func (t *TracingTransactionManager) Rollback(ctx context.Context, txID string) error {
	ctx, span := t.tracer.Start(ctx, "tx.rollback", trace.WithAttributes(AttrTxID.String(txID)))
	defer span.End()
	start := time.Now()
	err := t.inner.Rollback(ctx, txID)
	t.txDuration.Record(ctx, time.Since(start).Seconds(), t.opRollback)
	if err != nil {
		span.RecordError(err)
		return err
	}
	t.txActive.Add(ctx, -1)
	return nil
}

func (t *TracingTransactionManager) Join(ctx context.Context, txID string) (context.Context, error) {
	return t.inner.Join(ctx, txID)
}

func (t *TracingTransactionManager) GetSubmitTime(ctx context.Context, txID string) (time.Time, error) {
	return t.inner.GetSubmitTime(ctx, txID)
}

func (t *TracingTransactionManager) Savepoint(ctx context.Context, txID string) (string, error) {
	ctx, span := t.tracer.Start(ctx, "tx.savepoint", trace.WithAttributes(AttrTxID.String(txID)))
	defer span.End()
	spID, err := t.inner.Savepoint(ctx, txID)
	if err != nil {
		span.RecordError(err)
		return "", err
	}
	span.SetAttributes(attribute.String("tx.savepoint_id", spID))
	return spID, nil
}

func (t *TracingTransactionManager) RollbackToSavepoint(ctx context.Context, txID string, savepointID string) error {
	ctx, span := t.tracer.Start(ctx, "tx.rollback_to_savepoint", trace.WithAttributes(
		AttrTxID.String(txID),
		attribute.String("tx.savepoint_id", savepointID),
	))
	defer span.End()
	err := t.inner.RollbackToSavepoint(ctx, txID, savepointID)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

func (t *TracingTransactionManager) ReleaseSavepoint(ctx context.Context, txID string, savepointID string) error {
	ctx, span := t.tracer.Start(ctx, "tx.release_savepoint", trace.WithAttributes(
		AttrTxID.String(txID),
		attribute.String("tx.savepoint_id", savepointID),
	))
	defer span.End()
	err := t.inner.ReleaseSavepoint(ctx, txID, savepointID)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// isConflict reports whether err is a transaction serialization conflict.
func isConflict(err error) bool {
	return errors.Is(err, spi.ErrConflict)
}
